package providercontrol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

const (
	IONOSSharedPoolAdapterID = "ionos-shared-pool-v1"
	IONOSSharedPoolLocation  = "de/fra/2"
)

// IONOSSharedPoolRefusal is a policy denial, never a successful lifecycle receipt.
type IONOSSharedPoolRefusal struct {
	Code   string
	Repair string
}

func (e *IONOSSharedPoolRefusal) Error() string {
	return IONOSSharedPoolAdapterID + ": " + e.Code + ": " + e.Repair
}
func ionosPoolRefuse(code, repair string) error {
	return &IONOSSharedPoolRefusal{Code: code, Repair: repair}
}

// IONOSSharedPoolNode describes an already admitted worker, not permission to
// purchase a server. ReservedMemoryMB includes the OS and other host workloads.
// Slots is the operator's density ceiling (30–40), including stopped workloads.
type IONOSSharedPoolNode struct {
	ID               string `json:"id"`
	Location         string `json:"location"`
	MemoryMB         int    `json:"memory_mb"`
	ReservedMemoryMB int    `json:"reserved_memory_mb"`
	Slots            int    `json:"slots"`
	Orchestrator     string `json:"orchestrator"`
}

func (n IONOSSharedPoolNode) validate() error {
	switch {
	case n.Location != IONOSSharedPoolLocation:
		return ionosPoolRefuse("location_refused", "pin the worker to de/fra/2")
	case !ionosPoolIdentifier.MatchString(n.ID):
		return ionosPoolRefuse("node_refused", "supply an exact admitted worker ID")
	case n.MemoryMB < 32*1024 || n.MemoryMB > 64*1024 || n.ReservedMemoryMB < 0 || n.ReservedMemoryMB >= n.MemoryMB:
		return ionosPoolRefuse("node_memory_refused", "use a 32–64 GB worker with explicit available RAM")
	case n.Slots < 30 || n.Slots > 40:
		return ionosPoolRefuse("node_density_refused", "set a ceiling of 30–40 lightweight apps")
	case n.Orchestrator != "":
		return ionosPoolRefuse("orchestrator_refused", "use a standalone gVisor or Incus worker; no Kubernetes, k3s or Talos")
	}
	return nil
}

var ionosPoolIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// IONOSSharedPoolWorkload is an immutable generation. Handle must come from the
// governed command's resource generation, and must never be recycled.
type IONOSSharedPoolWorkload struct {
	TenantID             string `json:"tenant_id"`
	Handle               string `json:"handle"`
	Location             string `json:"location"`
	Image                string `json:"image"`
	MemoryMB             int    `json:"memory_mb"`
	Runtime              string `json:"runtime"`
	RequiresDockerSocket bool   `json:"requires_docker_socket"`
}

// IONOSSharedPoolSpec leaves no caller-supplied mount, network, schema, host
// namespace, privileged flag or Docker socket escape hatch. The node agent must
// enforce these boundaries, including schema roles without cross-tenant grants.
type IONOSSharedPoolSpec struct {
	Workload         IONOSSharedPoolWorkload `json:"workload"`
	NodeID           string                  `json:"node_id"`
	RuntimeBinary    string                  `json:"runtime_binary"`
	Schema           string                  `json:"schema"`
	VolumePrefix     string                  `json:"volume_prefix"`
	NetworkNamespace string                  `json:"network_namespace"`
	TenantLabel      string                  `json:"tenant_label"`
}

func ionosPoolSpec(node IONOSSharedPoolNode, w IONOSSharedPoolWorkload) (IONOSSharedPoolSpec, error) {
	if w.TenantID == "" || strings.TrimSpace(w.TenantID) != w.TenantID || len(w.TenantID) > 256 || strings.ContainsAny(w.TenantID, "\x00\r\n") || !ionosPoolIdentifier.MatchString(w.Handle) {
		return IONOSSharedPoolSpec{}, ionosPoolRefuse("identity_refused", "supply the authenticated tenant and exact generation handle")
	}
	if w.Location != IONOSSharedPoolLocation {
		return IONOSSharedPoolSpec{}, ionosPoolRefuse("location_refused", "pin the workload to de/fra/2")
	}
	if w.RequiresDockerSocket {
		return IONOSSharedPoolSpec{}, ionosPoolRefuse("docker_socket_refused", "use a curated app without Docker socket access")
	}
	binary := ""
	switch w.Runtime {
	case "gvisor", "runsc":
		w.Runtime = "gvisor"
		binary = "runsc"
	case "incus":
		binary = "incus"
	default:
		return IONOSSharedPoolSpec{}, ionosPoolRefuse("isolation_runtime_refused", "use gvisor (runsc) or incus; bare shared Docker and runc are forbidden")
	}
	if w.MemoryMB < 1 || w.MemoryMB > 512 {
		return IONOSSharedPoolSpec{}, ionosPoolRefuse("workload_memory_refused", "select a curated app with a positive memory limit at most 512 MB")
	}
	if strings.TrimSpace(w.Image) != w.Image || w.Image == "" {
		return IONOSSharedPoolSpec{}, ionosPoolRefuse("image_refused", "supply the curated image admitted by the governed command")
	}
	tenantHash := sha256.Sum256([]byte(w.TenantID))
	workloadHash := sha256.Sum256([]byte(w.TenantID + "\x00" + w.Handle))
	tenant := hex.EncodeToString(tenantHash[:])
	workload := hex.EncodeToString(workloadHash[:])
	return IONOSSharedPoolSpec{Workload: w, NodeID: node.ID, RuntimeBinary: binary,
		Schema: "tenant_" + tenant[:24] + "_app_" + workload[:24], VolumePrefix: "tenants/" + tenant + "/workloads/" + w.Handle + "/",
		NetworkNamespace: "net-" + workload, TenantLabel: w.TenantID}, nil
}

type IONOSSharedPoolAction string

const (
	IONOSPoolProvision    IONOSSharedPoolAction = "provision"
	IONOSPoolStart        IONOSSharedPoolAction = "start"
	IONOSPoolStop         IONOSSharedPoolAction = "stop"
	IONOSPoolObserve      IONOSSharedPoolAction = "observe"
	IONOSPoolBackup       IONOSSharedPoolAction = "backup"
	IONOSPoolRestore      IONOSSharedPoolAction = "restore"
	IONOSPoolDecommission IONOSSharedPoolAction = "decommission"
)

// IONOSSharedPoolBackupManifest is a node-owned, secret-free R2 manifest. The
// verifier must authenticate the manifest and check the exact object version,
// size and SHA256 in R2; neither an arbitrary URL nor an ETag is proof.
type IONOSSharedPoolBackupManifest struct {
	Store     string              `json:"store"`
	Bucket    string              `json:"bucket"`
	Key       string              `json:"key"`
	Version   string              `json:"version"`
	SHA256    string              `json:"sha256"`
	SizeBytes int64               `json:"size_bytes"`
	Spec      IONOSSharedPoolSpec `json:"spec"`
}

type IONOSSharedPoolCommand struct {
	Action IONOSSharedPoolAction          `json:"action"`
	Spec   IONOSSharedPoolSpec            `json:"spec"`
	Backup *IONOSSharedPoolBackupManifest `json:"backup,omitempty"`
}

type IONOSSharedPoolObservation struct {
	Spec        IONOSSharedPoolSpec `json:"spec"`
	State       string              `json:"state"`
	EvidenceRef string              `json:"evidence_ref"`
	// IsolationRemoved means the exact workload's process, volume and network
	// have gone and its schema grants/data have been removed without touching
	// the owning tenant's other workloads.
	IsolationRemoved bool                           `json:"isolation_removed"`
	Backup           *IONOSSharedPoolBackupManifest `json:"backup,omitempty"`
}

// IONOSSharedPoolRuntime is the trusted, authenticated node-agent seam. Execute
// must enforce the supplied isolation spec and exact handle on every verb.
// Provision is an idempotent ensure of this immutable generation, never a blind
// create. Decommission must remove only this handle; absence is read separately.
// Backup/restore use node-held R2 custody and never return credentials or bytes.
type IONOSSharedPoolRuntime interface {
	Execute(context.Context, IONOSSharedPoolCommand) (IONOSSharedPoolObservation, error)
}

// IONOSSharedPoolAuthorizer binds the authenticated identity, positive backend
// entitlement, curated image and immutable command to the action and spec.
// Mutations require a current side-effecting claim; Provision also requires the
// durable dispatch permit. It must revalidate under the node lock on every call.
// Strings in Workload are never authentication by themselves.
type IONOSSharedPoolAuthorizer interface {
	AuthorizeIONOSSharedPool(context.Context, IONOSSharedPoolCommand) error
}

type IONOSSharedPoolBackupVerifier interface {
	VerifyIONOSSharedPoolBackup(context.Context, IONOSSharedPoolBackupManifest) error
}

// IONOSSharedPoolRecord preserves pending allocations after uncertain dispatch
// and terminal tombstones after absence. Released generations cannot be reused.
type IONOSSharedPoolRecord struct {
	Spec               IONOSSharedPoolSpec
	Released           bool
	AbsenceEvidenceRef string
}

// IONOSSharedPoolLockedCustody is a durable node-scoped store. Put commits before
// returning, even if the enclosing callback later fails. Records returns copies.
// A production implementation must persist records across process restarts.
type IONOSSharedPoolLockedCustody interface {
	Records(context.Context) ([]IONOSSharedPoolRecord, error)
	Put(context.Context, IONOSSharedPoolRecord) error
}

// IONOSSharedPoolCustody serializes all adapter instances for this worker,
// including the entire agent call/readback sequence. A process-local mutex is
// insufficient in production. The lock must remain held until the callback
// returns; loss of its fencing authority must cancel the operation.
type IONOSSharedPoolCustody interface {
	WithNode(context.Context, string, func(IONOSSharedPoolLockedCustody) error) error
}

// IONOSSharedPoolAdapter is intentionally not registered as a production
// executor. Wiring must supply durable custody, governed authorization and an
// admitted node agent; the managed-server safety contract is not claimed here.
type IONOSSharedPoolAdapter struct {
	node       IONOSSharedPoolNode
	runtime    IONOSSharedPoolRuntime
	custody    IONOSSharedPoolCustody
	authorizer IONOSSharedPoolAuthorizer
	backups    IONOSSharedPoolBackupVerifier
}

func NewIONOSSharedPoolAdapter(node IONOSSharedPoolNode, runtime IONOSSharedPoolRuntime, custody IONOSSharedPoolCustody, authorizer IONOSSharedPoolAuthorizer, backups IONOSSharedPoolBackupVerifier) (*IONOSSharedPoolAdapter, error) {
	if err := node.validate(); err != nil {
		return nil, err
	}
	if runtime == nil || custody == nil || authorizer == nil || backups == nil {
		return nil, ionosPoolRefuse("dependency_refused", "configure the admitted runtime, durable custody, command authorizer and R2 verifier")
	}
	return &IONOSSharedPoolAdapter{node: node, runtime: runtime, custody: custody, authorizer: authorizer, backups: backups}, nil
}

func (a *IONOSSharedPoolAdapter) Provision(ctx context.Context, w IONOSSharedPoolWorkload) (IONOSSharedPoolObservation, error) {
	return a.execute(ctx, IONOSPoolProvision, w.TenantID, w.Handle, &w, nil)
}
func (a *IONOSSharedPoolAdapter) Start(ctx context.Context, tenant, handle string) (IONOSSharedPoolObservation, error) {
	return a.execute(ctx, IONOSPoolStart, tenant, handle, nil, nil)
}
func (a *IONOSSharedPoolAdapter) Stop(ctx context.Context, tenant, handle string) (IONOSSharedPoolObservation, error) {
	return a.execute(ctx, IONOSPoolStop, tenant, handle, nil, nil)
}
func (a *IONOSSharedPoolAdapter) Observe(ctx context.Context, tenant, handle string) (IONOSSharedPoolObservation, error) {
	return a.execute(ctx, IONOSPoolObserve, tenant, handle, nil, nil)
}
func (a *IONOSSharedPoolAdapter) Backup(ctx context.Context, tenant, handle string) (IONOSSharedPoolObservation, error) {
	return a.execute(ctx, IONOSPoolBackup, tenant, handle, nil, nil)
}
func (a *IONOSSharedPoolAdapter) Restore(ctx context.Context, tenant, handle string, backup IONOSSharedPoolBackupManifest) (IONOSSharedPoolObservation, error) {
	return a.execute(ctx, IONOSPoolRestore, tenant, handle, nil, &backup)
}
func (a *IONOSSharedPoolAdapter) Decommission(ctx context.Context, tenant, handle string) (IONOSSharedPoolObservation, error) {
	return a.execute(ctx, IONOSPoolDecommission, tenant, handle, nil, nil)
}

func (a *IONOSSharedPoolAdapter) execute(ctx context.Context, action IONOSSharedPoolAction, tenant, handle string, workload *IONOSSharedPoolWorkload, backup *IONOSSharedPoolBackupManifest) (IONOSSharedPoolObservation, error) {
	var result IONOSSharedPoolObservation
	err := a.custody.WithNode(ctx, a.node.ID, func(store IONOSSharedPoolLockedCustody) error {
		records, err := store.Records(ctx)
		if err != nil {
			return err
		}
		var existing *IONOSSharedPoolRecord
		usedMB, slots := a.node.ReservedMemoryMB, 0
		seen := map[string]bool{}
		for _, record := range records {
			// Custody is a persistence boundary: corrupted accounting fails closed.
			expected, err := ionosPoolSpec(a.node, record.Spec.Workload)
			if err != nil || expected != record.Spec || seen[record.Spec.Workload.Handle] || (record.Released && record.AbsenceEvidenceRef == "") {
				return ionosPoolRefuse("custody_refused", "repair inconsistent worker custody before dispatch")
			}
			seen[record.Spec.Workload.Handle] = true
			if !record.Released {
				usedMB += record.Spec.Workload.MemoryMB
				slots++
			}
			if record.Spec.Workload.Handle == handle {
				copy := record
				existing = &copy
			}
		}
		var spec IONOSSharedPoolSpec
		if workload != nil {
			spec, err = ionosPoolSpec(a.node, *workload)
			if err != nil {
				return err
			}
		} else if existing != nil && existing.Spec.Workload.TenantID == tenant {
			spec = existing.Spec
		} else {
			return ionosPoolRefuse("ownership_refused", "use a workload owned by the authenticated tenant")
		}
		command := IONOSSharedPoolCommand{Action: action, Spec: spec, Backup: backup}
		if err := a.authorizer.AuthorizeIONOSSharedPool(ctx, command); err != nil {
			return ionosPoolRefuse("authorization_refused", "obtain a current tenant-bound governed command and entitlement")
		}
		if existing != nil && (existing.Spec.Workload.TenantID != tenant || existing.Spec != spec) {
			return ionosPoolRefuse("ownership_refused", "use the immutable owning tenant generation")
		}
		if existing != nil && existing.Released {
			if action != IONOSPoolObserve && action != IONOSPoolDecommission {
				return ionosPoolRefuse("generation_released", "admit a new generation; released handles cannot be reused")
			}
			result = IONOSSharedPoolObservation{Spec: spec, State: "absent", EvidenceRef: existing.AbsenceEvidenceRef, IsolationRemoved: true}
			return nil
		}
		if existing == nil {
			if slots >= a.node.Slots || usedMB > a.node.MemoryMB-spec.Workload.MemoryMB {
				return ionosPoolRefuse("capacity_refused", "select another admitted worker with a free slot and enough RAM")
			}
			if err := store.Put(ctx, IONOSSharedPoolRecord{Spec: spec}); err != nil {
				return err
			}
		}
		if action == IONOSPoolRestore {
			if err := a.verifyBackup(ctx, spec, backup); err != nil {
				return err
			}
		}
		if action != IONOSPoolProvision && action != IONOSPoolObserve {
			before, err := a.runtime.Execute(ctx, IONOSSharedPoolCommand{Action: IONOSPoolObserve, Spec: spec})
			if err != nil {
				return err
			}
			if err := ionosPoolReadback(spec, before); err != nil {
				return err
			}
			if before.State == "absent" && action != IONOSPoolDecommission {
				return ionosPoolRefuse("workload_absent", "provision a new owning generation before this action")
			}
		}
		result, err = a.runtime.Execute(ctx, command)
		if err != nil {
			return err
		}
		if err := ionosPoolReadback(spec, result); err != nil {
			return err
		}
		if action == IONOSPoolDecommission {
			result, err = a.runtime.Execute(ctx, IONOSSharedPoolCommand{Action: IONOSPoolObserve, Spec: spec})
			if err != nil {
				return err
			}
			if err := ionosPoolReadback(spec, result); err != nil {
				return err
			}
			if result.State != "absent" || !result.IsolationRemoved {
				return ionosPoolRefuse("absence_unconfirmed", "retain capacity until the exact workload and its isolation resources are absent")
			}
			return store.Put(ctx, IONOSSharedPoolRecord{Spec: spec, Released: true, AbsenceEvidenceRef: result.EvidenceRef})
		}
		if action == IONOSPoolBackup {
			return a.verifyBackup(ctx, spec, result.Backup)
		}
		if (action == IONOSPoolStart && result.State != "running") || ((action == IONOSPoolStop || action == IONOSPoolRestore) && result.State != "stopped") || (action == IONOSPoolProvision && result.State == "absent") {
			return ionosPoolRefuse("state_unconfirmed", "reconcile the exact workload state before reporting success")
		}
		return nil
	})
	if err != nil {
		return IONOSSharedPoolObservation{}, err
	}
	return result, nil
}

func ionosPoolReadback(spec IONOSSharedPoolSpec, observation IONOSSharedPoolObservation) error {
	if observation.Spec != spec || strings.TrimSpace(observation.EvidenceRef) == "" {
		return ionosPoolRefuse("readback_refused", "require exact tenant, handle and isolation evidence from the admitted agent")
	}
	switch observation.State {
	case "running", "stopped", "absent":
		return nil
	}
	return ionosPoolRefuse("readback_refused", "require a definitive runtime state")
}

func (a *IONOSSharedPoolAdapter) verifyBackup(ctx context.Context, spec IONOSSharedPoolSpec, backup *IONOSSharedPoolBackupManifest) error {
	if err := ionosPoolBackupShape(spec, backup); err != nil {
		return err
	}
	if err := a.backups.VerifyIONOSSharedPoolBackup(ctx, *backup); err != nil {
		return ionosPoolRefuse("backup_refused", "verify the exact immutable R2 object before backup or restore")
	}
	return nil
}
