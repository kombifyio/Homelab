package providercontrol

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const managedRuntimeSlotStateActive = "active"

var managedRuntimeSlotKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

var ErrManagedRuntimeUnclassifiedCustody = errors.New("providercontrol: unclassified managed runtime custody")

// ManagedRuntimeSlotGenerationResolution is the server-side lifecycle epoch
// used to derive job, lease, server, and provider-operation identities. The
// current epoch is stable while custody is unresolved; only an exact
// append-only capacity release advances it.
type ManagedRuntimeSlotGenerationResolution struct {
	RuntimeSlotID      string
	GenerationOrdinal  uint64
	ExistingUnreleased bool
}

// ManagedRuntimeUnclassifiedCustodyError keeps new creates closed when an
// older lease/server in the same stack/provider has no stable slot binding.
// Operators must reconcile it; admission never guesses a slot from a name/IP.
type ManagedRuntimeUnclassifiedCustodyError struct {
	ProviderID string
	StackID    string
}

func (e ManagedRuntimeUnclassifiedCustodyError) Error() string {
	return fmt.Sprintf("providercontrol: provider %s stack %s has unclassified managed runtime custody",
		strings.TrimSpace(e.ProviderID), strings.TrimSpace(e.StackID))
}

func (ManagedRuntimeUnclassifiedCustodyError) Unwrap() error {
	return ErrManagedRuntimeUnclassifiedCustody
}

// DeriveManagedRuntimeSlotID is the single provider-neutral slot-identity
// authority. Provider belongs to an exact generation, never to the product
// slot, so two providers cannot occupy the same stack slot concurrently. The
// ID is tenant- and stack-scoped and contains no customer input in clear text.
func DeriveManagedRuntimeSlotID(tenantID, stackID, slotKey string) string {
	hash := sha256.New()
	for _, part := range []string{
		strings.TrimSpace(tenantID), strings.TrimSpace(stackID), strings.ToLower(strings.TrimSpace(slotKey)),
	} {
		_, _ = hash.Write([]byte(part))
		_, _ = hash.Write([]byte{0})
	}
	return "runtime-slot-" + hex.EncodeToString(hash.Sum(nil)[:16])
}

// ResolveManagedRuntimeSlotGeneration returns the one stable lifecycle epoch
// for a logical slot. It is read-only: concurrent callers may observe the same
// next ordinal, and transactional admission serializes which exact intent wins.
func (a *NativeAdmission) ResolveManagedRuntimeSlotGeneration(
	ctx context.Context,
	tenantID, stackID, slotKey string,
) (ManagedRuntimeSlotGenerationResolution, error) {
	ctx = SystemAuthorityContext(ctx)
	tenantID = strings.TrimSpace(tenantID)
	stackID = strings.TrimSpace(stackID)
	slotKey = strings.ToLower(strings.TrimSpace(slotKey))
	if a == nil || a.db == nil || tenantID == "" || stackID == "" ||
		!managedRuntimeSlotKeyPattern.MatchString(slotKey) {
		return ManagedRuntimeSlotGenerationResolution{}, fmt.Errorf("%w: canonical runtime slot identity is required", ErrInvalidRequest)
	}
	slotID := DeriveManagedRuntimeSlotID(tenantID, stackID, slotKey)
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ManagedRuntimeSlotGenerationResolution{}, fmt.Errorf("providercontrol: begin runtime slot generation resolution: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, tenantContextKey, tenantID); err != nil {
		return ManagedRuntimeSlotGenerationResolution{}, fmt.Errorf("providercontrol: scope runtime slot generation resolution: %w", err)
	}
	var maximum, unresolvedOrdinal int64
	var unresolvedCount int
	err = tx.QueryRowContext(ctx, `
		SELECT
			coalesce(max(g.generation_ordinal), 0),
			coalesce(max(g.generation_ordinal) FILTER (WHERE release.lease_id IS NULL), 0),
			count(g.resource_generation_id) FILTER (WHERE release.lease_id IS NULL)
		FROM managed_runtime_server_slots AS slot
		LEFT JOIN managed_runtime_server_slot_generations AS g
		  ON g.tenant_id = slot.tenant_id AND g.slot_id = slot.slot_id
		LEFT JOIN managed_runtime_capacity_release_facts AS release
		  ON release.tenant_id = g.tenant_id
		 AND release.lease_id = g.lease_id
		 AND release.resource_generation_id = g.resource_generation_id
		WHERE slot.tenant_id = $1 AND slot.stack_id = $2 AND slot.slot_key = $3
	`, tenantID, stackID, slotKey).Scan(&maximum, &unresolvedOrdinal, &unresolvedCount)
	if err != nil {
		return ManagedRuntimeSlotGenerationResolution{}, fmt.Errorf("providercontrol: resolve runtime slot generation: %w", err)
	}
	if unresolvedCount == 2 {
		var linkedOrdinal sql.NullInt64
		linkedErr := tx.QueryRowContext(ctx, `
			SELECT sr.destination_generation_ordinal
			FROM managed_runtime_support_replacements AS sr
			WHERE sr.tenant_id = $1
			  AND sr.runtime_slot_id = $2
			  AND sr.phase IN (
			      'destination_admitted',
			      'staging',
			      'cutover_in_progress',
			      'cutover_verified',
			      'source_decommissioning',
			      'rolling_back',
			      'fenced_conflict'
			  )
			  AND sr.destination_generation_ordinal IS NOT NULL
			  AND EXISTS (
			      SELECT 1
			      FROM managed_runtime_server_slot_generations AS sg_src
			      LEFT JOIN managed_runtime_capacity_release_facts AS rel_src
			        ON rel_src.tenant_id = sg_src.tenant_id
			       AND rel_src.lease_id = sg_src.lease_id
			       AND rel_src.resource_generation_id = sg_src.resource_generation_id
			      WHERE sg_src.tenant_id = sr.tenant_id
			        AND sg_src.slot_id = sr.runtime_slot_id
			        AND sg_src.lease_id = sr.source_lease_id
			        AND sg_src.resource_generation_id::text = sr.source_resource_generation_id
			        AND sg_src.generation_ordinal = sr.source_generation_ordinal
			        AND rel_src.lease_id IS NULL
			  )
			  AND EXISTS (
			      SELECT 1
			      FROM managed_runtime_server_slot_generations AS sg_dst
			      LEFT JOIN managed_runtime_capacity_release_facts AS rel_dst
			        ON rel_dst.tenant_id = sg_dst.tenant_id
			       AND rel_dst.lease_id = sg_dst.lease_id
			       AND rel_dst.resource_generation_id = sg_dst.resource_generation_id
			      WHERE sg_dst.tenant_id = sr.tenant_id
			        AND sg_dst.slot_id = sr.runtime_slot_id
			        AND sg_dst.lease_id = sr.destination_lease_id
			        AND sg_dst.resource_generation_id::text = sr.destination_resource_generation_id
			        AND sg_dst.generation_ordinal = sr.destination_generation_ordinal
			        AND rel_dst.lease_id IS NULL
			  )
			ORDER BY sr.authorized_at DESC
			LIMIT 1
		`, tenantID, slotID).Scan(&linkedOrdinal)
		if linkedErr == nil && linkedOrdinal.Valid && linkedOrdinal.Int64 > 0 {
			if err := tx.Commit(); err != nil {
				return ManagedRuntimeSlotGenerationResolution{}, fmt.Errorf("providercontrol: commit runtime slot generation resolution: %w", err)
			}
			return ManagedRuntimeSlotGenerationResolution{
				RuntimeSlotID: slotID, GenerationOrdinal: uint64(linkedOrdinal.Int64), ExistingUnreleased: true,
			}, nil
		}
	}
	if unresolvedCount > 1 || maximum < 0 || unresolvedOrdinal < 0 {
		return ManagedRuntimeSlotGenerationResolution{}, fmt.Errorf("%w: runtime slot has ambiguous generation custody", ErrNativeAdmissionConflict)
	}
	ordinal := maximum + 1
	existing := unresolvedCount == 1
	if existing {
		ordinal = unresolvedOrdinal
	}
	if ordinal <= 0 {
		return ManagedRuntimeSlotGenerationResolution{}, fmt.Errorf("%w: runtime slot generation overflow", ErrNativeAdmissionConflict)
	}
	if err := tx.Commit(); err != nil {
		return ManagedRuntimeSlotGenerationResolution{}, fmt.Errorf("providercontrol: commit runtime slot generation resolution: %w", err)
	}
	return ManagedRuntimeSlotGenerationResolution{
		RuntimeSlotID: slotID, GenerationOrdinal: uint64(ordinal), ExistingUnreleased: existing,
	}, nil
}

func requireNoUnclassifiedManagedRuntimeCustodyTx(
	ctx context.Context,
	tx *sql.Tx,
	request NativeProvisionAdmissionRequest,
) error {
	var unclassified bool
	err := tx.QueryRowContext(ctx, `
		SELECT
			EXISTS (
				SELECT 1
				FROM techstack_vm_leases AS lease
				LEFT JOIN managed_runtime_server_slot_generations AS generation
				  ON generation.tenant_id = lease.tenant_id
				 AND generation.lease_id = lease.id
				WHERE lease.tenant_id = $1
				  AND lease.desired_state IN ('running', 'stopped')
				  AND coalesce(lease.lease_json->'metadata'->>'stack_id', '') = $2
				  AND generation.lease_id IS NULL
			)
			OR EXISTS (
				SELECT 1
				FROM servers AS server
				LEFT JOIN managed_runtime_server_slot_generations AS generation
				  ON generation.tenant_id = server.tenant_id
				 AND generation.runtime_server_id = server.id
				WHERE server.tenant_id = $1
				  AND server.stack_id = $2
				  AND server.lifecycle_state <> 'decommissioned'
				  AND generation.runtime_server_id IS NULL
			)
	`, request.TenantID, request.Server.StackID).Scan(&unclassified)
	if err != nil {
		return fmt.Errorf("providercontrol: inspect unclassified managed runtime custody: %w", err)
	}
	if unclassified {
		return ManagedRuntimeUnclassifiedCustodyError{
			ProviderID: request.Lease.Resource.ProviderID, StackID: request.Server.StackID,
		}
	}
	return nil
}

// insertManagedRuntimeServerSlotTx binds the stable product slot to the exact
// provider generation admitted in this transaction. The database key, not a
// browser retry key, enforces one active generation per stack slot.
func insertManagedRuntimeServerSlotTx(
	ctx context.Context,
	tx *sql.Tx,
	request NativeProvisionAdmissionRequest,
	result NativeProvisionAdmissionResult,
	createdAt time.Time,
) error {
	if _, err := tx.ExecContext(ctx, `
		SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))
	`, request.TenantID, "managed-runtime-slot:"+request.RuntimeSlotID); err != nil {
		return fmt.Errorf("providercontrol: lock managed runtime server slot: %w", err)
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO managed_runtime_server_slots (
			tenant_id, stack_id, slot_key, slot_id, owner_subject_id, created_at
		) VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT DO NOTHING
	`,
		request.TenantID, request.Server.StackID, request.RuntimeSlotKey, request.RuntimeSlotID,
		request.OwnerSubjectID, createdAt,
	)
	if err != nil {
		return fmt.Errorf("providercontrol: bind managed runtime server slot: %w", err)
	}
	var storedSlotID, storedOwnerID string
	if err := tx.QueryRowContext(ctx, `
		SELECT slot_id, owner_subject_id
		FROM managed_runtime_server_slots
		WHERE tenant_id = $1 AND stack_id = $2 AND slot_key = $3
	`, request.TenantID, request.Server.StackID, request.RuntimeSlotKey).Scan(
		&storedSlotID, &storedOwnerID,
	); err != nil {
		return fmt.Errorf("providercontrol: load managed runtime server slot: %w", err)
	}
	if storedSlotID != request.RuntimeSlotID || storedOwnerID != request.OwnerSubjectID {
		return fmt.Errorf("%w: managed runtime slot identity mismatch", ErrNativeAdmissionConflict)
	}
	var maximumOrdinal int64
	var unresolvedCount int
	if err := tx.QueryRowContext(ctx, `
		SELECT
			coalesce(max(g.generation_ordinal), 0),
			count(g.resource_generation_id) FILTER (WHERE release.lease_id IS NULL)
		FROM managed_runtime_server_slot_generations AS g
		LEFT JOIN managed_runtime_capacity_release_facts AS release
		  ON release.tenant_id = g.tenant_id
		 AND release.lease_id = g.lease_id
		 AND release.resource_generation_id = g.resource_generation_id
		WHERE g.tenant_id = $1 AND g.slot_id = $2
	`, request.TenantID, request.RuntimeSlotID).Scan(&maximumOrdinal, &unresolvedCount); err != nil {
		return fmt.Errorf("providercontrol: inspect managed runtime slot occupancy: %w", err)
	}
	if maximumOrdinal < 0 || uint64(maximumOrdinal)+1 != request.RuntimeSlotGeneration {
		return fmt.Errorf("%w: managed runtime slot still owns another generation", ErrNativeAdmissionConflict)
	}
	switch unresolvedCount {
	case 0:
	case 1:
		if strings.TrimSpace(request.SupportReplacementID) == "" ||
			strings.ToLower(strings.TrimSpace(request.Lease.Resource.ProviderID)) != SupportReplacementTargetProvider {
			return fmt.Errorf("%w: managed runtime slot still owns another generation", ErrNativeAdmissionConflict)
		}
		var replacementMatches bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM managed_runtime_support_replacements AS sr
				WHERE sr.tenant_id = $1
				  AND sr.replacement_id = $2
				  AND sr.runtime_slot_id = $3
				  AND sr.source_generation_ordinal < sr.destination_generation_ordinal
				  AND sr.destination_generation_ordinal = $4
				  AND sr.destination_lease_id = $5
				  AND sr.destination_resource_generation_id = $6
				  AND sr.customer_accepted_at IS NOT NULL
				  AND sr.no_charge_confirmed = true
				  AND sr.target_product_slug = $7
				  AND sr.phase IN ('accepted', 'destination_admitted', 'staging')
				  AND EXISTS (
				      SELECT 1
				      FROM managed_runtime_server_slot_generations AS sg
				      WHERE sg.tenant_id = sr.tenant_id
				        AND sg.slot_id = sr.runtime_slot_id
				        AND sg.lease_id = sr.source_lease_id
				        AND sg.resource_generation_id::text = sr.source_resource_generation_id
				        AND sg.generation_ordinal = sr.source_generation_ordinal
				  )
				  AND NOT EXISTS (
				      SELECT 1
				      FROM managed_runtime_capacity_release_facts AS rel
				      WHERE rel.tenant_id = sr.tenant_id
				        AND rel.lease_id = sr.source_lease_id
				        AND rel.resource_generation_id::text = sr.source_resource_generation_id
				  )
			)
		`,
			request.TenantID, strings.TrimSpace(request.SupportReplacementID), request.RuntimeSlotID,
			request.RuntimeSlotGeneration,
			result.LeaseID, result.ResourceGenerationID, SupportReplacementTargetProductSlug,
		).Scan(&replacementMatches); err != nil {
			return fmt.Errorf("providercontrol: verify support replacement slot continuity: %w", err)
		}
		if !replacementMatches {
			return fmt.Errorf("%w: support replacement slot continuity verification failed", ErrNativeAdmissionConflict)
		}
	default:
		return fmt.Errorf("%w: managed runtime slot still owns another generation", ErrNativeAdmissionConflict)
	}
	generationInserted, err := tx.ExecContext(ctx, `
		INSERT INTO managed_runtime_server_slot_generations (
			tenant_id, slot_id, provider_id, lease_id, runtime_server_id,
			resource_generation_id, generation_ordinal, provision_operation_id,
			intent_digest, state, created_at
		) VALUES ($1,$2,$3,$4,$5,$6::uuid,$7,$8,$9,$10,$11)
		ON CONFLICT DO NOTHING
	`, request.TenantID, request.RuntimeSlotID, request.Lease.Resource.ProviderID,
		result.LeaseID, result.RuntimeServerID, result.ResourceGenerationID,
		request.RuntimeSlotGeneration, result.Operation.Command.OperationID,
		result.RequestDigest, managedRuntimeSlotStateActive, createdAt)
	if err != nil {
		return fmt.Errorf("providercontrol: bind managed runtime slot generation: %w", err)
	}
	rows, err := generationInserted.RowsAffected()
	if err != nil || rows != 1 {
		return fmt.Errorf("%w: managed runtime slot generation already exists", ErrNativeAdmissionConflict)
	}
	return nil
}

func validateManagedRuntimeServerSlotReplayTx(
	ctx context.Context,
	tx *sql.Tx,
	request NativeProvisionAdmissionRequest,
	result NativeProvisionAdmissionResult,
) error {
	var slotID, providerID, ownerID, leaseID, serverID, generationID, operationID, intentDigest, state string
	var generationOrdinal uint64
	err := tx.QueryRowContext(ctx, `
		SELECT s.slot_id, g.provider_id, s.owner_subject_id, g.lease_id, g.runtime_server_id,
		       g.resource_generation_id::text, g.generation_ordinal,
		       g.provision_operation_id, g.intent_digest, g.state
		FROM managed_runtime_server_slots s
		JOIN managed_runtime_server_slot_generations g
		  ON g.tenant_id = s.tenant_id AND g.slot_id = s.slot_id
		WHERE s.tenant_id = $1 AND s.stack_id = $2 AND s.slot_key = $3
		  AND g.resource_generation_id = $4::uuid
		  AND NOT EXISTS (
		      SELECT 1 FROM managed_runtime_capacity_release_facts AS release
		      WHERE release.tenant_id = g.tenant_id
		        AND release.lease_id = g.lease_id
		        AND release.resource_generation_id = g.resource_generation_id
		  )
	`, request.TenantID, request.Server.StackID, request.RuntimeSlotKey,
		result.ResourceGenerationID).Scan(
		&slotID, &providerID, &ownerID, &leaseID, &serverID, &generationID,
		&generationOrdinal, &operationID, &intentDigest, &state,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: managed runtime replay has no slot custody", ErrNativeAdmissionConflict)
	}
	if err != nil {
		return fmt.Errorf("providercontrol: load managed runtime slot replay: %w", err)
	}
	if slotID != request.RuntimeSlotID || providerID != request.Lease.Resource.ProviderID ||
		ownerID != request.OwnerSubjectID || leaseID != result.LeaseID ||
		serverID != result.RuntimeServerID || generationID != result.ResourceGenerationID ||
		generationOrdinal != request.RuntimeSlotGeneration ||
		operationID != result.Operation.Command.OperationID || intentDigest != result.RequestDigest ||
		strings.TrimSpace(state) != managedRuntimeSlotStateActive {
		return fmt.Errorf("%w: managed runtime slot custody projection mismatch", ErrNativeAdmissionConflict)
	}
	return nil
}
