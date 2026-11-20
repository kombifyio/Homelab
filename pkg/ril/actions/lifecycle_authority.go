package actions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	rilaction "github.com/kombifyio/homelab/pkg/ril/actioncontract"
	"github.com/kombifyio/stackkits/contracts/runtimelease"
)

// LifecycleAuthorityKind names what admits a server's runtime for one RIL
// action: the VM lease of a leased offering, or the owner's own admitted
// server of an owner-operated offering.
type LifecycleAuthorityKind string

const (
	LifecycleAuthorityVMLease     LifecycleAuthorityKind = "vm-lease"
	LifecycleAuthorityOwnerServer LifecycleAuthorityKind = "owner-server"
)

// ownerServerGenerationNamespace names the stable UUID that records an
// owner-operated server's generation identity in the admission's resource
// generation column. It identifies `server:<id>:<generation>`; it is never a
// VM lease row.
var ownerServerGenerationNamespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://kombify.io/homelab/ril-owner-server-generation/v1"))

// LifecycleAuthority is a server's one admitted runtime authority. A leased
// offering (substrate_vm, managed_vps) is admitted by its running, uncancelled,
// currently valid VM lease; an owner-operated offering (self_owned_device,
// external_vps) without a lease is admitted by the owner's own admitted server
// for a bounded window. Every other combination refuses.
type LifecycleAuthority struct {
	Kind              LifecycleAuthorityKind
	ServerID          string
	WorkerID          string
	InventoryRevision int64
	ServerRevision    int64
	ServerGeneration  int64
	// ID is the VM lease id, or `server:<server id>`.
	ID string
	// Revision is the lease revision, or the server generation.
	Revision int64
	// GenerationRef is the lease's resource generation id, or
	// `server:<server id>:<generation>`.
	GenerationRef string
	// GenerationID is the UUID persisted with the admission: the lease's
	// resource generation, or the name-based UUID of GenerationRef.
	GenerationID string
	ValidUntil   time.Time
}

// OwnerServerGenerationID is the stable UUID of an owner-operated server's
// generation identity.
func OwnerServerGenerationID(serverID string, generation int64) string {
	return uuid.NewSHA1(ownerServerGenerationNamespace, []byte(ownerServerGenerationRef(serverID, generation))).String()
}

func ownerServerGenerationRef(serverID string, generation int64) string {
	return "server:" + serverID + ":" + strconv.FormatInt(generation, 10)
}

// LockLifecycleAuthorityTx row-locks the active, desired-running, connected
// server of the owner and stack and, for a leased offering, its VM lease, and
// returns the one admitted runtime authority at now. The caller's transaction
// must carry the tenant scope.
func LockLifecycleAuthorityTx(ctx context.Context, tx *sql.Tx, tenantID, ownerID, serverID, stackID string, now time.Time) (LifecycleAuthority, error) {
	if tx == nil || tenantID == "" || ownerID == "" || serverID == "" || stackID == "" {
		return LifecycleAuthority{}, fmt.Errorf("%w: lifecycle authority binding incomplete", ErrExecutionAdmission)
	}
	var (
		authority                     = LifecycleAuthority{ServerID: serverID}
		lifecycle, desired, connected string
		offering, leaseID, evidence   string
	)
	err := tx.QueryRowContext(ctx, `
		SELECT inventory_revision, revision, generation, COALESCE(worker_id, ''),
		       lifecycle_state, desired_state, connection_state,
		       COALESCE(offering, ''), COALESCE(lease_id, ''), COALESCE(runtime_target_evidence_ref, '')
		FROM servers
		WHERE tenant_id = $1 AND owner_subject_id = $2 AND id = $3 AND stack_id = $4
		FOR UPDATE
	`, tenantID, ownerID, serverID, stackID).Scan(
		&authority.InventoryRevision, &authority.ServerRevision, &authority.ServerGeneration, &authority.WorkerID,
		&lifecycle, &desired, &connected, &offering, &leaseID, &evidence,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return LifecycleAuthority{}, fmt.Errorf("%w: current inventory binding not found", ErrExecutionAdmission)
	}
	if err != nil {
		return LifecycleAuthority{}, fmt.Errorf("load lifecycle authority: %w", err)
	}
	if authority.InventoryRevision < 1 || authority.ServerRevision < 1 || authority.ServerGeneration < 1 {
		return LifecycleAuthority{}, fmt.Errorf("%w: current inventory or server revision is missing", ErrExecutionAdmission)
	}
	if lifecycle != "active" || desired != "running" || connected != "connected" || strings.TrimSpace(authority.WorkerID) == "" {
		return LifecycleAuthority{}, fmt.Errorf("%w: runtime server is not active, desired-running, and connected", ErrExecutionAdmission)
	}
	switch {
	case (offering == "substrate_vm" || offering == "managed_vps") && leaseID != "":
		return lockVMLeaseAuthority(ctx, tx, tenantID, ownerID, leaseID, authority, now)
	case (offering == "self_owned_device" || offering == "external_vps") && leaseID == "" && evidence != "":
		authority.Kind = LifecycleAuthorityOwnerServer
		authority.ID = "server:" + serverID
		authority.Revision = authority.ServerGeneration
		authority.GenerationRef = ownerServerGenerationRef(serverID, authority.ServerGeneration)
		authority.GenerationID = OwnerServerGenerationID(serverID, authority.ServerGeneration)
		authority.ValidUntil = now.UTC().Add(rilaction.MaxAuthorityValidity)
		return authority, nil
	default:
		return LifecycleAuthority{}, fmt.Errorf("%w: server offering has no admitted lifecycle authority", ErrExecutionAdmission)
	}
}

func lockVMLeaseAuthority(ctx context.Context, tx *sql.Tx, tenantID, ownerID, leaseID string, authority LifecycleAuthority, now time.Time) (LifecycleAuthority, error) {
	var (
		revision              int64
		generationID, desired string
		validFrom, validUntil time.Time
		cancelledAt           sql.NullTime
	)
	err := tx.QueryRowContext(ctx, `
		SELECT lease_revision, resource_generation_id::text, desired_state, valid_from, valid_until, cancelled_at
		FROM techstack_vm_leases
		WHERE tenant_id = $1 AND id = $2 AND server_id = $3 AND owner_subject_id = $4
		FOR UPDATE
	`, tenantID, leaseID, authority.ServerID, ownerID).Scan(&revision, &generationID, &desired, &validFrom, &validUntil, &cancelledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return LifecycleAuthority{}, fmt.Errorf("%w: runtime lease binding not found", ErrExecutionAdmission)
	}
	if err != nil {
		return LifecycleAuthority{}, fmt.Errorf("load runtime lease authority: %w", err)
	}
	if revision < 1 || desired != string(runtimelease.DesiredStateRunning) {
		return LifecycleAuthority{}, fmt.Errorf("%w: runtime lease is not running", ErrExecutionAdmission)
	}
	parsed, err := uuid.Parse(strings.TrimSpace(generationID))
	if err != nil || parsed.String() != generationID {
		return LifecycleAuthority{}, fmt.Errorf("%w: runtime lease generation is not canonical", ErrExecutionAdmission)
	}
	lease := runtimelease.Lease{
		ID: runtimelease.LeaseID(leaseID), Revision: uint64(revision),
		TenantID: tenantID, OwnerID: ownerID,
		ServerID:             runtimelease.RuntimeServerID(authority.ServerID),
		ResourceGenerationID: runtimelease.ResourceGenerationID(generationID),
		DesiredState:         runtimelease.DesiredState(desired),
		ValidFrom:            validFrom, ValidUntil: validUntil,
	}
	if cancelledAt.Valid {
		lease.CancelledAt = &cancelledAt.Time
	}
	if err := lease.Validate(now); err != nil {
		return LifecycleAuthority{}, fmt.Errorf("%w: %v", ErrExecutionAdmission, err)
	}
	authority.Kind = LifecycleAuthorityVMLease
	authority.ID = leaseID
	authority.Revision = revision
	authority.GenerationRef = generationID
	authority.GenerationID = generationID
	authority.ValidUntil = validUntil.UTC()
	return authority, nil
}

// MatchesAdmission reports whether this authority is still exactly the one an
// execution was admitted under.
func (a LifecycleAuthority) MatchesAdmission(admission ExecutionAdmission) bool {
	return a.InventoryRevision == admission.InventoryRevision && a.ServerRevision == admission.ServerRevision &&
		a.ServerGeneration == admission.ServerGeneration && a.ID == admission.LifecycleAuthorityID &&
		a.Revision == admission.LifecycleRevision && a.GenerationID == admission.LifecycleGenerationID
}
