package providercontrol

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

const (
	FlyMachinesFlag              = "homelab.managed.apps.fly.machines"
	FlyMachinesSynchronousBudget = 2 * time.Minute
	flyMachinesClaimTTL          = 3 * time.Minute
	flyMachinesCustodyWriteTTL   = 15 * time.Second
	flyMachinesDefaultMaxWorkers = 4
)

type FlyMachineAction string

const (
	FlyMachineActionProvision    FlyMachineAction = "provision"
	FlyMachineActionStart        FlyMachineAction = "start"
	FlyMachineActionStop         FlyMachineAction = "stop"
	FlyMachineActionWake         FlyMachineAction = "wake"
	FlyMachineActionObserve      FlyMachineAction = "observe"
	FlyMachineActionBackup       FlyMachineAction = "backup"
	FlyMachineActionRestore      FlyMachineAction = "restore"
	FlyMachineActionDecommission FlyMachineAction = "decommission"
)

type FlyMachinesReadinessError struct {
	Code string `json:"code"`
}

func (e *FlyMachinesReadinessError) Error() string {
	return "fly-machines: " + e.Code
}

type FlyMachinesStatus struct {
	Enabled          bool                       `json:"enabled"`
	State            string                     `json:"state"`
	Region           string                     `json:"region,omitempty"`
	IsolationMode    string                     `json:"isolation_mode,omitempty"`
	BackupConfigured bool                       `json:"backup_configured"`
	Error            *FlyMachinesReadinessError `json:"error,omitempty"`
}

type FlyMachinesIdentity = ManagedAppIdentity

type FlyMachinesApplication struct {
	Image        string `json:"image"`
	MemoryMB     int    `json:"memory_mb"`
	InternalPort int    `json:"internal_port"`
}

// FlyMachinesTenantBinding binds exactly one tenant to its own dedicated,
// privately routed Fly app, isolated Fly network, and app-scoped API token.
// Sharing an app, network, or token across tenants is rejected fail-closed at startup.
type FlyMachinesTenantBinding struct {
	App     string `json:"app"`
	Network string `json:"network"`
	Token   string `json:"token"`
}

// FlyMachinesRequest carries no tenant, image, region, isolation, or resource limits.
// The authenticated context, server catalog, and durable custody ledger supply them.
type FlyMachinesRequest struct {
	CommandID    string             `json:"command_id"`
	GenerationID string             `json:"generation_id"`
	AppID        string             `json:"app_id"`
	Action       FlyMachineAction   `json:"action"`
	MachineID    string             `json:"machine_id,omitempty"`
	Backup       *FlyBackupArtifact `json:"backup,omitempty"`
}

// FlyMachineCommandResult is returned by the Fly Machines control plane without
// exposing internal custody hashes, tokens, or R2 credentials.
type FlyMachineCommandResult struct {
	App          string             `json:"app"`
	Region       string             `json:"region"`
	GenerationID string             `json:"generation_id"`
	AppID        string             `json:"app_id"`
	MachineID    string             `json:"machine_id,omitempty"`
	State        string             `json:"state"`
	Health       FlyMachineHealth   `json:"health,omitempty"`
	EvidenceRef  string             `json:"evidence_ref,omitempty"`
	Backup       *FlyBackupArtifact `json:"backup,omitempty"`
}

type FlyMachineGeneration struct {
	TenantID           string
	GenerationID       string
	SubjectID          string
	AppID              string
	FlyApp             string
	MachineID          string
	CustodyHash        string
	Spec               FlyMachineSpec
	Released           bool
	AbsenceEvidenceRef string
	ProvisionInFlight  bool
}

type FlyMachineClaimRequest struct {
	Identity      FlyMachinesIdentity
	Request       FlyMachinesRequest
	FlyApp        string
	CustodyHash   string
	Spec          FlyMachineSpec
	RequestDigest string
	Budget        ManagedAppBudget
}

type FlyMachineClaimOutcome struct {
	Cached     *FlyMachineCommandResult
	Generation FlyMachineGeneration
}

// FlyMachinesLockedCustody is a durable generation-scoped store serialized across
// control-plane instances. RecordMachineID, MarkReleased, and RejectAndRelease
// commit independently before returning so handles and tombstones survive errors.
type FlyMachinesLockedCustody interface {
	Claim(context.Context, FlyMachineClaimRequest) (FlyMachineClaimOutcome, error)
	RecordMachineID(ctx context.Context, tenantID, generationID, machineID string) error
	MarkReleased(ctx context.Context, tenantID, generationID, evidenceRef string) error
	Complete(ctx context.Context, identity FlyMachinesIdentity, request FlyMachinesRequest, digest string, result FlyMachineCommandResult) error
	Reject(ctx context.Context, identity FlyMachinesIdentity, request FlyMachinesRequest, digest string, refusal *ManagedAppRefusal) error
	RejectAndRelease(ctx context.Context, identity FlyMachinesIdentity, request FlyMachinesRequest, digest, evidenceRef string, refusal *ManagedAppRefusal) error
}

type FlyMachinesCustody interface {
	WithGeneration(ctx context.Context, tenantID, generationID string, fn func(context.Context, FlyMachinesLockedCustody) error) error
}

type FlyMachinesAdmission func(context.Context, FlyMachinesIdentity, FlyMachinesRequest) (ManagedAppBudget, error)

type FlyMachinesConfig struct {
	Database      *sql.DB
	Custody       FlyMachinesCustody
	Region        string
	Tenants       map[string]FlyMachinesTenantBinding
	APIBase       string
	Apps          map[string]FlyMachinesApplication
	R2            FlyMachineR2Store
	Admit         FlyMachinesAdmission
	MaxConcurrent int
}

type FlyMachinesControl struct {
	custody FlyMachinesCustody
	tenants map[string]FlyMachinesTenantBinding
	apiBase string
	apps    map[string]FlyMachinesApplication
	r2      FlyMachineR2Store
	admit   FlyMachinesAdmission
}

func NewFlyMachinesControl(cfg FlyMachinesConfig) (*FlyMachinesControl, error) {
	if cfg.Region != FlyMachinesRegion {
		return nil, &FlyMachinesReadinessError{Code: "fly_region_refused"}
	}
	if len(cfg.Tenants) == 0 {
		return nil, &FlyMachinesReadinessError{Code: "fly_tenant_isolation_invalid"}
	}
	tenants := make(map[string]FlyMachinesTenantBinding, len(cfg.Tenants))
	seenApps := make(map[string]struct{}, len(cfg.Tenants))
	seenNetworks := make(map[string]struct{}, len(cfg.Tenants))
	seenTokens := make(map[string]struct{}, len(cfg.Tenants))
	for tenantID, binding := range cfg.Tenants {
		if tenantID == "" || strings.TrimSpace(tenantID) != tenantID || len(tenantID) > 256 || strings.ContainsAny(tenantID, "\x00\r\n") {
			return nil, &FlyMachinesReadinessError{Code: "fly_tenant_isolation_invalid"}
		}
		if !flyMachinePathID(binding.App) || !flyMachinePathID(binding.Network) || binding.Network == "default" ||
			strings.TrimSpace(binding.Token) == "" || strings.ContainsAny(binding.Token, "\r\n") {
			return nil, &FlyMachinesReadinessError{Code: "fly_tenant_isolation_invalid"}
		}
		if _, dup := seenApps[binding.App]; dup {
			return nil, &FlyMachinesReadinessError{Code: "fly_tenant_isolation_invalid"}
		}
		if _, dup := seenNetworks[binding.Network]; dup {
			return nil, &FlyMachinesReadinessError{Code: "fly_tenant_isolation_invalid"}
		}
		if _, dup := seenTokens[binding.Token]; dup {
			return nil, &FlyMachinesReadinessError{Code: "fly_tenant_isolation_invalid"}
		}
		seenApps[binding.App] = struct{}{}
		seenNetworks[binding.Network] = struct{}{}
		seenTokens[binding.Token] = struct{}{}
		tenants[tenantID] = binding
	}
	custody := cfg.Custody
	if custody == nil {
		if cfg.Database == nil {
			return nil, &FlyMachinesReadinessError{Code: "fly_custody_unavailable"}
		}
		maxWorkers := cfg.MaxConcurrent
		if maxWorkers <= 0 {
			maxWorkers = flyMachinesDefaultMaxWorkers
		}
		custody = newFlyMachinesPostgresCustody(cfg.Database, maxWorkers)
	}
	if cfg.Admit == nil || len(cfg.Apps) == 0 {
		return nil, &FlyMachinesReadinessError{Code: "fly_configuration_invalid"}
	}
	catalog := make(map[string]FlyMachinesApplication, len(cfg.Apps))
	for id, app := range cfg.Apps {
		if !flyCuratedAppAllowed(id) || !ionosPinnedImage.MatchString(app.Image) || !flyMachineMemoryAllowed(app.MemoryMB) || app.InternalPort < 1 || app.InternalPort > 65535 {
			return nil, &FlyMachinesReadinessError{Code: "fly_catalog_invalid"}
		}
		catalog[id] = app
	}
	apiBase := strings.TrimRight(strings.TrimSpace(cfg.APIBase), "/")
	if apiBase == "" {
		apiBase = flyMachinesAPIBase
	}
	return &FlyMachinesControl{
		custody: custody,
		tenants: tenants,
		apiBase: apiBase,
		apps:    catalog,
		r2:      cfg.R2,
		admit:   cfg.Admit,
	}, nil
}

func (c *FlyMachinesControl) backupReady() bool {
	if c.r2 == nil {
		return false
	}
	if avail, ok := c.r2.(FlyMachineR2Availability); ok {
		return avail.BackupReady()
	}
	return true
}

func (c *FlyMachinesControl) Status() FlyMachinesStatus {
	return FlyMachinesStatus{
		Enabled:          true,
		State:            "ready",
		Region:           FlyMachinesRegion,
		IsolationMode:    "dedicated-app-per-tenant",
		BackupConfigured: c.backupReady(),
	}
}

func (c *FlyMachinesControl) Execute(ctx context.Context, identity FlyMachinesIdentity, request FlyMachinesRequest) (FlyMachineCommandResult, error) {
	ctx, cancel := context.WithTimeout(ctx, FlyMachinesSynchronousBudget)
	defer cancel()
	if identity.TenantID == "" || strings.TrimSpace(identity.TenantID) != identity.TenantID || len(identity.TenantID) > 256 || strings.ContainsAny(identity.TenantID, "\x00\r\n") ||
		identity.SubjectID == "" || strings.TrimSpace(identity.SubjectID) != identity.SubjectID ||
		!flyIdentifierValid(request.CommandID) || !flyIdentifierValid(request.GenerationID) {
		return FlyMachineCommandResult{}, managedAppRefuse("identity_refused", "use an authenticated identity, command, generation and allocated slot")
	}
	binding, ok := c.tenants[identity.TenantID]
	if !ok {
		return FlyMachineCommandResult{}, managedAppRefuse("tenant_isolation_unavailable", "provision a dedicated tenant-isolated Fly app and network before dispatching Fly Machine workloads")
	}
	app, ok := c.apps[request.AppID]
	if !ok {
		return FlyMachineCommandResult{}, managedAppRefuse("catalog_refused", "select a curated lightweight application")
	}
	switch request.Action {
	case FlyMachineActionProvision, FlyMachineActionStart, FlyMachineActionStop, FlyMachineActionWake,
		FlyMachineActionObserve, FlyMachineActionBackup, FlyMachineActionRestore, FlyMachineActionDecommission:
	default:
		return FlyMachineCommandResult{}, managedAppRefuse("action_refused", "use an implemented workload action")
	}
	if request.Action == FlyMachineActionProvision && request.MachineID != "" {
		return FlyMachineCommandResult{}, managedAppRefuse("handle_refused", "do not supply a machine ID for provision; the custody ledger records it")
	}
	if request.MachineID != "" && !flyMachinePathID(request.MachineID) {
		return FlyMachineCommandResult{}, managedAppRefuse("handle_mismatch", "supply a valid Fly Machine handle or omit it to use the custody ledger")
	}
	if (request.Action == FlyMachineActionRestore) != (request.Backup != nil) {
		return FlyMachineCommandResult{}, managedAppRefuse("backup_refused", "attach a backup artifact only for restore")
	}
	if request.Backup != nil && !request.Backup.valid() {
		return FlyMachineCommandResult{}, managedAppRefuse("backup_refused", "supply a valid R2 backup artifact reference and SHA-256 digest")
	}
	if (request.Action == FlyMachineActionBackup || request.Action == FlyMachineActionRestore) && !c.backupReady() {
		return FlyMachineCommandResult{}, managedAppRefuse("backup_unavailable", "configure EU R2 custody and a workload data-plane transport before running Fly Machine backup or restore")
	}
	spec := FlyMachineSpec{
		Region:         FlyMachinesRegion,
		IsolationClass: FlyMachinesIsolation,
		MemoryMB:       app.MemoryMB,
		Image:          app.Image,
		InternalPort:   app.InternalPort,
	}
	custodyHash := flyMachineCustodyHash(identity.TenantID, identity.SubjectID, request.GenerationID, request.AppID, binding.App, binding.Network)
	digest, err := flyMachineRequestDigest(identity, request, binding.App, spec)
	if err != nil {
		return FlyMachineCommandResult{}, err
	}
	// Authorize before acquiring a per-generation database connection and lock.
	budget, err := c.admit(ctx, identity, request)
	if err != nil {
		return FlyMachineCommandResult{}, err
	}
	var result FlyMachineCommandResult
	err = c.custody.WithGeneration(ctx, identity.TenantID, request.GenerationID, func(ctx context.Context, store FlyMachinesLockedCustody) error {
		outcome, err := store.Claim(ctx, FlyMachineClaimRequest{
			Identity:      identity,
			Request:       request,
			FlyApp:        binding.App,
			CustodyHash:   custodyHash,
			Spec:          spec,
			RequestDigest: digest,
			Budget:        budget,
		})
		if err != nil {
			return err
		}
		if outcome.Cached != nil {
			result = *outcome.Cached
			return nil
		}
		gen := outcome.Generation
		if gen.Released {
			// Idempotent replay of decommission or observe against an already released
			// generation uses the durably persisted deterministic absence evidence.
			result = FlyMachineCommandResult{
				App:          gen.FlyApp,
				Region:       FlyMachinesRegion,
				GenerationID: gen.GenerationID,
				AppID:        gen.AppID,
				MachineID:    gen.MachineID,
				State:        "absent",
				EvidenceRef:  gen.AbsenceEvidenceRef,
			}
			return completeDetached(ctx, store, identity, request, digest, result)
		}
		owner := FlyMachineOwner{
			TenantID:    gen.TenantID,
			WorkloadID:  gen.GenerationID,
			SlotRef:     gen.AppID,
			CustodyHash: gen.CustodyHash,
		}
		adapter, err := newFlyMachinesAdapter(gen.FlyApp, binding.Token, owner, c.r2, c.apiBase)
		if err != nil {
			refusal := &ManagedAppRefusal{Code: "ownership_refused", Repair: "repair the generation custody binding before dispatch"}
			if rejectErr := rejectDetached(ctx, store, identity, request, digest, refusal); rejectErr != nil {
				return rejectErr
			}
			return refusal
		}
		handle := FlyMachineHandle{App: gen.FlyApp, MachineID: gen.MachineID, Owner: owner}

		// Reconcile unconfirmed provisions (machine_id == "") via Fly's metadata-
		// filtered machine list before observe/stop/decommission so lost create
		// responses never orphan a running Fly machine or leak a Managed App slot.
		if request.Action != FlyMachineActionProvision && handle.MachineID == "" {
			discovered, found, findErr := adapter.FindOwned(ctx)
			if findErr != nil {
				return findErr
			}
			if !found {
				if request.Action == FlyMachineActionDecommission {
					if gen.ProvisionInFlight {
						// Do not release a slot while the initial provision command's
						// claim window is still open; Fly may still be creating the machine.
						refusal := &ManagedAppRefusal{
							Code:   "absence_pending",
							Repair: "wait until the in-flight provision claim window expires and retry decommission with a new command ID",
						}
						if rejectErr := rejectDetached(ctx, store, identity, request, digest, refusal); rejectErr != nil {
							return errors.Join(refusal, rejectErr)
						}
						return refusal
					}
					if err := c.purgeR2(ctx, gen.TenantID, gen.GenerationID); err != nil {
						return err
					}
					evidenceRef := flyAbsenceNeverCreatedRef(gen.FlyApp, gen.GenerationID)
					if err := markReleasedDetached(ctx, store, gen.TenantID, gen.GenerationID, evidenceRef); err != nil {
						return err
					}
					result = FlyMachineCommandResult{
						App:          gen.FlyApp,
						Region:       FlyMachinesRegion,
						GenerationID: gen.GenerationID,
						AppID:        gen.AppID,
						State:        "absent",
						EvidenceRef:  evidenceRef,
					}
					return completeDetached(ctx, store, identity, request, digest, result)
				}
				result = FlyMachineCommandResult{
					App:          gen.FlyApp,
					Region:       FlyMachinesRegion,
					GenerationID: gen.GenerationID,
					AppID:        gen.AppID,
					State:        "not_found",
				}
				return completeDetached(ctx, store, identity, request, digest, result)
			}
			if err := recordMachineIDDetached(ctx, store, gen.TenantID, gen.GenerationID, discovered.ID); err != nil {
				return err
			}
			handle.MachineID = discovered.ID
			if flyMachineAbsent(discovered) {
				if request.Action == FlyMachineActionDecommission {
					if err := c.purgeR2(ctx, gen.TenantID, gen.GenerationID); err != nil {
						return err
					}
					evidenceRef := flyAbsenceEvidenceRef(gen.FlyApp, discovered.ID, gen.GenerationID)
					if err := markReleasedDetached(ctx, store, gen.TenantID, gen.GenerationID, evidenceRef); err != nil {
						return err
					}
					result = FlyMachineCommandResult{
						App:          gen.FlyApp,
						Region:       FlyMachinesRegion,
						GenerationID: gen.GenerationID,
						AppID:        gen.AppID,
						MachineID:    discovered.ID,
						State:        "absent",
						EvidenceRef:  evidenceRef,
					}
					return completeDetached(ctx, store, identity, request, digest, result)
				}
				result = FlyMachineCommandResult{
					App:          gen.FlyApp,
					Region:       FlyMachinesRegion,
					GenerationID: gen.GenerationID,
					AppID:        gen.AppID,
					MachineID:    discovered.ID,
					State:        "destroyed",
				}
				return completeDetached(ctx, store, identity, request, digest, result)
			}
		}

		switch request.Action {
		case FlyMachineActionProvision:
			// Read the tenant's Fly app back from Fly's API before creating any
			// machine to prove the app runs on its dedicated non-default network.
			if netErr := adapter.VerifyAppNetwork(ctx, binding.Network); netErr != nil {
				if errors.Is(netErr, ErrFlyMachineIsolation) {
					evidenceRef := flyNetworkRefusedEvidenceRef(gen.FlyApp, gen.GenerationID)
					refusal := &ManagedAppRefusal{
						Code:   "tenant_isolation_unavailable",
						Repair: "verify that the tenant Fly app is bound to its dedicated non-default Fly network",
					}
					if err := rejectAndReleaseDetached(ctx, store, identity, request, digest, evidenceRef, refusal); err != nil {
						return errors.Join(netErr, err)
					}
					return refusal
				}
				return netErr
			}
			created, provErr := adapter.Provision(ctx, gen.Spec)
			if created.MachineID != "" {
				// Persist the created Fly machine ID on a detached bounded context so
				// request cancellation never orphans a running Fly machine.
				if recErr := recordMachineIDDetached(ctx, store, gen.TenantID, gen.GenerationID, created.MachineID); recErr != nil {
					if provErr != nil {
						return errors.Join(provErr, recErr)
					}
					return recErr
				}
				if provErr != nil {
					// Best-effort immediate stop so a drifted or non-fra machine is not
					// left running while awaiting operator/tenant cleanup.
					stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
					_ = adapter.Stop(stopCtx, created)
					stopCancel()
					return provErr
				}
			}
			if provErr != nil {
				// Definitive 4xx refusal from Fly (excluding 408 timeout / 409 conflict):
				// verify via one FindOwned readback that no machine with this deterministic
				// name exists before tombstoning the generation and releasing the slot.
				if isDefinitiveFlyCreateRefusal(provErr) {
					discovered, found, findErr := adapter.FindOwned(ctx)
					if findErr != nil {
						return provErr
					}
					if found {
						if recErr := recordMachineIDDetached(ctx, store, gen.TenantID, gen.GenerationID, discovered.ID); recErr != nil {
							return errors.Join(provErr, recErr)
						}
						stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
						_ = adapter.Stop(stopCtx, FlyMachineHandle{App: gen.FlyApp, MachineID: discovered.ID, Owner: owner})
						stopCancel()
						return provErr
					}
					evidenceRef := flyCreateRefusedEvidenceRef(gen.FlyApp, gen.GenerationID)
					refusal := &ManagedAppRefusal{
						Code:   "provider_provision_refused",
						Repair: "resolve the Fly.io placement or credential refusal and retry with a new generation ID",
					}
					if err := rejectAndReleaseDetached(ctx, store, identity, request, digest, evidenceRef, refusal); err != nil {
						return errors.Join(provErr, err)
					}
					return refusal
				}
				return provErr
			}
			result = FlyMachineCommandResult{
				App:          gen.FlyApp,
				Region:       FlyMachinesRegion,
				GenerationID: gen.GenerationID,
				AppID:        gen.AppID,
				MachineID:    created.MachineID,
				State:        "started",
				EvidenceRef:  flyProvisionEvidenceRef(gen.FlyApp, created.MachineID, gen.GenerationID),
			}
			return completeDetached(ctx, store, identity, request, digest, result)
		case FlyMachineActionStart:
			if err := adapter.Start(ctx, handle); err != nil {
				return err
			}
			result = FlyMachineCommandResult{
				App:          gen.FlyApp,
				Region:       FlyMachinesRegion,
				GenerationID: gen.GenerationID,
				AppID:        gen.AppID,
				MachineID:    handle.MachineID,
				State:        "started",
			}
			return completeDetached(ctx, store, identity, request, digest, result)
		case FlyMachineActionStop:
			if err := adapter.Stop(ctx, handle); err != nil {
				return err
			}
			result = FlyMachineCommandResult{
				App:          gen.FlyApp,
				Region:       FlyMachinesRegion,
				GenerationID: gen.GenerationID,
				AppID:        gen.AppID,
				MachineID:    handle.MachineID,
				State:        "stopped",
			}
			return completeDetached(ctx, store, identity, request, digest, result)
		case FlyMachineActionWake:
			if err := adapter.Wake(ctx, handle); err != nil {
				return err
			}
			result = FlyMachineCommandResult{
				App:          gen.FlyApp,
				Region:       FlyMachinesRegion,
				GenerationID: gen.GenerationID,
				AppID:        gen.AppID,
				MachineID:    handle.MachineID,
				State:        "started",
			}
			return completeDetached(ctx, store, identity, request, digest, result)
		case FlyMachineActionObserve:
			obs, err := adapter.Observe(ctx, handle)
			if errors.Is(err, ErrFlyMachineNotFound) {
				// Never release a Managed App slot on Observe 404 (a renamed/missing app
				// must not release slots). Explicit Decommission owns slot release.
				result = FlyMachineCommandResult{
					App:          gen.FlyApp,
					Region:       FlyMachinesRegion,
					GenerationID: gen.GenerationID,
					AppID:        gen.AppID,
					MachineID:    handle.MachineID,
					State:        "not_found",
				}
				return completeDetached(ctx, store, identity, request, digest, result)
			}
			if err != nil {
				return err
			}
			result = FlyMachineCommandResult{
				App:          gen.FlyApp,
				Region:       FlyMachinesRegion,
				GenerationID: gen.GenerationID,
				AppID:        gen.AppID,
				MachineID:    handle.MachineID,
				State:        obs.State,
				Health:       obs.Health,
			}
			return completeDetached(ctx, store, identity, request, digest, result)
		case FlyMachineActionBackup:
			manifest, err := adapter.Backup(ctx, handle)
			if err != nil {
				return err
			}
			artifact := manifest.Artifact
			result = FlyMachineCommandResult{
				App:          gen.FlyApp,
				Region:       FlyMachinesRegion,
				GenerationID: gen.GenerationID,
				AppID:        gen.AppID,
				MachineID:    handle.MachineID,
				State:        "backed_up",
				Backup:       &artifact,
			}
			return completeDetached(ctx, store, identity, request, digest, result)
		case FlyMachineActionRestore:
			manifest := FlyBackupManifest{Source: handle, Artifact: *request.Backup}
			if err := adapter.Restore(ctx, handle, manifest); err != nil {
				return err
			}
			artifact := *request.Backup
			result = FlyMachineCommandResult{
				App:          gen.FlyApp,
				Region:       FlyMachinesRegion,
				GenerationID: gen.GenerationID,
				AppID:        gen.AppID,
				MachineID:    handle.MachineID,
				State:        "restored",
				Backup:       &artifact,
			}
			return completeDetached(ctx, store, identity, request, digest, result)
		case FlyMachineActionDecommission:
			absence, err := adapter.Decommission(ctx, handle)
			if err != nil {
				if errors.Is(err, ErrFlyMachineAbsencePending) {
					refusal := &ManagedAppRefusal{
						Code:   "absence_pending",
						Repair: "retry decommission with a new command ID until Fly confirms the exact machine is destroyed",
					}
					if rejectErr := rejectDetached(ctx, store, identity, request, digest, refusal); rejectErr != nil {
						return errors.Join(refusal, rejectErr)
					}
					return refusal
				}
				return err
			}
			if err := c.purgeR2(ctx, gen.TenantID, gen.GenerationID); err != nil {
				return err
			}
			evidenceRef := flyAbsenceEvidenceRef(gen.FlyApp, absence.Handle.MachineID, gen.GenerationID)
			if err := markReleasedDetached(ctx, store, gen.TenantID, gen.GenerationID, evidenceRef); err != nil {
				return err
			}
			result = FlyMachineCommandResult{
				App:          gen.FlyApp,
				Region:       FlyMachinesRegion,
				GenerationID: gen.GenerationID,
				AppID:        gen.AppID,
				MachineID:    absence.Handle.MachineID,
				State:        "absent",
				EvidenceRef:  evidenceRef,
			}
			return completeDetached(ctx, store, identity, request, digest, result)
		}
		return managedAppRefuse("action_refused", "use an implemented workload action")
	})
	if err != nil {
		return FlyMachineCommandResult{}, err
	}
	return result, nil
}

func (c *FlyMachinesControl) purgeR2(ctx context.Context, tenantID, generationID string) error {
	purger, ok := c.r2.(FlyMachineR2Purger)
	if !ok || purger == nil {
		return nil
	}
	return purger.PurgeWorkload(ctx, tenantID, generationID)
}

func recordMachineIDDetached(ctx context.Context, store FlyMachinesLockedCustody, tenantID, generationID, machineID string) error {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flyMachinesCustodyWriteTTL)
	defer cancel()
	return store.RecordMachineID(wctx, tenantID, generationID, machineID)
}

func markReleasedDetached(ctx context.Context, store FlyMachinesLockedCustody, tenantID, generationID, evidenceRef string) error {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flyMachinesCustodyWriteTTL)
	defer cancel()
	return store.MarkReleased(wctx, tenantID, generationID, evidenceRef)
}

func completeDetached(ctx context.Context, store FlyMachinesLockedCustody, identity FlyMachinesIdentity, request FlyMachinesRequest, digest string, result FlyMachineCommandResult) error {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flyMachinesCustodyWriteTTL)
	defer cancel()
	return store.Complete(wctx, identity, request, digest, result)
}

func rejectDetached(ctx context.Context, store FlyMachinesLockedCustody, identity FlyMachinesIdentity, request FlyMachinesRequest, digest string, refusal *ManagedAppRefusal) error {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flyMachinesCustodyWriteTTL)
	defer cancel()
	return store.Reject(wctx, identity, request, digest, refusal)
}

func rejectAndReleaseDetached(ctx context.Context, store FlyMachinesLockedCustody, identity FlyMachinesIdentity, request FlyMachinesRequest, digest, evidenceRef string, refusal *ManagedAppRefusal) error {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flyMachinesCustodyWriteTTL)
	defer cancel()
	return store.RejectAndRelease(wctx, identity, request, digest, evidenceRef, refusal)
}

func isDefinitiveFlyCreateRefusal(err error) bool {
	var apiErr *FlyMachinesAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode >= http.StatusBadRequest &&
		apiErr.StatusCode < http.StatusInternalServerError &&
		apiErr.StatusCode != http.StatusRequestTimeout &&
		apiErr.StatusCode != http.StatusConflict
}

func flyIdentifierValid(value string) bool {
	return ionosPoolIdentifier.MatchString(value) && !strings.Contains(value, "..")
}

func flyCuratedAppAllowed(id string) bool {
	return id == "vaultwarden" || id == "cloudreve-r2"
}

func flyMachineCustodyHash(tenantID, subjectID, generationID, appID, flyApp, flyNetwork string) string {
	sum := sha256.Sum256([]byte(tenantID + "\x00" + subjectID + "\x00" + generationID + "\x00" + appID + "\x00" + flyApp + "\x00" + flyNetwork))
	return hex.EncodeToString(sum[:])
}

func flyAbsenceEvidenceRef(app, machineID, generationID string) string {
	return "fly:" + app + ":" + machineID + ":" + generationID
}

func flyAbsenceNeverCreatedRef(app, generationID string) string {
	return "fly-never-created:" + app + ":" + generationID
}

func flyCreateRefusedEvidenceRef(app, generationID string) string {
	return "fly-create-refused:" + app + ":" + generationID
}

func flyNetworkRefusedEvidenceRef(app, generationID string) string {
	return "fly-network-refused:" + app + ":" + generationID
}

func flyProvisionEvidenceRef(app, machineID, generationID string) string {
	return "fly-provision:" + app + ":" + machineID + ":" + generationID
}

func flyMachineRequestDigest(identity FlyMachinesIdentity, request FlyMachinesRequest, flyApp string, spec FlyMachineSpec) (string, error) {
	payload := struct {
		Identity FlyMachinesIdentity
		Request  FlyMachinesRequest
		FlyApp   string
		Spec     *FlyMachineSpec `json:"spec,omitempty"`
	}{
		Identity: identity,
		Request:  request,
		FlyApp:   flyApp,
	}
	if request.Action == FlyMachineActionProvision {
		payload.Spec = &spec
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}
