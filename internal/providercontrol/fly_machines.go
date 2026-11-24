package providercontrol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

const (
	FlyMachinesAdapterID = "fly-machines-v1"
	FlyMachinesRegion    = "fra"
	FlyMachinesIsolation = "firecracker"
)

var (
	ErrFlyMachineSpec           = errors.New("providercontrol: invalid Fly Machine specification")
	ErrFlyMachineMemory         = errors.New("providercontrol: Fly Machines supports 256 or 512 MiB for this shared CPU profile")
	ErrFlyMachineIsolation      = errors.New("providercontrol: Fly Machine requires Firecracker without shared Docker or a Docker socket")
	ErrFlyMachineOwnership      = errors.New("providercontrol: Fly Machine does not match workload custody")
	ErrFlyMachinePolicyDrift    = errors.New("providercontrol: Fly Machine configuration failed policy readback")
	ErrFlyMachineNotFound       = errors.New("providercontrol: Fly Machine not found")
	ErrFlyMachineAbsencePending = errors.New("providercontrol: Fly Machine absence not yet confirmed")
	ErrFlyMachineBackup         = errors.New("providercontrol: Fly Machine R2 backup or restore failed custody verification")
)

type FlyMachineRegionError struct {
	Region string
}

func (*FlyMachineRegionError) Error() string {
	return "providercontrol: Fly Machine requires explicit Frankfurt (fra) placement"
}

type FlyMachineOwner struct {
	TenantID    string
	WorkloadID  string
	SlotRef     string
	CustodyHash string
}

type FlyMachineHandle struct {
	App       string
	MachineID string
	Owner     FlyMachineOwner
}

type FlyMachineSpec struct {
	Region         string
	IsolationClass string
	MemoryMB       int
	Image          string
	InternalPort   int
	DockerSocket   bool
	SharedDocker   bool
}

type FlyMachineHealth string

const (
	FlyMachineHealthUnknown   FlyMachineHealth = "unknown"
	FlyMachineHealthHealthy   FlyMachineHealth = "healthy"
	FlyMachineHealthUnhealthy FlyMachineHealth = "unhealthy"
)

type FlyMachineObservation struct {
	State  string
	Health FlyMachineHealth
}

type FlyMachineAbsence struct {
	Handle     FlyMachineHandle
	ObservedAt time.Time
}

type FlyBackupArtifact struct {
	Ref    string
	SHA256 string
}

type FlyBackupManifest struct {
	Source   FlyMachineHandle
	Artifact FlyBackupArtifact
}

// FlyMachineR2Store resolves each opaque artifact reference inside the given
// workload's EU R2 custody, and verifies its digest before applying a restore.
type FlyMachineR2Store interface {
	Backup(context.Context, FlyMachineHandle) (FlyBackupArtifact, error)
	Restore(context.Context, FlyMachineHandle, FlyBackupArtifact) error
}

// FlyMachinesAdapter operates in one dedicated, privately routed Flycast app
// with an app-scoped token. Workload admission must supply a tenant-isolated
// Fly network, a curated image, and handles held by the workload ledger, never
// handles supplied by a user. Only this control plane may mutate the app's
// machines or their ownership metadata.
// It is not a server-lease executor and does not write or release custody.
type FlyMachinesAdapter struct {
	client flyMachinesClient
	app    string
	owner  FlyMachineOwner
	r2     FlyMachineR2Store
}

func NewFlyMachinesAdapter(app, token string, owner FlyMachineOwner, r2 FlyMachineR2Store) (*FlyMachinesAdapter, error) {
	return newFlyMachinesAdapter(app, token, owner, r2, flyMachinesAPIBase)
}

func newFlyMachinesAdapter(app, token string, owner FlyMachineOwner, r2 FlyMachineR2Store, apiBase string) (*FlyMachinesAdapter, error) {
	if !flyMachinePathID(app) || token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, ErrFlyMachineSpec
	}
	for _, value := range owner.metadata() {
		if strings.TrimSpace(value) != value || value == "" {
			return nil, ErrFlyMachineOwnership
		}
	}
	return &FlyMachinesAdapter{client: newFlyMachinesClient(apiBase, token), app: app, owner: owner, r2: r2}, nil
}

// Provision issues one create, with no retry or adoption. Dispatch must be
// durably authorized at most once by the caller. A returned handle must be
// retained for reconciliation even if configuration readback fails.
func (a *FlyMachinesAdapter) Provision(ctx context.Context, spec FlyMachineSpec) (FlyMachineHandle, error) {
	if spec.Region != FlyMachinesRegion {
		return FlyMachineHandle{}, &FlyMachineRegionError{Region: spec.Region}
	}
	if spec.IsolationClass != FlyMachinesIsolation || spec.DockerSocket || spec.SharedDocker {
		return FlyMachineHandle{}, ErrFlyMachineIsolation
	}
	if !flyMachineMemoryAllowed(spec.MemoryMB) {
		return FlyMachineHandle{}, ErrFlyMachineMemory
	}
	if spec.Image == "" || strings.ContainsAny(spec.Image, " \t\r\n") || spec.InternalPort < 1 || spec.InternalPort > 65535 {
		return FlyMachineHandle{}, ErrFlyMachineSpec
	}
	request := flyMachineCreate{
		Region: FlyMachinesRegion,
		Config: flyMachineConfig{
			Image:    spec.Image,
			Metadata: a.owner.metadata(),
			Guest:    flyMachineGuest{CPUKind: "shared", CPUs: 1, MemoryMB: spec.MemoryMB, MaxMemoryMB: spec.MemoryMB},
			Restart:  &flyMachineRestart{Policy: "on-failure"},
			Services: []flyMachineService{{
				Protocol: "tcp", InternalPort: spec.InternalPort,
				Autostop: json.RawMessage(`"stop"`), Autostart: true,
				Ports: []flyMachinePort{{Port: 80, Handlers: []string{"http"}}},
			}},
			Checks: map[string]flyMachineCheck{"workload": {Type: "tcp", Port: spec.InternalPort, Interval: "15s", Timeout: "2s"}},
		},
	}
	var created flyMachine
	if err := a.client.request(ctx, http.MethodPost, a.machinesPath(), request, &created); err != nil {
		return FlyMachineHandle{}, err
	}
	if !flyMachinePathID(created.ID) {
		return FlyMachineHandle{}, ErrFlyMachineResponse
	}
	handle := FlyMachineHandle{App: a.app, MachineID: created.ID, Owner: a.owner}
	machine, err := a.readOwnedInFrankfurt(ctx, handle)
	if err != nil {
		if errors.Is(err, ErrFlyMachineNotFound) {
			return handle, ErrFlyMachineResponse
		}
		return handle, err
	}
	if !machine.satisfiesPolicy() || machine.Config.Image != spec.Image ||
		machine.Config.Guest.MemoryMB != spec.MemoryMB || machine.Config.Services[0].InternalPort != spec.InternalPort {
		return handle, ErrFlyMachinePolicyDrift
	}
	return handle, nil
}

func (a *FlyMachinesAdapter) Start(ctx context.Context, handle FlyMachineHandle) error {
	machine, err := a.readOwnedInFrankfurt(ctx, handle)
	if err != nil {
		return err
	}
	if !machine.satisfiesPolicy() {
		return ErrFlyMachinePolicyDrift
	}
	return a.client.request(ctx, http.MethodPost, a.machinePath(handle)+"/start", nil, nil)
}

func (a *FlyMachinesAdapter) Stop(ctx context.Context, handle FlyMachineHandle) error {
	if _, err := a.readOwned(ctx, handle); err != nil {
		return err
	}
	return a.client.request(ctx, http.MethodPost, a.machinePath(handle)+"/stop", nil, nil)
}

func (a *FlyMachinesAdapter) Wake(ctx context.Context, handle FlyMachineHandle) error {
	return a.Start(ctx, handle)
}

func (a *FlyMachinesAdapter) Observe(ctx context.Context, handle FlyMachineHandle) (FlyMachineObservation, error) {
	machine, err := a.readOwnedInFrankfurt(ctx, handle)
	if err != nil {
		return FlyMachineObservation{}, err
	}
	return FlyMachineObservation{State: machine.State, Health: machine.health()}, nil
}

func (a *FlyMachinesAdapter) Backup(ctx context.Context, handle FlyMachineHandle) (FlyBackupManifest, error) {
	if _, err := a.readOwnedInFrankfurt(ctx, handle); err != nil {
		return FlyBackupManifest{}, err
	}
	if a.r2 == nil {
		return FlyBackupManifest{}, ErrFlyMachineBackup
	}
	artifact, err := a.r2.Backup(ctx, handle)
	if err != nil || !artifact.valid() {
		return FlyBackupManifest{}, ErrFlyMachineBackup
	}
	return FlyBackupManifest{Source: handle, Artifact: artifact}, nil
}

func (a *FlyMachinesAdapter) Restore(ctx context.Context, handle FlyMachineHandle, manifest FlyBackupManifest) error {
	if manifest.Source != handle {
		return ErrFlyMachineOwnership
	}
	machine, err := a.readOwnedInFrankfurt(ctx, handle)
	if err != nil {
		return err
	}
	if !machine.satisfiesPolicy() {
		return ErrFlyMachinePolicyDrift
	}
	if a.r2 == nil || !manifest.Artifact.valid() || a.r2.Restore(ctx, handle, manifest.Artifact) != nil {
		return ErrFlyMachineBackup
	}
	return nil
}

// Decommission returns a provider observation for the exact recorded handle,
// not a custody release. An authorized retry may observe that the previous
// deletion already completed. A successful DELETE alone is never proof.
func (a *FlyMachinesAdapter) Decommission(ctx context.Context, handle FlyMachineHandle) (FlyMachineAbsence, error) {
	machine, err := a.readOwned(ctx, handle)
	if errors.Is(err, ErrFlyMachineNotFound) || (err == nil && flyMachineAbsent(machine)) {
		return FlyMachineAbsence{Handle: handle, ObservedAt: time.Now().UTC()}, nil
	}
	if err != nil {
		return FlyMachineAbsence{}, err
	}
	err = a.client.request(ctx, http.MethodDelete, a.machinePath(handle)+"?force=true", nil, nil)
	if err != nil && !errors.Is(err, ErrFlyMachineNotFound) {
		return FlyMachineAbsence{}, err
	}
	machine, err = a.readOwned(ctx, handle)
	if errors.Is(err, ErrFlyMachineNotFound) || (err == nil && flyMachineAbsent(machine)) {
		return FlyMachineAbsence{Handle: handle, ObservedAt: time.Now().UTC()}, nil
	}
	if err != nil {
		return FlyMachineAbsence{}, err
	}
	return FlyMachineAbsence{}, ErrFlyMachineAbsencePending
}

func flyMachineAbsent(machine flyMachine) bool {
	return machine.State == "destroyed"
}

func (a *FlyMachinesAdapter) machinesPath() string {
	return "/apps/" + a.app + "/machines"
}

func (a *FlyMachinesAdapter) machinePath(handle FlyMachineHandle) string {
	return a.machinesPath() + "/" + handle.MachineID
}

func (a *FlyMachinesAdapter) readOwned(ctx context.Context, handle FlyMachineHandle) (flyMachine, error) {
	if handle.App != a.app || handle.Owner != a.owner || !flyMachinePathID(handle.MachineID) {
		return flyMachine{}, ErrFlyMachineOwnership
	}
	var machine flyMachine
	if err := a.client.request(ctx, http.MethodGet, a.machinePath(handle), nil, &machine); err != nil {
		return flyMachine{}, err
	}
	if machine.ID != handle.MachineID {
		return flyMachine{}, ErrFlyMachineOwnership
	}
	for key, value := range a.owner.metadata() {
		if machine.Config.Metadata[key] != value {
			return flyMachine{}, ErrFlyMachineOwnership
		}
	}
	return machine, nil
}

func (a *FlyMachinesAdapter) readOwnedInFrankfurt(ctx context.Context, handle FlyMachineHandle) (flyMachine, error) {
	machine, err := a.readOwned(ctx, handle)
	if err != nil {
		return flyMachine{}, err
	}
	if machine.Region != FlyMachinesRegion {
		return flyMachine{}, &FlyMachineRegionError{Region: machine.Region}
	}
	return machine, nil
}

func (owner FlyMachineOwner) metadata() map[string]string {
	return map[string]string{
		"tenant_id": owner.TenantID, "workload_id": owner.WorkloadID,
		"slot_ref": owner.SlotRef, "custody_hash": owner.CustodyHash,
	}
}

func flyMachineMemoryAllowed(memoryMB int) bool {
	return memoryMB == 256 || memoryMB == 512
}

func (artifact FlyBackupArtifact) valid() bool {
	if !flyBackupRefValid(artifact.Ref) {
		return false
	}
	return len(artifact.SHA256) == 64 &&
		strings.Trim(artifact.SHA256, "0123456789abcdef") == ""
}

func flyBackupRefValid(ref string) bool {
	if ref == "" || len(ref) > 512 || strings.TrimSpace(ref) != ref ||
		strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "..") || strings.Contains(ref, "//") ||
		strings.ContainsAny(ref, " \t\r\n\\\x00") {
		return false
	}
	for i := 0; i < len(ref); i++ {
		if ref[i] < 0x20 || ref[i] == 0x7f {
			return false
		}
	}
	return true
}
