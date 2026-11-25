package providercontrol

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

const IONOSSharedPoolFlag = "homelab.managed.apps.ionos.pool"

var ionosPinnedImage = regexp.MustCompile(`^[a-z0-9][a-z0-9.:/_-]+@sha256:[0-9a-f]{64}$`)

// IONOSPoolRequest carries no tenant, image, runtime, network or resource limits.
// The authenticated context and server catalog supply them.
type IONOSPoolRequest struct {
	CommandID    string                         `json:"command_id"`
	GenerationID string                         `json:"generation_id"`
	AppID        string                         `json:"app_id"`
	Action       IONOSSharedPoolAction          `json:"action"`
	Backup       *IONOSSharedPoolBackupManifest `json:"backup,omitempty"`
}

type IONOSPoolIdentity = ManagedAppIdentity
type IONOSPoolApplication struct {
	Image    string `json:"image"`
	MemoryMB int    `json:"memory_mb"`
}

type IONOSPoolAdmission func(context.Context, IONOSPoolIdentity, IONOSPoolRequest) (ManagedAppBudget, error)

type IONOSSharedPoolControl struct {
	database *sql.DB
	node     IONOSSharedPoolNode
	runtime  IONOSSharedPoolRuntime
	verifier IONOSSharedPoolBackupVerifier
	apps     map[string]IONOSPoolApplication
	admit    IONOSPoolAdmission
}

func NewIONOSSharedPoolControl(database *sql.DB, node IONOSSharedPoolNode, runtime IONOSSharedPoolRuntime, verifier IONOSSharedPoolBackupVerifier, apps map[string]IONOSPoolApplication, admit IONOSPoolAdmission) (*IONOSSharedPoolControl, error) {
	if err := node.validate(); err != nil {
		return nil, err
	}
	if database == nil || runtime == nil || verifier == nil || admit == nil || len(apps) == 0 {
		return nil, ionosPoolRefuse("configuration_refused", "configure database custody, mTLS worker, R2 verifier, catalog and Cloud admission")
	}
	catalog := make(map[string]IONOSPoolApplication, len(apps))
	for id, app := range apps {
		if (id != "vaultwarden" && id != "cloudreve-r2" && id != "workpaths-lite") || !ionosPinnedImage.MatchString(app.Image) || app.MemoryMB < 128 || app.MemoryMB > 512 {
			return nil, ionosPoolRefuse("catalog_refused", "pin curated lightweight images and 128–512 MB limits")
		}
		catalog[id] = app
	}
	return &IONOSSharedPoolControl{database: database, node: node, runtime: runtime, verifier: verifier, apps: catalog, admit: admit}, nil
}

func (c *IONOSSharedPoolControl) Execute(ctx context.Context, identity IONOSPoolIdentity, request IONOSPoolRequest) (IONOSSharedPoolObservation, error) {
	ctx, cancel := context.WithTimeout(ctx, IONOSPoolSynchronousBudget)
	defer cancel()
	if identity.TenantID == "" || identity.SubjectID == "" || !ionosPoolIdentifier.MatchString(request.CommandID) || !ionosPoolIdentifier.MatchString(request.GenerationID) {
		return IONOSSharedPoolObservation{}, ionosPoolRefuse("identity_refused", "use an authenticated identity, command, generation and allocated slot")
	}
	app, ok := c.apps[request.AppID]
	if !ok {
		return IONOSSharedPoolObservation{}, ionosPoolRefuse("catalog_refused", "select a curated lightweight application")
	}
	switch request.Action {
	case IONOSPoolProvision, IONOSPoolStart, IONOSPoolStop, IONOSPoolObserve, IONOSPoolBackup, IONOSPoolRestore, IONOSPoolDecommission:
	default:
		return IONOSSharedPoolObservation{}, ionosPoolRefuse("action_refused", "use an implemented workload action")
	}
	if (request.Action == IONOSPoolRestore) != (request.Backup != nil) {
		return IONOSSharedPoolObservation{}, ionosPoolRefuse("backup_refused", "attach a manifest only for restore")
	}
	workload := IONOSSharedPoolWorkload{TenantID: identity.TenantID, Handle: request.GenerationID, Location: IONOSSharedPoolLocation, Image: app.Image, MemoryMB: app.MemoryMB, Runtime: "gvisor"}
	spec, err := ionosPoolSpec(c.node, workload)
	if err != nil {
		return IONOSSharedPoolObservation{}, err
	}
	// Authorize before acquiring a dedicated worker connection.
	budget, err := c.admit(ctx, identity, request)
	if err != nil {
		return IONOSSharedPoolObservation{}, err
	}
	var result IONOSSharedPoolObservation
	err = withIONOSPoolNode(ctx, c.database, identity.TenantID, c.node.ID, func(ctx context.Context, custody *ionosPostgresCustody) error {
		records, err := custody.Records(ctx)
		if err != nil {
			return err
		}
		for _, record := range records {
			if record.Spec.Workload.Handle == request.GenerationID {
				spec = record.Spec
				workload = spec.Workload
				break
			}
		}
		encoded, err := json.Marshal(struct {
			Identity IONOSPoolIdentity
			Request  IONOSPoolRequest
			Spec     IONOSSharedPoolSpec
		}{identity, request, spec})
		if err != nil {
			return err
		}
		digest := sha256.Sum256(encoded)
		binding := hex.EncodeToString(digest[:])
		if request.Backup != nil {
			if err := ionosPoolBackupShape(spec, request.Backup); err != nil {
				return err
			}
		}
		cached, err := custody.claim(ctx, identity, request, binding, budget, c.node, spec)
		if err != nil {
			return err
		}
		if cached != nil {
			result = *cached
			return nil
		}
		authorizer := &ionosBoundAuthorizer{custody: custody, identity: identity, request: request, spec: spec, digest: binding}
		dispatch := &ionosTrackedDispatch{runtime: c.runtime}
		adapter, err := NewIONOSSharedPoolAdapter(c.node, dispatch, custody, authorizer, c.verifier)
		if err != nil {
			return err
		}
		switch request.Action {
		case IONOSPoolProvision:
			result, err = adapter.Provision(ctx, workload)
		case IONOSPoolStart:
			result, err = adapter.Start(ctx, identity.TenantID, request.GenerationID)
		case IONOSPoolStop:
			result, err = adapter.Stop(ctx, identity.TenantID, request.GenerationID)
		case IONOSPoolObserve:
			result, err = adapter.Observe(ctx, identity.TenantID, request.GenerationID)
		case IONOSPoolBackup:
			result, err = adapter.Backup(ctx, identity.TenantID, request.GenerationID)
		case IONOSPoolRestore:
			result, err = adapter.Restore(ctx, identity.TenantID, request.GenerationID, *request.Backup)
		case IONOSPoolDecommission:
			result, err = adapter.Decommission(ctx, identity.TenantID, request.GenerationID)
		}
		if err != nil {
			var refusal *ManagedAppRefusal
			if !dispatch.mutated && errors.As(err, &refusal) {
				if receiptErr := custody.reject(ctx, request, binding, refusal); receiptErr != nil {
					return receiptErr
				}
			}
			return err
		} // Ambiguous mutations retain the durable unknown-outcome claim.
		return custody.complete(ctx, request, binding, result)
	})
	if err != nil {
		return IONOSSharedPoolObservation{}, err
	}
	return result, nil
}

type ionosBoundAuthorizer struct {
	custody  *ionosPostgresCustody
	identity IONOSPoolIdentity
	request  IONOSPoolRequest
	spec     IONOSSharedPoolSpec
	digest   string
}

func (a *ionosBoundAuthorizer) AuthorizeIONOSSharedPool(ctx context.Context, command IONOSSharedPoolCommand) error {
	if command.Spec != a.spec || command.Action != a.request.Action {
		return errors.New("workload command binding mismatch")
	}
	left, _ := json.Marshal(command.Backup)
	right, _ := json.Marshal(a.request.Backup)
	if string(left) != string(right) {
		return errors.New("backup command binding mismatch")
	}
	var live bool
	err := a.custody.transaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ionos_pool_commands WHERE tenant_id=$1 AND command_id=$2 AND generation_id=$3 AND subject_id=$4 AND request_digest=$5 AND result IS NULL AND claim_until>clock_timestamp())`, a.identity.TenantID, a.request.CommandID, a.request.GenerationID, a.identity.SubjectID, a.digest).Scan(&live)
	})
	if err != nil {
		return err
	}
	if !live {
		return ErrExecutionClaimLost
	}
	return nil
}

// Synchronous calls must finish while the request's database identity is valid.
// Longer operations require an asynchronous result-custody design.
const IONOSPoolSynchronousBudget = 4 * time.Minute
const ionosPoolClaimTTL = 5 * time.Minute

type ionosTrackedDispatch struct {
	runtime IONOSSharedPoolRuntime
	mutated bool
}

func (r *ionosTrackedDispatch) Execute(ctx context.Context, command IONOSSharedPoolCommand) (IONOSSharedPoolObservation, error) {
	if command.Action != IONOSPoolObserve && command.Action != IONOSPoolVerifyBackup {
		r.mutated = true
	}
	return r.runtime.Execute(ctx, command)
}
