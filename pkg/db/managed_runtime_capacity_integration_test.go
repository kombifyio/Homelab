package db

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/kombifyio/homelab/internal/managedstackkit"
	"github.com/kombifyio/homelab/pkg/auth"
	"github.com/kombifyio/homelab/pkg/backupstore"
	"github.com/kombifyio/homelab/pkg/originca"
	"github.com/kombifyio/stackkits/pkg/hostdelivery"
)

const managedRuntimeCapacityMigration = "migrations/033_managed_runtime_capacity_reservations.sql"

func TestIntegrationManagedRuntimeCapacityQuarantinesCanonicalLegacyAlias(t *testing.T) {
	database := openManagedRuntimeCapacityPremigrationDB(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	applyManagedRuntimeCapacityMigrationsBefore033(t, ctx, database)

	generationID := uuid.NewString()
	seedManagedRuntimeCapacityHistoricalLease(t, ctx, database, historicalCapacityLease{
		TenantID: "tenant-capacity-known", OwnerID: "owner-capacity-known",
		LeaseID: "lease-capacity-known", ServerID: "server-capacity-known",
		GenerationID: generationID, ResourceProviderID: "ionos-managed",
	})
	if err := applyManagedRuntimeCapacityMigration033(ctx, database); err != nil {
		t.Fatalf("apply migration 033 over canonical legacy alias: %v", err)
	}

	tx, err := database.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin quarantine assertion: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, "tenant-capacity-known"); err != nil {
		t.Fatalf("set quarantine assertion tenant: %v", err)
	}
	var ownerID, providerID, storedGeneration, mode, origin, policySource, policyDigest string
	var operationID sql.NullString
	var capacityLimit sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
		SELECT owner_subject_id, provider_id, resource_generation_id::text, operation_id,
		       reservation_mode, capacity_limit, policy_source, policy_digest,
		       reservation_origin
		FROM managed_runtime_capacity_reservations
		WHERE tenant_id = $1 AND lease_id = $2
	`, "tenant-capacity-known", "lease-capacity-known").Scan(
		&ownerID, &providerID, &storedGeneration, &operationID, &mode, &capacityLimit,
		&policySource, &policyDigest, &origin,
	); err != nil {
		t.Fatalf("load historical capacity quarantine: %v", err)
	}
	if ownerID != "owner-capacity-known" || providerID != "ionos" ||
		storedGeneration != generationID || operationID.Valid ||
		mode != "quarantine" || capacityLimit.Valid || origin != "migration_quarantine" ||
		policySource != "migration_quarantine:managed-runtime-capacity/v1" ||
		!strings.HasPrefix(policyDigest, "sha256:") {
		t.Fatalf(
			"historical capacity quarantine = owner %q provider %q generation %q operation %v mode %q limit %v source %q policy %q origin %q",
			ownerID, providerID, storedGeneration, operationID, mode, capacityLimit,
			policySource, policyDigest, origin,
		)
	}
}

func TestIntegrationManagedRuntimeCapacityRejectsUnknownProviderHandleWithoutBillingMetadata(t *testing.T) {
	database := openManagedRuntimeCapacityPremigrationDB(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	applyManagedRuntimeCapacityMigrationsBefore033(t, ctx, database)

	generationID := uuid.NewString()
	seedManagedRuntimeCapacityHistoricalLease(t, ctx, database, historicalCapacityLease{
		TenantID: "tenant-capacity-handle", OwnerID: "owner-capacity-handle",
		LeaseID: "lease-capacity-handle", ServerID: "server-capacity-handle",
		GenerationID: generationID, TypedProviderID: "unregistered-cloud",
		ResourceProviderID: "unregistered-cloud",
	})
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin unknown-handle fixture: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, "tenant-capacity-handle"); err != nil {
		t.Fatalf("set unknown-handle tenant: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE techstack_vm_leases
		SET engine_vm_id = 'external-vm-without-billing-metadata',
		    lease_json = jsonb_build_object(
		        'metadata', jsonb_build_object('resource_generation_id', $3::text)
		    )
		WHERE tenant_id = $1 AND id = $2
	`, "tenant-capacity-handle", "lease-capacity-handle", generationID); err != nil {
		t.Fatalf("remove optional billing metadata and retain provider handle: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit unknown-handle fixture: %v", err)
	}

	err = applyManagedRuntimeCapacityMigration033(ctx, database)
	if err == nil || !strings.Contains(err.Error(),
		"externally cost-bearing custody without exact IONOS or Centron identity") {
		t.Fatalf("unknown provider handle migration error = %v, want fail-closed custody rejection", err)
	}
}

func TestIntegrationManagedRuntimeCapacityRejectsUnknownCanonicalProvider(t *testing.T) {
	database := openManagedRuntimeCapacityPremigrationDB(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	applyManagedRuntimeCapacityMigrationsBefore033(t, ctx, database)

	seedManagedRuntimeCapacityHistoricalLease(t, ctx, database, historicalCapacityLease{
		TenantID: "tenant-capacity-unknown", OwnerID: "owner-capacity-unknown",
		LeaseID: "lease-capacity-unknown", ServerID: "server-capacity-unknown",
		GenerationID: uuid.NewString(), ResourceProviderID: "unregistered-cloud",
	})
	err := applyManagedRuntimeCapacityMigration033(ctx, database)
	if err == nil || !strings.Contains(err.Error(),
		"externally cost-bearing custody without exact IONOS or Centron identity") {
		t.Fatalf("unknown canonical provider migration error = %v, want fail-closed custody rejection", err)
	}
}

func TestIntegrationManagedRuntimeCapacityRejectsConflictingProviderCustody(t *testing.T) {
	database := openManagedRuntimeCapacityPremigrationDB(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	applyManagedRuntimeCapacityMigrationsBefore033(t, ctx, database)

	seedManagedRuntimeCapacityHistoricalLease(t, ctx, database, historicalCapacityLease{
		TenantID: "tenant-capacity-conflict", OwnerID: "owner-capacity-conflict",
		LeaseID: "lease-capacity-conflict", ServerID: "server-capacity-conflict",
		GenerationID: uuid.NewString(), TypedProviderID: "ionos", ResourceProviderID: "centron",
	})
	err := applyManagedRuntimeCapacityMigration033(ctx, database)
	if err == nil || !strings.Contains(err.Error(),
		"conflicting IONOS and Centron custody identity") {
		t.Fatalf("conflicting provider migration error = %v, want fail-closed custody rejection", err)
	}
}

func TestIntegrationManagedRuntimeCapacityCutoverRejectsActiveClaim(t *testing.T) {
	database := openManagedRuntimeCapacityPremigrationDB(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	applyManagedRuntimeCapacityMigrationsBefore(t, ctx, database, "029_")

	lease := historicalCapacityLease{
		TenantID: "tenant-capacity-active-claim", OwnerID: "owner-capacity-active-claim",
		LeaseID: "lease-capacity-active-claim", ServerID: "server-capacity-active-claim",
		GenerationID: uuid.NewString(), TypedProviderID: "ionos", ResourceProviderID: "ionos",
	}
	seedManagedRuntimeCapacityHistoricalLease(t, ctx, database, lease)
	seedManagedRuntimeCapacityPreDispatchActiveClaim(t, ctx, database, lease)
	for _, migration := range []string{
		"029_provider_provision_dispatch_guards.sql",
		"030_ril_action_execution_ledger.sql",
		"031_provider_provision_operator_resolution.sql",
		"032_provider_claim_credential_authority.sql",
	} {
		applyManagedRuntimeCapacityMigrationFile(t, ctx, database, "migrations/"+migration)
	}

	err := applyManagedRuntimeCapacityMigration033(ctx, database)
	if err == nil || !strings.Contains(err.Error(),
		"managed runtime capacity cutover requires all provider execution claims to be drained") {
		t.Fatalf("active claim migration error = %v, want stopped-lane drain rejection", err)
	}
}

func seedManagedRuntimeCapacityPreDispatchActiveClaim(
	t *testing.T,
	ctx context.Context,
	database *sql.DB,
	lease historicalCapacityLease,
) {
	t.Helper()
	const (
		manifestDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		commandDigest  = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		requested      = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
		accepted       = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
		specDigest     = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
		capabilityHash = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
		profileHash    = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
		claimToken     = "capacity-cutover-token"
		claimOwner     = "capacity-cutover-worker"
	)
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin pre-dispatch active claim seed: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, lease.TenantID); err != nil {
		t.Fatalf("set pre-dispatch claim tenant: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		SELECT
			set_config('app.provider_execution_claim_token', $1, true),
			set_config('app.provider_execution_claim_owner', $2, true)
	`, claimToken, claimOwner); err != nil {
		t.Fatalf("bind pre-dispatch claim capability: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO runtime_lease_execution_authorities (
			tenant_id, lease_id, execution_authority, bound_at
		) VALUES ($1, $2, 'techstack_provider_control', clock_timestamp())
	`, lease.TenantID, lease.LeaseID); err != nil {
		t.Fatalf("seed pre-dispatch native authority: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_catalog_versions (catalog_version, status)
		VALUES ('catalog-capacity-cutover', 'draft')
	`); err != nil {
		t.Fatalf("seed pre-dispatch catalog version: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_catalog_profiles (
			catalog_version, provider_id, adapter_id, credential_mode,
			runtime_profile_id, offering_id, can_pause, stop_effect,
			can_recreate, capability_snapshot
		) VALUES (
			'catalog-capacity-cutover', 'ionos', 'ionos-v1', 'managed',
			'ionos-managed-pvm-monthly', 'monthly-runtime-standard',
			true, 'pause', true, '{}'::jsonb
		)
	`); err != nil {
		t.Fatalf("seed pre-dispatch execution profile: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_desired_spec_revisions (
			tenant_id, lease_id, revision, spec_ref, spec_digest, spec_json, created_at
		) VALUES ($1, $2, 1, $3, $4, '{}'::jsonb, clock_timestamp())
	`, lease.TenantID, lease.LeaseID,
		"desired-spec://capacity-cutover/"+lease.LeaseID+"/revision-1", specDigest); err != nil {
		t.Fatalf("seed pre-dispatch desired spec: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_operations (
			tenant_id, operation_id, lease_id, operation, idempotency_key,
			adapter_id, command_digest, command_json, ledger_revision,
			desired_spec_revision, status, phase, head_sequence,
			head_receipt_digest, requested_at, created_at, updated_at
		) VALUES (
			$1, 'operation-capacity-claim', $2, 'provision',
			'capacity-cutover-idempotency', 'ionos-v1', $3,
			jsonb_build_object(
				'schema_version', 'techstack.provider-control-operation/v1',
				'execution_authority', 'techstack_provider_control',
				'execution_profile', jsonb_build_object(
					'catalog_version', 'catalog-capacity-cutover',
					'provider_id', 'ionos',
					'adapter_id', 'ionos-v1',
					'credential_mode', 'managed',
					'runtime_profile_id', 'ionos-managed-pvm-monthly',
					'offering_id', 'monthly-runtime-standard',
					'adapter_manifest_hash', $4::text,
					'capability_snapshot_hash', $8::text,
					'execution_profile_hash', $9::text
				),
				'command', jsonb_build_object(
					'provider_id', 'ionos',
					'lease_revision', 1,
					'runtime_server_id', $5::text,
					'resource_generation_id', $6::text
				)
			),
			1, 1, 'pending', 'requested', 1, $7,
			clock_timestamp(), clock_timestamp(), clock_timestamp()
		)
	`, lease.TenantID, lease.LeaseID, commandDigest, manifestDigest,
		lease.ServerID, lease.GenerationID, requested, capabilityHash, profileHash); err != nil {
		t.Fatalf("seed pre-dispatch provision operation: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_operation_receipts (
			tenant_id, operation_id, sequence, previous_receipt_digest,
			receipt_digest, status, phase, receipt_json, issued_at
		) VALUES
			($1, 'operation-capacity-claim', 1, NULL, $2,
			 'pending', 'requested', '{}'::jsonb, clock_timestamp()),
			($1, 'operation-capacity-claim', 2, $2, $3,
			 'pending', 'accepted', '{}'::jsonb, clock_timestamp())
	`, lease.TenantID, requested, accepted); err != nil {
		t.Fatalf("seed pre-dispatch provision receipts: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE provider_operations
		SET phase = 'accepted', head_sequence = 2,
			head_receipt_digest = $3, updated_at = clock_timestamp()
		WHERE tenant_id = $1 AND operation_id = 'operation-capacity-claim'
		  AND lease_id = $2
	`, lease.TenantID, lease.LeaseID, accepted); err != nil {
		t.Fatalf("advance pre-dispatch provision head: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_operation_execution_claims (
			tenant_id, operation_id, head_sequence, head_receipt_digest,
			claim_token_digest, claim_owner, state, claimed_at, lease_expires_at
		) VALUES (
			$1, 'operation-capacity-claim', 2, $2,
			encode(sha256(convert_to($3, 'UTF8')), 'hex'), $4, 'active',
			clock_timestamp(), clock_timestamp() + interval '1 minute'
		)
	`, lease.TenantID, accepted, claimToken, claimOwner); err != nil {
		t.Fatalf("seed pre-dispatch active provider claim: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit pre-dispatch active provider claim: %v", err)
	}
}

func TestIntegrationManagedRuntimeCapacityQuarantineCannotAuthorizeProvisionClaim(t *testing.T) {
	database := openManagedRuntimeCapacityPremigrationDB(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	applyManagedRuntimeCapacityMigrationsBefore033(t, ctx, database)

	generationID := uuid.NewString()
	lease := historicalCapacityLease{
		TenantID: "tenant-capacity-claim", OwnerID: "owner-capacity-claim",
		LeaseID: "lease-capacity-claim", ServerID: "server-capacity-claim",
		GenerationID: generationID, TypedProviderID: "ionos", ResourceProviderID: "ionos",
	}
	seedManagedRuntimeCapacityHistoricalLease(t, ctx, database, lease)
	receiptDigest := seedManagedRuntimeCapacityHistoricalNativeOperation(t, ctx, database, lease)
	if err := applyManagedRuntimeCapacityMigration033(ctx, database); err != nil {
		t.Fatalf("apply migration 033 over historical native operation: %v", err)
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin quarantined claim attempt: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, lease.TenantID); err != nil {
		t.Fatalf("set quarantined claim tenant: %v", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO provider_operation_execution_claims (
			tenant_id, operation_id, head_sequence, head_receipt_digest,
			claim_token_digest, claim_owner, claim_access, state,
			claimed_at, lease_expires_at
		) VALUES (
			$1, 'operation-capacity-claim', 1, $2,
			$3, 'capacity-claim-worker', 'side_effecting', 'active',
			clock_timestamp(), clock_timestamp() + interval '1 minute'
		)
	`, lease.TenantID, receiptDigest, strings.Repeat("a", 64))
	if err == nil || !strings.Contains(err.Error(),
		"provider execution claim is blocked by missing native managed runtime capacity authority") {
		t.Fatalf("quarantined provision claim error = %v, want native capacity authority rejection", err)
	}
}

func TestIntegrationManagedRuntimeCapacityRejectsPreCapacityNativeWriter(t *testing.T) {
	database := openManagedRuntimeCapacityPremigrationDB(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	applyManagedRuntimeCapacityMigrationsBefore033(t, ctx, database)

	generationID := uuid.NewString()
	seedManagedRuntimeCapacityHistoricalLease(t, ctx, database, historicalCapacityLease{
		TenantID: "tenant-capacity-writer", OwnerID: "owner-capacity-writer",
		LeaseID: "lease-capacity-writer", ServerID: "server-capacity-writer",
		GenerationID: generationID, TypedProviderID: "ionos", ResourceProviderID: "ionos",
	})
	if err := applyManagedRuntimeCapacityMigration033(ctx, database); err != nil {
		t.Fatalf("apply migration 033 before old-writer check: %v", err)
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin pre-capacity writer transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, "tenant-capacity-writer"); err != nil {
		t.Fatalf("set old-writer tenant: %v", err)
	}
	const manifestDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_catalog_versions (catalog_version, status)
		VALUES ('catalog-capacity-writer', 'draft')
	`); err != nil {
		t.Fatalf("seed old-writer catalog version: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_catalog_profiles (
			catalog_version, provider_id, adapter_id, credential_mode,
			runtime_profile_id, offering_id, can_pause, stop_effect,
			can_recreate, capability_snapshot, adapter_manifest_hash,
			provision_dispatch_mode
		) VALUES (
			'catalog-capacity-writer', 'ionos', 'ionos-v1', 'managed',
			'ionos-managed-pvm-monthly', 'monthly-runtime-standard',
			true, 'pause', true, '{}'::jsonb, $1, 'native_idempotency'
		)
	`, manifestDigest); err != nil {
		t.Fatalf("seed old-writer execution profile: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_desired_spec_revisions (
			tenant_id, lease_id, revision, spec_ref, spec_digest, spec_json, created_at
		) VALUES (
			'tenant-capacity-writer', 'lease-capacity-writer', 1,
			'desired-spec://capacity-writer/revision-1',
			'sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',
			'{}'::jsonb, clock_timestamp()
		)
	`); err != nil {
		t.Fatalf("seed old-writer desired spec: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_operations (
			tenant_id, operation_id, lease_id, operation, idempotency_key,
			adapter_id, command_digest, command_json, ledger_revision,
			desired_spec_revision, status, phase, head_sequence,
			head_receipt_digest, requested_at, created_at, updated_at,
			provision_dispatch_mode
		) VALUES (
			'tenant-capacity-writer', 'operation-capacity-writer',
			'lease-capacity-writer', 'provision', 'capacity-writer-idempotency',
			'ionos-v1',
			'sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc',
			jsonb_build_object(
				'schema_version', 'techstack.provider-control-operation/v1',
				'execution_authority', 'techstack_provider_control',
				'execution_profile', jsonb_build_object(
					'catalog_version', 'catalog-capacity-writer',
					'provider_id', 'ionos',
					'adapter_id', 'ionos-v1',
					'credential_mode', 'managed',
					'runtime_profile_id', 'ionos-managed-pvm-monthly',
					'offering_id', 'monthly-runtime-standard',
					'provision_dispatch_mode', 'native_idempotency',
					'adapter_manifest_hash', $1::text
				),
				'command', jsonb_build_object(
					'resource_generation_id', $2::text
				)
			),
			1, 1, 'pending', 'requested', 1,
			'sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd',
			clock_timestamp(), clock_timestamp(), clock_timestamp(),
			'native_idempotency'
		)
	`, manifestDigest, generationID); err != nil {
		t.Fatalf("old native writer insert: %v", err)
	}
	_, err = tx.ExecContext(ctx, `SET CONSTRAINTS provider_operations_require_capacity_reservation IMMEDIATE`)
	if err == nil || !strings.Contains(err.Error(),
		"native provision operation requires an atomic managed runtime capacity reservation") {
		t.Fatalf("old native writer constraint error = %v, want atomic capacity fence", err)
	}
}

type historicalCapacityLease struct {
	TenantID           string
	OwnerID            string
	LeaseID            string
	ServerID           string
	GenerationID       string
	TypedProviderID    any
	ResourceProviderID string
}

func seedManagedRuntimeCapacityHistoricalNativeOperation(
	t *testing.T,
	ctx context.Context,
	database *sql.DB,
	lease historicalCapacityLease,
) string {
	t.Helper()
	const (
		manifestDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		commandDigest  = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		receiptDigest  = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
		specDigest     = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	)
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin historical native operation seed: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, lease.TenantID); err != nil {
		t.Fatalf("set historical native operation tenant: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO runtime_lease_execution_authorities (
			tenant_id, lease_id, execution_authority, bound_at
		) VALUES ($1, $2, 'techstack_provider_control', clock_timestamp())
	`, lease.TenantID, lease.LeaseID); err != nil {
		t.Fatalf("seed historical native authority: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_catalog_versions (catalog_version, status)
		VALUES ('catalog-capacity-claim', 'draft')
	`); err != nil {
		t.Fatalf("seed historical claim catalog version: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_catalog_profiles (
			catalog_version, provider_id, adapter_id, credential_mode,
			runtime_profile_id, offering_id, can_pause, stop_effect,
			can_recreate, capability_snapshot, adapter_manifest_hash,
			provision_dispatch_mode
		) VALUES (
			'catalog-capacity-claim', 'ionos', 'ionos-v1', 'managed',
			'ionos-managed-pvm-monthly', 'monthly-runtime-standard',
			true, 'pause', true, '{}'::jsonb, $1, 'native_idempotency'
		)
	`, manifestDigest); err != nil {
		t.Fatalf("seed historical claim execution profile: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_desired_spec_revisions (
			tenant_id, lease_id, revision, spec_ref, spec_digest, spec_json, created_at
		) VALUES ($1, $2, 1, $3, $4, '{}'::jsonb, clock_timestamp())
	`, lease.TenantID, lease.LeaseID,
		"desired-spec://capacity-claim/"+lease.LeaseID+"/revision-1", specDigest); err != nil {
		t.Fatalf("seed historical claim desired spec: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_operations (
			tenant_id, operation_id, lease_id, operation, idempotency_key,
			adapter_id, command_digest, command_json, ledger_revision,
			desired_spec_revision, status, phase, head_sequence,
			head_receipt_digest, requested_at, created_at, updated_at,
			provision_dispatch_mode
		) VALUES (
			$1, 'operation-capacity-claim', $2, 'provision',
			'capacity-claim-idempotency', 'ionos-v1', $3,
			jsonb_build_object(
				'schema_version', 'techstack.provider-control-operation/v1',
				'execution_authority', 'techstack_provider_control',
				'execution_profile', jsonb_build_object(
					'catalog_version', 'catalog-capacity-claim',
					'provider_id', 'ionos',
					'adapter_id', 'ionos-v1',
					'credential_mode', 'managed',
					'runtime_profile_id', 'ionos-managed-pvm-monthly',
					'offering_id', 'monthly-runtime-standard',
					'provision_dispatch_mode', 'native_idempotency',
					'adapter_manifest_hash', $4::text
				),
				'command', jsonb_build_object(
					'resource_generation_id', $5::text
				)
			),
			1, 1, 'pending', 'requested', 1, $6,
			clock_timestamp(), clock_timestamp(), clock_timestamp(),
			'native_idempotency'
		)
	`, lease.TenantID, lease.LeaseID, commandDigest, manifestDigest,
		lease.GenerationID, receiptDigest); err != nil {
		t.Fatalf("seed historical native provision operation: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_operation_receipts (
			tenant_id, operation_id, sequence, previous_receipt_digest,
			receipt_digest, status, phase, receipt_json, issued_at
		) VALUES (
			$1, 'operation-capacity-claim', 1, NULL, $2,
			'pending', 'requested', '{}'::jsonb, clock_timestamp()
		)
	`, lease.TenantID, receiptDigest); err != nil {
		t.Fatalf("seed historical native provision receipt: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit historical native operation seed: %v", err)
	}
	return receiptDigest
}

func seedManagedRuntimeCapacityHistoricalLease(
	t *testing.T,
	ctx context.Context,
	database *sql.DB,
	lease historicalCapacityLease,
) {
	t.Helper()
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin historical capacity seed: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, lease.TenantID); err != nil {
		t.Fatalf("set historical capacity tenant: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO techstack_tenants (id, display_name, kind, status)
		VALUES ($1, $1, 'saas', 'active')
	`, lease.TenantID); err != nil {
		t.Fatalf("seed historical capacity tenant: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO servers (
			id, tenant_id, owner_subject_id, lease_id, name,
			lifecycle_state, desired_state, connection_state, health_state
		) VALUES (
			$1, $2, $3, $4, $1, 'active', 'running', 'pending', 'unknown'
		)
	`, lease.ServerID, lease.TenantID, lease.OwnerID, lease.LeaseID); err != nil {
		t.Fatalf("seed historical capacity server: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO techstack_vm_leases (
			id, tenant_id, subject_id, org_id, provider_id, engine_vm_id,
			desired_state, idempotency_key, lease_json, lease_revision,
			owner_subject_id, server_id, resource_generation_id,
			valid_from, valid_until, renewed_at, created_at, updated_at
		) VALUES (
			$1, $2, $3, $2, $4, NULL,
			'running', $1 || '-idempotency',
			jsonb_build_object(
				'billing_mode', 'subscription',
				'lifecycle_class', 'subscription',
				'resource', jsonb_build_object('provider_id', $5::text),
				'metadata', jsonb_build_object('resource_generation_id', $6::text)
			),
			1, $3, $7, $6::uuid,
			clock_timestamp(), clock_timestamp() + interval '1 year',
			clock_timestamp(), clock_timestamp(), clock_timestamp()
		)
	`, lease.LeaseID, lease.TenantID, lease.OwnerID, lease.TypedProviderID,
		lease.ResourceProviderID, lease.GenerationID, lease.ServerID); err != nil {
		t.Fatalf("seed historical capacity lease: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit historical capacity seed: %v", err)
	}
}

func openManagedRuntimeCapacityPremigrationDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := integrationDSN()
	if dsn == "" {
		t.Skip("HOMELAB_TEST_POSTGRES_URL not set; skipping managed capacity PostgreSQL integration test")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse managed capacity integration DSN: %v", err)
	}
	admin := stdlib.OpenDB(*config)
	if err := admin.PingContext(t.Context()); err != nil {
		_ = admin.Close()
		t.Fatalf("ping managed capacity integration PostgreSQL: %v", err)
	}
	schema := "managed_capacity_pre033_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.ExecContext(t.Context(), "CREATE SCHEMA "+quotedSchema); err != nil {
		_ = admin.Close()
		t.Fatalf("create pre-033 schema: %v", err)
	}

	scoped := config.Copy()
	scoped.RuntimeParams = make(map[string]string, len(config.RuntimeParams)+1)
	for key, value := range config.RuntimeParams {
		scoped.RuntimeParams[key] = value
	}
	scoped.RuntimeParams["search_path"] = schema + ",public"
	database := stdlib.OpenDB(*scoped)
	if err := database.PingContext(t.Context()); err != nil {
		_ = database.Close()
		_, _ = admin.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+quotedSchema+" CASCADE")
		_ = admin.Close()
		t.Fatalf("open pre-033 schema: %v", err)
	}
	t.Cleanup(func() {
		_ = database.Close()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.ExecContext(cleanupCtx, "DROP SCHEMA IF EXISTS "+quotedSchema+" CASCADE")
		_ = admin.Close()
	})
	return database
}

func applyManagedRuntimeCapacityMigrationsBefore033(
	t *testing.T,
	ctx context.Context,
	database *sql.DB,
) {
	t.Helper()
	applyManagedRuntimeCapacityMigrationsBefore(t, ctx, database, "033_")
}

func applyManagedRuntimeCapacityMigrationsBefore(
	t *testing.T,
	ctx context.Context,
	database *sql.DB,
	stopPrefix string,
) {
	t.Helper()
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() >= stopPrefix {
			continue
		}
		applyManagedRuntimeCapacityMigrationFile(t, ctx, database, "migrations/"+entry.Name())
	}
}

func applyManagedRuntimeCapacityMigrationFile(
	t *testing.T,
	ctx context.Context,
	database *sql.DB,
	path string,
) {
	t.Helper()
	payload, err := migrationsFS.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin %s: %v", path, err)
	}
	if _, err := tx.ExecContext(ctx, string(payload)); err != nil {
		_ = tx.Rollback()
		t.Fatalf("apply %s: %v", path, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit %s: %v", path, err)
	}
}

func applyManagedRuntimeCapacityMigration033(ctx context.Context, database *sql.DB) error {
	payload, err := migrationsFS.ReadFile(managedRuntimeCapacityMigration)
	if err != nil {
		return fmt.Errorf("read migration 033: %w", err)
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration 033: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, string(payload)); err != nil {
		return err
	}
	return tx.Commit()
}

func TestIntegrationMigration205SupportReplacementGuardsAndRLS(t *testing.T) {
	dsn := forwardNamesDSN(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	target, admin, _ := newForwardBatchDatabase(t, ctx, dsn)
	prepareForwardNamesRoles(t, ctx, admin)
	pinApplyMigrationsBefore(t, ctx, target, "999_")

	var pin ExpectedDatabaseIdentity
	if err := admin.QueryRowContext(ctx, `SELECT current_database(), system_identifier::text FROM pg_control_system()`).Scan(
		&pin.DatabaseName, &pin.SystemIdentifier); err != nil {
		t.Fatal(err)
	}
	if err := target.VerifyMigrationsAtFloor(ctx, pin, SchemaFloor); err != nil {
		t.Fatalf("VerifyMigrationsAtFloor(%d): %v", SchemaFloor, err)
	}

	// Verify initial insert into managed_runtime_support_replacements enforces
	// authorized start, exact target pinning, and no_charge_confirmed = true.
	execForTenant := func(tenantID, query string, args ...any) error {
		tx, err := admin.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return err
		}
		return tx.Commit()
	}
	tenantA := "tenant-sr-a"
	tenantB := "tenant-sr-b"
	genA := uuid.NewString()
	genDest := uuid.NewString()
	if err := execForTenant(tenantA, `
		INSERT INTO managed_runtime_support_replacements (
			tenant_id, replacement_id, owner_subject_id, stack_id, runtime_slot_id,
			source_lease_id, source_server_id, source_resource_generation_id, source_generation_ordinal,
			no_charge_confirmed, case_ref, reason, authorized_by, evidence_ref,
			idempotency_key, request_digest, phase
		) VALUES (
			$1, 'sr-1', 'owner-a', 'stack-a', 'slot-a',
			'lease-src-a', 'srv-src-a', $2, 1,
			true, 'CASE-101', 'Free IONOS Memory Cube S replacement', 'staff@kombify.io', 'ev://case-101',
			'idem-sr-1', 'sha256:aaa', 'authorized'
		)
	`, tenantA, genA); err != nil {
		t.Fatalf("seed support replacement sr-1: %v", err)
	}
	if err := execForTenant(tenantB, `
		INSERT INTO managed_runtime_support_replacements (
			tenant_id, replacement_id, owner_subject_id, stack_id, runtime_slot_id,
			source_lease_id, source_server_id, source_resource_generation_id, source_generation_ordinal,
			no_charge_confirmed, case_ref, reason, authorized_by, evidence_ref,
			idempotency_key, request_digest, phase
		) VALUES (
			$1, 'sr-2', 'owner-b', 'stack-b', 'slot-b',
			'lease-src-b', 'srv-src-b', $2, 1,
			true, 'CASE-102', 'Free IONOS Memory Cube S replacement', 'staff@kombify.io', 'ev://case-102',
			'idem-sr-2', 'sha256:bbb', 'authorized'
		)
	`, tenantB, uuid.NewString()); err != nil {
		t.Fatalf("seed support replacement sr-2: %v", err)
	}

	// Reject starting directly in completed, non-Memory-Cube-S target, or with pre-populated destination columns (P2-6).
	if err := execForTenant(tenantA, `
		INSERT INTO managed_runtime_support_replacements (
			tenant_id, replacement_id, owner_subject_id, stack_id, runtime_slot_id,
			source_lease_id, source_server_id, source_resource_generation_id, source_generation_ordinal,
			target_product_slug, no_charge_confirmed, case_ref, reason, authorized_by, evidence_ref,
			idempotency_key, request_digest, phase
		) VALUES (
			$1, 'sr-bad-target', 'owner-a', 'stack-a', 'slot-x',
			'lease-x', 'srv-x', $2, 1,
			'ionos-cube-standard-basic', true, 'CASE-BAD', 'bad', 'staff@kombify.io', 'ev://bad',
			'idem-bad-target', 'sha256:bad', 'authorized'
		)
	`, tenantA, uuid.NewString()); err == nil {
		t.Fatal("non-Memory-Cube-S target_product_slug was accepted")
	}
	if err := execForTenant(tenantA, `
		INSERT INTO managed_runtime_support_replacements (
			tenant_id, replacement_id, owner_subject_id, stack_id, runtime_slot_id,
			source_lease_id, source_server_id, source_resource_generation_id, source_generation_ordinal,
			destination_lease_id, destination_server_id, destination_resource_generation_id, destination_generation_ordinal, destination_operation_id,
			no_charge_confirmed, case_ref, reason, authorized_by, evidence_ref,
			idempotency_key, request_digest, phase
		) VALUES (
			$1, 'sr-bad-dest-insert', 'owner-a', 'stack-a', 'slot-x',
			'lease-x', 'srv-x', $2, 1,
			'lease-dest-x', 'srv-dest-x', $3, 2, 'op-dest-x',
			true, 'CASE-BAD-DEST', 'bad dest on insert', 'staff@kombify.io', 'ev://bad-dest',
			'idem-bad-dest-insert', 'sha256:baddest', 'authorized'
		)
	`, tenantA, uuid.NewString(), uuid.NewString()); err == nil {
		t.Fatal("INSERT with non-NULL destination columns was accepted")
	}

	// Verify FORCE ROW LEVEL SECURITY tenant isolation under a NOBYPASSRLS role.
	if _, err := admin.ExecContext(ctx, `
		DO $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'techstack_provider_control_runtime') THEN
				CREATE ROLE techstack_provider_control_runtime NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION;
			END IF;
		END $$;
		GRANT USAGE ON SCHEMA public TO techstack_provider_control_runtime;
		GRANT SELECT, INSERT, UPDATE ON managed_runtime_support_replacements TO techstack_provider_control_runtime;
	`); err != nil {
		t.Fatalf("prepare provider runtime role for RLS check: %v", err)
	}
	appTx, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = appTx.Rollback() }()
	if _, err := appTx.ExecContext(ctx, `SET LOCAL ROLE techstack_provider_control_runtime`); err != nil {
		t.Fatalf("assume techstack_provider_control_runtime: %v", err)
	}
	var visibleCount int
	if err := appTx.QueryRowContext(ctx, `SELECT count(*) FROM managed_runtime_support_replacements`).Scan(&visibleCount); err != nil {
		t.Fatalf("unscoped RLS query: %v", err)
	}
	if visibleCount != 0 {
		t.Fatalf("unscoped RLS visible count = %d, want 0", visibleCount)
	}
	if _, err := appTx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantA); err != nil {
		t.Fatalf("set app.tenant_id: %v", err)
	}
	if err := appTx.QueryRowContext(ctx, `SELECT count(*) FROM managed_runtime_support_replacements`).Scan(&visibleCount); err != nil {
		t.Fatalf("query support replacements under RLS: %v", err)
	}
	if visibleCount != 1 {
		t.Fatalf("RLS visible count = %d, want 1", visibleCount)
	}
	if _, err := appTx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantB); err != nil {
		t.Fatal(err)
	}
	var seenID string
	if err := appTx.QueryRowContext(ctx, `SELECT replacement_id FROM managed_runtime_support_replacements`).Scan(&seenID); err != nil {
		t.Fatal(err)
	}
	if seenID != "sr-2" {
		t.Fatalf("tenant B saw %q, want sr-2", seenID)
	}
	_ = appTx.Rollback()

	// Verify P2-1 owner-binding CHECK, P2-6 transition trigger guards, and immutability:
	// - customer_accepted_by != owner_subject_id is rejected
	// - destination_admitted -> cutover_in_progress shortcut is rejected (must pass through staging)
	// - destination binding columns cannot mutate after destination_admitted
	// - rolled_back is blocked once destination_writes_observed = true
	// - source_decommissioning is blocked without cutover_verified_at
	if err := execForTenant(tenantA, `
		UPDATE managed_runtime_support_replacements
		SET phase = 'accepted', customer_accepted_at = now(), customer_accepted_by = 'different-subject',
		    destination_lease_id = 'lease-dest-a', destination_server_id = 'srv-dest-a',
		    destination_resource_generation_id = $2, destination_generation_ordinal = 2,
		    destination_operation_id = 'op-dest-a'
		WHERE tenant_id = $1 AND replacement_id = 'sr-1'
	`, tenantA, genDest); err == nil {
		t.Fatal("customer_accepted_by != owner_subject_id was accepted")
	}
	if err := execForTenant(tenantA, `
		UPDATE managed_runtime_support_replacements
		SET phase = 'accepted', customer_accepted_at = now(), customer_accepted_by = 'owner-a',
		    destination_lease_id = 'lease-dest-a', destination_server_id = 'srv-dest-a',
		    destination_resource_generation_id = $2, destination_generation_ordinal = 2,
		    destination_operation_id = 'op-dest-a'
		WHERE tenant_id = $1 AND replacement_id = 'sr-1'
	`, tenantA, genDest); err != nil {
		t.Fatalf("accept sr-1: %v", err)
	}
	if err := execForTenant(tenantA, `
		UPDATE managed_runtime_support_replacements
		SET phase = 'destination_admitted'
		WHERE tenant_id = $1 AND replacement_id = 'sr-1'
	`, tenantA); err != nil {
		t.Fatalf("destination_admitted sr-1: %v", err)
	}
	if err := execForTenant(tenantA, `
		UPDATE managed_runtime_support_replacements
		SET phase = 'cutover_in_progress', destination_writes_observed = true
		WHERE tenant_id = $1 AND replacement_id = 'sr-1'
	`, tenantA); err == nil {
		t.Fatal("destination_admitted -> cutover_in_progress shortcut was allowed")
	}
	if err := execForTenant(tenantA, `
		UPDATE managed_runtime_support_replacements
		SET phase = 'staging'
		WHERE tenant_id = $1 AND replacement_id = 'sr-1'
	`, tenantA); err != nil {
		t.Fatalf("staging sr-1: %v", err)
	}
	if err := execForTenant(tenantA, `
		UPDATE managed_runtime_support_replacements
		SET destination_lease_id = 'lease-dest-mutated'
		WHERE tenant_id = $1 AND replacement_id = 'sr-1'
	`, tenantA); err == nil {
		t.Fatal("destination_lease_id mutation after destination_admitted was allowed")
	}
	if err := execForTenant(tenantA, `
		UPDATE managed_runtime_support_replacements
		SET phase = 'cutover_in_progress', destination_writes_observed = true
		WHERE tenant_id = $1 AND replacement_id = 'sr-1'
	`, tenantA); err != nil {
		t.Fatalf("cutover_in_progress sr-1: %v", err)
	}
	if err := execForTenant(tenantA, `
		UPDATE managed_runtime_support_replacements
		SET phase = 'rolled_back', rollback_reason = 'should fail'
		WHERE tenant_id = $1 AND replacement_id = 'sr-1'
	`, tenantA); err == nil {
		t.Fatal("rolled_back allowed after destination_writes_observed = true")
	}
	if err := execForTenant(tenantA, `
		UPDATE managed_runtime_support_replacements
		SET phase = 'source_decommissioning'
		WHERE tenant_id = $1 AND replacement_id = 'sr-1'
	`, tenantA); err == nil {
		t.Fatal("source_decommissioning allowed without cutover_verified_at")
	}
	if err := execForTenant(tenantA, `
		UPDATE managed_runtime_support_replacements
		SET phase = 'fenced_conflict', fenced_conflict_reason = 'lost ack'
		WHERE tenant_id = $1 AND replacement_id = 'sr-1'
	`, tenantA); err != nil {
		t.Fatalf("fenced_conflict sr-1: %v", err)
	}

	// Verify provider_control_list_never_enrolled_runtime_candidates executes cleanly at 205.
	var candidateCount int
	if err := admin.QueryRowContext(ctx, `
		SELECT count(*) FROM provider_control_list_never_enrolled_runtime_candidates('', '', 300, 10)
	`).Scan(&candidateCount); err != nil {
		t.Fatalf("provider_control_list_never_enrolled_runtime_candidates failed: %v", err)
	}

	// Verify BUG-2 (> source_generation_ordinal), rolling_back phase, and BUG-3 (continuity guard
	// ignores pre-cutover rolled_back replacement destinations while preserving completed Memory Cube S continuity).
	contExec := func(query string, args ...any) {
		t.Helper()
		tx, err := admin.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin continuity tx: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', 'tenant-sr-cont', true)`); err != nil {
			t.Fatalf("set continuity app.tenant_id: %v", err)
		}
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("continuity sql failed: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit continuity tx: %v", err)
		}
	}
	contExec(`
		INSERT INTO techstack_tenants (id, display_name, kind, status)
		VALUES ('tenant-sr-cont', 'Support Replacement Continuity', 'saas', 'active')
		ON CONFLICT DO NOTHING;
		INSERT INTO stacks (id, tenant_id, owner_subject_id, name, status)
		VALUES ('stack-cont', 'tenant-sr-cont', 'owner-cont', 'Continuity Stack', 'active')
		ON CONFLICT DO NOTHING;
		INSERT INTO managed_runtime_server_slots (tenant_id, stack_id, slot_key, slot_id, owner_subject_id, created_at)
		VALUES ('tenant-sr-cont', 'stack-cont', 'foundation', 'slot-cont-1', 'owner-cont', now())
		ON CONFLICT DO NOTHING;
	`)

	contExec(`
		INSERT INTO provider_catalog_versions (catalog_version, status)
		VALUES ('catalog-sr-cont', 'draft')
	`)
	contExec(`
		INSERT INTO provider_catalog_profiles (
			catalog_version, provider_id, adapter_id, credential_mode,
			runtime_profile_id, offering_id, can_pause, stop_effect,
			can_recreate, capability_snapshot, adapter_manifest_hash,
			provision_dispatch_mode
		) VALUES (
			'catalog-sr-cont', 'ionos', 'ionos-cloudapi-v6', 'managed',
			'ionos-basic-cube-standard', 'monthly-runtime-standard',
			true, 'pause', true, '{}'::jsonb,
			'sha256:640fcf4852e807ed28fa571bdad0ff2b7a21d74a4be975f2729ca5c6e3aef8ad',
			'native_idempotency'
		), (
			'catalog-sr-cont', 'ionos', 'ionos-cloudapi-v6-memory-s', 'managed',
			'ionos-memory-cube-s-standard', 'ionos-cube-standard-memory',
			true, 'pause', true, '{}'::jsonb,
			'sha256:6167cd0c480813ea24c3730bde830958fe0ff2042f572a4e89ba53606d4e5fed',
			'native_idempotency'
		)
	`)
	contExec(`UPDATE provider_catalog_versions SET status = 'retired', retired_at = clock_timestamp() WHERE status = 'active'`)
	contExec(`UPDATE provider_catalog_versions SET status = 'active', activated_at = clock_timestamp() WHERE catalog_version = 'catalog-sr-cont'`)

	seedGen := func(leaseID, serverID, opID, adapterID string, ordinal int64, released bool) string {
		genID := uuid.NewString()
		manifestHash := "sha256:640fcf4852e807ed28fa571bdad0ff2b7a21d74a4be975f2729ca5c6e3aef8ad"
		if adapterID == "ionos-cloudapi-v6-memory-s" {
			manifestHash = "sha256:6167cd0c480813ea24c3730bde830958fe0ff2042f572a4e89ba53606d4e5fed"
		}
		contExec(`
			INSERT INTO servers (id, tenant_id, stack_id, owner_subject_id, provider_ref, name, lifecycle_state, desired_state, connection_state, generation)
			VALUES ($1, 'tenant-sr-cont', 'stack-cont', 'owner-cont', 'ionos', $1, 'active', 'running', 'connected', 1)
		`, serverID)
		contExec(`
			INSERT INTO techstack_vm_leases (id, tenant_id, owner_subject_id, provider_id, server_id, lease_revision, resource_generation_id, desired_state, lease_json)
			VALUES ($1, 'tenant-sr-cont', 'owner-cont', 'ionos', $2, 1, $3::uuid, 'running',
				jsonb_build_object(
					'id', $1::text,
					'subject', jsonb_build_object('kind','user','id','owner-cont','org_id','tenant-sr-cont'),
					'resource', jsonb_build_object('provider_id','ionos','region','de/fra/2'),
					'metadata', jsonb_build_object(
						'tenant_id','tenant-sr-cont',
						'resource_generation_id', $3::text,
						'runtime_offering_id','monthly-runtime-standard',
						'provider_package', CASE WHEN $4 = 'ionos-cloudapi-v6-memory-s' THEN 'Memory Cube S' ELSE 'Basic Cube S' END,
						'provider_execution_offering_id', CASE WHEN $4 = 'ionos-cloudapi-v6-memory-s' THEN 'ionos-cube-standard-memory' ELSE '' END
					)
				)
			)
		`, leaseID, serverID, genID, adapterID)
		contExec(`UPDATE servers SET lease_id = $2 WHERE tenant_id = 'tenant-sr-cont' AND id = $1`, serverID, leaseID)
		contExec(`
			INSERT INTO runtime_lease_execution_authorities (tenant_id, lease_id, execution_authority, bound_at)
			VALUES ('tenant-sr-cont', $1, 'techstack_provider_control', clock_timestamp())
		`, leaseID)
		contExec(`
			INSERT INTO provider_desired_spec_revisions (tenant_id, lease_id, revision, spec_ref, spec_digest, spec_json, created_at)
			VALUES ('tenant-sr-cont', $1, 1, 'desired-spec://homelab/leases/' || $1 || '/revisions/1', 'sha256:spec-' || $1, '{}'::jsonb, now())
		`, leaseID)
		opTx, err := admin.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin continuity op tx: %v", err)
		}
		if _, err := opTx.ExecContext(ctx, `SELECT set_config('app.tenant_id', 'tenant-sr-cont', true)`); err != nil {
			_ = opTx.Rollback()
			t.Fatalf("set continuity op tenant: %v", err)
		}
		if _, err := opTx.ExecContext(ctx, `
			INSERT INTO provider_operations (
				tenant_id, operation_id, lease_id, operation, idempotency_key, adapter_id,
				command_digest, command_json, ledger_revision, desired_spec_revision,
				status, phase, head_sequence, head_receipt_digest, requested_at, created_at, updated_at,
				provision_dispatch_mode
			)
			VALUES (
				'tenant-sr-cont', $1, $2, 'provision', 'idem-' || $1, $3,
				'sha256:cmd-' || $1,
				jsonb_build_object(
					'schema_version','techstack.provider-control-operation/v1',
					'execution_authority','techstack_provider_control',
					'runtime_server_generation', 1,
					'command', jsonb_build_object(
						'provider_id','ionos',
						'adapter_id',$3::text,
						'resource_generation_id',$4::text,
						'runtime_server_id',$6::text,
						'lease_revision', 1
					),
					'execution_profile', jsonb_build_object(
						'catalog_version', 'catalog-sr-cont',
						'provider_id','ionos',
						'adapter_id',$3::text,
						'credential_mode', 'managed',
						'runtime_profile_id', CASE WHEN $3 = 'ionos-cloudapi-v6-memory-s' THEN 'ionos-memory-cube-s-standard' ELSE 'ionos-basic-cube-standard' END,
						'offering_id', CASE WHEN $3 = 'ionos-cloudapi-v6-memory-s' THEN 'ionos-cube-standard-memory' ELSE 'monthly-runtime-standard' END,
						'provision_dispatch_mode', 'native_idempotency',
						'adapter_manifest_hash', $5::text
					)
				),
				1, 1, 'pending', 'requested', 1, 'sha256:rcpt-' || $1, now(), now(), now(),
				'native_idempotency'
			)
		`, opID, leaseID, adapterID, genID, manifestHash, serverID); err != nil {
			_ = opTx.Rollback()
			t.Fatalf("insert continuity op: %v", err)
		}
		if _, err := opTx.ExecContext(ctx, `
			INSERT INTO managed_runtime_capacity_reservations (
				tenant_id, owner_subject_id, provider_id, lease_id, resource_generation_id,
				operation_id, admission_idempotency_key, reservation_mode, capacity_limit,
				policy_source, policy_digest, reservation_origin
			) VALUES (
				'tenant-sr-cont', 'owner-cont', 'ionos', $1, $2::uuid,
				$3, 'idem-' || $3, 'limited', 5,
				'edge_v2_entitlement+signed_budget:cloud.runtime.credits#managed_servers',
				managed_runtime_capacity_policy_digest(
					'tenant-sr-cont', 'ionos', 'owner_subject', 'owner-cont', 'limited', 5,
					'edge_v2_entitlement+signed_budget:cloud.runtime.credits#managed_servers'
				),
				'native_admission'
			)
		`, leaseID, genID, opID); err != nil {
			_ = opTx.Rollback()
			t.Fatalf("insert continuity reservation: %v", err)
		}
		if err := opTx.Commit(); err != nil {
			t.Fatalf("commit continuity op tx: %v", err)
		}
		contExec(`
			INSERT INTO managed_runtime_server_slot_generations (
				tenant_id, slot_id, provider_id, lease_id, runtime_server_id,
				resource_generation_id, generation_ordinal, provision_operation_id, intent_digest, state, created_at
			) VALUES (
				'tenant-sr-cont', 'slot-cont-1', 'ionos', $1, $2, $3::uuid, $4, $5, 'sha256:1111111111111111111111111111111111111111111111111111111111111111', 'active', now()
			)
		`, leaseID, serverID, genID, ordinal, opID)
		if released {
			relOpID := "decom-" + opID
			relRcpt1Digest := "sha256:7777777777777777777777777777777777777777777777777777777777777777"
			relRcpt2Digest := "sha256:9999999999999999999999999999999999999999999999999999999999999999"
			contExec(`
				UPDATE servers
				SET lifecycle_state = 'decommissioned', desired_state = 'absent', decommissioned_at = clock_timestamp()
				WHERE tenant_id = 'tenant-sr-cont' AND id = $1
			`, serverID)
			contExec(`
				INSERT INTO provider_operations (
					tenant_id, operation_id, lease_id, operation, idempotency_key, adapter_id,
					command_digest, command_json, ledger_revision, desired_spec_revision,
					status, phase, head_sequence, head_receipt_digest, requested_at, created_at, updated_at,
					provision_dispatch_mode
				)
				SELECT
					'tenant-sr-cont', $1, $2, 'decommission', 'idem-' || $1, p.adapter_id,
					'sha256:8888888888888888888888888888888888888888888888888888888888888888',
					p.command_json, 1, NULL,
					'succeeded', 'absent', 2, $4, now(), now(), now(),
					p.provision_dispatch_mode
				FROM provider_operations AS p
				WHERE p.tenant_id = 'tenant-sr-cont' AND p.operation_id = $3
			`, relOpID, leaseID, opID, relRcpt2Digest)
			contExec(`
				INSERT INTO provider_operation_receipts (
					tenant_id, operation_id, sequence, previous_receipt_digest, receipt_digest, status, phase, receipt_json, issued_at
				) VALUES (
					'tenant-sr-cont', $1, 1, NULL, $2, 'pending', 'requested', '{}'::jsonb, clock_timestamp()
				), (
					'tenant-sr-cont', $1, 2, $2, $3, 'succeeded', 'absent', '{}'::jsonb, clock_timestamp()
				)
			`, relOpID, relRcpt1Digest, relRcpt2Digest)
			contExec(`
				INSERT INTO managed_runtime_capacity_release_facts (
					tenant_id, lease_id, resource_generation_id, server_id, server_generation,
					release_operation_id, release_authority, receipt_sequence, receipt_digest, released_at
				) VALUES (
					'tenant-sr-cont', $1, $2::uuid, $3, 1,
					$4, 'provider_absence', 2, $5, clock_timestamp()
				)
			`, leaseID, genID, serverID, relOpID, relRcpt2Digest)
		}
		return genID
	}

	// Ordinal 1: Basic Cube S (released later).
	gen1 := seedGen("lease-cont-1", "srv-cont-1", "op-cont-1", "ionos-cloudapi-v6", 1, false)
	// Ordinal 2: Memory Cube S replacement that is rolled back pre-cutover via rolling_back -> rolled_back.
	gen2 := seedGen("lease-cont-2", "srv-cont-2", "op-cont-2", "ionos-cloudapi-v6-memory-s", 2, true)
	contExec(`
		INSERT INTO managed_runtime_support_replacements (
			tenant_id, replacement_id, owner_subject_id, stack_id, runtime_slot_id,
			source_lease_id, source_server_id, source_resource_generation_id, source_generation_ordinal,
			no_charge_confirmed, case_ref, reason, authorized_by, evidence_ref,
			idempotency_key, request_digest, phase
		) VALUES (
			'tenant-sr-cont', 'sr-cont-rb', 'owner-cont', 'stack-cont', 'slot-cont-1',
			'lease-cont-1', 'srv-cont-1', $1, 1,
			true, 'CASE-CONT-1', 'rollback continuity test', 'staff@kombify.io', 'ev://cont-1',
			'idem-cont-1', 'sha256:cont1', 'authorized'
		)
	`, gen1)
	contExec(`
		UPDATE managed_runtime_support_replacements
		SET phase = 'accepted', customer_accepted_at = now(), customer_accepted_by = 'owner-cont',
		    destination_lease_id = 'lease-cont-2', destination_server_id = 'srv-cont-2',
		    destination_resource_generation_id = $1, destination_generation_ordinal = 2,
		    destination_operation_id = 'op-cont-2'
		WHERE tenant_id = 'tenant-sr-cont' AND replacement_id = 'sr-cont-rb'
	`, gen2)
	contExec(`UPDATE managed_runtime_support_replacements SET phase = 'destination_admitted' WHERE tenant_id = 'tenant-sr-cont' AND replacement_id = 'sr-cont-rb'`)
	contExec(`UPDATE managed_runtime_support_replacements SET phase = 'staging' WHERE tenant_id = 'tenant-sr-cont' AND replacement_id = 'sr-cont-rb'`)
	contExec(`UPDATE managed_runtime_support_replacements SET phase = 'rolling_back', rollback_reason = 'async teardown' WHERE tenant_id = 'tenant-sr-cont' AND replacement_id = 'sr-cont-rb'`)
	contExec(`UPDATE managed_runtime_support_replacements SET phase = 'rolled_back' WHERE tenant_id = 'tenant-sr-cont' AND replacement_id = 'sr-cont-rb'`)

	// Retry support replacement at ordinal 3 (> source ordinal 1) must be admitted by CHECK constraint.
	contExec(`
		INSERT INTO managed_runtime_support_replacements (
			tenant_id, replacement_id, owner_subject_id, stack_id, runtime_slot_id,
			source_lease_id, source_server_id, source_resource_generation_id, source_generation_ordinal,
			no_charge_confirmed, case_ref, reason, authorized_by, evidence_ref,
			idempotency_key, request_digest, phase
		) VALUES (
			'tenant-sr-cont', 'sr-cont-retry', 'owner-cont', 'stack-cont', 'slot-cont-1',
			'lease-cont-1', 'srv-cont-1', $1, 1,
			true, 'CASE-CONT-2', 'retry after pre-cutover rollback', 'staff@kombify.io', 'ev://cont-2',
			'idem-cont-2', 'sha256:cont2', 'authorized'
		)
	`, gen1)
	contExec(`
		UPDATE managed_runtime_support_replacements
		SET phase = 'accepted', customer_accepted_at = now(), customer_accepted_by = 'owner-cont',
		    destination_lease_id = 'lease-cont-3-retry', destination_server_id = 'srv-cont-3-retry',
		    destination_resource_generation_id = $1, destination_generation_ordinal = 3,
		    destination_operation_id = 'op-cont-3-retry'
		WHERE tenant_id = 'tenant-sr-cont' AND replacement_id = 'sr-cont-retry'
	`, uuid.NewString())
	contExec(`UPDATE managed_runtime_support_replacements SET phase = 'rolled_back', rollback_reason = 'cleanup before recreate test' WHERE tenant_id = 'tenant-sr-cont' AND replacement_id = 'sr-cont-retry'`)

	// BUG-3: Because ordinal 2 was pre-cutover rolled_back, recreating Basic Cube S at ordinal 3 must SUCCEED.
	gen3Basic := seedGen("lease-cont-3-basic", "srv-cont-3-basic", "op-cont-3-basic", "ionos-cloudapi-v6", 3, false)

	// Contrast: after a COMPLETED Memory Cube S replacement (cutover_verified_at IS NOT NULL) at ordinal 4,
	// recreating Basic Cube S at ordinal 5 MUST be blocked by continuity guard.
	gen4 := seedGen("lease-cont-4-mem", "srv-cont-4-mem", "op-cont-4-mem", "ionos-cloudapi-v6-memory-s", 4, false)
	contExec(`
		INSERT INTO managed_runtime_support_replacements (
			tenant_id, replacement_id, owner_subject_id, stack_id, runtime_slot_id,
			source_lease_id, source_server_id, source_resource_generation_id, source_generation_ordinal,
			no_charge_confirmed, case_ref, reason, authorized_by, evidence_ref,
			idempotency_key, request_digest, phase
		) VALUES (
			'tenant-sr-cont', 'sr-cont-done', 'owner-cont', 'stack-cont', 'slot-cont-1',
			'lease-cont-3-basic', 'srv-cont-3-basic', $1, 3,
			true, 'CASE-CONT-3', 'completed memory replacement', 'staff@kombify.io', 'ev://cont-3',
			'idem-cont-3', 'sha256:cont3', 'authorized'
		)
	`, gen3Basic)
	contExec(`
		UPDATE managed_runtime_support_replacements
		SET phase = 'accepted', customer_accepted_at = now(), customer_accepted_by = 'owner-cont',
		    destination_lease_id = 'lease-cont-4-mem', destination_server_id = 'srv-cont-4-mem',
		    destination_resource_generation_id = $1, destination_generation_ordinal = 4,
		    destination_operation_id = 'op-cont-4-mem'
		WHERE tenant_id = 'tenant-sr-cont' AND replacement_id = 'sr-cont-done'
	`, gen4)
	contExec(`UPDATE managed_runtime_support_replacements SET phase = 'destination_admitted' WHERE tenant_id = 'tenant-sr-cont' AND replacement_id = 'sr-cont-done'`)
	contExec(`UPDATE managed_runtime_support_replacements SET phase = 'staging' WHERE tenant_id = 'tenant-sr-cont' AND replacement_id = 'sr-cont-done'`)
	contExec(`UPDATE managed_runtime_support_replacements SET phase = 'cutover_in_progress', destination_writes_observed = true WHERE tenant_id = 'tenant-sr-cont' AND replacement_id = 'sr-cont-done'`)
	contExec(`UPDATE managed_runtime_support_replacements SET phase = 'cutover_verified', cutover_verified_at = now() WHERE tenant_id = 'tenant-sr-cont' AND replacement_id = 'sr-cont-done'`)
	contExec(`UPDATE managed_runtime_support_replacements SET phase = 'completed' WHERE tenant_id = 'tenant-sr-cont' AND replacement_id = 'sr-cont-done'`)

	gen5 := uuid.NewString()
	contExec(`
		INSERT INTO servers (id, tenant_id, stack_id, owner_subject_id, provider_ref, name, lifecycle_state, desired_state, connection_state, generation)
		VALUES ('srv-cont-5-basic', 'tenant-sr-cont', 'stack-cont', 'owner-cont', 'ionos', 'srv-cont-5-basic', 'active', 'running', 'connected', 1)
	`)
	contExec(`
		INSERT INTO techstack_vm_leases (id, tenant_id, owner_subject_id, provider_id, server_id, lease_revision, resource_generation_id, desired_state, lease_json)
		VALUES ('lease-cont-5-basic', 'tenant-sr-cont', 'owner-cont', 'ionos', 'srv-cont-5-basic', 1, $1::uuid, 'running',
			jsonb_build_object(
				'id','lease-cont-5-basic',
				'subject',jsonb_build_object('kind','user','id','owner-cont','org_id','tenant-sr-cont'),
				'resource',jsonb_build_object('provider_id','ionos'),
				'metadata',jsonb_build_object('tenant_id','tenant-sr-cont','resource_generation_id',$1::text,'runtime_offering_id','monthly-runtime-standard','provider_package','Basic Cube S')
			)
		)
	`, gen5)
	contExec(`UPDATE servers SET lease_id = 'lease-cont-5-basic' WHERE tenant_id = 'tenant-sr-cont' AND id = 'srv-cont-5-basic'`)
	contExec(`
		INSERT INTO runtime_lease_execution_authorities (tenant_id, lease_id, execution_authority, bound_at)
		VALUES ('tenant-sr-cont', 'lease-cont-5-basic', 'techstack_provider_control', clock_timestamp())
	`)
	contExec(`
		INSERT INTO provider_desired_spec_revisions (tenant_id, lease_id, revision, spec_ref, spec_digest, spec_json, created_at)
		VALUES ('tenant-sr-cont', 'lease-cont-5-basic', 1, 'desired-spec://homelab/leases/lease-cont-5-basic/revisions/1', 'sha256:5555555555555555555555555555555555555555555555555555555555555555', '{}'::jsonb, now())
	`)
	tx5Op, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx5Op.ExecContext(ctx, `SELECT set_config('app.tenant_id', 'tenant-sr-cont', true)`); err != nil {
		_ = tx5Op.Rollback()
		t.Fatal(err)
	}
	if _, err := tx5Op.ExecContext(ctx, `
		INSERT INTO provider_operations (
			tenant_id, operation_id, lease_id, operation, idempotency_key, adapter_id,
			command_digest, command_json, ledger_revision, desired_spec_revision,
			status, phase, head_sequence, head_receipt_digest, requested_at, created_at, updated_at,
			provision_dispatch_mode
		)
		VALUES (
			'tenant-sr-cont', 'op-cont-5-basic', 'lease-cont-5-basic', 'provision', 'idem-op-cont-5-basic', 'ionos-cloudapi-v6',
			'sha256:6666666666666666666666666666666666666666666666666666666666666666',
			jsonb_build_object(
				'schema_version','techstack.provider-control-operation/v1',
				'execution_authority','techstack_provider_control',
				'runtime_server_generation', 1,
				'command', jsonb_build_object('provider_id','ionos','adapter_id','ionos-cloudapi-v6','resource_generation_id',$1::text,'runtime_server_id','srv-cont-5-basic','lease_revision',1),
				'execution_profile', jsonb_build_object(
					'catalog_version','catalog-sr-cont','provider_id','ionos','adapter_id','ionos-cloudapi-v6',
					'credential_mode','managed','runtime_profile_id','ionos-basic-cube-standard','offering_id','monthly-runtime-standard',
					'provision_dispatch_mode','native_idempotency',
					'adapter_manifest_hash','sha256:640fcf4852e807ed28fa571bdad0ff2b7a21d74a4be975f2729ca5c6e3aef8ad'
				)
			),
			1, 1, 'pending', 'requested', 1, 'sha256:7777777777777777777777777777777777777777777777777777777777777777', now(), now(), now(),
			'native_idempotency'
		)
	`, gen5); err != nil {
		_ = tx5Op.Rollback()
		t.Fatal(err)
	}
	if _, err := tx5Op.ExecContext(ctx, `
		INSERT INTO managed_runtime_capacity_reservations (
			tenant_id, owner_subject_id, provider_id, lease_id, resource_generation_id,
			operation_id, admission_idempotency_key, reservation_mode, capacity_limit,
			policy_source, policy_digest, reservation_origin
		) VALUES (
			'tenant-sr-cont', 'owner-cont', 'ionos', 'lease-cont-5-basic', $1::uuid,
			'op-cont-5-basic', 'idem-op-cont-5-basic', 'limited', 5,
			'edge_v2_entitlement+signed_budget:cloud.runtime.credits#managed_servers',
			managed_runtime_capacity_policy_digest(
				'tenant-sr-cont', 'ionos', 'owner_subject', 'owner-cont', 'limited', 5,
				'edge_v2_entitlement+signed_budget:cloud.runtime.credits#managed_servers'
			),
			'native_admission'
		)
	`, gen5); err != nil {
		_ = tx5Op.Rollback()
		t.Fatal(err)
	}
	if err := tx5Op.Commit(); err != nil {
		t.Fatal(err)
	}
	tx5, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx5.Rollback() }()
	_, _ = tx5.ExecContext(ctx, `SELECT set_config('app.tenant_id', 'tenant-sr-cont', true)`)
	if _, err := tx5.ExecContext(ctx, `
		INSERT INTO managed_runtime_server_slot_generations (
			tenant_id, slot_id, provider_id, lease_id, runtime_server_id,
			resource_generation_id, generation_ordinal, provision_operation_id, intent_digest, state, created_at
		) VALUES (
			'tenant-sr-cont', 'slot-cont-1', 'ionos', 'lease-cont-5-basic', 'srv-cont-5-basic', $1::uuid, 5, 'op-cont-5-basic', 'sha256:5555555555555555555555555555555555555555555555555555555555555555', 'active', now()
		)
	`, gen5); err == nil {
		t.Fatal("continuity guard failed to block Basic Cube S recreate after completed Memory Cube S replacement")
	} else if !strings.Contains(err.Error(), "IONOS Memory Cube S execution profile") {
		t.Fatalf("expected Memory Cube S continuity error, got: %v", err)
	}
	_ = tx5.Rollback()

	// P2-6: terminal replacement rows (completed, rolled_back) must reject all mutations.
	if err := execForTenant("tenant-sr-cont", `
		UPDATE managed_runtime_support_replacements
		SET reason = 'mutated after completion'
		WHERE tenant_id = 'tenant-sr-cont' AND replacement_id = 'sr-cont-done'
	`); err == nil {
		t.Fatal("terminal completed replacement row permitted mutation")
	}

	// P2-8: verify LoadApplicationPolicy & narrowed techstack_application grants on PG17.
	policy, err := LoadApplicationPolicy(ctx, admin, false)
	if err != nil {
		t.Fatalf("LoadApplicationPolicy: %v", err)
	}
	var foundSR, foundHandoff bool
	for oid, tableName := range policy.TableNames {
		switch tableName {
		case "managed_runtime_support_replacements":
			foundSR = true
			if !policy.Tables[oid]["SELECT"] || policy.Tables[oid]["INSERT"] || policy.Tables[oid]["UPDATE"] || policy.Tables[oid]["DELETE"] {
				t.Fatalf("unexpected managed_runtime_support_replacements policy privileges: %#v", policy.Tables[oid])
			}
		case "stack_backup_replacement_handoffs":
			foundHandoff = true
			if !policy.Tables[oid]["SELECT"] || !policy.Tables[oid]["INSERT"] || !policy.Tables[oid]["UPDATE"] || policy.Tables[oid]["DELETE"] {
				t.Fatalf("unexpected stack_backup_replacement_handoffs policy privileges: %#v", policy.Tables[oid])
			}
		}
	}
	if !foundSR || !foundHandoff {
		t.Fatalf("expected both support replacement tables in LoadApplicationPolicy (sr=%v handoff=%v)", foundSR, foundHandoff)
	}
	privTx, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = privTx.Rollback() }()
	if _, err := privTx.ExecContext(ctx, `SET LOCAL ROLE techstack_application`); err != nil {
		t.Fatal(err)
	}
	if _, err := privTx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantA); err != nil {
		t.Fatal(err)
	}
	if _, err := privTx.ExecContext(ctx, `SELECT count(*) FROM managed_runtime_support_replacements`); err != nil {
		t.Fatalf("techstack_application SELECT on managed_runtime_support_replacements failed: %v", err)
	}
	if _, err := privTx.ExecContext(ctx, `SAVEPOINT sp_ins_sr`); err != nil {
		t.Fatal(err)
	}
	if _, err := privTx.ExecContext(ctx, `
		INSERT INTO managed_runtime_support_replacements (
			tenant_id, replacement_id, owner_subject_id, stack_id, runtime_slot_id,
			source_lease_id, source_server_id, source_resource_generation_id, source_generation_ordinal,
			no_charge_confirmed, case_ref, reason, authorized_by, evidence_ref,
			idempotency_key, request_digest, phase
		) VALUES ($1, 'sr-unpriv', 'owner-a', 'stack-a', 'slot-u', 'lease-u', 'srv-u', $2, 1, true, 'C', 'r', 'staff', 'e', 'i', 'd', 'authorized')
	`, tenantA, uuid.NewString()); err == nil {
		t.Fatal("techstack_application was able to INSERT into managed_runtime_support_replacements")
	}
	if _, err := privTx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT sp_ins_sr`); err != nil {
		t.Fatal(err)
	}
	if _, err := privTx.ExecContext(ctx, `SAVEPOINT sp_del_handoff`); err != nil {
		t.Fatal(err)
	}
	if _, err := privTx.ExecContext(ctx, `DELETE FROM stack_backup_replacement_handoffs`); err == nil {
		t.Fatal("techstack_application was able to DELETE from stack_backup_replacement_handoffs")
	}
	if _, err := privTx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT sp_del_handoff`); err != nil {
		t.Fatal(err)
	}
	_ = privTx.Rollback()

	// P2-5 & P1-7: verify active_temp_count releases completed replacements once source is decommissioned,
	// and verify provider_operations deletion fence + provider_stale_capacity_recovery_candidates exclusion & refresh.
	p25Tenant := "tenant-sr-p25"
	p25Exec := func(query string, args ...any) {
		t.Helper()
		if err := execForTenant(p25Tenant, query, args...); err != nil {
			t.Fatalf("p25 sql failed: %v", err)
		}
	}
	p25Exec(`
		INSERT INTO techstack_tenants (id, display_name, kind, status) VALUES ('tenant-sr-p25', 'P25 & Fence Tenant', 'saas', 'active') ON CONFLICT DO NOTHING;
		INSERT INTO stacks (id, tenant_id, owner_subject_id, name, status) VALUES ('stack-p25', 'tenant-sr-p25', 'owner-p25', 'P25 Stack', 'active') ON CONFLICT DO NOTHING;
		INSERT INTO managed_runtime_server_slots (tenant_id, stack_id, slot_key, slot_id, owner_subject_id, created_at)
		VALUES ('tenant-sr-p25', 'stack-p25', 'foundation', 'slot-p25', 'owner-p25', now()) ON CONFLICT DO NOTHING;
	`)
	p25SeedBase := func(ownerID, leaseID, srvID, profileID string) string {
		genID := uuid.NewString()
		p25Exec(`
			INSERT INTO servers (id, tenant_id, stack_id, owner_subject_id, provider_ref, name, lifecycle_state, desired_state, connection_state, generation)
			VALUES ($1, 'tenant-sr-p25', 'stack-p25', $2, 'ionos', $1, 'active', 'running', 'connected', 1)
			ON CONFLICT (id) DO NOTHING
		`, srvID, ownerID)
		p25Exec(`
			INSERT INTO techstack_vm_leases (id, tenant_id, owner_subject_id, provider_id, server_id, lease_revision, resource_generation_id, desired_state, lease_json)
			VALUES ($1, 'tenant-sr-p25', $5, 'ionos', $2, 1, $3::uuid, 'running',
				jsonb_build_object(
					'id', $1::text,
					'subject', jsonb_build_object('kind','user','id',$5::text,'org_id','tenant-sr-p25'),
					'resource', jsonb_build_object('provider_id','ionos','region','de/fra/2'),
					'metadata', jsonb_build_object(
						'tenant_id','tenant-sr-p25',
						'stack_id','stack-p25',
						'resource_generation_id', $3::text,
						'runtime_offering_id','monthly-runtime-standard',
						'provider_package', CASE WHEN $4 = 'ionos-cloudapi-v6-memory-s' THEN 'Memory Cube S' ELSE 'Basic Cube S' END,
						'provider_execution_offering_id', CASE WHEN $4 = 'ionos-cloudapi-v6-memory-s' THEN 'ionos-cube-standard-memory' ELSE '' END
					)
				)
			)
			ON CONFLICT (id) DO NOTHING
		`, leaseID, srvID, genID, profileID, ownerID)
		p25Exec(`UPDATE servers SET lease_id = $2 WHERE tenant_id = 'tenant-sr-p25' AND id = $1`, srvID, leaseID)
		p25Exec(`
			INSERT INTO runtime_lease_execution_authorities (tenant_id, lease_id, execution_authority, bound_at)
			VALUES ('tenant-sr-p25', $1, 'techstack_provider_control', clock_timestamp())
			ON CONFLICT DO NOTHING
		`, leaseID)
		p25Exec(`
			INSERT INTO provider_desired_spec_revisions (tenant_id, lease_id, revision, spec_ref, spec_digest, spec_json, created_at)
			VALUES ('tenant-sr-p25', $1, 1, 'desired-spec://homelab/leases/'||$1||'/revisions/1', 'sha256:9999999999999999999999999999999999999999999999999999999999999999', '{}'::jsonb, now())
			ON CONFLICT DO NOTHING
		`, leaseID)
		return genID
	}
	p25TryReserveOp := func(ownerID string, capLimit int, leaseID, srvID, opID, profileID, genID, reserveReason string) error {
		manifestHash := "sha256:640fcf4852e807ed28fa571bdad0ff2b7a21d74a4be975f2729ca5c6e3aef8ad"
		runtimeProfileID := "ionos-basic-cube-standard"
		offeringID := "monthly-runtime-standard"
		if profileID == "ionos-cloudapi-v6-memory-s" {
			manifestHash = "sha256:6167cd0c480813ea24c3730bde830958fe0ff2042f572a4e89ba53606d4e5fed"
			runtimeProfileID = "ionos-memory-cube-s-standard"
			offeringID = "ionos-cube-standard-memory"
		}
		policySource := "edge_v2_entitlement+signed_budget:cloud.runtime.credits#managed_servers"
		if reserveReason == "support_replacement_temporary" {
			policySource = "support_replacement:ionos_memory_cube_s/v1"
			capLimit = 1
		}
		tx, err := admin.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, p25Tenant); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO provider_operations (
				tenant_id, operation_id, lease_id, operation, idempotency_key, adapter_id,
				command_digest, command_json, ledger_revision, desired_spec_revision,
				status, phase, head_sequence, head_receipt_digest, requested_at, created_at, updated_at,
				provision_dispatch_mode
			) VALUES (
				'tenant-sr-p25', $1, $2, 'provision', 'idem-'||$1, $3,
				'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
				jsonb_build_object(
					'schema_version','techstack.provider-control-operation/v1',
					'execution_authority','techstack_provider_control',
					'runtime_server_generation', 1,
					'command', jsonb_build_object('provider_id','ionos','adapter_id',$3::text,'resource_generation_id',$4::text,'runtime_server_id',$5::text,'lease_revision',1),
					'execution_profile', jsonb_build_object(
						'catalog_version','catalog-sr-cont','provider_id','ionos','adapter_id',$3::text,
						'credential_mode','managed','runtime_profile_id',$6::text,'offering_id',$8::text,
						'provision_dispatch_mode','native_idempotency',
						'adapter_manifest_hash',$7::text
					)
				),
				1, 1, 'pending', 'requested', 1, 'sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', now(), now(), now(),
				'native_idempotency'
			)
		`, opID, leaseID, profileID, genID, srvID, runtimeProfileID, manifestHash, offeringID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO managed_runtime_capacity_reservations (
				tenant_id, owner_subject_id, provider_id, lease_id, resource_generation_id,
				operation_id, admission_idempotency_key, reservation_mode, capacity_limit,
				policy_source, policy_digest, reservation_origin
			) VALUES (
				'tenant-sr-p25', $4, 'ionos', $2, $3::uuid,
				$1, 'idem-'||$1, 'limited', $5,
				$7,
				managed_runtime_capacity_policy_digest(
					'tenant-sr-p25', 'ionos', 'owner_subject', $4, 'limited', $5,
					$7
				),
				$6
			)
		`, opID, leaseID, genID, ownerID, capLimit, reserveReason, policySource); err != nil {
			return err
		}
		return tx.Commit()
	}

	p25SrcGen := p25SeedBase("owner-p25", "lease-p25-src", "srv-p25-src", "ionos-cloudapi-v6")
	if err := p25TryReserveOp("owner-p25", 1, "lease-p25-src", "srv-p25-src", "op-p25-src", "ionos-cloudapi-v6", p25SrcGen, "native_admission"); err != nil {
		t.Fatalf("p25 reserve src: %v", err)
	}
	p25Dest1Gen := p25SeedBase("owner-p25", "lease-p25-dest1", "srv-p25-dest1", "ionos-cloudapi-v6-memory-s")
	p25Exec(`
		INSERT INTO managed_runtime_support_replacements (
			tenant_id, replacement_id, owner_subject_id, stack_id, runtime_slot_id,
			source_lease_id, source_server_id, source_resource_generation_id, source_generation_ordinal,
			no_charge_confirmed, case_ref, reason, authorized_by, evidence_ref,
			idempotency_key, request_digest, phase
		) VALUES (
			'tenant-sr-p25', 'sr-p25-1', 'owner-p25', 'stack-p25', 'slot-p25',
			'lease-p25-src', 'srv-p25-src', $1, 1,
			true, 'CASE-P25-1', 'p25 completion temp count', 'staff@kombify.io', 'ev://p25-1',
			'idem-p25-1', 'sha256:p25-1', 'authorized'
		)
	`, p25SrcGen)
	p25Exec(`
		UPDATE managed_runtime_support_replacements
		SET phase = 'accepted', customer_accepted_at = now(), customer_accepted_by = 'owner-p25',
		    destination_lease_id = 'lease-p25-dest1', destination_server_id = 'srv-p25-dest1',
		    destination_resource_generation_id = $1, destination_generation_ordinal = 2,
		    destination_operation_id = 'op-p25-dest1'
		WHERE tenant_id = 'tenant-sr-p25' AND replacement_id = 'sr-p25-1'
	`, p25Dest1Gen)
	if err := p25TryReserveOp("owner-p25", 1, "lease-p25-dest1", "srv-p25-dest1", "op-p25-dest1", "ionos-cloudapi-v6-memory-s", p25Dest1Gen, "support_replacement_temporary"); err != nil {
		t.Fatalf("p25 reserve dest1: %v", err)
	}
	p25Exec(`UPDATE managed_runtime_support_replacements SET phase = 'destination_admitted' WHERE tenant_id = 'tenant-sr-p25' AND replacement_id = 'sr-p25-1'`)
	p25Exec(`UPDATE managed_runtime_support_replacements SET phase = 'staging' WHERE tenant_id = 'tenant-sr-p25' AND replacement_id = 'sr-p25-1'`)
	p25Exec(`UPDATE managed_runtime_support_replacements SET phase = 'cutover_in_progress', destination_writes_observed = true WHERE tenant_id = 'tenant-sr-p25' AND replacement_id = 'sr-p25-1'`)
	p25Exec(`UPDATE managed_runtime_support_replacements SET phase = 'cutover_verified', cutover_verified_at = now() WHERE tenant_id = 'tenant-sr-p25' AND replacement_id = 'sr-p25-1'`)
	p25Exec(`UPDATE managed_runtime_support_replacements SET phase = 'completed' WHERE tenant_id = 'tenant-sr-p25' AND replacement_id = 'sr-p25-1'`)

	// Before source is decommissioned, second support_replacement_temporary reservation MUST be rejected (active_temp_count = 1).
	p25Exec(`
		INSERT INTO stacks (id, tenant_id, owner_subject_id, name, status) VALUES ('stack-p25-2', 'tenant-sr-p25', 'owner-p25', 'P25 Stack 2', 'active') ON CONFLICT DO NOTHING;
		INSERT INTO managed_runtime_server_slots (tenant_id, stack_id, slot_key, slot_id, owner_subject_id, created_at)
		VALUES ('tenant-sr-p25', 'stack-p25-2', 'foundation', 'slot-p25-2', 'owner-p25', now()) ON CONFLICT DO NOTHING;
	`)
	p25Src2Gen := p25SeedBase("owner-p25", "lease-p25-src2", "srv-p25-src2", "ionos-cloudapi-v6")
	p25Dest2Gen := p25SeedBase("owner-p25", "lease-p25-dest2", "srv-p25-dest2", "ionos-cloudapi-v6-memory-s")
	p25Exec(`
		INSERT INTO managed_runtime_support_replacements (
			tenant_id, replacement_id, owner_subject_id, stack_id, runtime_slot_id,
			source_lease_id, source_server_id, source_resource_generation_id, source_generation_ordinal,
			no_charge_confirmed, case_ref, reason, authorized_by, evidence_ref,
			idempotency_key, request_digest, phase
		) VALUES (
			'tenant-sr-p25', 'sr-p25-2', 'owner-p25', 'stack-p25-2', 'slot-p25-2',
			'lease-p25-src2', 'srv-p25-src2', $1, 1,
			true, 'CASE-P25-2', 'p25 second temp count', 'staff@kombify.io', 'ev://p25-2',
			'idem-p25-2', 'sha256:p25-2', 'authorized'
		)
	`, p25Src2Gen)
	p25Exec(`
		UPDATE managed_runtime_support_replacements
		SET phase = 'accepted', customer_accepted_at = now(), customer_accepted_by = 'owner-p25',
		    destination_lease_id = 'lease-p25-dest2', destination_server_id = 'srv-p25-dest2',
		    destination_resource_generation_id = $1, destination_generation_ordinal = 2,
		    destination_operation_id = 'op-p25-dest2'
		WHERE tenant_id = 'tenant-sr-p25' AND replacement_id = 'sr-p25-2'
	`, p25Dest2Gen)
	if err := p25TryReserveOp("owner-p25", 1, "lease-p25-dest2", "srv-p25-dest2", "op-p25-dest2", "ionos-cloudapi-v6-memory-s", p25Dest2Gen, "support_replacement_temporary"); err == nil {
		t.Fatal("second support_replacement_temporary reservation allowed while completed replacement source was not decommissioned")
	}

	// Mark source decommissioned & absent + record release fact -> active_temp_count releases sr-p25-1 -> second temporary reservation succeeds.
	relRcpt1 := "sha256:7777777777777777777777777777777777777777777777777777777777777777"
	relRcpt2 := "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	p25Exec(`UPDATE servers SET desired_state = 'absent', lifecycle_state = 'decommissioned', decommissioned_at = now() WHERE id = 'srv-p25-src'`)
	p25Exec(`
		INSERT INTO provider_operations (
			tenant_id, operation_id, lease_id, operation, idempotency_key, adapter_id,
			command_digest, command_json, ledger_revision, desired_spec_revision,
			status, phase, head_sequence, head_receipt_digest, requested_at, created_at, updated_at,
			provision_dispatch_mode
		)
		SELECT
			'tenant-sr-p25', 'op-p25-src-decomm', 'lease-p25-src', 'decommission', 'idem-op-p25-src-decomm', p.adapter_id,
			'sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc',
			p.command_json, 1, NULL, 'succeeded', 'absent', 2, $1, now(), now(), now(), p.provision_dispatch_mode
		FROM provider_operations AS p
		WHERE p.tenant_id = 'tenant-sr-p25' AND p.operation_id = 'op-p25-src'
	`, relRcpt2)
	p25Exec(`
		INSERT INTO provider_operation_receipts (
			tenant_id, operation_id, sequence, previous_receipt_digest, receipt_digest, status, phase, receipt_json, issued_at
		) VALUES
			('tenant-sr-p25', 'op-p25-src-decomm', 1, NULL, $1, 'pending', 'requested', '{}'::jsonb, clock_timestamp()),
			('tenant-sr-p25', 'op-p25-src-decomm', 2, $1, $2, 'succeeded', 'absent', '{}'::jsonb, clock_timestamp())
	`, relRcpt1, relRcpt2)
	p25Exec(`
		INSERT INTO managed_runtime_capacity_release_facts (
			tenant_id, lease_id, resource_generation_id, server_id, server_generation,
			release_operation_id, release_authority, receipt_sequence, receipt_digest, released_at
		) VALUES (
			'tenant-sr-p25', 'lease-p25-src', $1::uuid, 'srv-p25-src', 1,
			'op-p25-src-decomm', 'provider_absence', 2, $2, clock_timestamp()
		)
	`, p25SrcGen, relRcpt2)
	if err := p25TryReserveOp("owner-p25", 1, "lease-p25-dest2", "srv-p25-dest2", "op-p25-dest2", "ionos-cloudapi-v6-memory-s", p25Dest2Gen, "support_replacement_temporary"); err != nil {
		t.Fatalf("second support_replacement_temporary reservation failed after source decommission: %v", err)
	}

	// P1-7: fenced_conflict blocks provider_operations decommission INSERT AND excludes lease from stale recovery until reconciled.
	p25Exec(`
		INSERT INTO stacks (id, tenant_id, owner_subject_id, name, status) VALUES ('stack-fence', 'tenant-sr-p25', 'owner-fence', 'Fence Stack', 'active') ON CONFLICT DO NOTHING;
		INSERT INTO managed_runtime_server_slots (tenant_id, stack_id, slot_key, slot_id, owner_subject_id, created_at)
		VALUES ('tenant-sr-p25', 'stack-fence', 'foundation', 'slot-fence', 'owner-fence', now()) ON CONFLICT DO NOTHING;
	`)
	p25FenceSrcGen := p25SeedBase("owner-fence", "lease-fence-src", "srv-fence-src", "ionos-cloudapi-v6")
	if err := p25TryReserveOp("owner-fence", 5, "lease-fence-src", "srv-fence-src", "op-fence-src", "ionos-cloudapi-v6", p25FenceSrcGen, "native_admission"); err != nil {
		t.Fatalf("p25 reserve fence src: %v", err)
	}
	p25FenceDestGen := p25SeedBase("owner-fence", "lease-fence-dest", "srv-fence-dest", "ionos-cloudapi-v6-memory-s")
	p25Exec(`
		INSERT INTO managed_runtime_support_replacements (
			tenant_id, replacement_id, owner_subject_id, stack_id, runtime_slot_id,
			source_lease_id, source_server_id, source_resource_generation_id, source_generation_ordinal,
			no_charge_confirmed, case_ref, reason, authorized_by, evidence_ref,
			idempotency_key, request_digest, phase
		) VALUES (
			'tenant-sr-p25', 'sr-fence', 'owner-fence', 'stack-fence', 'slot-fence',
			'lease-fence-src', 'srv-fence-src', $1, 1,
			true, 'CASE-FENCE', 'fenced conflict trigger guard', 'staff@kombify.io', 'ev://fence',
			'idem-fence', 'sha256:fence', 'authorized'
		)
	`, p25FenceSrcGen)
	p25Exec(`
		UPDATE managed_runtime_support_replacements
		SET phase = 'accepted', customer_accepted_at = now(), customer_accepted_by = 'owner-fence',
		    destination_lease_id = 'lease-fence-dest', destination_server_id = 'srv-fence-dest',
		    destination_resource_generation_id = $1, destination_generation_ordinal = 2,
		    destination_operation_id = 'op-fence-dest'
		WHERE tenant_id = 'tenant-sr-p25' AND replacement_id = 'sr-fence'
	`, p25FenceDestGen)
	if err := p25TryReserveOp("owner-fence", 1, "lease-fence-dest", "srv-fence-dest", "op-fence-dest", "ionos-cloudapi-v6-memory-s", p25FenceDestGen, "support_replacement_temporary"); err != nil {
		t.Fatalf("p25 reserve fence dest: %v", err)
	}
	p25Exec(`UPDATE managed_runtime_support_replacements SET phase = 'destination_admitted' WHERE tenant_id = 'tenant-sr-p25' AND replacement_id = 'sr-fence'`)
	p25Exec(`UPDATE managed_runtime_support_replacements SET phase = 'staging' WHERE tenant_id = 'tenant-sr-p25' AND replacement_id = 'sr-fence'`)
	p25Exec(`UPDATE managed_runtime_support_replacements SET phase = 'fenced_conflict', fenced_conflict_reason = 'ack lost' WHERE tenant_id = 'tenant-sr-p25' AND replacement_id = 'sr-fence'`)
	if err := execForTenant(p25Tenant, `
		INSERT INTO provider_operations (
			tenant_id, operation_id, lease_id, operation, idempotency_key, adapter_id,
			command_digest, command_json, ledger_revision, desired_spec_revision,
			status, phase, head_sequence, head_receipt_digest, requested_at, created_at, updated_at,
			provision_dispatch_mode
		) VALUES (
			'tenant-sr-p25', 'op-fence-decomm-blocked', 'lease-fence-src', 'decommission', 'idem-fence-decomm', 'ionos-cloudapi-v6',
			'sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee',
			jsonb_build_object(
				'schema_version','techstack.provider-control-operation/v1',
				'execution_authority','techstack_provider_control',
				'runtime_server_generation', 1,
				'command', jsonb_build_object('provider_id','ionos','adapter_id','ionos-cloudapi-v6','resource_generation_id',$1::text,'runtime_server_id','srv-fence-src','lease_revision',1)
			),
			2, 1, 'pending', 'requested', 1, 'sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff', now(), now(), now(),
			'native_idempotency'
		)
	`, p25FenceSrcGen); err == nil {
		t.Fatal("provider_operations decommission INSERT was allowed while lease was in fenced_conflict")
	}

	// Mark fenced source server absent and verify provider_stale_capacity_recovery_candidates excludes it while fenced,
	// then reconcile to rolled_back and verify trigger refreshes the directory to include it.
	p25Exec(`UPDATE servers SET desired_state = 'absent', lifecycle_state = 'decommissioned', decommissioned_at = now() WHERE id = 'srv-fence-src'`)
	var staleCount int
	if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM provider_stale_capacity_recovery_candidates WHERE tenant_id = 'tenant-sr-p25' AND lease_id = 'lease-fence-src'`).Scan(&staleCount); err != nil {
		t.Fatalf("query stale recovery candidates while fenced: %v", err)
	}
	if staleCount != 0 {
		t.Fatalf("expected fenced lease excluded from provider_stale_capacity_recovery_candidates, got count=%d", staleCount)
	}
	p25Exec(`UPDATE managed_runtime_support_replacements SET phase = 'rolled_back', rollback_reason = 'verified zero destination writes' WHERE tenant_id = 'tenant-sr-p25' AND replacement_id = 'sr-fence'`)
	if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM provider_stale_capacity_recovery_candidates WHERE tenant_id = 'tenant-sr-p25' AND lease_id = 'lease-fence-src'`).Scan(&staleCount); err != nil {
		t.Fatalf("query stale recovery candidates after rolled_back reconcile: %v", err)
	}
	if staleCount != 1 {
		t.Fatalf("expected reconciled rolled_back lease in provider_stale_capacity_recovery_candidates, got count=%d", staleCount)
	}

	// P1-8 & P2-8: real PG17 custody store, BackupDelivery (AdmitRecipient / Seal / AdmitReplacementRecipient / SealReplacement /
	// CommitReplacementCustody / DiscardReplacementCustody), stack_backup_replacement_handoff_enforce trigger, and originca.PostgresStore.
	custTenant := "tenant-sr-cust"
	custStack := "stack-sr-cust"
	custSrcGen := uuid.NewString()
	custDest1Gen := uuid.NewString()
	custDest2Gen := uuid.NewString()
	custExec := func(query string, args ...any) {
		t.Helper()
		if err := execForTenant(custTenant, query, args...); err != nil {
			t.Fatalf("cust sql failed: %v", err)
		}
	}
	custExec(`
		INSERT INTO techstack_tenants (id, display_name, kind, status) VALUES ('tenant-sr-cust', 'Custody Tenant', 'saas', 'active') ON CONFLICT DO NOTHING;
		INSERT INTO stacks (id, tenant_id, owner_subject_id, name, status) VALUES ('stack-sr-cust', 'tenant-sr-cust', 'owner-cust', 'Custody Stack', 'active') ON CONFLICT DO NOTHING;
	`)
	seedCustReplacement := func(replID, destLease, destSrv, destGen, idemp string) {
		custExec(`
			INSERT INTO managed_runtime_support_replacements (
				tenant_id, replacement_id, owner_subject_id, stack_id, runtime_slot_id,
				source_lease_id, source_server_id, source_resource_generation_id, source_generation_ordinal,
				no_charge_confirmed, case_ref, reason, authorized_by, evidence_ref,
				idempotency_key, request_digest, phase
			) VALUES (
				'tenant-sr-cust', $1, 'owner-cust', 'stack-sr-cust', 'slot-cust',
				'lease-cust-src', 'srv-cust-src', $2, 1,
				true, 'CASE-'||$1, 'custody integration test', 'staff@kombify.io', 'ev://'||$1,
				$3, 'sha256:'||$1, 'authorized'
			)
		`, replID, custSrcGen, idemp)
		custExec(`
			UPDATE managed_runtime_support_replacements
			SET phase = 'accepted', customer_accepted_at = now(), customer_accepted_by = 'owner-cust',
			    destination_lease_id = $2, destination_server_id = $3,
			    destination_resource_generation_id = $4, destination_generation_ordinal = 2,
			    destination_operation_id = 'op-'||$1
			WHERE tenant_id = 'tenant-sr-cust' AND replacement_id = $1
		`, replID, destLease, destSrv, destGen)
		custExec(`UPDATE managed_runtime_support_replacements SET phase = 'destination_admitted' WHERE tenant_id = 'tenant-sr-cust' AND replacement_id = $1`, replID)
		custExec(`UPDATE managed_runtime_support_replacements SET phase = 'staging' WHERE tenant_id = 'tenant-sr-cust' AND replacement_id = $1`, replID)
	}
	seedCustReplacement("sr-cust-rb", "lease-cust-dest-1", "srv-cust-dest-1", custDest1Gen, "idem-cust-rb")

	encKey := make([]byte, 32)
	if _, err := rand.Read(encKey); err != nil {
		t.Fatal(err)
	}
	enc, err := auth.NewSecretEncryptor(encKey)
	if err != nil {
		t.Fatal(err)
	}
	custodyStore, err := backupstore.NewPostgresCustodyStore(admin, enc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := custodyStore.Upsert(ctx, backupstore.Credentials{
		TenantID:           custTenant,
		StackID:            custStack,
		Bucket:             "skbk-" + custStack,
		Endpoint:           "https://acct.r2.cloudflarestorage.com",
		TokenID:            "token-cust-1",
		AccessKeyID:        "access-cust-1",
		SecretAccessKey:    "SECRET-ACCESS-CUST-1",
		RepositoryPassword: "REPO-PASSPHRASE-CUST-1",
	}); err != nil {
		t.Fatalf("custodyStore.Upsert: %v", err)
	}
	custodian, err := backupstore.NewCustodian(noopBackupProvisioner{}, custodyStore)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := managedstackkit.NewBackupDelivery(custodian, custodyStore)
	if err != nil {
		t.Fatal(err)
	}
	makeNodeStmt := func(challenge string) (ed25519.PrivateKey, *ecdh.PrivateKey, []byte) {
		_, ownerPriv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		recipPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		pub := recipPriv.PublicKey().Bytes()
		stmt := hostdelivery.Statement{
			APIVersion:   hostdelivery.StatementAPIVersion,
			SiteRef:      "cloud",
			NodeRef:      "cloud-main",
			RecipientKey: base64.RawStdEncoding.EncodeToString(pub),
			Fingerprint:  hostdelivery.Fingerprint(pub),
			Generation:   1,
			Challenge:    challenge,
			IssuedAt:     time.Now().UTC().Format(time.RFC3339Nano),
			OwnerKeyID:   "owner-1",
			OwnerPublic:  base64.RawStdEncoding.EncodeToString(ownerPriv.Public().(ed25519.PublicKey)),
		}
		payload, err := stmt.SigningBytes()
		if err != nil {
			t.Fatal(err)
		}
		stmt.Signature = hostdelivery.Signature{
			OwnerRef: "owner/local/1",
			KeyID:    "owner-1",
			Value:    base64.RawStdEncoding.EncodeToString(ed25519.Sign(ownerPriv, hostdelivery.StatementDigest(payload))),
		}
		encoded, err := json.Marshal(stmt)
		if err != nil {
			t.Fatal(err)
		}
		return ownerPriv, recipPriv, encoded
	}
	delScope := managedstackkit.DeliveryScope{TenantID: custTenant, StackID: custStack, OwnerID: "owner-cust"}
	chSrc, err := delivery.NewChallenge()
	if err != nil {
		t.Fatal(err)
	}
	_, _, stmtSrc := makeNodeStmt(chSrc)
	srcRecip, err := delivery.AdmitRecipient(ctx, delScope, chSrc, stmtSrc)
	if err != nil {
		t.Fatalf("AdmitRecipient src: %v", err)
	}
	// Sealing itself attests the live backup target over S3; this PG17 test
	// asserts the persisted pins and the fail-closed substitution checks that
	// run before any network attestation.
	if pin, pinned, err := custodyStore.Recipient(ctx, custTenant, custStack); err != nil || !pinned || pin.Fingerprint != srcRecip.Fingerprint {
		t.Fatalf("source recipient pin = (%+v, %v, %v), want fingerprint %q", pin, pinned, err, srcRecip.Fingerprint)
	}

	rbScope := managedstackkit.ReplacementScope{
		TenantID:                      custTenant,
		StackID:                       custStack,
		OwnerID:                       "owner-cust",
		ReplacementID:                 "sr-cust-rb",
		DestinationLeaseID:            "lease-cust-dest-1",
		DestinationServerID:           "srv-cust-dest-1",
		DestinationResourceGeneration: custDest1Gen,
	}
	chDest1, err := delivery.NewChallenge()
	if err != nil {
		t.Fatal(err)
	}
	_, _, stmtDest1 := makeNodeStmt(chDest1)
	dest1Recip, err := delivery.AdmitReplacementRecipient(ctx, rbScope, chDest1, stmtDest1)
	if err != nil {
		t.Fatalf("AdmitReplacementRecipient dest1: %v", err)
	}
	// A different node (new owner and recipient keys) for the same
	// replacement_id is refused and leaves the staged pin untouched.
	chBad, _ := delivery.NewChallenge()
	_, _, stmtBad := makeNodeStmt(chBad)
	if _, err := delivery.AdmitReplacementRecipient(ctx, rbScope, chBad, stmtBad); err == nil {
		t.Fatal("AdmitReplacementRecipient with a different node's keys succeeded for an already staged replacement")
	}
	if staged, ok, err := custodyStore.ReplacementRecipient(ctx, custTenant, custStack, "sr-cust-rb"); err != nil || !ok || staged.Fingerprint != dest1Recip.Fingerprint {
		t.Fatalf("staged replacement pin after refused substitution = (%+v, %v, %v), want dest1 fingerprint", staged, ok, err)
	}
	// Normal Seal MUST reject dest1 recipient; SealReplacement MUST succeed.
	if _, err := delivery.Seal(ctx, delScope, dest1Recip, hostdelivery.OperationBackupTargetImport); !errors.Is(err, backupstore.ErrRecipientSubstituted) {
		t.Fatalf("Seal with dest1 recipient err = %v, want ErrRecipientSubstituted", err)
	}
	if _, err := delivery.SealReplacement(ctx, rbScope, srcRecip, hostdelivery.OperationBackupTargetImport); !errors.Is(err, backupstore.ErrRecipientSubstituted) {
		t.Fatalf("SealReplacement with source recipient err = %v, want ErrRecipientSubstituted", err)
	}
	if staged, ok, err := custodyStore.ReplacementRecipient(ctx, custTenant, custStack, "sr-cust-rb"); err != nil || !ok ||
		staged.Fingerprint != dest1Recip.Fingerprint || staged.DestinationLeaseID != "lease-cust-dest-1" {
		t.Fatalf("staged replacement pin = (%+v, %v, %v), want dest1 fingerprint on lease-cust-dest-1", staged, ok, err)
	}
	if pin, _, err := custodyStore.Recipient(ctx, custTenant, custStack); err != nil || pin.Fingerprint != srcRecip.Fingerprint {
		t.Fatalf("source pin disturbed by staged replacement: (%+v, %v)", pin, err)
	}
	if err := delivery.RecordReplacementNodeBinding(ctx, rbScope, backupstore.NodeBinding{
		BackupTargetRef: "backup-target://sha256/" + strings.Repeat("1", 64), CustodyAttestationRef: "backup-custody-attestation://sha256/" + strings.Repeat("2", 64), RecordedAt: time.Now(),
	}); err != nil {
		t.Fatalf("RecordReplacementNodeBinding rb: %v", err)
	}
	// Move sr-cust-rb into fenced_conflict with destination_writes_observed=true: handoff rolled_back MUST fail while fenced!
	custExec(`UPDATE managed_runtime_support_replacements SET phase = 'cutover_in_progress', destination_writes_observed = true WHERE tenant_id = 'tenant-sr-cust' AND replacement_id = 'sr-cust-rb'`)
	custExec(`UPDATE managed_runtime_support_replacements SET phase = 'fenced_conflict', fenced_conflict_reason = 'cutover ack lost' WHERE tenant_id = 'tenant-sr-cust' AND replacement_id = 'sr-cust-rb'`)
	if err := delivery.DiscardReplacementCustody(ctx, custTenant, custStack, "sr-cust-rb"); err == nil {
		t.Fatal("DiscardReplacementCustody succeeded while sr-cust-rb was in fenced_conflict with destination_writes_observed=true")
	}
	// Reconcile sr-cust-rb to rolled_back -> DiscardReplacementCustody now succeeds!
	custExec(`UPDATE managed_runtime_support_replacements SET phase = 'rolled_back', rollback_reason = 'verified reversible' WHERE tenant_id = 'tenant-sr-cust' AND replacement_id = 'sr-cust-rb'`)
	if err := delivery.DiscardReplacementCustody(ctx, custTenant, custStack, "sr-cust-rb"); err != nil {
		t.Fatalf("DiscardReplacementCustody after reconciled rolled_back: %v", err)
	}

	// Test CommitReplacementCustody on sr-cust-commit + immutability afterward.
	seedCustReplacement("sr-cust-commit", "lease-cust-dest-2", "srv-cust-dest-2", custDest2Gen, "idem-cust-commit")
	commitScope := managedstackkit.ReplacementScope{
		TenantID:                      custTenant,
		StackID:                       custStack,
		OwnerID:                       "owner-cust",
		ReplacementID:                 "sr-cust-commit",
		DestinationLeaseID:            "lease-cust-dest-2",
		DestinationServerID:           "srv-cust-dest-2",
		DestinationResourceGeneration: custDest2Gen,
	}
	chDest2, _ := delivery.NewChallenge()
	_, _, stmtDest2 := makeNodeStmt(chDest2)
	dest2Recip, err := delivery.AdmitReplacementRecipient(ctx, commitScope, chDest2, stmtDest2)
	if err != nil {
		t.Fatalf("AdmitReplacementRecipient dest2: %v", err)
	}
	if err := delivery.RecordReplacementNodeBinding(ctx, commitScope, backupstore.NodeBinding{
		BackupTargetRef: "backup-target://sha256/" + strings.Repeat("3", 64), CustodyAttestationRef: "backup-custody-attestation://sha256/" + strings.Repeat("4", 64), RecordedAt: time.Now(),
	}); err != nil {
		t.Fatalf("RecordReplacementNodeBinding commit: %v", err)
	}
	gotBinding, err := delivery.ReplacementNodeBinding(ctx, custTenant, custStack, "sr-cust-commit")
	if err != nil || gotBinding.BackupTargetRef != "backup-target://sha256/"+strings.Repeat("3", 64) {
		t.Fatalf("ReplacementNodeBinding = (%+v, %v)", gotBinding, err)
	}
	// Committing before destination writes are observed is refused.
	if err := delivery.CommitReplacementCustody(ctx, custTenant, custStack, "sr-cust-commit"); err == nil {
		t.Fatal("CommitReplacementCustody succeeded before destination writes were observed")
	}
	custExec(`UPDATE managed_runtime_support_replacements SET phase = 'cutover_in_progress', destination_writes_observed = true WHERE tenant_id = 'tenant-sr-cust' AND replacement_id = 'sr-cust-commit'`)
	if err := delivery.CommitReplacementCustody(ctx, custTenant, custStack, "sr-cust-commit"); err != nil {
		t.Fatalf("CommitReplacementCustody: %v", err)
	}
	if pin, pinned, err := custodyStore.Recipient(ctx, custTenant, custStack); err != nil || !pinned || pin.Fingerprint != dest2Recip.Fingerprint {
		t.Fatalf("recipient pin after CommitReplacementCustody = (%+v, %v, %v), want dest2 fingerprint %q", pin, pinned, err, dest2Recip.Fingerprint)
	}
	if _, err := delivery.Seal(ctx, delScope, srcRecip, hostdelivery.OperationBackupTargetImport); !errors.Is(err, backupstore.ErrRecipientSubstituted) {
		t.Fatalf("Seal with old source recipient after commit err = %v, want ErrRecipientSubstituted", err)
	}
	if err := delivery.DiscardReplacementCustody(ctx, custTenant, custStack, "sr-cust-commit"); !errors.Is(err, backupstore.ErrInvalidCustody) {
		t.Fatalf("DiscardReplacementCustody on committed row err = %v, want ErrInvalidCustody", err)
	}

	// Real PG17 originca.PostgresStore Track + CompleteInstall + Current roundtrip.
	originStore, err := originca.NewPostgresStore(admin)
	if err != nil {
		t.Fatal(err)
	}
	originSvc := originca.NewService(noopOriginAuthority{}, nil, originStore)
	if err := originSvc.Track(ctx, custTenant, custStack, originca.Certificate{
		ID:        "orig-cert-1",
		Hosts:     []string{"cust-app.kombify.me"},
		ExpiresOn: time.Now().Add(90 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("originca Track 1: %v", err)
	}
	if err := originSvc.Track(ctx, custTenant, custStack, originca.Certificate{
		ID:        "orig-cert-2",
		Hosts:     []string{"cust-app.kombify.me"},
		ExpiresOn: time.Now().Add(90 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("originca Track 2: %v", err)
	}
	if err := originSvc.CompleteInstall(ctx, custTenant, custStack, "orig-cert-2", "sha256:"+strings.Repeat("a", 64), time.Now()); err != nil {
		t.Fatalf("originca CompleteInstall: %v", err)
	}
	curCert, foundCert, err := originSvc.Current(ctx, custTenant, custStack)
	if err != nil || !foundCert || curCert.CertificateID != "orig-cert-2" || len(curCert.RetiredIDs) != 0 {
		t.Fatalf("originca Current = (%+v, %v, %v)", curCert, foundCert, err)
	}
}

type noopBackupProvisioner struct{}

func (noopBackupProvisioner) Provision(context.Context, string) (*backupstore.Store, error) {
	return nil, errors.New("unused in pre-seeded custody test")
}

func (noopBackupProvisioner) RevokeToken(context.Context, string) error { return nil }

type noopOriginAuthority struct{}

func (noopOriginAuthority) Issue(context.Context, []string, string) (originca.Certificate, error) {
	return originca.Certificate{}, errors.New("unused")
}

func (noopOriginAuthority) Revoke(context.Context, string) error { return nil }

// neverEnrolledFixture is one managed lease of the migration 208 case: its
// server state decides whether it ever enrolled, reservedAgo how long ago its
// capacity was reserved.
type neverEnrolledFixture struct {
	tenantID, ownerID, leaseID, serverID, stackID, operationID string
	authority, lifecycleState, connectionState                 string
	reservedAgo                                                time.Duration
}

type neverEnrolledCandidateRow struct {
	tenantID, ownerID, leaseID, stackID string
	reservedAt                          time.Time
}

// Sensitive billing boundary (B1): the never-enrolled reaper must discover a
// billed managed VM that never enrolled although the provider-control runtime
// and the migration owner are NOBYPASSRLS, every source table is FORCE RLS and
// the runtime calls the read without app.tenant_id. Before migration 208 the
// SECURITY DEFINER read saw no tenant's rows and returned nothing.
func TestIntegrationMigration208NeverEnrolledRuntimeDirectoryUnderForceRLS(t *testing.T) {
	dsn := forwardNamesDSN(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	target, admin, _ := newForwardBatchDatabase(t, ctx, dsn)
	prepareForwardNamesRoles(t, ctx, admin)
	const directoryMigration = "208_never_enrolled_runtime_directory.sql"
	pinApplyMigrationsBefore(t, ctx, target, "208_")

	const (
		tenantA        = "tenant-ne-a"
		tenantB        = "tenant-ne-b"
		basicManifest  = "sha256:640fcf4852e807ed28fa571bdad0ff2b7a21d74a4be975f2729ca5c6e3aef8ad"
		memoryManifest = "sha256:6167cd0c480813ea24c3730bde830958fe0ff2042f572a4e89ba53606d4e5fed"
	)
	inTenant := func(tenantID, statement string, args ...any) {
		t.Helper()
		pinExecInTenant(t, ctx, admin, tenantID, statement, args...)
	}
	for _, tenantID := range []string{tenantA, tenantB} {
		inTenant(tenantID, `INSERT INTO techstack_tenants (id, display_name, kind, status)
			VALUES ($1, $1, 'saas', 'active') ON CONFLICT DO NOTHING`, tenantID)
	}
	inTenant(tenantA, `INSERT INTO provider_catalog_versions (catalog_version, status) VALUES ('catalog-ne', 'draft')`)
	inTenant(tenantA, `
		INSERT INTO provider_catalog_profiles (
			catalog_version, provider_id, adapter_id, credential_mode,
			runtime_profile_id, offering_id, can_pause, stop_effect,
			can_recreate, capability_snapshot, adapter_manifest_hash,
			provision_dispatch_mode
		) VALUES (
			'catalog-ne', 'ionos', 'ionos-cloudapi-v6', 'managed',
			'ionos-basic-cube-standard', 'monthly-runtime-standard',
			true, 'pause', true, '{}'::jsonb, $1, 'native_idempotency'
		), (
			'catalog-ne', 'ionos', 'ionos-cloudapi-v6-memory-s', 'managed',
			'ionos-memory-cube-s-standard', 'ionos-cube-standard-memory',
			true, 'pause', true, '{}'::jsonb, $2, 'native_idempotency'
		)
	`, basicManifest, memoryManifest)
	inTenant(tenantA, `UPDATE provider_catalog_versions SET status = 'retired', retired_at = clock_timestamp() WHERE status = 'active'`)
	inTenant(tenantA, `UPDATE provider_catalog_versions SET status = 'active', activated_at = clock_timestamp() WHERE catalog_version = 'catalog-ne'`)

	// seedLease writes one native managed lease exactly as native admission
	// does: server, lease, execution authority, desired spec, the initial
	// provision operation and its capacity reservation.
	seedLease := func(lease neverEnrolledFixture) string {
		t.Helper()
		generationID := uuid.NewString()
		inTenant(lease.tenantID, `INSERT INTO stacks (id, tenant_id, owner_subject_id, name, status)
			VALUES ($1, $2, $3, $1, 'active')`, lease.stackID, lease.tenantID, lease.ownerID)
		inTenant(lease.tenantID, `
			INSERT INTO servers (id, tenant_id, stack_id, owner_subject_id, provider_ref, name, lifecycle_state, desired_state, connection_state, generation)
			VALUES ($1, $2, $3, $4, 'ionos', $1, $5, 'running', $6, 1)
		`, lease.serverID, lease.tenantID, lease.stackID, lease.ownerID, lease.lifecycleState, lease.connectionState)
		inTenant(lease.tenantID, `
			INSERT INTO techstack_vm_leases (id, tenant_id, owner_subject_id, provider_id, server_id, lease_revision, resource_generation_id, desired_state, lease_json)
			VALUES ($1, $2, $3, 'ionos', $4, 1, $5::uuid, 'running',
				jsonb_build_object(
					'id', $1::text,
					'subject', jsonb_build_object('kind', 'user', 'id', $3::text, 'org_id', $2::text),
					'resource', jsonb_build_object('provider_id', 'ionos', 'region', 'de/fra/2'),
					'metadata', jsonb_build_object(
						'tenant_id', $2::text,
						'stack_id', $6::text,
						'resource_generation_id', $5::text,
						'runtime_offering_id', 'monthly-runtime-standard',
						'provider_package', 'Basic Cube S'
					)
				)
			)
		`, lease.leaseID, lease.tenantID, lease.ownerID, lease.serverID, generationID, lease.stackID)
		inTenant(lease.tenantID, `UPDATE servers SET lease_id = $3 WHERE tenant_id = $1 AND id = $2`,
			lease.tenantID, lease.serverID, lease.leaseID)
		inTenant(lease.tenantID, `
			INSERT INTO runtime_lease_execution_authorities (tenant_id, lease_id, execution_authority, bound_at)
			VALUES ($1, $2, $3, clock_timestamp())
		`, lease.tenantID, lease.leaseID, lease.authority)
		inTenant(lease.tenantID, `
			INSERT INTO provider_desired_spec_revisions (tenant_id, lease_id, revision, spec_ref, spec_digest, spec_json, created_at)
			VALUES ($1, $2, 1, 'desired-spec://homelab/leases/' || $2 || '/revisions/1', 'sha256:spec-' || $2, '{}'::jsonb, now())
		`, lease.tenantID, lease.leaseID)
		tx := pinBeginInTenant(t, ctx, admin, lease.tenantID)
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO provider_operations (
				tenant_id, operation_id, lease_id, operation, idempotency_key, adapter_id,
				command_digest, command_json, ledger_revision, desired_spec_revision,
				status, phase, head_sequence, head_receipt_digest, requested_at, created_at, updated_at,
				provision_dispatch_mode
			) VALUES (
				$1, $2, $3, 'provision', 'idem-' || $2, 'ionos-cloudapi-v6',
				'sha256:cmd-' || $2,
				jsonb_build_object(
					'schema_version', 'techstack.provider-control-operation/v1',
					'execution_authority', 'techstack_provider_control',
					'runtime_server_generation', 1,
					'command', jsonb_build_object(
						'provider_id', 'ionos',
						'adapter_id', 'ionos-cloudapi-v6',
						'resource_generation_id', $4::text,
						'runtime_server_id', $5::text,
						'lease_revision', 1
					),
					'execution_profile', jsonb_build_object(
						'catalog_version', 'catalog-ne',
						'provider_id', 'ionos',
						'adapter_id', 'ionos-cloudapi-v6',
						'credential_mode', 'managed',
						'runtime_profile_id', 'ionos-basic-cube-standard',
						'offering_id', 'monthly-runtime-standard',
						'provision_dispatch_mode', 'native_idempotency',
						'adapter_manifest_hash', $6::text
					)
				),
				1, 1, 'pending', 'requested', 1, 'sha256:rcpt-' || $2, now(), now(), now(),
				'native_idempotency'
			)
		`, lease.tenantID, lease.operationID, lease.leaseID, generationID, lease.serverID, basicManifest); err != nil {
			t.Fatalf("insert provision operation of %s: %v", lease.leaseID, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO managed_runtime_capacity_reservations (
				tenant_id, owner_subject_id, provider_id, lease_id, resource_generation_id,
				operation_id, admission_idempotency_key, reservation_mode, capacity_limit,
				policy_source, policy_digest, reservation_origin, reserved_at
			) VALUES (
				$1, $2, 'ionos', $3, $4::uuid,
				$5, 'idem-' || $5, 'limited', 5,
				'edge_v2_entitlement+signed_budget:cloud.runtime.credits#managed_servers',
				managed_runtime_capacity_policy_digest(
					$1, 'ionos', 'owner_subject', $2, 'limited', 5,
					'edge_v2_entitlement+signed_budget:cloud.runtime.credits#managed_servers'
				),
				'native_admission',
				clock_timestamp() - make_interval(secs => $6::integer)
			)
		`, lease.tenantID, lease.ownerID, lease.leaseID, generationID, lease.operationID,
			int(lease.reservedAgo/time.Second)); err != nil {
			t.Fatalf("insert capacity reservation of %s: %v", lease.leaseID, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit native admission of %s: %v", lease.leaseID, err)
		}
		return generationID
	}
	fixture := func(tenantID, ownerID, name, authority, lifecycle, connection string, reservedAgo time.Duration) neverEnrolledFixture {
		return neverEnrolledFixture{
			tenantID: tenantID, ownerID: ownerID, leaseID: "lease-ne-" + name, serverID: "srv-ne-" + name,
			stackID: "stack-ne-" + name, operationID: "op-ne-" + name, authority: authority,
			lifecycleState: lifecycle, connectionState: connection, reservedAgo: reservedAgo,
		}
	}

	// Tenant A exists before migration 208, so its directory entries come from
	// the seed: one never-enrolled lease, one enrolled, one held by an active
	// support replacement and one whose stack has a pending job.
	candidateA := fixture(tenantA, "owner-ne-a", "a-candidate", "techstack_provider_control", "provisioning", "pending", 2*time.Hour)
	enrolledA := fixture(tenantA, "owner-ne-a", "a-enrolled", "techstack_provider_control", "active", "connected", 2*time.Hour)
	protectedA := fixture(tenantA, "owner-ne-a", "a-protected", "techstack_provider_control", "provisioning", "pending", 2*time.Hour)
	busyA := fixture(tenantA, "owner-ne-a", "a-busy", "techstack_provider_control", "provisioning", "pending", 2*time.Hour)
	for _, lease := range []neverEnrolledFixture{candidateA, enrolledA, busyA} {
		seedLease(lease)
	}
	protectedGeneration := seedLease(protectedA)
	inTenant(tenantA, `
		INSERT INTO managed_runtime_support_replacements (
			tenant_id, replacement_id, owner_subject_id, stack_id, runtime_slot_id,
			source_lease_id, source_server_id, source_resource_generation_id, source_generation_ordinal,
			no_charge_confirmed, case_ref, reason, authorized_by, evidence_ref,
			idempotency_key, request_digest, phase
		) VALUES (
			$1, 'sr-ne-a', $2, $3, 'slot-ne-a',
			$4, $5, $6, 1,
			true, 'CASE-NE-A', 'Free IONOS Memory Cube S replacement', 'staff@kombify.io', 'ev://case-ne-a',
			'idem-sr-ne-a', 'sha256:ne-a', 'authorized'
		)
	`, tenantA, protectedA.ownerID, protectedA.stackID, protectedA.leaseID, protectedA.serverID, protectedGeneration)
	inTenant(tenantA, `
		UPDATE managed_runtime_support_replacements
		SET phase = 'accepted', customer_accepted_at = now(), customer_accepted_by = $2,
		    destination_lease_id = 'lease-ne-a-protected-dest', destination_server_id = 'srv-ne-a-protected-dest',
		    destination_resource_generation_id = $3, destination_generation_ordinal = 2,
		    destination_operation_id = 'op-ne-a-protected-dest'
		WHERE tenant_id = $1 AND replacement_id = 'sr-ne-a'
	`, tenantA, protectedA.ownerID, uuid.NewString())
	inTenant(tenantA, `UPDATE managed_runtime_support_replacements SET phase = 'destination_admitted' WHERE tenant_id = $1 AND replacement_id = 'sr-ne-a'`, tenantA)
	inTenant(tenantA, `INSERT INTO jobs (id, tenant_id, stack_id, type, state) VALUES ('job-ne-a-busy', $1, $2, 'deploy', 'pending')`,
		tenantA, busyA.stackID)

	pinExec(t, ctx, admin, `
		DO $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'techstack_provider_control_runtime') THEN
				CREATE ROLE techstack_provider_control_runtime NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION;
			END IF;
		END $$`)
	pinExec(t, ctx, admin, `GRANT USAGE ON SCHEMA public TO techstack_provider_control_runtime`)
	pinExec(t, ctx, admin, `GRANT EXECUTE ON FUNCTION provider_control_list_never_enrolled_runtime_candidates(text, text, integer, integer) TO techstack_provider_control_runtime`)

	// listAsRuntime calls the read exactly as NeverEnrolledRecovery does: as
	// the NOBYPASSRLS provider-control runtime role without any tenant scope.
	listAsRuntime := func(afterTenant, afterLease string) []neverEnrolledCandidateRow {
		t.Helper()
		tx, err := admin.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, `SET LOCAL ROLE techstack_provider_control_runtime`); err != nil {
			t.Fatalf("assume techstack_provider_control_runtime: %v", err)
		}
		var scope sql.NullString
		var bypass bool
		if err := tx.QueryRowContext(ctx, `
			SELECT NULLIF(current_setting('app.tenant_id', true), ''), rolsuper OR rolbypassrls
			FROM pg_roles WHERE rolname = current_user
		`).Scan(&scope, &bypass); err != nil {
			t.Fatal(err)
		}
		if scope.Valid || bypass {
			t.Fatalf("runtime read ran with tenant scope %v or bypass %v", scope, bypass)
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT tenant_id, owner_subject_id, lease_id, stack_id, reserved_at
			FROM provider_control_list_never_enrolled_runtime_candidates($1, $2, $3, $4)
		`, afterTenant, afterLease, 300, 101)
		if err != nil {
			t.Fatalf("list never-enrolled candidates as the runtime role: %v", err)
		}
		defer rows.Close()
		var candidates []neverEnrolledCandidateRow
		for rows.Next() {
			var row neverEnrolledCandidateRow
			if err := rows.Scan(&row.tenantID, &row.ownerID, &row.leaseID, &row.stackID, &row.reservedAt); err != nil {
				t.Fatal(err)
			}
			candidates = append(candidates, row)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return candidates
	}
	leaseIDs := func(candidates []neverEnrolledCandidateRow) []string {
		ids := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			ids = append(ids, candidate.tenantID+"/"+candidate.leaseID)
		}
		return ids
	}
	requireCandidates := func(stage string, got []neverEnrolledCandidateRow, want ...neverEnrolledFixture) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: candidates = %v, want %d", stage, leaseIDs(got), len(want))
		}
		horizon := time.Now().Add(-time.Hour)
		for i, lease := range want {
			row := got[i]
			if row.tenantID != lease.tenantID || row.ownerID != lease.ownerID ||
				row.leaseID != lease.leaseID || row.stackID != lease.stackID || !row.reservedAt.Before(horizon) {
				t.Fatalf("%s: candidate %d = %+v, want %s/%s owner %s stack %s reserved before %s",
					stage, i, row, lease.tenantID, lease.leaseID, lease.ownerID, lease.stackID, horizon)
			}
		}
	}

	// The defect: before 208 the FORCE-RLS joins hide every tenant's rows.
	if got := listAsRuntime("", ""); len(got) != 0 {
		t.Fatalf("pre-208 read returned %v; the reproduction no longer holds", leaseIDs(got))
	}

	pinApplyMigration(t, ctx, target, directoryMigration)
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && entry.Name() > directoryMigration {
			pinApplyMigration(t, ctx, target, entry.Name())
		}
	}

	// The seed found tenant A's never-enrolled lease and nothing it excludes.
	requireCandidates("after the seed", listAsRuntime("", ""), candidateA)

	// Tenant B is admitted after 208, so the triggers maintain its entries: a
	// homelab_provider_control lease past its window and one inside it.
	candidateB := fixture(tenantB, "owner-ne-b", "b-candidate", "homelab_provider_control", "provisioning", "pending", 3*time.Hour)
	freshB := fixture(tenantB, "owner-ne-b", "b-fresh", "techstack_provider_control", "provisioning", "pending", 0)
	seedLease(candidateB)
	seedLease(freshB)
	requireCandidates("across tenants", listAsRuntime("", ""), candidateA, candidateB)
	requireCandidates("after the keyset cursor", listAsRuntime(candidateA.tenantID, candidateA.leaseID), candidateB)

	// The runtime holds EXECUTE on the read and nothing on the directories.
	runtimeTx, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtimeTx.ExecContext(ctx, `SET LOCAL ROLE techstack_provider_control_runtime`); err != nil {
		_ = runtimeTx.Rollback()
		t.Fatal(err)
	}
	for i, table := range []string{
		"provider_never_enrolled_runtime_leases",
		"provider_never_enrolled_runtime_server_evidence",
		"provider_never_enrolled_runtime_active_jobs",
	} {
		assertStatementRejected(t, ctx, runtimeTx, fmt.Sprintf("never_enrolled_directory_%d", i), "SELECT count(*) FROM "+table)
	}
	_ = runtimeTx.Rollback()

	// The server and job triggers follow later writes: enrollment removes the
	// lease, the finished job releases its stack, a heartbeat changes nothing.
	inTenant(tenantA, `UPDATE servers SET connection_state = 'connected', last_heartbeat_at = clock_timestamp() WHERE tenant_id = $1 AND id = $2`,
		tenantA, candidateA.serverID)
	inTenant(tenantA, `UPDATE servers SET last_heartbeat_at = clock_timestamp() WHERE tenant_id = $1 AND id = $2`,
		tenantA, candidateA.serverID)
	requireCandidates("after enrollment", listAsRuntime("", ""), candidateB)
	inTenant(tenantA, `UPDATE jobs SET state = 'completed', completed_at = clock_timestamp() WHERE tenant_id = $1 AND id = 'job-ne-a-busy'`, tenantA)
	requireCandidates("after the stack job finished", listAsRuntime("", ""), busyA, candidateB)
}
