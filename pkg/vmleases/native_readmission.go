package vmleases

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kombifyio/homelab/internal/runtimeproduct/vmlease"
	"github.com/kombifyio/homelab/pkg/db/tenantcontext"
)

// NativeReadmissionRequest re-admits one managed runtime lease after its
// tenant move to usr:<sub> (providercontrol.ArchiveManagedRuntimeCustody and
// the setup re-key). The lease row itself never left: its server must always
// hold it. Re-admission rewrites that row into the admission form of the new
// tenant and keeps the lease id and the resource generation, because the
// provider resources belong to that generation and are adopted again.
type NativeReadmissionRequest struct {
	// Lease is the new admission form: Subject.OrgID is the new tenant, no
	// generation, no provider handles, no authority times.
	Lease          vmlease.Lease
	OwnerSubjectID string
	ServerID       string
	ValidFor       time.Duration
	// ArchivedLeaseJSON is the archived lease document; the live row must
	// still hold exactly this document, so nothing changed since the archive.
	ArchivedLeaseJSON            json.RawMessage
	ArchivedResourceGenerationID string
}

// ReadmitRetenantedLeaseTx rewrites the moved lease row inside the
// caller-owned transaction and returns the authority facts for the rest of the
// re-admission.
func (s *PostgresStore) ReadmitRetenantedLeaseTx(
	ctx context.Context,
	tx *sql.Tx,
	request NativeReadmissionRequest,
) (NativeAdmissionResult, error) {
	if s == nil || s.db == nil || tx == nil {
		return NativeAdmissionResult{}, fmt.Errorf("vmleases: database and transaction required")
	}
	tenantID, err := tenantIDFromLease(request.Lease)
	if err != nil {
		return NativeAdmissionResult{}, err
	}
	ownerSubjectID := strings.TrimSpace(request.OwnerSubjectID)
	serverID := strings.TrimSpace(request.ServerID)
	generationID := strings.TrimSpace(request.ArchivedResourceGenerationID)
	if ownerSubjectID == "" || serverID == "" || generationID == "" || len(request.ArchivedLeaseJSON) == 0 ||
		ownerSubjectID != strings.TrimSpace(request.Lease.Subject.ID) {
		return NativeAdmissionResult{}, fmt.Errorf("vmleases: native readmission owner, server, generation and archived lease required")
	}
	if err := tenantcontext.ApplyTenant(ctx, tx, tenantID); err != nil {
		return NativeAdmissionResult{}, fmt.Errorf("vmleases: set native readmission tenant context: %w", err)
	}
	var databaseTime time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&databaseTime); err != nil {
		return NativeAdmissionResult{}, fmt.Errorf("vmleases: read native readmission database time: %w", err)
	}
	databaseTime = databaseTime.UTC()
	lease, err := prepareNativeReadmissionLease(request.Lease, generationID, databaseTime, request.ValidFor)
	if err != nil {
		return NativeAdmissionResult{}, err
	}
	if _, err := RuntimeContract(lease, NativeAdmissionLeaseRevision, serverID, databaseTime); err != nil {
		return NativeAdmissionResult{}, err
	}
	payload, err := encodeLease(lease)
	if err != nil {
		return NativeAdmissionResult{}, fmt.Errorf("vmleases: encode native readmission lease: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE techstack_vm_leases
		SET org_id = NULLIF($4, ''), engine_vm_id = NULL, lease_json = $5::jsonb,
		    valid_from = $6, valid_until = $7, renewed_at = $8, updated_at = $9
		WHERE tenant_id = $1 AND id = $2 AND resource_generation_id = $3::uuid
		  AND lease_json = $10::jsonb
		  AND owner_subject_id = $11 AND subject_id = $11 AND server_id = $12
		  AND lease_revision = $13 AND cancelled_at IS NULL
		  AND desired_state IN ('running', 'stopped')
	`,
		tenantID, lease.ID, generationID, lease.Subject.OrgID, payload,
		lease.ValidFrom, lease.ValidUntil, lease.RenewedAt, databaseTime,
		[]byte(request.ArchivedLeaseJSON), ownerSubjectID, serverID, int64(NativeAdmissionLeaseRevision),
	)
	if err != nil {
		return NativeAdmissionResult{}, fmt.Errorf("vmleases: rewrite readmitted lease: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return NativeAdmissionResult{}, fmt.Errorf("%w: the moved lease changed since its custody was archived", ErrLeaseIdentityConflict)
	}
	return NativeAdmissionResult{
		Lease: lease, ResourceGenerationID: generationID,
		LeaseRevision: NativeAdmissionLeaseRevision, DatabaseTime: databaseTime,
	}, nil
}

// prepareNativeReadmissionLease applies the fresh-admission rules except the
// generation: provider handles stay unset until the resolver re-observes them,
// and the validity window starts at database time.
func prepareNativeReadmissionLease(
	lease vmlease.Lease,
	generationID string,
	now time.Time,
	validFor time.Duration,
) (vmlease.Lease, error) {
	lease = cloneLease(lease)
	lease.ID = vmlease.LeaseID(strings.TrimSpace(string(lease.ID)))
	lease.Subject.ID = strings.TrimSpace(lease.Subject.ID)
	lease.Subject.OrgID = strings.TrimSpace(lease.Subject.OrgID)
	lease.Resource.ProviderID = strings.ToLower(strings.TrimSpace(lease.Resource.ProviderID))
	if ResourceGenerationID(lease) != "" ||
		strings.TrimSpace(lease.Metadata[MetadataKeyDecommissionClaimDigest]) != "" {
		return vmlease.Lease{}, ErrResourceGenerationImmutable
	}
	if strings.TrimSpace(lease.Resource.EngineVMID) != "" ||
		strings.TrimSpace(lease.Resource.SimulationID) != "" ||
		strings.TrimSpace(lease.Resource.VMID) != "" {
		return vmlease.Lease{}, ErrProviderRefRequired
	}
	if now.IsZero() {
		return vmlease.Lease{}, fmt.Errorf("%w: database time required", ErrResourceGenerationUnavailable)
	}
	if validFor == 0 {
		validFor = DefaultNativeAdmissionValidity
	}
	if validFor < time.Minute || validFor > MaxNativeAdmissionValidity {
		return vmlease.Lease{}, fmt.Errorf("%w: invalid native lease validity", ErrResourceGenerationUnavailable)
	}
	if !lease.ValidFrom.IsZero() || !lease.ValidUntil.IsZero() ||
		!lease.RenewedAt.IsZero() || lease.CancelledAt != nil {
		return vmlease.Lease{}, fmt.Errorf("%w: lease authority times must be unset", ErrResourceGenerationUnavailable)
	}
	now = now.UTC()
	lease.ValidFrom, lease.ValidUntil, lease.RenewedAt = now, now.Add(validFor), now
	setLeaseMetadata(&lease, MetadataKeyResourceGenerationID, generationID)
	if err := lease.Validate(now); err != nil {
		return vmlease.Lease{}, err
	}
	return lease, nil
}
