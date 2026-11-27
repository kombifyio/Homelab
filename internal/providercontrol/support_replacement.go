package providercontrol

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kombifyio/homelab/internal/runtimeproduct/serverruntime"
	"github.com/kombifyio/homelab/internal/runtimeproduct/vmlease"
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/monthlyruntime"
	"github.com/kombifyio/homelab/pkg/runtimeidentity"
	"github.com/kombifyio/homelab/pkg/serverregistry"
	"github.com/kombifyio/homelab/pkg/vmleases"
	"github.com/kombifyio/stackkits/contracts/providerexecutor/v1beta1"
)

const (
	SupportReplacementTargetProvider         = "ionos"
	SupportReplacementTargetPackageID        = "monthly-runtime-standard"
	SupportReplacementTargetVariantName      = "Memory Cube S"
	SupportReplacementTargetProductSlug      = "ionos-cube-standard-memory"
	SupportReplacementTargetExecutionProfile = "ionos-memory-cube-s-standard"
	SupportReplacementTargetProviderProfile  = "ionos-cloudapi-v6-memory-s"

	SupportReplacementPhaseAuthorized            = "authorized"
	SupportReplacementPhaseAccepted              = "accepted"
	SupportReplacementPhaseDestinationAdmitted   = "destination_admitted"
	SupportReplacementPhaseStaging               = "staging"
	SupportReplacementPhaseCutoverInProgress     = "cutover_in_progress"
	SupportReplacementPhaseCutoverVerified       = "cutover_verified"
	SupportReplacementPhaseSourceDecommissioning = "source_decommissioning"
	SupportReplacementPhaseCompleted             = "completed"
	SupportReplacementPhaseRollingBack           = "rolling_back"
	SupportReplacementPhaseRolledBack            = "rolled_back"
	SupportReplacementPhaseFencedConflict        = "fenced_conflict"
	SupportReplacementPhaseCancelled             = "cancelled"
)

type SupportReplacementRecord struct {
	TenantID                      string          `json:"tenant_id"`
	ReplacementID                 string          `json:"replacement_id"`
	OwnerSubjectID                string          `json:"owner_subject_id"`
	StackID                       string          `json:"stack_id"`
	RuntimeSlotID                 string          `json:"runtime_slot_id"`
	SourceLeaseID                 string          `json:"source_lease_id"`
	SourceServerID                string          `json:"source_server_id"`
	SourceResourceGenerationID    string          `json:"source_resource_generation_id"`
	SourceGenerationOrdinal       int64           `json:"source_generation_ordinal"`
	SourceProvider                string          `json:"source_provider"`
	SourcePackageID               string          `json:"source_package_id"`
	SourceProductSlug             string          `json:"source_product_slug"`
	SourceExecutionProfile        string          `json:"source_execution_profile"`
	TargetProvider                string          `json:"target_provider"`
	TargetPackageID               string          `json:"target_package_id"`
	TargetVariantName             string          `json:"target_variant_name"`
	TargetProductSlug             string          `json:"target_product_slug"`
	TargetExecutionProfile        string          `json:"target_execution_profile"`
	TargetProviderProfile         string          `json:"target_provider_profile"`
	NoChargeConfirmed             bool            `json:"no_charge_confirmed"`
	CaseRef                       string          `json:"case_ref"`
	Reason                        string          `json:"reason"`
	AuthorizedBy                  string          `json:"authorized_by"`
	AuthorizedAt                  time.Time       `json:"authorized_at"`
	EvidenceRef                   string          `json:"evidence_ref"`
	CustomerAcceptedAt            *time.Time      `json:"customer_accepted_at,omitempty"`
	CustomerAcceptedBy            string          `json:"customer_accepted_by,omitempty"`
	IdempotencyKey                string          `json:"idempotency_key"`
	RequestDigest                 string          `json:"request_digest"`
	DestinationLeaseID            string          `json:"destination_lease_id,omitempty"`
	DestinationServerID           string          `json:"destination_server_id,omitempty"`
	DestinationResourceGeneration string          `json:"destination_resource_generation_id,omitempty"`
	DestinationGenerationOrdinal  *int64          `json:"destination_generation_ordinal,omitempty"`
	DestinationOperationID        string          `json:"destination_operation_id,omitempty"`
	DestinationWritesObserved     bool            `json:"destination_writes_observed"`
	CutoverVerifiedAt             *time.Time      `json:"cutover_verified_at,omitempty"`
	SourceTeardownOperationID     string          `json:"source_teardown_operation_id,omitempty"`
	RollbackReason                string          `json:"rollback_reason,omitempty"`
	FencedConflictReason          string          `json:"fenced_conflict_reason,omitempty"`
	CheckpointJSON                json.RawMessage `json:"checkpoint_json,omitempty"`
	Phase                         string          `json:"phase"`
	SettledAt                     *time.Time      `json:"settled_at,omitempty"`
	CreatedAt                     time.Time       `json:"created_at"`
	UpdatedAt                     time.Time       `json:"updated_at"`
	Created                       bool            `json:"created"`
}

type AuthorizeSupportReplacementRequest struct {
	TenantID               string
	ReplacementID          string
	OwnerSubjectID         string
	StackID                string
	SourceLeaseID          string
	CaseRef                string
	Reason                 string
	AuthorizedBy           string
	EvidenceRef            string
	IdempotencyKey         string
	NoChargeConfirmed      bool
	TargetProvider         string
	TargetPackageID        string
	TargetVariantName      string
	TargetProductSlug      string
	TargetExecutionProfile string
	TargetProviderProfile  string
}

type AcceptSupportReplacementRequest struct {
	TenantID       string
	ReplacementID  string
	OwnerSubjectID string
	AcceptedBy     string
}

type SupportReplacementProvisionRequest struct {
	TenantID        string
	ReplacementID   string
	RuntimeSlotKey  string
	RuntimeServerID string
	IdempotencyKey  string
	ValidFor        time.Duration
	DesiredSpecRef  string
	DesiredSpec     json.RawMessage
	Region          string
	ServerName      string
	InstanceID      string
}

type SupportReplacementTransitionRequest struct {
	TenantID                  string
	ReplacementID             string
	Phase                     string
	DestinationWritesObserved *bool
	CutoverVerifiedAt         *time.Time
	SourceTeardownOperationID string
	RollbackReason            string
	FencedConflictReason      string
	CheckpointPatchJSON       json.RawMessage
}

type SupportReplacementCapacitySnapshot struct {
	HeldCapacity        int   `json:"held_capacity"`
	UnreleasedSlots     int   `json:"unreleased_slots"`
	ActiveOrdinal       int64 `json:"active_ordinal"`
	SourceReleased      bool  `json:"source_released"`
	DestinationReleased bool  `json:"destination_released"`
}

func (a *NativeAdmission) AuthorizeSupportReplacement(
	ctx context.Context,
	req AuthorizeSupportReplacementRequest,
) (SupportReplacementRecord, error) {
	ctx = SystemAuthorityContext(ctx)
	if !a.configured() {
		return SupportReplacementRecord{}, fmt.Errorf("%w: native admission is not configured", ErrInvalidRequest)
	}
	norm, digest, err := normalizeSupportReplacementAuthorization(req)
	if err != nil {
		return SupportReplacementRecord{}, err
	}
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return SupportReplacementRecord{}, fmt.Errorf("providercontrol: begin support replacement auth: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, tenantContextKey, norm.TenantID); err != nil {
		return SupportReplacementRecord{}, fmt.Errorf("providercontrol: set tenant context: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`,
		norm.TenantID, "support-replacement:auth:"+norm.IdempotencyKey); err != nil {
		return SupportReplacementRecord{}, fmt.Errorf("providercontrol: lock support replacement auth: %w", err)
	}

	if existing, found, err := loadSupportReplacementByIdempotencyTx(ctx, tx, norm.TenantID, norm.IdempotencyKey); err != nil {
		return SupportReplacementRecord{}, err
	} else if found {
		if existing.RequestDigest != digest {
			return SupportReplacementRecord{}, fmt.Errorf("%w: idempotency key %q already used with different support replacement intent", ErrNativeAdmissionConflict, norm.IdempotencyKey)
		}
		if norm.ReplacementID != "" && existing.ReplacementID != norm.ReplacementID {
			return SupportReplacementRecord{}, fmt.Errorf("%w: idempotency key %q belongs to replacement %q", ErrNativeAdmissionConflict, norm.IdempotencyKey, existing.ReplacementID)
		}
		if err := tx.Commit(); err != nil {
			return SupportReplacementRecord{}, fmt.Errorf("providercontrol: commit support replacement replay: %w", err)
		}
		committed = true
		existing.Created = false
		return existing, nil
	}

	source, err := loadEligibleSupportReplacementSourceTx(ctx, tx, norm)
	if err != nil {
		return SupportReplacementRecord{}, err
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`,
		norm.TenantID, "support-replacement:source:"+source.LeaseID+":"+source.GenerationID); err != nil {
		return SupportReplacementRecord{}, fmt.Errorf("providercontrol: lock support replacement source generation: %w", err)
	}
	var activeReplacementID string
	if err := tx.QueryRowContext(ctx, `
		SELECT replacement_id
		FROM managed_runtime_support_replacements
		WHERE tenant_id = $1
		  AND source_lease_id = $2
		  AND source_resource_generation_id = $3
		  AND phase NOT IN ('rolled_back', 'cancelled')
		LIMIT 1
	`, norm.TenantID, source.LeaseID, source.GenerationID).Scan(&activeReplacementID); err == nil {
		return SupportReplacementRecord{}, fmt.Errorf("%w: source lease %q generation %q already has active support replacement %q", ErrNativeAdmissionConflict, source.LeaseID, source.GenerationID, activeReplacementID)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return SupportReplacementRecord{}, fmt.Errorf("providercontrol: check active support replacement for source generation: %w", err)
	}
	replacementID := norm.ReplacementID
	if replacementID == "" {
		replacementID = "sr-" + uuid.NewString()
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO managed_runtime_support_replacements (
			tenant_id, replacement_id, owner_subject_id, stack_id, runtime_slot_id,
			source_lease_id, source_server_id, source_resource_generation_id, source_generation_ordinal,
			source_provider, source_package_id, source_product_slug, source_execution_profile,
			target_provider, target_package_id, target_variant_name, target_product_slug,
			target_execution_profile, target_provider_profile, no_charge_confirmed,
			case_ref, reason, authorized_by, evidence_ref, idempotency_key, request_digest, phase
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9,
			'ionos', $10, $11, $12,
			$13, $14, $15, $16,
			$17, $18, true,
			$19, $20, $21, $22, $23, $24, 'authorized'
		)
	`,
		norm.TenantID, replacementID, source.OwnerSubjectID, source.StackID, source.RuntimeSlotID,
		source.LeaseID, source.ServerID, source.GenerationID, source.GenerationOrdinal,
		SupportReplacementTargetPackageID, source.ProductSlug, source.ExecutionProfile,
		SupportReplacementTargetProvider, SupportReplacementTargetPackageID, SupportReplacementTargetVariantName,
		SupportReplacementTargetProductSlug, SupportReplacementTargetExecutionProfile, SupportReplacementTargetProviderProfile,
		norm.CaseRef, norm.Reason, norm.AuthorizedBy, norm.EvidenceRef, norm.IdempotencyKey, digest,
	); err != nil {
		return SupportReplacementRecord{}, mapSupportReplacementWriteErr("authorize support replacement", err)
	}
	rec, err := loadSupportReplacementByIDTx(ctx, tx, norm.TenantID, replacementID)
	if err != nil {
		return SupportReplacementRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return SupportReplacementRecord{}, fmt.Errorf("providercontrol: commit support replacement auth: %w", err)
	}
	committed = true
	rec.Created = true
	return rec, nil
}

func (a *NativeAdmission) AcceptSupportReplacement(
	ctx context.Context,
	req AcceptSupportReplacementRequest,
) (SupportReplacementRecord, error) {
	ctx = SystemAuthorityContext(ctx)
	if !a.configured() {
		return SupportReplacementRecord{}, fmt.Errorf("%w: native admission is not configured", ErrInvalidRequest)
	}
	tenantID := strings.TrimSpace(req.TenantID)
	replacementID := strings.TrimSpace(req.ReplacementID)
	acceptedBy := strings.TrimSpace(req.AcceptedBy)
	ownerSubjectID := strings.TrimSpace(req.OwnerSubjectID)
	if tenantID == "" || replacementID == "" || ownerSubjectID == "" || acceptedBy == "" {
		return SupportReplacementRecord{}, fmt.Errorf("%w: tenant_id, replacement_id, owner_subject_id, and accepted_by are required", ErrInvalidRequest)
	}
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return SupportReplacementRecord{}, fmt.Errorf("providercontrol: begin support replacement accept: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, tenantContextKey, tenantID); err != nil {
		return SupportReplacementRecord{}, fmt.Errorf("providercontrol: set tenant context: %w", err)
	}
	rec, err := lockSupportReplacementByIDTx(ctx, tx, tenantID, replacementID)
	if err != nil {
		return SupportReplacementRecord{}, err
	}
	if rec.OwnerSubjectID != ownerSubjectID || acceptedBy != rec.OwnerSubjectID {
		return SupportReplacementRecord{}, fmt.Errorf("%w: customer acceptance must match replacement owner %q", ErrNativeAdmissionConflict, rec.OwnerSubjectID)
	}
	if !rec.NoChargeConfirmed {
		return SupportReplacementRecord{}, fmt.Errorf("%w: support replacement requires confirmed zero-charge authorization", ErrNativeAdmissionConflict)
	}
	if rec.Phase != SupportReplacementPhaseAuthorized {
		if rec.CustomerAcceptedAt != nil && rec.Phase != SupportReplacementPhaseCancelled && rec.Phase != SupportReplacementPhaseRolledBack {
			if err := tx.Commit(); err != nil {
				return SupportReplacementRecord{}, fmt.Errorf("providercontrol: commit accept replay: %w", err)
			}
			committed = true
			return rec, nil
		}
		return SupportReplacementRecord{}, fmt.Errorf("%w: support replacement in phase %q cannot be accepted", ErrNativeAdmissionConflict, rec.Phase)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE managed_runtime_support_replacements
		SET phase = 'accepted',
		    customer_accepted_at = now(),
		    customer_accepted_by = $3
		WHERE tenant_id = $1 AND replacement_id = $2
	`, tenantID, replacementID, acceptedBy); err != nil {
		return SupportReplacementRecord{}, mapSupportReplacementWriteErr("accept support replacement", err)
	}
	updated, err := loadSupportReplacementByIDTx(ctx, tx, tenantID, replacementID)
	if err != nil {
		return SupportReplacementRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return SupportReplacementRecord{}, fmt.Errorf("providercontrol: commit support replacement accept: %w", err)
	}
	committed = true
	return updated, nil
}

// AdmitSupportReplacementProvision admits the free company-funded Memory Cube S
// destination cube for an already authorized and customer-accepted support
// replacement. It bypasses customer cloud.runtime.credits capacity budgets and
// customer-facing ionos_standard_photo_memory flags while keeping customer
// quota slots intact until cutover and source teardown.
func (a *NativeAdmission) AdmitSupportReplacementProvision(
	ctx context.Context,
	req SupportReplacementProvisionRequest,
) (NativeProvisionAdmissionResult, SupportReplacementRecord, error) {
	ctx = SystemAuthorityContext(ctx)
	if !a.configured() {
		return NativeProvisionAdmissionResult{}, SupportReplacementRecord{}, fmt.Errorf("%w: native admission is not configured", ErrInvalidRequest)
	}
	if err := a.providerCreates.RequireProviderCreate(ctx, SupportReplacementTargetProvider); err != nil {
		return NativeProvisionAdmissionResult{}, SupportReplacementRecord{}, err
	}
	if err := a.activation.Require(ctx); err != nil {
		return NativeProvisionAdmissionResult{}, SupportReplacementRecord{}, err
	}
	if err := a.IONOSStandardMemoryAvailable(ctx); err != nil {
		return NativeProvisionAdmissionResult{}, SupportReplacementRecord{}, err
	}

	tenantID := strings.TrimSpace(req.TenantID)
	replacementID := strings.TrimSpace(req.ReplacementID)
	if tenantID == "" || replacementID == "" {
		return NativeProvisionAdmissionResult{}, SupportReplacementRecord{}, fmt.Errorf("%w: tenant_id and replacement_id are required", ErrInvalidRequest)
	}
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return NativeProvisionAdmissionResult{}, SupportReplacementRecord{}, fmt.Errorf("providercontrol: begin support replacement provision: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, tenantContextKey, tenantID); err != nil {
		return NativeProvisionAdmissionResult{}, SupportReplacementRecord{}, fmt.Errorf("providercontrol: set tenant context: %w", err)
	}
	sr, err := lockSupportReplacementByIDTx(ctx, tx, tenantID, replacementID)
	if err != nil {
		return NativeProvisionAdmissionResult{}, SupportReplacementRecord{}, err
	}
	if err := validateSupportReplacementReadyForProvisionTx(ctx, tx, sr); err != nil {
		return NativeProvisionAdmissionResult{}, SupportReplacementRecord{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))
	`, tenantID, "managed-runtime-slot:"+sr.RuntimeSlotID); err != nil {
		return NativeProvisionAdmissionResult{}, SupportReplacementRecord{}, fmt.Errorf("providercontrol: lock support replacement slot: %w", err)
	}
	slotKey, destOrdinal, sourceMeta, err := resolveSupportReplacementProvisionInputsTx(ctx, tx, req, sr)
	if err != nil {
		return NativeProvisionAdmissionResult{}, SupportReplacementRecord{}, err
	}

	nativeReq, err := buildSupportReplacementNativeRequest(req, sr, slotKey, destOrdinal, sourceMeta)
	if err != nil {
		return NativeProvisionAdmissionResult{}, SupportReplacementRecord{}, err
	}
	norm, digest, digestText, err := normalizeSupportReplacementProvision(nativeReq)
	if err != nil {
		return NativeProvisionAdmissionResult{}, SupportReplacementRecord{}, err
	}
	if err := lockNativeAdmissionTx(ctx, tx, norm.TenantID, norm.IdempotencyKey); err != nil {
		return NativeProvisionAdmissionResult{}, SupportReplacementRecord{}, err
	}
	if replay, found, err := a.loadNativeAdmissionReplayTx(ctx, tx, norm, digest, digestText); err != nil {
		return NativeProvisionAdmissionResult{}, SupportReplacementRecord{}, err
	} else if found {
		if err := tx.Commit(); err != nil {
			return NativeProvisionAdmissionResult{}, SupportReplacementRecord{}, fmt.Errorf("providercontrol: commit support replacement provision replay: %w", err)
		}
		committed = true
		return replay, sr, nil
	}
	if sr.Phase != SupportReplacementPhaseAccepted {
		return NativeProvisionAdmissionResult{}, SupportReplacementRecord{}, fmt.Errorf("%w: support replacement phase %q cannot admit new destination", ErrNativeAdmissionConflict, sr.Phase)
	}

	if err := a.requireNativeExecutionOfferingTx(ctx, tx, norm.Lease); err != nil {
		return NativeProvisionAdmissionResult{}, SupportReplacementRecord{}, err
	}
	region, err := a.coordinator.registry.CanonicalCreateRegion(norm.Lease.Resource.ProviderID, norm.Lease.Metadata[runtimeOfferingMetadataKey], norm.Lease.Resource.Region)
	if err != nil {
		return NativeProvisionAdmissionResult{}, SupportReplacementRecord{}, err
	}
	norm.Lease.Resource.Region = region

	result, err := a.insertSupportReplacementAdmissionTx(ctx, tx, sr, norm, digest, digestText)
	if err != nil {
		return NativeProvisionAdmissionResult{}, SupportReplacementRecord{}, err
	}
	updatedSR, err := loadSupportReplacementByIDTx(ctx, tx, tenantID, replacementID)
	if err != nil {
		return NativeProvisionAdmissionResult{}, SupportReplacementRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return NativeProvisionAdmissionResult{}, SupportReplacementRecord{}, fmt.Errorf("providercontrol: commit support replacement provision: %w", err)
	}
	committed = true
	return result, updatedSR, nil
}

func (a *NativeAdmission) insertSupportReplacementAdmissionTx(
	ctx context.Context,
	tx *sql.Tx,
	sr SupportReplacementRecord,
	request NativeProvisionAdmissionRequest,
	requestDigest [sha256.Size]byte,
	requestDigestText string,
) (NativeProvisionAdmissionResult, error) {
	admittedLease, err := a.leases.AdmitNativeLeaseTx(ctx, tx, vmleases.NativeAdmissionRequest{
		Lease: request.Lease, OwnerSubjectID: request.OwnerSubjectID,
		ServerID: request.RuntimeServerID, IdempotencyKey: request.IdempotencyKey,
		ValidFor: request.ValidFor,
	})
	if err != nil {
		return NativeProvisionAdmissionResult{}, fmt.Errorf("providercontrol: admit support replacement lease: %w", err)
	}
	lease := admittedLease.Lease
	generationID := admittedLease.ResourceGenerationID
	databaseNow := admittedLease.DatabaseTime
	leaseRevision := admittedLease.LeaseRevision
	serverDesired, err := nativeServerDesiredState(lease.DesiredState)
	if err != nil {
		return NativeProvisionAdmissionResult{}, err
	}
	target := serverregistry.ManagedVPSRuntimeTarget(lease.Resource.ProviderID, lease.Resource.EngineVMID, string(lease.ID), databaseNow)
	serverEvent := controlplane.ServerEvent{
		TenantID: request.TenantID, ServerID: request.RuntimeServerID,
		ExpectedRevision: 0, Generation: 1,
		Authority: controlplane.ServerEventAuthorityControlPlane,
		Source:    nativeAdmissionSource, SourceID: "admission:" + hex.EncodeToString(requestDigest[:16]),
		ObservedAt: databaseNow,
		Outcome: providerProvisioningOutcome(lease.Resource.ProviderID, map[string]any{
			"runtime_server_id":   request.RuntimeServerID,
			"lease_id":            string(lease.ID),
			"support_replacement": sr.ReplacementID,
		}),
		Runtime: controlplane.ServerRuntime{
			ID: request.RuntimeServerID, TenantID: request.TenantID,
			InstanceID: request.Server.InstanceID, StackID: request.Server.StackID,
			OwnerSubjectID: request.OwnerSubjectID, LeaseID: string(lease.ID),
			ProviderRef: lease.Resource.ProviderID, RuntimeTarget: target,
			Name: request.Server.Name, LifecycleState: string(serverregistry.LifecyclePlanned),
			DesiredState: string(serverDesired), ConnectionState: string(serverregistry.ConnectionPending),
			HealthState: string(serverregistry.HealthUnknown), Metadata: request.Server.Metadata,
		},
		Evidence: map[string]any{
			"operation_scope":     nativeProvisionOperationScope,
			"request_digest":      requestDigestText,
			"support_replacement": sr.ReplacementID,
		},
	}
	serverResult, err := a.servers.ApplyServerEventTx(ctx, tx, serverEvent)
	if err != nil {
		return NativeProvisionAdmissionResult{}, fmt.Errorf("providercontrol: create support replacement runtime server: %w", err)
	}
	if !nativeServerAdmissionMatches(serverResult, string(lease.ID)) {
		return NativeProvisionAdmissionResult{}, fmt.Errorf("%w: runtime server admission projection mismatch", ErrNativeAdmissionConflict)
	}
	if err := ensureManagedCredentialAuthorityTx(ctx, tx, request.TenantID, lease, databaseNow, a.managedCredentialAuthorities); err != nil {
		return NativeProvisionAdmissionResult{}, err
	}
	desiredSpec := &DesiredSpecRevision{
		TenantID: request.TenantID, LeaseID: string(lease.ID), Revision: leaseRevision,
		Ref: request.DesiredSpecRef, Digest: sha256Digest(request.DesiredSpec),
		Payload: request.DesiredSpec, CreatedAt: databaseNow,
	}
	prepared, err := a.coordinator.prepareStartTx(ctx, tx, StartRequest{
		TenantID: request.TenantID, LeaseID: string(lease.ID),
		LeaseRevision: leaseRevision, RuntimeServerID: request.RuntimeServerID,
		ResourceGenerationID: generationID, Operation: providerexecutor.OperationProvision,
		IdempotencyKey: request.IdempotencyKey, LedgerRevision: nativeInitialLedgerRevision,
		DesiredSpec: desiredSpec, RequestedAt: databaseNow,
	})
	if err != nil {
		return NativeProvisionAdmissionResult{}, err
	}
	if err := insertNativeExecutionAuthorityTx(ctx, tx, request.TenantID, string(lease.ID), databaseNow); err != nil {
		return NativeProvisionAdmissionResult{}, err
	}
	record, created, err := a.ledger.beginOperationInTx(
		ctx, tx, ExecutionAuthorityHomelabProviderControl, prepared.profileSnapshot,
		prepared.provisionDispatch, prepared.command, prepared.initial, prepared.desiredSpec,
	)
	if err != nil {
		return NativeProvisionAdmissionResult{}, err
	}
	if !created {
		return NativeProvisionAdmissionResult{}, fmt.Errorf("%w: provider operation already exists without admission custody", ErrNativeAdmissionConflict)
	}
	if err := insertNativeIdempotencyTx(
		ctx, tx, request.TenantID, request.IdempotencyKey, requestDigest[:], string(lease.ID), record.Command.OperationID,
	); err != nil {
		return NativeProvisionAdmissionResult{}, err
	}

	destOrdinal := int64(request.RuntimeSlotGeneration)
	if _, err := tx.ExecContext(ctx, `
		UPDATE managed_runtime_support_replacements
		SET destination_lease_id = $3,
		    destination_server_id = $4,
		    destination_resource_generation_id = $5,
		    destination_generation_ordinal = $6,
		    destination_operation_id = $7,
		    phase = 'destination_admitted'
		WHERE tenant_id = $1 AND replacement_id = $2
	`, request.TenantID, sr.ReplacementID, string(lease.ID), request.RuntimeServerID,
		generationID, destOrdinal, record.Command.OperationID,
	); err != nil {
		return NativeProvisionAdmissionResult{}, mapSupportReplacementWriteErr("bind destination to support replacement", err)
	}

	result := NativeProvisionAdmissionResult{
		RuntimeSlotKey: request.RuntimeSlotKey, RuntimeSlotID: request.RuntimeSlotID,
		RuntimeSlotGeneration: request.RuntimeSlotGeneration,
		LeaseID:               string(lease.ID), LeaseRevision: leaseRevision,
		RuntimeServerID: request.RuntimeServerID, ResourceGenerationID: generationID,
		Operation: record, RequestDigest: requestDigestText, Created: true,
	}
	tempGrant := normalizedCapacityGrant{
		ScopeKind:      managedRuntimeCapacityScopeOwner,
		ScopeID:        request.OwnerSubjectID,
		Mode:           CapacityModeLimited,
		Limit:          1,
		DecisionSource: CapacityDecisionSourceSupportReplacementMemoryCubeS,
		PolicyDigest: managedRuntimeCapacityPolicyDigest(
			request.TenantID, SupportReplacementTargetProvider, managedRuntimeCapacityScopeOwner,
			request.OwnerSubjectID, CapacityModeLimited, 1, CapacityDecisionSourceSupportReplacementMemoryCubeS,
		),
	}
	if err := insertManagedRuntimeCapacityReservationTx(ctx, tx, request, result, tempGrant, databaseNow); err != nil {
		return NativeProvisionAdmissionResult{}, err
	}
	if err := insertManagedRuntimeServerSlotTx(ctx, tx, request, result, databaseNow); err != nil {
		return NativeProvisionAdmissionResult{}, err
	}
	return result, nil
}

func (a *NativeAdmission) TransitionSupportReplacement(
	ctx context.Context,
	req SupportReplacementTransitionRequest,
) (SupportReplacementRecord, error) {
	ctx = SystemAuthorityContext(ctx)
	if !a.configured() {
		return SupportReplacementRecord{}, fmt.Errorf("%w: native admission is not configured", ErrInvalidRequest)
	}
	tenantID := strings.TrimSpace(req.TenantID)
	replacementID := strings.TrimSpace(req.ReplacementID)
	nextPhase := strings.TrimSpace(req.Phase)
	if tenantID == "" || replacementID == "" || nextPhase == "" {
		return SupportReplacementRecord{}, fmt.Errorf("%w: tenant_id, replacement_id, and phase are required", ErrInvalidRequest)
	}
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return SupportReplacementRecord{}, fmt.Errorf("providercontrol: begin support replacement transition: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, tenantContextKey, tenantID); err != nil {
		return SupportReplacementRecord{}, fmt.Errorf("providercontrol: set tenant context: %w", err)
	}
	rec, err := lockSupportReplacementByIDTx(ctx, tx, tenantID, replacementID)
	if err != nil {
		return SupportReplacementRecord{}, err
	}
	writesObserved := rec.DestinationWritesObserved
	if req.DestinationWritesObserved != nil {
		if rec.DestinationWritesObserved && !*req.DestinationWritesObserved {
			return SupportReplacementRecord{}, fmt.Errorf("%w: destination_writes_observed cannot be cleared", ErrNativeAdmissionConflict)
		}
		writesObserved = *req.DestinationWritesObserved
	}
	if nextPhase == SupportReplacementPhaseRolledBack && writesObserved && rec.Phase != SupportReplacementPhaseFencedConflict {
		return SupportReplacementRecord{}, fmt.Errorf("%w: cannot roll back support replacement after destination writes were observed", ErrNativeAdmissionConflict)
	}
	cutoverAt := rec.CutoverVerifiedAt
	if req.CutoverVerifiedAt != nil {
		utc := req.CutoverVerifiedAt.UTC()
		cutoverAt = &utc
	}
	teardownOpID := rec.SourceTeardownOperationID
	if v := strings.TrimSpace(req.SourceTeardownOperationID); v != "" {
		teardownOpID = v
	}
	rollbackReason := rec.RollbackReason
	if v := strings.TrimSpace(req.RollbackReason); v != "" {
		rollbackReason = v
	}
	fencedReason := rec.FencedConflictReason
	if v := strings.TrimSpace(req.FencedConflictReason); v != "" {
		fencedReason = v
	}
	checkpointPatch := "{}"
	if len(req.CheckpointPatchJSON) > 0 {
		canonicalPatch, err := canonicalJSON(req.CheckpointPatchJSON)
		if err != nil {
			return SupportReplacementRecord{}, fmt.Errorf("%w: invalid checkpoint_json patch: %v", ErrInvalidRequest, err)
		}
		checkpointPatch = string(canonicalPatch)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE managed_runtime_support_replacements
		SET phase = $3,
		    destination_writes_observed = $4,
		    cutover_verified_at = $5,
		    source_teardown_operation_id = NULLIF($6, ''),
		    rollback_reason = NULLIF($7, ''),
		    fenced_conflict_reason = NULLIF($8, ''),
		    checkpoint_json = COALESCE(checkpoint_json, '{}'::jsonb) || $9::jsonb
		WHERE tenant_id = $1 AND replacement_id = $2
	`, tenantID, replacementID, nextPhase, writesObserved, cutoverAt, teardownOpID, rollbackReason, fencedReason, checkpointPatch); err != nil {
		return SupportReplacementRecord{}, mapSupportReplacementWriteErr("transition support replacement", err)
	}
	updated, err := loadSupportReplacementByIDTx(ctx, tx, tenantID, replacementID)
	if err != nil {
		return SupportReplacementRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return SupportReplacementRecord{}, fmt.Errorf("providercontrol: commit support replacement transition: %w", err)
	}
	committed = true
	return updated, nil
}

// ValidateSupportReplacementCLIRollback verifies pre-write CLI rollback
// preconditions without mutating or locking the row in an immutable terminal phase.
func (a *NativeAdmission) ValidateSupportReplacementCLIRollback(
	ctx context.Context,
	tenantID, replacementID, reason string,
) (SupportReplacementRecord, error) {
	ctx = SystemAuthorityContext(ctx)
	if !a.configured() {
		return SupportReplacementRecord{}, fmt.Errorf("%w: native admission is not configured", ErrInvalidRequest)
	}
	tenantID = strings.TrimSpace(tenantID)
	replacementID = strings.TrimSpace(replacementID)
	reason = strings.TrimSpace(reason)
	if tenantID == "" || replacementID == "" || reason == "" {
		return SupportReplacementRecord{}, fmt.Errorf("%w: tenant_id, replacement_id, and reason are required", ErrInvalidRequest)
	}
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return SupportReplacementRecord{}, fmt.Errorf("providercontrol: begin validate cli rollback: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, tenantContextKey, tenantID); err != nil {
		return SupportReplacementRecord{}, fmt.Errorf("providercontrol: set tenant context: %w", err)
	}
	rec, err := loadSupportReplacementByIDTx(ctx, tx, tenantID, replacementID)
	if err != nil {
		return SupportReplacementRecord{}, err
	}
	if err := validateSupportReplacementCLIRollbackTx(ctx, tx, tenantID, rec); err != nil {
		return SupportReplacementRecord{}, err
	}
	return rec, nil
}

func validateSupportReplacementCLIRollbackTx(
	ctx context.Context,
	tx *sql.Tx,
	tenantID string,
	rec SupportReplacementRecord,
) error {
	if rec.Phase == SupportReplacementPhaseRolledBack {
		return nil
	}
	if rec.DestinationWritesObserved {
		return fmt.Errorf("%w: cannot roll back after destination writes were observed", ErrNativeAdmissionConflict)
	}
	switch rec.Phase {
	case SupportReplacementPhaseAuthorized, SupportReplacementPhaseAccepted,
		SupportReplacementPhaseDestinationAdmitted, SupportReplacementPhaseStaging,
		SupportReplacementPhaseRollingBack:
	default:
		return fmt.Errorf("%w: phase %q is not eligible for pre-write CLI rollback; run workflow or conflict reconciliation", ErrNativeAdmissionConflict, rec.Phase)
	}
	sourceReleased, err := leaseCapacityReleasedTx(ctx, tx, tenantID, rec.SourceLeaseID, rec.SourceResourceGenerationID)
	if err != nil {
		return err
	}
	if sourceReleased {
		return fmt.Errorf("%w: cannot mark pre-write rollback when source lease %q is already released", ErrNativeAdmissionConflict, rec.SourceLeaseID)
	}
	if rec.DestinationLeaseID != "" {
		absent, released, err := leaseAbsenceAndReleaseTx(ctx, tx, tenantID, rec.DestinationLeaseID, rec.DestinationResourceGeneration)
		if err != nil {
			return err
		}
		if !absent || !released {
			return fmt.Errorf("%w: destination lease %q requires verified provider absence (%v) and capacity release (%v) before CLI rollback", ErrNativeAdmissionConflict, rec.DestinationLeaseID, absent, released)
		}
	}
	return nil
}

// RollbackSupportReplacementFromCLI records a manual pre-write rollback only
// after proving no destination writes occurred and any admitted destination
// lease has both provider-absence and capacity-release custody on record.
func (a *NativeAdmission) RollbackSupportReplacementFromCLI(
	ctx context.Context,
	tenantID, replacementID, reason string,
) (SupportReplacementRecord, error) {
	ctx = SystemAuthorityContext(ctx)
	if !a.configured() {
		return SupportReplacementRecord{}, fmt.Errorf("%w: native admission is not configured", ErrInvalidRequest)
	}
	tenantID = strings.TrimSpace(tenantID)
	replacementID = strings.TrimSpace(replacementID)
	reason = strings.TrimSpace(reason)
	if tenantID == "" || replacementID == "" || reason == "" {
		return SupportReplacementRecord{}, fmt.Errorf("%w: tenant_id, replacement_id, and reason are required", ErrInvalidRequest)
	}
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return SupportReplacementRecord{}, fmt.Errorf("providercontrol: begin cli rollback: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, tenantContextKey, tenantID); err != nil {
		return SupportReplacementRecord{}, fmt.Errorf("providercontrol: set tenant context: %w", err)
	}
	rec, err := lockSupportReplacementByIDTx(ctx, tx, tenantID, replacementID)
	if err != nil {
		return SupportReplacementRecord{}, err
	}
	if err := validateSupportReplacementCLIRollbackTx(ctx, tx, tenantID, rec); err != nil {
		return SupportReplacementRecord{}, err
	}
	if rec.Phase == SupportReplacementPhaseRolledBack {
		if err := tx.Commit(); err != nil {
			return SupportReplacementRecord{}, err
		}
		committed = true
		return rec, nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE managed_runtime_support_replacements
		SET phase = 'rolled_back',
		    rollback_reason = $3
		WHERE tenant_id = $1 AND replacement_id = $2
	`, tenantID, replacementID, reason); err != nil {
		return SupportReplacementRecord{}, mapSupportReplacementWriteErr("cli rollback support replacement", err)
	}
	updated, err := loadSupportReplacementByIDTx(ctx, tx, tenantID, replacementID)
	if err != nil {
		return SupportReplacementRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return SupportReplacementRecord{}, fmt.Errorf("providercontrol: commit cli rollback: %w", err)
	}
	committed = true
	return updated, nil
}

type supportReplacementReconcilePlan struct {
	writesObserved       bool
	cutoverVerifiedAt    *time.Time
	fencedConflictReason string
	rollbackReason       string
}

// ValidateSupportReplacementConflictReconcile checks conflict reconciliation
// preconditions without mutating or locking the row in an immutable terminal phase.
func (a *NativeAdmission) ValidateSupportReplacementConflictReconcile(
	ctx context.Context,
	tenantID, replacementID, targetPhase, reason string,
) (SupportReplacementRecord, error) {
	ctx = SystemAuthorityContext(ctx)
	if !a.configured() {
		return SupportReplacementRecord{}, fmt.Errorf("%w: native admission is not configured", ErrInvalidRequest)
	}
	tenantID = strings.TrimSpace(tenantID)
	replacementID = strings.TrimSpace(replacementID)
	targetPhase = strings.TrimSpace(targetPhase)
	reason = strings.TrimSpace(reason)
	if tenantID == "" || replacementID == "" || reason == "" {
		return SupportReplacementRecord{}, fmt.Errorf("%w: tenant_id, replacement_id, and reason are required", ErrInvalidRequest)
	}
	if !isValidSupportReplacementReconcileTarget(targetPhase) {
		return SupportReplacementRecord{}, fmt.Errorf("%w: conflict reconcile target must be completed, rolled_back, cutover_in_progress, or rolling_back", ErrInvalidRequest)
	}
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return SupportReplacementRecord{}, fmt.Errorf("providercontrol: begin validate conflict reconcile: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, tenantContextKey, tenantID); err != nil {
		return SupportReplacementRecord{}, fmt.Errorf("providercontrol: set tenant context: %w", err)
	}
	rec, err := loadSupportReplacementByIDTx(ctx, tx, tenantID, replacementID)
	if err != nil {
		return SupportReplacementRecord{}, err
	}
	if _, err := buildSupportReplacementReconcilePlanTx(ctx, tx, tenantID, rec, targetPhase, reason); err != nil {
		return SupportReplacementRecord{}, err
	}
	return rec, nil
}

func isValidSupportReplacementReconcileTarget(targetPhase string) bool {
	switch targetPhase {
	case SupportReplacementPhaseCompleted,
		SupportReplacementPhaseRolledBack,
		SupportReplacementPhaseCutoverInProgress,
		SupportReplacementPhaseRollingBack:
		return true
	default:
		return false
	}
}

func buildSupportReplacementReconcilePlanTx(
	ctx context.Context,
	tx *sql.Tx,
	tenantID string,
	rec SupportReplacementRecord,
	targetPhase, reason string,
) (supportReplacementReconcilePlan, error) {
	if rec.Phase != SupportReplacementPhaseFencedConflict {
		return supportReplacementReconcilePlan{}, fmt.Errorf("%w: conflict reconciliation requires phase fenced_conflict (got %q)", ErrNativeAdmissionConflict, rec.Phase)
	}
	sourceAbsent, sourceReleased, err := leaseAbsenceAndReleaseTx(ctx, tx, tenantID, rec.SourceLeaseID, rec.SourceResourceGenerationID)
	if err != nil {
		return supportReplacementReconcilePlan{}, err
	}
	destAbsent, destReleased := true, true
	if rec.DestinationLeaseID != "" {
		destAbsent, destReleased, err = leaseAbsenceAndReleaseTx(ctx, tx, tenantID, rec.DestinationLeaseID, rec.DestinationResourceGeneration)
		if err != nil {
			return supportReplacementReconcilePlan{}, err
		}
	}
	requireActiveServer := func(serverID, label string) error {
		if strings.TrimSpace(serverID) == "" {
			return fmt.Errorf("%w: %s server id is missing", ErrNativeAdmissionConflict, label)
		}
		var lifecycle string
		var decomm bool
		if qErr := tx.QueryRowContext(ctx, `
			SELECT lifecycle_state, decommissioned_at IS NOT NULL
			FROM servers
			WHERE tenant_id = $1 AND id = $2
		`, tenantID, serverID).Scan(&lifecycle, &decomm); qErr != nil {
			return fmt.Errorf("providercontrol: verify %s server active during conflict reconcile: %w", label, qErr)
		}
		if lifecycle != string(serverregistry.LifecycleActive) || decomm {
			return fmt.Errorf("%w: %s server %q must be active and not decommissioned (got %q, decommissioned=%v)", ErrNativeAdmissionConflict, label, serverID, lifecycle, decomm)
		}
		return nil
	}
	plan := supportReplacementReconcilePlan{
		fencedConflictReason: "reconciled:" + reason,
		writesObserved:       rec.DestinationWritesObserved,
		cutoverVerifiedAt:    rec.CutoverVerifiedAt,
	}
	switch targetPhase {
	case SupportReplacementPhaseRolledBack:
		if rec.DestinationLeaseID != "" && (!destAbsent || !destReleased) {
			return supportReplacementReconcilePlan{}, fmt.Errorf("%w: cannot reconcile conflict to rolled_back until destination absence (%v) and release (%v) are verified", ErrNativeAdmissionConflict, destAbsent, destReleased)
		}
		if sourceReleased {
			return supportReplacementReconcilePlan{}, fmt.Errorf("%w: cannot reconcile conflict to rolled_back after source lease %q was released", ErrNativeAdmissionConflict, rec.SourceLeaseID)
		}
		if err := requireActiveServer(rec.SourceServerID, "source"); err != nil {
			return supportReplacementReconcilePlan{}, err
		}
		plan.rollbackReason = reason
	case SupportReplacementPhaseRollingBack:
		if rec.DestinationWritesObserved || rec.CutoverVerifiedAt != nil {
			return supportReplacementReconcilePlan{}, fmt.Errorf("%w: cannot resume rolling_back after destination writes (%v) or cutover verification", ErrNativeAdmissionConflict, rec.DestinationWritesObserved)
		}
		if sourceReleased {
			return supportReplacementReconcilePlan{}, fmt.Errorf("%w: cannot resume rolling_back after source lease %q was released", ErrNativeAdmissionConflict, rec.SourceLeaseID)
		}
		if err := requireActiveServer(rec.SourceServerID, "source"); err != nil {
			return supportReplacementReconcilePlan{}, err
		}
		plan.fencedConflictReason = "reconciled_rollback:" + reason
		plan.rollbackReason = reason
	case SupportReplacementPhaseCutoverInProgress:
		if rec.DestinationLeaseID == "" {
			return supportReplacementReconcilePlan{}, fmt.Errorf("%w: cannot resume cutover_in_progress without an admitted destination lease", ErrNativeAdmissionConflict)
		}
		if sourceReleased {
			return supportReplacementReconcilePlan{}, fmt.Errorf("%w: cannot resume cutover_in_progress after source lease %q was released", ErrNativeAdmissionConflict, rec.SourceLeaseID)
		}
		if destReleased {
			return supportReplacementReconcilePlan{}, fmt.Errorf("%w: cannot resume cutover_in_progress after destination lease %q was released", ErrNativeAdmissionConflict, rec.DestinationLeaseID)
		}
		if err := requireActiveServer(rec.DestinationServerID, "destination"); err != nil {
			return supportReplacementReconcilePlan{}, err
		}
		plan.fencedConflictReason = "reconciled_resume:" + reason
	case SupportReplacementPhaseCompleted:
		if rec.DestinationLeaseID == "" {
			return supportReplacementReconcilePlan{}, fmt.Errorf("%w: cannot reconcile conflict to completed without a destination lease", ErrNativeAdmissionConflict)
		}
		if !sourceAbsent || !sourceReleased {
			return supportReplacementReconcilePlan{}, fmt.Errorf("%w: cannot reconcile conflict to completed until source absence (%v) and release (%v) are verified", ErrNativeAdmissionConflict, sourceAbsent, sourceReleased)
		}
		if destReleased {
			return supportReplacementReconcilePlan{}, fmt.Errorf("%w: cannot reconcile conflict to completed when destination lease %q is already released", ErrNativeAdmissionConflict, rec.DestinationLeaseID)
		}
		if err := requireActiveServer(rec.DestinationServerID, "destination"); err != nil {
			return supportReplacementReconcilePlan{}, err
		}
		plan.writesObserved = true
		if plan.cutoverVerifiedAt == nil {
			now := time.Now().UTC()
			plan.cutoverVerifiedAt = &now
		}
	}
	return plan, nil
}

// ReconcileSupportReplacementConflict settles a fenced_conflict replacement
// strictly after verifying provider absence + capacity release readback for the
// retired cube and confirming the surviving cube remains active and unreleased.
func (a *NativeAdmission) ReconcileSupportReplacementConflict(
	ctx context.Context,
	tenantID, replacementID, targetPhase, reason string,
) (SupportReplacementRecord, error) {
	ctx = SystemAuthorityContext(ctx)
	if !a.configured() {
		return SupportReplacementRecord{}, fmt.Errorf("%w: native admission is not configured", ErrInvalidRequest)
	}
	tenantID = strings.TrimSpace(tenantID)
	replacementID = strings.TrimSpace(replacementID)
	targetPhase = strings.TrimSpace(targetPhase)
	reason = strings.TrimSpace(reason)
	if tenantID == "" || replacementID == "" || reason == "" {
		return SupportReplacementRecord{}, fmt.Errorf("%w: tenant_id, replacement_id, and reason are required", ErrInvalidRequest)
	}
	if !isValidSupportReplacementReconcileTarget(targetPhase) {
		return SupportReplacementRecord{}, fmt.Errorf("%w: conflict reconcile target must be completed, rolled_back, cutover_in_progress, or rolling_back", ErrInvalidRequest)
	}
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return SupportReplacementRecord{}, fmt.Errorf("providercontrol: begin conflict reconcile: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, tenantContextKey, tenantID); err != nil {
		return SupportReplacementRecord{}, fmt.Errorf("providercontrol: set tenant context: %w", err)
	}
	rec, err := lockSupportReplacementByIDTx(ctx, tx, tenantID, replacementID)
	if err != nil {
		return SupportReplacementRecord{}, err
	}
	plan, err := buildSupportReplacementReconcilePlanTx(ctx, tx, tenantID, rec, targetPhase, reason)
	if err != nil {
		return SupportReplacementRecord{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE managed_runtime_support_replacements
		SET phase = $3,
		    destination_writes_observed = $4,
		    cutover_verified_at = COALESCE(cutover_verified_at, $5),
		    fenced_conflict_reason = $6,
		    rollback_reason = COALESCE(NULLIF(rollback_reason, ''), NULLIF($7, ''))
		WHERE tenant_id = $1 AND replacement_id = $2
	`, tenantID, replacementID, targetPhase, plan.writesObserved, plan.cutoverVerifiedAt, plan.fencedConflictReason, plan.rollbackReason); err != nil {
		return SupportReplacementRecord{}, mapSupportReplacementWriteErr("reconcile support replacement conflict", err)
	}
	updated, err := loadSupportReplacementByIDTx(ctx, tx, tenantID, replacementID)
	if err != nil {
		return SupportReplacementRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return SupportReplacementRecord{}, fmt.Errorf("providercontrol: commit conflict reconcile: %w", err)
	}
	committed = true
	return updated, nil
}

func leaseCapacityReleasedTx(ctx context.Context, tx *sql.Tx, tenantID, leaseID, generationID string) (bool, error) {
	var released bool
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM managed_runtime_capacity_release_facts
			WHERE tenant_id = $1
			  AND lease_id = $2
			  AND resource_generation_id::text = $3
		)
	`, tenantID, leaseID, generationID).Scan(&released)
	if err != nil {
		return false, fmt.Errorf("providercontrol: check capacity release fact: %w", err)
	}
	return released, nil
}

func leaseAbsenceAndReleaseTx(ctx context.Context, tx *sql.Tx, tenantID, leaseID, generationID string) (absent bool, released bool, err error) {
	err = tx.QueryRowContext(ctx, `
		SELECT
			(
				EXISTS (
					SELECT 1
					FROM provider_absence_observations AS obs
					JOIN provider_operations AS op
					  ON op.tenant_id = obs.tenant_id
					 AND op.operation_id = obs.operation_id
					WHERE obs.tenant_id = $1
					  AND op.lease_id = $2
					  AND (op.command_json #>> '{command,resource_generation_id}')::uuid = $3::uuid
					  AND obs.observation = 'absent'
					  AND obs.source = 'provider_api'
				)
				OR EXISTS (
					SELECT 1
					FROM managed_runtime_capacity_release_facts AS fact
					WHERE fact.tenant_id = $1
					  AND fact.lease_id = $2
					  AND fact.resource_generation_id = $3::uuid
					  AND fact.release_authority = 'resource_free_teardown'
				)
			),
			EXISTS (
				SELECT 1
				FROM managed_runtime_capacity_release_facts
				WHERE tenant_id = $1
				  AND lease_id = $2
				  AND resource_generation_id = $3::uuid
			)
	`, tenantID, leaseID, generationID).Scan(&absent, &released)
	if err != nil {
		return false, false, fmt.Errorf("providercontrol: check absence and release evidence: %w", err)
	}
	return absent, released, nil
}

func (a *NativeAdmission) GetSupportReplacement(
	ctx context.Context,
	tenantID, replacementID string,
) (SupportReplacementRecord, error) {
	ctx = SystemAuthorityContext(ctx)
	if !a.configured() {
		return SupportReplacementRecord{}, fmt.Errorf("%w: native admission is not configured", ErrInvalidRequest)
	}
	tenantID = strings.TrimSpace(tenantID)
	replacementID = strings.TrimSpace(replacementID)
	if tenantID == "" || replacementID == "" {
		return SupportReplacementRecord{}, fmt.Errorf("%w: tenant_id and replacement_id are required", ErrInvalidRequest)
	}
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return SupportReplacementRecord{}, fmt.Errorf("providercontrol: begin get support replacement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, tenantContextKey, tenantID); err != nil {
		return SupportReplacementRecord{}, fmt.Errorf("providercontrol: set tenant context: %w", err)
	}
	return loadSupportReplacementByIDTx(ctx, tx, tenantID, replacementID)
}

func (a *NativeAdmission) GetSupportReplacementByStack(
	ctx context.Context,
	tenantID, ownerSubjectID, stackID string,
) (*SupportReplacementRecord, error) {
	ctx = SystemAuthorityContext(ctx)
	if !a.configured() {
		return nil, fmt.Errorf("%w: native admission is not configured", ErrInvalidRequest)
	}
	tenantID = strings.TrimSpace(tenantID)
	ownerSubjectID = strings.TrimSpace(ownerSubjectID)
	stackID = strings.TrimSpace(stackID)
	if tenantID == "" || stackID == "" {
		return nil, fmt.Errorf("%w: tenant_id and stack_id are required", ErrInvalidRequest)
	}
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("providercontrol: begin get support replacement by stack: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, tenantContextKey, tenantID); err != nil {
		return nil, fmt.Errorf("providercontrol: set tenant context: %w", err)
	}
	row := tx.QueryRowContext(ctx, supportReplacementSelectColumns+`
		WHERE tenant_id = $1 AND stack_id = $2
		  AND ($3 = '' OR owner_subject_id = $3)
		ORDER BY created_at DESC
		LIMIT 1
	`, tenantID, stackID, ownerSubjectID)
	rec, err := scanSupportReplacementRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("providercontrol: load support replacement by stack: %w", err)
	}
	return &rec, nil
}

func (a *NativeAdmission) ListSupportReplacements(
	ctx context.Context,
	tenantID, phaseFilter string,
) ([]SupportReplacementRecord, error) {
	ctx = SystemAuthorityContext(ctx)
	if !a.configured() {
		return nil, fmt.Errorf("%w: native admission is not configured", ErrInvalidRequest)
	}
	tenantID = strings.TrimSpace(tenantID)
	phaseFilter = strings.TrimSpace(phaseFilter)
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenant_id is required", ErrInvalidRequest)
	}
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("providercontrol: begin list support replacements: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, tenantContextKey, tenantID); err != nil {
		return nil, fmt.Errorf("providercontrol: set tenant context: %w", err)
	}
	rows, err := tx.QueryContext(ctx, supportReplacementSelectColumns+`
		WHERE tenant_id = $1
		  AND ($2 = '' OR phase = $2)
		ORDER BY created_at DESC
		LIMIT 200
	`, tenantID, phaseFilter)
	if err != nil {
		return nil, fmt.Errorf("providercontrol: list support replacements: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]SupportReplacementRecord, 0)
	for rows.Next() {
		rec, err := scanSupportReplacementRow(rows)
		if err != nil {
			return nil, fmt.Errorf("providercontrol: scan support replacement: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (a *NativeAdmission) InspectSupportReplacementCapacity(
	ctx context.Context,
	tenantID, replacementID string,
) (SupportReplacementCapacitySnapshot, error) {
	ctx = SystemAuthorityContext(ctx)
	if !a.configured() {
		return SupportReplacementCapacitySnapshot{}, fmt.Errorf("%w: native admission is not configured", ErrInvalidRequest)
	}
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return SupportReplacementCapacitySnapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, tenantContextKey, tenantID); err != nil {
		return SupportReplacementCapacitySnapshot{}, err
	}
	rec, err := loadSupportReplacementByIDTx(ctx, tx, tenantID, replacementID)
	if err != nil {
		return SupportReplacementCapacitySnapshot{}, err
	}
	held, err := countHeldManagedRuntimeCapacity(ctx, tx, tenantID, rec.OwnerSubjectID)
	if err != nil {
		return SupportReplacementCapacitySnapshot{}, err
	}
	var snap SupportReplacementCapacitySnapshot
	snap.HeldCapacity = held
	err = tx.QueryRowContext(ctx, `
		SELECT
			count(g.resource_generation_id) FILTER (WHERE rel.lease_id IS NULL),
			coalesce(max(g.generation_ordinal) FILTER (WHERE rel.lease_id IS NULL), 0),
			EXISTS (
				SELECT 1 FROM managed_runtime_capacity_release_facts srel
				WHERE srel.tenant_id = $1 AND srel.lease_id = $3 AND srel.resource_generation_id::text = $4
			),
			CASE WHEN $5 = '' OR $6 = '' THEN false ELSE EXISTS (
				SELECT 1 FROM managed_runtime_capacity_release_facts drel
				WHERE drel.tenant_id = $1 AND drel.lease_id = $5 AND drel.resource_generation_id::text = $6
			) END
		FROM managed_runtime_server_slot_generations AS g
		LEFT JOIN managed_runtime_capacity_release_facts AS rel
		  ON rel.tenant_id = g.tenant_id AND rel.lease_id = g.lease_id AND rel.resource_generation_id = g.resource_generation_id
		WHERE g.tenant_id = $1 AND g.slot_id = $2
	`, tenantID, rec.RuntimeSlotID, rec.SourceLeaseID, rec.SourceResourceGenerationID,
		rec.DestinationLeaseID, rec.DestinationResourceGeneration).Scan(
		&snap.UnreleasedSlots, &snap.ActiveOrdinal, &snap.SourceReleased, &snap.DestinationReleased,
	)
	if err != nil {
		return SupportReplacementCapacitySnapshot{}, err
	}
	return snap, nil
}

type eligibleSupportReplacementSource struct {
	OwnerSubjectID    string
	StackID           string
	RuntimeSlotID     string
	LeaseID           string
	ServerID          string
	GenerationID      string
	GenerationOrdinal int64
	ProductSlug       string
	ExecutionProfile  string
}

func loadEligibleSupportReplacementSourceTx(
	ctx context.Context,
	tx *sql.Tx,
	norm AuthorizeSupportReplacementRequest,
) (eligibleSupportReplacementSource, error) {
	var src eligibleSupportReplacementSource
	var providerID, runtimeOffering, sku, execOffering, poAdapterID, srcProfileID, poOfferingID string
	var serverLifecycle, poStatus, poPhase string
	var serverDecommissioned, released bool
	err := tx.QueryRowContext(ctx, `
		SELECT
			l.owner_subject_id,
			COALESCE(NULLIF(BTRIM(s.stack_id), ''), COALESCE(l.lease_json->'metadata'->>'stack_id', '')),
			g.slot_id,
			l.id,
			l.server_id,
			l.resource_generation_id::text,
			g.generation_ordinal,
			LOWER(COALESCE(NULLIF(BTRIM(l.provider_id), ''), l.lease_json->'resource'->>'provider_id', '')),
			COALESCE(l.lease_json->'metadata'->>'runtime_offering_id', ''),
			COALESCE(l.lease_json->'metadata'->>'provider_package', ''),
			COALESCE(l.lease_json->'metadata'->>'provider_execution_offering_id', ''),
			COALESCE(po.adapter_id, ''),
			COALESCE(po.command_json #>> '{execution_profile,runtime_profile_id}', ''),
			COALESCE(po.command_json #>> '{execution_profile,offering_id}', ''),
			COALESCE(s.lifecycle_state, ''),
			s.decommissioned_at IS NOT NULL,
			COALESCE(po.status, ''),
			COALESCE(po.phase, ''),
			EXISTS (
				SELECT 1
				FROM managed_runtime_capacity_release_facts rel
				WHERE rel.tenant_id = l.tenant_id
				  AND rel.lease_id = l.id
				  AND rel.resource_generation_id = l.resource_generation_id
			)
		FROM techstack_vm_leases l
		JOIN managed_runtime_server_slot_generations g
		  ON g.tenant_id = l.tenant_id
		 AND g.lease_id = l.id
		 AND g.resource_generation_id = l.resource_generation_id
		JOIN provider_operations po
		  ON po.tenant_id = g.tenant_id
		 AND po.operation_id = g.provision_operation_id
		JOIN servers s
		  ON s.tenant_id = l.tenant_id
		 AND s.id = l.server_id
		WHERE l.tenant_id = $1
		  AND ($2 = '' OR l.id = $2)
		  AND ($3 = '' OR s.stack_id = $3 OR COALESCE(l.lease_json->'metadata'->>'stack_id', '') = $3)
		  AND ($4 = '' OR l.owner_subject_id = $4)
		  AND l.cancelled_at IS NULL
		  AND l.desired_state <> 'absent'
		ORDER BY g.generation_ordinal DESC
		LIMIT 1
	`, norm.TenantID, norm.SourceLeaseID, norm.StackID, norm.OwnerSubjectID).Scan(
		&src.OwnerSubjectID, &src.StackID, &src.RuntimeSlotID,
		&src.LeaseID, &src.ServerID, &src.GenerationID, &src.GenerationOrdinal,
		&providerID, &runtimeOffering, &sku, &execOffering, &poAdapterID, &srcProfileID, &poOfferingID,
		&serverLifecycle, &serverDecommissioned, &poStatus, &poPhase, &released,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return eligibleSupportReplacementSource{}, fmt.Errorf("%w: no eligible unreleased IONOS Basic Cube S source runtime found", ErrNativeAdmissionConflict)
	}
	if err != nil {
		return eligibleSupportReplacementSource{}, fmt.Errorf("providercontrol: load support replacement source: %w", err)
	}
	if released {
		return eligibleSupportReplacementSource{}, fmt.Errorf("%w: source runtime is already released", ErrNativeAdmissionConflict)
	}
	if serverLifecycle != string(serverregistry.LifecycleActive) || serverDecommissioned {
		return eligibleSupportReplacementSource{}, fmt.Errorf("%w: source server %q must be active and not decommissioned (got %q)", ErrNativeAdmissionConflict, src.ServerID, serverLifecycle)
	}
	if poStatus != "succeeded" || poPhase != "present" {
		return eligibleSupportReplacementSource{}, fmt.Errorf("%w: source provision operation must be succeeded/present (got %s/%s)", ErrNativeAdmissionConflict, poStatus, poPhase)
	}
	if providerID != SupportReplacementTargetProvider {
		return eligibleSupportReplacementSource{}, fmt.Errorf("%w: support replacement only supports IONOS source runtimes (got %q)", ErrInvalidRequest, providerID)
	}
	if execOffering == SupportReplacementTargetProductSlug || sku == SupportReplacementTargetVariantName || poAdapterID == SupportReplacementTargetProviderProfile {
		return eligibleSupportReplacementSource{}, fmt.Errorf("%w: source runtime is already on IONOS Memory Cube S", ErrNativeAdmissionConflict)
	}
	if runtimeOffering != string(serverruntime.RuntimeOfferingStandard) {
		return eligibleSupportReplacementSource{}, fmt.Errorf("%w: support replacement only supports monthly-runtime-standard Basic Cube S (got offering %q)", ErrNativeAdmissionConflict, runtimeOffering)
	}
	if execOffering != "" || sku != "Basic Cube S" {
		return eligibleSupportReplacementSource{}, fmt.Errorf("%w: support replacement only supports IONOS Basic Cube S (got package %q, execution offering %q)", ErrNativeAdmissionConflict, sku, execOffering)
	}
	if poAdapterID != "ionos-cloudapi-v6" || srcProfileID != "ionos-basic-cube-standard" ||
		(poOfferingID != "" && poOfferingID != string(serverruntime.RuntimeOfferingStandard)) {
		return eligibleSupportReplacementSource{}, fmt.Errorf("%w: source runtime execution profile %q (%q, %q) is not Basic Cube S", ErrNativeAdmissionConflict, poAdapterID, srcProfileID, poOfferingID)
	}
	src.ProductSlug = "ionos-cube-standard-basic"
	src.ExecutionProfile = "ionos-basic-cube-standard"
	return src, nil
}

func validateSupportReplacementReadyForProvisionTx(ctx context.Context, tx *sql.Tx, sr SupportReplacementRecord) error {
	if !sr.NoChargeConfirmed || sr.CustomerAcceptedAt == nil || strings.TrimSpace(sr.CustomerAcceptedBy) == "" {
		return fmt.Errorf("%w: support replacement requires customer acceptance and confirmed zero charge", ErrNativeAdmissionConflict)
	}
	if sr.TargetProvider != SupportReplacementTargetProvider ||
		sr.TargetPackageID != SupportReplacementTargetPackageID ||
		sr.TargetVariantName != SupportReplacementTargetVariantName ||
		sr.TargetProductSlug != SupportReplacementTargetProductSlug ||
		sr.TargetExecutionProfile != SupportReplacementTargetExecutionProfile ||
		sr.TargetProviderProfile != SupportReplacementTargetProviderProfile {
		return fmt.Errorf("%w: support replacement target mismatch", ErrNativeAdmissionConflict)
	}
	var sourceReleased bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM managed_runtime_capacity_release_facts
			WHERE tenant_id = $1
			  AND lease_id = $2
			  AND resource_generation_id::text = $3
		)
	`, sr.TenantID, sr.SourceLeaseID, sr.SourceResourceGenerationID).Scan(&sourceReleased); err != nil {
		return fmt.Errorf("providercontrol: verify source capacity unreleased: %w", err)
	}
	if sourceReleased && sr.Phase == SupportReplacementPhaseAccepted {
		return fmt.Errorf("%w: source cube already released before replacement admission", ErrNativeAdmissionConflict)
	}
	return nil
}

type supportReplacementSourceMetadata struct {
	StackName string
	Region    string
	StackKit  string
	NodeRole  string
	Services  []string
}

func resolveSupportReplacementProvisionInputsTx(
	ctx context.Context,
	tx *sql.Tx,
	req SupportReplacementProvisionRequest,
	sr SupportReplacementRecord,
) (string, int64, supportReplacementSourceMetadata, error) {
	var slotKey string
	if err := tx.QueryRowContext(ctx, `
		SELECT slot_key
		FROM managed_runtime_server_slots
		WHERE tenant_id = $1 AND slot_id = $2
	`, sr.TenantID, sr.RuntimeSlotID).Scan(&slotKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", 0, supportReplacementSourceMetadata{}, fmt.Errorf("%w: managed runtime slot %q not found", ErrNativeAdmissionConflict, sr.RuntimeSlotID)
		}
		return "", 0, supportReplacementSourceMetadata{}, fmt.Errorf("providercontrol: load support replacement slot key: %w", err)
	}
	slotKey = strings.ToLower(strings.TrimSpace(slotKey))
	if reqKey := strings.ToLower(strings.TrimSpace(req.RuntimeSlotKey)); reqKey != "" && reqKey != slotKey {
		return "", 0, supportReplacementSourceMetadata{}, fmt.Errorf("%w: requested runtime_slot_key %q does not match slot %q", ErrNativeAdmissionConflict, reqKey, slotKey)
	}

	var destOrdinal int64
	if sr.DestinationGenerationOrdinal != nil && *sr.DestinationGenerationOrdinal > 0 {
		destOrdinal = *sr.DestinationGenerationOrdinal
	} else {
		var maxOrdinal int64
		if err := tx.QueryRowContext(ctx, `
			SELECT coalesce(max(generation_ordinal), 0)
			FROM managed_runtime_server_slot_generations
			WHERE tenant_id = $1 AND slot_id = $2
		`, sr.TenantID, sr.RuntimeSlotID).Scan(&maxOrdinal); err != nil {
			return "", 0, supportReplacementSourceMetadata{}, fmt.Errorf("providercontrol: compute destination slot ordinal: %w", err)
		}
		destOrdinal = maxOrdinal + 1
	}
	if destOrdinal <= sr.SourceGenerationOrdinal {
		return "", 0, supportReplacementSourceMetadata{}, fmt.Errorf("%w: destination ordinal %d must exceed source ordinal %d", ErrNativeAdmissionConflict, destOrdinal, sr.SourceGenerationOrdinal)
	}

	var meta supportReplacementSourceMetadata
	var servicesCSV string
	if err := tx.QueryRowContext(ctx, `
		SELECT
			COALESCE(NULLIF(BTRIM(l.lease_json->'metadata'->>'stack_name'), ''), NULLIF(BTRIM(s.name), ''), $3),
			COALESCE(NULLIF(BTRIM(l.lease_json->'metadata'->>'provider_region'), ''), NULLIF(BTRIM(l.lease_json->'resource'->>'region'), ''), 'de/fra/2'),
			COALESCE(NULLIF(BTRIM(l.lease_json->'metadata'->>'stackkit'), ''), 'cloud-kit'),
			COALESCE(NULLIF(BTRIM(l.lease_json->'metadata'->>'node_role'), ''), 'foundation'),
			COALESCE(NULLIF(BTRIM(l.lease_json->'metadata'->>'requested_services'), ''), '')
		FROM techstack_vm_leases AS l
		LEFT JOIN servers AS s
		  ON s.tenant_id = l.tenant_id
		 AND s.id = $4
		WHERE l.tenant_id = $1 AND l.id = $2
	`, sr.TenantID, sr.SourceLeaseID, sr.StackID, sr.SourceServerID).Scan(
		&meta.StackName, &meta.Region, &meta.StackKit, &meta.NodeRole, &servicesCSV,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", 0, supportReplacementSourceMetadata{}, fmt.Errorf("%w: source lease %q not found", ErrNativeAdmissionConflict, sr.SourceLeaseID)
		}
		return "", 0, supportReplacementSourceMetadata{}, fmt.Errorf("providercontrol: load source lease metadata: %w", err)
	}
	for _, item := range strings.Split(servicesCSV, ",") {
		if item = strings.TrimSpace(item); item != "" {
			meta.Services = append(meta.Services, item)
		}
	}
	if meta.Services == nil {
		meta.Services = []string{}
	}
	return slotKey, destOrdinal, meta, nil
}

func supportReplacementStableID(prefix string, parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = hash.Write([]byte(part))
		_, _ = hash.Write([]byte{0})
	}
	return prefix + "-" + hex.EncodeToString(hash.Sum(nil)[:16])
}

func buildSupportReplacementNativeRequest(
	req SupportReplacementProvisionRequest,
	sr SupportReplacementRecord,
	slotKey string,
	destOrdinal int64,
	sourceMeta supportReplacementSourceMetadata,
) (NativeProvisionAdmissionRequest, error) {
	ordinalText := strconv.FormatInt(destOrdinal, 10)
	leaseID := vmlease.LeaseID(supportReplacementStableID("lease", sr.TenantID, sr.StackID, slotKey, ordinalText))
	serverID := strings.TrimSpace(req.RuntimeServerID)
	if serverID == "" {
		serverID = runtimeidentity.LeaseServerID(string(leaseID))
	}
	idempotencyKey := strings.TrimSpace(req.IdempotencyKey)
	if idempotencyKey == "" {
		idempotencyKey = "support-replacement-provision:" + sr.ReplacementID
	}
	region := strings.TrimSpace(req.Region)
	if region == "" {
		region = sourceMeta.Region
	}
	if region == "" {
		region = "de/fra/2"
	}
	stackName := strings.TrimSpace(sourceMeta.StackName)
	if stackName == "" {
		stackName = sr.StackID
	}
	stackkit := strings.TrimSpace(sourceMeta.StackKit)
	if stackkit == "" {
		stackkit = "cloud-kit"
	}
	nodeRole := strings.TrimSpace(sourceMeta.NodeRole)
	if nodeRole == "" {
		nodeRole = "foundation"
	}
	serverName := strings.TrimSpace(req.ServerName)
	if serverName == "" {
		serverName = "memory-cube-s-" + sr.StackID + "-" + ordinalText
	}
	instanceID := strings.TrimSpace(req.InstanceID)
	desiredSpecRef := strings.TrimSpace(req.DesiredSpecRef)
	if desiredSpecRef == "" {
		desiredSpecRef = "desired-spec://homelab/leases/" + string(leaseID) + "/revisions/1"
	}
	desiredSpec := req.DesiredSpec
	if len(desiredSpec) == 0 {
		services := sourceMeta.Services
		if services == nil {
			services = []string{}
		}
		payload, err := json.Marshal(struct {
			SchemaVersion               string   `json:"schema_version"`
			ProviderID                  string   `json:"provider_id"`
			OfferingID                  string   `json:"offering_id"`
			ProviderPackage             string   `json:"provider_package"`
			ProviderExecutionOfferingID string   `json:"provider_execution_offering_id"`
			Region                      string   `json:"region"`
			StackID                     string   `json:"stack_id"`
			StackName                   string   `json:"stack_name"`
			StackKit                    string   `json:"stackkit"`
			NodeRole                    string   `json:"node_role"`
			RuntimeSlotKey              string   `json:"runtime_slot_key"`
			RuntimeSlotGeneration       uint64   `json:"runtime_slot_generation"`
			Services                    []string `json:"services"`
			SupportReplacementID        string   `json:"support_replacement_id"`
			NoChargeConfirmed           bool     `json:"no_charge_confirmed"`
		}{
			SchemaVersion:               "homelab/provider-desired-spec/v2",
			ProviderID:                  SupportReplacementTargetProvider,
			OfferingID:                  SupportReplacementTargetPackageID,
			ProviderPackage:             SupportReplacementTargetVariantName,
			ProviderExecutionOfferingID: SupportReplacementTargetProductSlug,
			Region:                      region,
			StackID:                     sr.StackID,
			StackName:                   stackName,
			StackKit:                    stackkit,
			NodeRole:                    nodeRole,
			RuntimeSlotKey:              slotKey,
			RuntimeSlotGeneration:       uint64(destOrdinal),
			Services:                    services,
			SupportReplacementID:        sr.ReplacementID,
			NoChargeConfirmed:           true,
		})
		if err != nil {
			return NativeProvisionAdmissionRequest{}, fmt.Errorf("providercontrol: encode support replacement desired spec: %w", err)
		}
		desiredSpec = payload
	}
	leaseMetadata := map[string]string{
		runtimeOfferingMetadataKey:                            string(serverruntime.RuntimeOfferingStandard),
		monthlyruntime.MetadataKeyProviderPackage:             SupportReplacementTargetVariantName,
		monthlyruntime.MetadataKeyProviderExecutionOfferingID: SupportReplacementTargetProductSlug,
		"stack_id":                            sr.StackID,
		"stack_name":                          stackName,
		"stackkit":                            stackkit,
		"node_role":                           nodeRole,
		"server_node_role":                    nodeRole,
		"requested_services":                  strings.Join(sourceMeta.Services, ","),
		"runtime_slot_key":                    slotKey,
		"runtime_slot_id":                     sr.RuntimeSlotID,
		"runtime_slot_generation":             ordinalText,
		"provider_region":                     region,
		monthlyruntime.MetadataKeyRuntimeLane: serverruntime.RuntimeLaneMonthly,
		monthlyruntime.MetadataKeyServerMode:  serverruntime.RuntimeLaneMonthly,
		monthlyruntime.MetadataKeyBillingMode: monthlyruntime.BillingSubscription,
		"support_replacement":                 sr.ReplacementID,
		"no_charge_confirmed":                 "true",
	}
	lease := vmlease.Lease{
		ID: leaseID,
		Subject: vmlease.Subject{
			Kind:  vmlease.SubjectUser,
			ID:    sr.OwnerSubjectID,
			OrgID: sr.TenantID,
		},
		Resource: vmlease.ResourceRef{
			ProviderID: SupportReplacementTargetProvider,
			Region:     region,
		},
		DesiredState:   vmlease.DesiredStateRunning,
		BillingMode:    vmlease.BillingModeSubscription,
		LifecycleClass: vmlease.LifecycleClassSubscription,
		RestartPolicy:  vmlease.RestartPolicyOnUnexpectedStop,
		RecreatePolicy: vmlease.RecreatePolicyManual,
		Metadata:       leaseMetadata,
	}
	return NativeProvisionAdmissionRequest{
		TenantID:              sr.TenantID,
		RuntimeSlotKey:        slotKey,
		RuntimeSlotID:         sr.RuntimeSlotID,
		RuntimeSlotGeneration: uint64(destOrdinal),
		RuntimeServerID:       serverID,
		OwnerSubjectID:        sr.OwnerSubjectID,
		IdempotencyKey:        idempotencyKey,
		ValidFor:              req.ValidFor,
		Lease:                 lease,
		Server: NativeAdmissionServer{
			InstanceID: instanceID,
			StackID:    sr.StackID,
			Name:       serverName,
			Metadata: map[string]any{
				"provider_id":             SupportReplacementTargetProvider,
				"offering_id":             SupportReplacementTargetPackageID,
				"stackkit":                stackkit,
				"node_role":               nodeRole,
				"runtime_slot_key":        slotKey,
				"runtime_slot_id":         sr.RuntimeSlotID,
				"runtime_slot_generation": uint64(destOrdinal),
				"support_replacement_id":  sr.ReplacementID,
				"target_product_slug":     SupportReplacementTargetProductSlug,
				"target_variant_name":     SupportReplacementTargetVariantName,
				"target_provider_profile": SupportReplacementTargetProviderProfile,
				"no_charge_confirmed":     true,
			},
		},
		DesiredSpecRef:       desiredSpecRef,
		DesiredSpec:          desiredSpec,
		SupportReplacementID: sr.ReplacementID,
	}, nil
}

func normalizeSupportReplacementProvision(
	request NativeProvisionAdmissionRequest,
) (NativeProvisionAdmissionRequest, [sha256.Size]byte, string, error) {
	request = normalizeNativeProvisionFields(request)
	if err := validateNativeAdmissionIdentity(request); err != nil {
		return NativeProvisionAdmissionRequest{}, [sha256.Size]byte{}, "", err
	}
	if err := validateNativeAdmissionLease(request); err != nil {
		return NativeProvisionAdmissionRequest{}, [sha256.Size]byte{}, "", err
	}
	canonicalDesired, err := canonicalJSON(request.DesiredSpec)
	if err != nil {
		return NativeProvisionAdmissionRequest{}, [sha256.Size]byte{}, "", fmt.Errorf("%w: desired spec payload must be valid JSON: %v", ErrInvalidRequest, err)
	}
	request.DesiredSpec = canonicalDesired
	serverMetadata, err := canonicalAdmissionMetadata(request.Server.Metadata)
	if err != nil {
		return NativeProvisionAdmissionRequest{}, [sha256.Size]byte{}, "", err
	}
	request.Server.Metadata = serverMetadata
	digestPayload, err := marshalNativeAdmissionDigest(request)
	if err != nil {
		return NativeProvisionAdmissionRequest{}, [sha256.Size]byte{}, "", err
	}
	digest := sha256.Sum256(digestPayload)
	return request, digest, "sha256:" + hex.EncodeToString(digest[:]), nil
}

func normalizeSupportReplacementAuthorization(
	req AuthorizeSupportReplacementRequest,
) (AuthorizeSupportReplacementRequest, string, error) {
	norm := AuthorizeSupportReplacementRequest{
		TenantID:               strings.TrimSpace(req.TenantID),
		ReplacementID:          strings.TrimSpace(req.ReplacementID),
		OwnerSubjectID:         strings.TrimSpace(req.OwnerSubjectID),
		StackID:                strings.TrimSpace(req.StackID),
		SourceLeaseID:          strings.TrimSpace(req.SourceLeaseID),
		CaseRef:                strings.TrimSpace(req.CaseRef),
		Reason:                 strings.TrimSpace(req.Reason),
		AuthorizedBy:           strings.TrimSpace(req.AuthorizedBy),
		EvidenceRef:            strings.TrimSpace(req.EvidenceRef),
		IdempotencyKey:         strings.TrimSpace(req.IdempotencyKey),
		NoChargeConfirmed:      req.NoChargeConfirmed,
		TargetProvider:         strings.TrimSpace(req.TargetProvider),
		TargetPackageID:        strings.TrimSpace(req.TargetPackageID),
		TargetVariantName:      strings.TrimSpace(req.TargetVariantName),
		TargetProductSlug:      strings.TrimSpace(req.TargetProductSlug),
		TargetExecutionProfile: strings.TrimSpace(req.TargetExecutionProfile),
		TargetProviderProfile:  strings.TrimSpace(req.TargetProviderProfile),
	}
	if norm.TenantID == "" || (norm.SourceLeaseID == "" && norm.StackID == "") ||
		norm.CaseRef == "" || norm.Reason == "" || norm.AuthorizedBy == "" || norm.EvidenceRef == "" || norm.IdempotencyKey == "" {
		return AuthorizeSupportReplacementRequest{}, "", fmt.Errorf("%w: tenant_id, source_lease_id or stack_id, case_ref, reason, authorized_by, evidence_ref, and idempotency_key are required", ErrInvalidRequest)
	}
	if !norm.NoChargeConfirmed {
		return AuthorizeSupportReplacementRequest{}, "", fmt.Errorf("%w: no_charge_confirmed must be true for free Memory Cube S support replacement", ErrInvalidRequest)
	}
	if (norm.TargetProvider != "" && norm.TargetProvider != SupportReplacementTargetProvider) ||
		(norm.TargetPackageID != "" && norm.TargetPackageID != SupportReplacementTargetPackageID) ||
		(norm.TargetVariantName != "" && norm.TargetVariantName != SupportReplacementTargetVariantName) ||
		(norm.TargetProductSlug != "" && norm.TargetProductSlug != SupportReplacementTargetProductSlug) ||
		(norm.TargetExecutionProfile != "" && norm.TargetExecutionProfile != SupportReplacementTargetExecutionProfile) ||
		(norm.TargetProviderProfile != "" && norm.TargetProviderProfile != SupportReplacementTargetProviderProfile) {
		return AuthorizeSupportReplacementRequest{}, "", fmt.Errorf("%w: support replacement target is strictly pinned to IONOS Memory Cube S", ErrInvalidRequest)
	}
	payload, _ := json.Marshal(struct {
		TenantID               string `json:"tenant_id"`
		ReplacementID          string `json:"replacement_id"`
		OwnerSubjectID         string `json:"owner_subject_id"`
		StackID                string `json:"stack_id"`
		SourceLeaseID          string `json:"source_lease_id"`
		CaseRef                string `json:"case_ref"`
		Reason                 string `json:"reason"`
		AuthorizedBy           string `json:"authorized_by"`
		EvidenceRef            string `json:"evidence_ref"`
		TargetProductSlug      string `json:"target_product_slug"`
		TargetExecutionProfile string `json:"target_execution_profile"`
		TargetProviderProfile  string `json:"target_provider_profile"`
		NoChargeConfirmed      bool   `json:"no_charge_confirmed"`
	}{
		TenantID:               norm.TenantID,
		ReplacementID:          norm.ReplacementID,
		OwnerSubjectID:         norm.OwnerSubjectID,
		StackID:                norm.StackID,
		SourceLeaseID:          norm.SourceLeaseID,
		CaseRef:                norm.CaseRef,
		Reason:                 norm.Reason,
		AuthorizedBy:           norm.AuthorizedBy,
		EvidenceRef:            norm.EvidenceRef,
		TargetProductSlug:      SupportReplacementTargetProductSlug,
		TargetExecutionProfile: SupportReplacementTargetExecutionProfile,
		TargetProviderProfile:  SupportReplacementTargetProviderProfile,
		NoChargeConfirmed:      true,
	})
	sum := sha256.Sum256(payload)
	return norm, "sha256:" + hex.EncodeToString(sum[:]), nil
}

const supportReplacementSelectColumns = `
	SELECT
		tenant_id, replacement_id, owner_subject_id, stack_id, runtime_slot_id,
		source_lease_id, source_server_id, source_resource_generation_id, source_generation_ordinal,
		source_provider, source_package_id, source_product_slug, source_execution_profile,
		target_provider, target_package_id, target_variant_name, target_product_slug,
		target_execution_profile, target_provider_profile, no_charge_confirmed,
		case_ref, reason, authorized_by, authorized_at, evidence_ref,
		customer_accepted_at, COALESCE(customer_accepted_by, ''),
		idempotency_key, request_digest,
		COALESCE(destination_lease_id, ''), COALESCE(destination_server_id, ''),
		COALESCE(destination_resource_generation_id, ''), destination_generation_ordinal,
		COALESCE(destination_operation_id, ''), destination_writes_observed,
		cutover_verified_at, COALESCE(source_teardown_operation_id, ''),
		COALESCE(rollback_reason, ''), COALESCE(fenced_conflict_reason, ''),
		COALESCE(checkpoint_json, '{}'::jsonb),
		phase, settled_at, created_at, updated_at
	FROM managed_runtime_support_replacements
`

type supportReplacementScanner interface {
	Scan(dest ...any) error
}

func scanSupportReplacementRow(row supportReplacementScanner) (SupportReplacementRecord, error) {
	var rec SupportReplacementRecord
	var acceptedAt, cutoverAt, settledAt sql.NullTime
	var destOrdinal sql.NullInt64
	var checkpointRaw []byte
	err := row.Scan(
		&rec.TenantID, &rec.ReplacementID, &rec.OwnerSubjectID, &rec.StackID, &rec.RuntimeSlotID,
		&rec.SourceLeaseID, &rec.SourceServerID, &rec.SourceResourceGenerationID, &rec.SourceGenerationOrdinal,
		&rec.SourceProvider, &rec.SourcePackageID, &rec.SourceProductSlug, &rec.SourceExecutionProfile,
		&rec.TargetProvider, &rec.TargetPackageID, &rec.TargetVariantName, &rec.TargetProductSlug,
		&rec.TargetExecutionProfile, &rec.TargetProviderProfile, &rec.NoChargeConfirmed,
		&rec.CaseRef, &rec.Reason, &rec.AuthorizedBy, &rec.AuthorizedAt, &rec.EvidenceRef,
		&acceptedAt, &rec.CustomerAcceptedBy,
		&rec.IdempotencyKey, &rec.RequestDigest,
		&rec.DestinationLeaseID, &rec.DestinationServerID,
		&rec.DestinationResourceGeneration, &destOrdinal,
		&rec.DestinationOperationID, &rec.DestinationWritesObserved,
		&cutoverAt, &rec.SourceTeardownOperationID,
		&rec.RollbackReason, &rec.FencedConflictReason,
		&checkpointRaw,
		&rec.Phase, &settledAt, &rec.CreatedAt, &rec.UpdatedAt,
	)
	if err != nil {
		return SupportReplacementRecord{}, err
	}
	if len(checkpointRaw) > 0 {
		rec.CheckpointJSON = json.RawMessage(checkpointRaw)
	}
	if acceptedAt.Valid {
		t := acceptedAt.Time
		rec.CustomerAcceptedAt = &t
	}
	if cutoverAt.Valid {
		t := cutoverAt.Time
		rec.CutoverVerifiedAt = &t
	}
	if settledAt.Valid {
		t := settledAt.Time
		rec.SettledAt = &t
	}
	if destOrdinal.Valid {
		v := destOrdinal.Int64
		rec.DestinationGenerationOrdinal = &v
	}
	return rec, nil
}

func loadSupportReplacementByIDTx(ctx context.Context, tx *sql.Tx, tenantID, replacementID string) (SupportReplacementRecord, error) {
	rec, err := scanSupportReplacementRow(tx.QueryRowContext(ctx, supportReplacementSelectColumns+`
		WHERE tenant_id = $1 AND replacement_id = $2
	`, tenantID, replacementID))
	if errors.Is(err, sql.ErrNoRows) {
		return SupportReplacementRecord{}, fmt.Errorf("%w: support replacement %q not found", ErrOperationNotFound, replacementID)
	}
	if err != nil {
		return SupportReplacementRecord{}, fmt.Errorf("providercontrol: load support replacement: %w", err)
	}
	return rec, nil
}

func lockSupportReplacementByIDTx(ctx context.Context, tx *sql.Tx, tenantID, replacementID string) (SupportReplacementRecord, error) {
	rec, err := scanSupportReplacementRow(tx.QueryRowContext(ctx, supportReplacementSelectColumns+`
		WHERE tenant_id = $1 AND replacement_id = $2
		FOR UPDATE
	`, tenantID, replacementID))
	if errors.Is(err, sql.ErrNoRows) {
		return SupportReplacementRecord{}, fmt.Errorf("%w: support replacement %q not found", ErrOperationNotFound, replacementID)
	}
	if err != nil {
		return SupportReplacementRecord{}, fmt.Errorf("providercontrol: lock support replacement: %w", err)
	}
	return rec, nil
}

func loadSupportReplacementByIdempotencyTx(ctx context.Context, tx *sql.Tx, tenantID, idempotencyKey string) (SupportReplacementRecord, bool, error) {
	rec, err := scanSupportReplacementRow(tx.QueryRowContext(ctx, supportReplacementSelectColumns+`
		WHERE tenant_id = $1 AND idempotency_key = $2
	`, tenantID, idempotencyKey))
	if errors.Is(err, sql.ErrNoRows) {
		return SupportReplacementRecord{}, false, nil
	}
	if err != nil {
		return SupportReplacementRecord{}, false, fmt.Errorf("providercontrol: load support replacement by idempotency: %w", err)
	}
	return rec, true, nil
}

func mapSupportReplacementWriteErr(action string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "23505" || pgErr.Code == "P0001" || pgErr.Code == "55000") {
		return fmt.Errorf("%w: %s: %v", ErrNativeAdmissionConflict, action, err)
	}
	if strings.Contains(err.Error(), "transition") || strings.Contains(err.Error(), "immutable") {
		return fmt.Errorf("%w: %s: %v", ErrNativeAdmissionConflict, action, err)
	}
	return fmt.Errorf("providercontrol: %s: %w", action, err)
}

// CheckSupportReplacementLeaseFence enforces P1-7 runtime fences against
// deletion, decommission, or writer restart on either lease held by an active
// or fenced_conflict support replacement.
func (a *NativeAdmission) CheckSupportReplacementLeaseFence(ctx context.Context, tenantID, leaseID string, action serverruntime.RuntimeAction) error {
	ctx = SystemAuthorityContext(ctx)
	if !a.configured() {
		return nil
	}
	tenantID = strings.TrimSpace(tenantID)
	leaseID = strings.TrimSpace(leaseID)
	if tenantID == "" || leaseID == "" {
		return nil
	}
	if action != serverruntime.RuntimeActionDecommission && action != serverruntime.RuntimeActionStart {
		return nil
	}
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("providercontrol: check support replacement lease fence: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, tenantContextKey, tenantID); err != nil {
		return fmt.Errorf("providercontrol: set tenant context for lease fence check: %w", err)
	}
	var replacementID, phase string
	err = tx.QueryRowContext(ctx, `
		SELECT replacement_id, phase
		FROM managed_runtime_support_replacements
		WHERE tenant_id = $1
		  AND (source_lease_id = $2 OR destination_lease_id = $2)
		  AND phase IN (
		      'destination_admitted',
		      'staging',
		      'cutover_in_progress',
		      'cutover_verified',
		      'source_decommissioning',
		      'rolling_back',
		      'fenced_conflict'
		  )
		ORDER BY updated_at DESC
		LIMIT 1
	`, tenantID, leaseID).Scan(&replacementID, &phase)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("providercontrol: inspect support replacement lease fence: %w", err)
	}
	return fmt.Errorf("%w: lease %q is fenced by support replacement %q in phase %q", ErrNativeAdmissionConflict, leaseID, replacementID, phase)
}

// EnsureNoActiveSupportReplacementForStack fails closed if the stack has any
// active or fenced_conflict support replacement that forbids stack destroy.
func (a *NativeAdmission) EnsureNoActiveSupportReplacementForStack(ctx context.Context, tenantID, stackID string) error {
	ctx = SystemAuthorityContext(ctx)
	if !a.configured() {
		return nil
	}
	tenantID = strings.TrimSpace(tenantID)
	stackID = strings.TrimSpace(stackID)
	if tenantID == "" || stackID == "" {
		return nil
	}
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("providercontrol: check support replacement stack fence: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, tenantContextKey, tenantID); err != nil {
		return fmt.Errorf("providercontrol: set tenant context for stack fence check: %w", err)
	}
	var replacementID, phase string
	err = tx.QueryRowContext(ctx, `
		SELECT replacement_id, phase
		FROM managed_runtime_support_replacements
		WHERE tenant_id = $1
		  AND stack_id = $2
		  AND phase IN (
		      'destination_admitted',
		      'staging',
		      'cutover_in_progress',
		      'cutover_verified',
		      'source_decommissioning',
		      'rolling_back',
		      'fenced_conflict'
		  )
		ORDER BY updated_at DESC
		LIMIT 1
	`, tenantID, stackID).Scan(&replacementID, &phase)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("providercontrol: inspect support replacement stack fence: %w", err)
	}
	return fmt.Errorf("%w: stack %q is fenced by support replacement %q in phase %q", ErrNativeAdmissionConflict, stackID, replacementID, phase)
}
