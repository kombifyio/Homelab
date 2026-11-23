package vmleases

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kombifyio/homelab/internal/runtimeproduct/vmlease"
)

// ErrNativeRenewalRefused means the lease is no longer the live, uncancelled
// subscription lease under provider-control custody, bound to a server that is
// not being torn down and with unreleased capacity, that the caller named.
// Nothing was changed.
var ErrNativeRenewalRefused = errors.New("vmleases: native lease renewal refused")

// NativeRenewalRequest names one exact lease generation whose owner the caller
// has just proved entitled. Renewal extends only the validity window: lease
// id, revision, resource generation, desired state and every metadata key stay
// as they are, so the resource generation digest and every admission bound to
// the lease remain valid.
type NativeRenewalRequest struct {
	TenantID             string
	LeaseID              vmlease.LeaseID
	OwnerSubjectID       string
	ResourceGenerationID string
	// ValidFor is measured from the transaction's database time. Zero selects
	// DefaultNativeAdmissionValidity, the window of a fresh admission.
	ValidFor time.Duration
}

// NativeRenewalResult reports the window before and after. Renewed is false
// when the stored window already reaches past database time plus ValidFor.
type NativeRenewalResult struct {
	Renewed            bool
	PreviousValidUntil time.Time
	ValidUntil         time.Time
	DatabaseTime       time.Time
}

type nativeRenewalState struct {
	lease        *vmlease.Lease
	revision     uint64
	serverID     string
	databaseTime time.Time
}

// NativeRenewalEligible reports, without locking or writing, whether
// RenewNativeLease would currently consider the lease at all. Callers use it
// to skip the entitlement read for a lease that cannot renew; RenewNativeLease
// repeats every check under its locks.
func (s *PostgresStore) NativeRenewalEligible(ctx context.Context, request NativeRenewalRequest) (bool, error) {
	tenantID, err := validateNativeRenewalRequest(s, request)
	if err != nil {
		return false, err
	}
	eligible := false
	err = s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		_, stateErr := loadNativeRenewalStateTx(ctx, tx, tenantID, request, false)
		if errors.Is(stateErr, ErrNativeRenewalRefused) {
			return nil
		}
		if stateErr != nil {
			return stateErr
		}
		eligible = true
		return nil
	})
	return eligible, err
}

// RenewNativeLease extends the validity window of one live provider-control
// subscription lease to database time plus ValidFor. It never shortens a
// window and never touches a cancelled lease, a lease that requests provider
// absence or carries a decommission claim, a lease whose server is gone or
// being torn down, a lease whose capacity was released, or a lease whose
// execution authority has left provider control (a custody archive removes
// it).
func (s *PostgresStore) RenewNativeLease(ctx context.Context, request NativeRenewalRequest) (NativeRenewalResult, error) {
	tenantID, err := validateNativeRenewalRequest(s, request)
	if err != nil {
		return NativeRenewalResult{}, err
	}
	validFor := request.ValidFor
	if validFor == 0 {
		validFor = DefaultNativeAdmissionValidity
	}
	if validFor < time.Minute || validFor > MaxNativeAdmissionValidity {
		return NativeRenewalResult{}, fmt.Errorf("vmleases: invalid native renewal validity")
	}
	var result NativeRenewalResult
	err = s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		state, stateErr := loadNativeRenewalStateTx(ctx, tx, tenantID, request, true)
		if stateErr != nil {
			return stateErr
		}
		lease, databaseTime := state.lease, state.databaseTime
		result = NativeRenewalResult{
			PreviousValidUntil: lease.ValidUntil, ValidUntil: lease.ValidUntil, DatabaseTime: databaseTime,
		}
		renewedUntil := databaseTime.Add(validFor)
		if !renewedUntil.After(lease.ValidUntil) {
			return nil
		}
		lease.ValidUntil = renewedUntil
		lease.RenewedAt = databaseTime
		if err := lease.Validate(databaseTime); err != nil {
			return fmt.Errorf("vmleases: renewed lease invalid: %w", err)
		}
		if _, err := RuntimeContract(*lease, state.revision, state.serverID, databaseTime); err != nil {
			return err
		}
		encoded, err := encodeLease(*lease)
		if err != nil {
			return fmt.Errorf("vmleases: encode renewed lease: %w", err)
		}
		updated, err := tx.ExecContext(ctx, `
			UPDATE techstack_vm_leases
			SET valid_until = $4, renewed_at = $5, lease_json = $6::jsonb, updated_at = $5
			WHERE tenant_id = $1 AND id = $2 AND resource_generation_id = $3::uuid
			  AND cancelled_at IS NULL AND valid_until = $7
		`, tenantID, request.LeaseID, strings.TrimSpace(request.ResourceGenerationID),
			lease.ValidUntil, databaseTime, encoded, result.PreviousValidUntil)
		if err != nil {
			return fmt.Errorf("vmleases: renew native lease: %w", err)
		}
		if rows, err := updated.RowsAffected(); err != nil || rows != 1 {
			return ErrNativeRenewalRefused
		}
		result.Renewed = true
		result.ValidUntil = lease.ValidUntil
		return nil
	})
	if err != nil {
		return NativeRenewalResult{}, err
	}
	return result, nil
}

func validateNativeRenewalRequest(s *PostgresStore, request NativeRenewalRequest) (string, error) {
	if s == nil || s.db == nil {
		return "", fmt.Errorf("vmleases: database not configured")
	}
	tenantID := strings.TrimSpace(request.TenantID)
	if tenantID == "" {
		return "", ErrTenantRequired
	}
	if strings.TrimSpace(string(request.LeaseID)) == "" || strings.TrimSpace(request.OwnerSubjectID) == "" ||
		strings.TrimSpace(request.ResourceGenerationID) == "" {
		return "", fmt.Errorf("vmleases: native renewal lease, owner and generation required")
	}
	return tenantID, nil
}

// loadNativeRenewalStateTx reads the lease with its server binding and checks
// every renewal condition. With lock it takes the lease row FOR UPDATE and the
// server row FOR SHARE in one statement: the custody archive locks the same
// lease row before it removes the execution authority, and a server teardown
// must update the server row, so neither can commit between this check and
// the renewal.
func loadNativeRenewalStateTx(
	ctx context.Context,
	tx *sql.Tx,
	tenantID string,
	request NativeRenewalRequest,
	lock bool,
) (nativeRenewalState, error) {
	leaseID := strings.TrimSpace(string(request.LeaseID))
	ownerSubjectID := strings.TrimSpace(request.OwnerSubjectID)
	generationID := strings.TrimSpace(request.ResourceGenerationID)
	var databaseTime time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&databaseTime); err != nil {
		return nativeRenewalState{}, fmt.Errorf("vmleases: read native renewal database time: %w", err)
	}
	locking := ""
	if lock {
		locking = "FOR UPDATE OF lease FOR SHARE OF server"
	}
	var payload []byte
	var desiredState string
	var serverID sql.NullString
	var revision sql.NullInt64
	var validFrom, validUntil, renewedAt, cancelledAt sql.NullTime
	err := tx.QueryRowContext(ctx, `
		SELECT lease.lease_json::text, lease.desired_state, lease.valid_from, lease.valid_until,
		       lease.renewed_at, lease.cancelled_at, lease.lease_revision, lease.server_id
		FROM techstack_vm_leases AS lease
		JOIN servers AS server
		  ON server.tenant_id = lease.tenant_id
		 AND server.id = lease.server_id
		 AND server.lease_id = lease.id
		WHERE lease.tenant_id = $1 AND lease.id = $2 AND lease.owner_subject_id = $3
		  AND lease.resource_generation_id = $4::uuid
		  AND lease.cancelled_at IS NULL AND lease.desired_state IN ('running', 'stopped')
		  AND server.desired_state IS DISTINCT FROM 'absent'
		  AND COALESCE(server.lifecycle_state, '') NOT IN ('decommissioning', 'decommissioned')
		  AND server.decommissioned_at IS NULL
		`+locking, tenantID, leaseID, ownerSubjectID, generationID).Scan(
		&payload, &desiredState, &validFrom, &validUntil, &renewedAt, &cancelledAt, &revision, &serverID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nativeRenewalState{}, ErrNativeRenewalRefused
	}
	if err != nil {
		return nativeRenewalState{}, fmt.Errorf("vmleases: read native renewal lease: %w", err)
	}
	var authority sql.NullString
	var released bool
	if err := tx.QueryRowContext(ctx, `
		SELECT
			(SELECT execution_authority FROM runtime_lease_execution_authorities
			 WHERE tenant_id = $1 AND lease_id = $2),
			EXISTS (SELECT 1 FROM managed_runtime_capacity_release_facts
			        WHERE tenant_id = $1 AND lease_id = $2 AND resource_generation_id = $3::uuid)
	`, tenantID, leaseID, generationID).Scan(&authority, &released); err != nil {
		return nativeRenewalState{}, fmt.Errorf("vmleases: read native renewal custody: %w", err)
	}
	if released || normalizeLeaseExecutionAuthority(LeaseExecutionAuthority(authority.String)) != LeaseExecutionAuthorityHomelabProviderControl {
		return nativeRenewalState{}, ErrNativeRenewalRefused
	}
	lease, err := decodeCanonicalLease(payload, desiredState, validFrom, validUntil, renewedAt, cancelledAt)
	if err != nil {
		return nativeRenewalState{}, err
	}
	if lease.CustodyClass != "" || lease.BillingMode != vmlease.BillingModeSubscription ||
		lease.LifecycleClass != vmlease.LifecycleClassSubscription ||
		ResourceGenerationID(*lease) != generationID ||
		strings.TrimSpace(lease.Metadata[MetadataKeyDecommissionClaimDigest]) != "" ||
		lease.ValidFrom.IsZero() || lease.ValidUntil.IsZero() ||
		!revision.Valid || revision.Int64 < 1 || strings.TrimSpace(serverID.String) == "" {
		return nativeRenewalState{}, ErrNativeRenewalRefused
	}
	return nativeRenewalState{
		lease: lease, revision: uint64(revision.Int64), serverID: serverID.String, databaseTime: databaseTime.UTC(),
	}, nil
}
