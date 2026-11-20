package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Sensitive migration boundary (kombify-StackKits-chw7): migration 189 lets a
// Proxmox customer-substrate provision that ended without any bound resource
// terminalize without a managed-runtime capacity release. Before it, the
// terminalization row's foreign key to managed_runtime_capacity_reservations
// and the deferred commit fence of migration 035 both required commercial
// capacity a substrate guest never holds (migration 107), so the lease could
// never terminalize. The insert validator is the unchanged fence of migrations
// 035, 081 and 098 and is disabled here so each case isolates the commit
// fence; every other trigger and foreign key of the terminalization stays live.
func TestIntegrationSubstrateResourceFreeTerminalization(t *testing.T) {
	dsn := integrationDSN()
	if dsn == "" {
		t.Skip("HOMELAB_TEST_POSTGRES_URL not set; skipping Postgres integration test")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	target, admin, _ := newForwardBatchDatabase(t, ctx, dsn)
	pinApplyMigrationsBefore(t, ctx, target, "189_")

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	tenantID := "substrate-tenant-" + suffix
	pinExec(t, ctx, admin, `INSERT INTO techstack_tenants (id, display_name, kind, status)
		VALUES ($1, $1, 'saas', 'active')`, tenantID)
	pinExec(t, ctx, target.DB, `ALTER TABLE provider_operation_resource_free_terminalizations
		DISABLE TRIGGER provider_operation_resource_free_terminalizations_validate_insert`)

	// The defect: through migration 188 the reservation foreign key refuses
	// the substrate terminalization at insert.
	before := seedResourceFreeSubstrateTeardown(t, ctx, admin, tenantID, "before", substrateTeardownExact)
	err := commitResourceFreeTerminalization(ctx, target.DB, before)
	var refusal *pgconn.PgError
	if !errors.As(err, &refusal) || refusal.Code != "23503" ||
		!strings.Contains(refusal.Detail, "managed_runtime_capacity_reservations") {
		t.Fatalf("pre-189 substrate terminalization error = %v", err)
	}

	pinApplyMigration(t, ctx, target, "189_substrate_resource_free_terminalization.sql")

	exact := seedResourceFreeSubstrateTeardown(t, ctx, admin, tenantID, "exact", substrateTeardownExact)
	if err := commitResourceFreeTerminalization(ctx, target.DB, exact); err != nil {
		t.Fatalf("exact substrate terminalization: %v", err)
	}
	var released bool
	if err := admin.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM managed_runtime_capacity_release_facts WHERE tenant_id = $1 AND lease_id = $2)`,
		tenantID, exact.leaseID).Scan(&released); err != nil || released {
		t.Fatalf("substrate terminalization released capacity = %t, %v", released, err)
	}

	// A commercial lease still commits through its exact release fact.
	commercial := seedResourceFreeSubstrateTeardown(t, ctx, admin, tenantID, "commercial-released", substrateTeardownCommercialReleased)
	if err := commitResourceFreeTerminalization(ctx, target.DB, commercial); err != nil {
		t.Fatalf("released commercial terminalization: %v", err)
	}

	// Commercial leases keep requiring their release fact, and anything that
	// is not exact substrate custody without a reservation fails closed.
	for _, refused := range []struct {
		name  string
		shape substrateTeardownShape
	}{
		{"commercial-without-release", substrateTeardownCommercial},
		{"unclassified-without-reservation", substrateTeardownUnclassified},
		{"foreign-guest-operation", substrateTeardownForeignGuestOperation},
		{"substrate-with-reservation", substrateTeardownReserved},
		{"server-not-tombstoned", substrateTeardownLiveServer},
	} {
		seed := seedResourceFreeSubstrateTeardown(t, ctx, admin, tenantID, refused.name, refused.shape)
		err := commitResourceFreeTerminalization(ctx, target.DB, seed)
		if !errors.As(err, &refusal) || refusal.Code != "55000" ||
			!strings.Contains(refusal.Message, "resource-free terminalization must atomically include") {
			t.Fatalf("%s terminalization error = %v", refused.name, err)
		}
	}

	// The replaced fence keeps the runtime boot posture of a provider-control
	// security boundary.
	var definer, publicExecute bool
	var config string
	if err := admin.QueryRowContext(ctx, `
		SELECT function.prosecdef,
		       pg_catalog.array_to_string(COALESCE(function.proconfig, ARRAY[]::text[]), '|'),
		       EXISTS (
		           SELECT 1 FROM pg_catalog.aclexplode(COALESCE(function.proacl,
		               pg_catalog.acldefault('f', function.proowner))) AS acl
		           WHERE acl.grantee = 0 AND acl.privilege_type = 'EXECUTE'
		       )
		FROM pg_catalog.pg_proc AS function
		WHERE function.oid = 'public.provider_resource_free_terminalization_validate_commit()'::regprocedure
	`).Scan(&definer, &config, &publicExecute); err != nil {
		t.Fatal(err)
	}
	if !definer || publicExecute || config != "search_path=pg_catalog, public, pg_temp" {
		t.Fatalf("commit fence posture: definer=%t public_execute=%t config=%v", definer, publicExecute, config)
	}
}

type substrateTeardownShape int

const (
	substrateTeardownExact substrateTeardownShape = iota
	substrateTeardownUnclassified
	substrateTeardownCommercial
	substrateTeardownCommercialReleased
	substrateTeardownForeignGuestOperation
	substrateTeardownReserved
	substrateTeardownLiveServer
)

type resourceFreeSubstrateTeardown struct {
	tenantID, leaseID, serverID, operationID, generation, receiptDigest string
	serverGeneration                                                    int64
}

// seedResourceFreeSubstrateTeardown writes, with triggers and foreign keys
// suspended as a restore would, the state FinalizeResourceFreeTeardown leaves
// in its transaction right before the commit fence runs: a never-dispatched
// Proxmox provision, its exact guest custody and the RuntimeServer tombstone.
func seedResourceFreeSubstrateTeardown(
	t *testing.T,
	ctx context.Context,
	admin *sql.DB,
	tenantID, name string,
	shape substrateTeardownShape,
) resourceFreeSubstrateTeardown {
	t.Helper()
	key := name + "-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	seed := resourceFreeSubstrateTeardown{
		tenantID: tenantID, leaseID: "lease-" + key, serverID: "server-" + key,
		operationID: "op-" + key, generation: uuid.NewString(),
		receiptDigest: "sha256:" + strings.Repeat("c", 64), serverGeneration: 3,
	}
	substrateServerID, guestID := "substrate-"+key, 9100
	targetRef := substrateServerID + "/9100"
	commercial := shape == substrateTeardownCommercial || shape == substrateTeardownCommercialReleased
	reserved := commercial || shape == substrateTeardownReserved
	custodyClass := "customer_substrate"
	if commercial || shape == substrateTeardownUnclassified {
		custodyClass = ""
	}
	guestOperation := seed.operationID
	if shape == substrateTeardownForeignGuestOperation {
		guestOperation = "op-foreign-" + key
	}
	lifecycle, desired, decommissioned := "decommissioned", "absent", "clock_timestamp()"
	if shape == substrateTeardownLiveServer {
		lifecycle, desired, decommissioned = "decommissioning", "absent", "NULL"
	}

	tx, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := tx.ExecContext(ctx, statement, args...); err != nil {
			t.Fatalf("seed %s: %s: %v", name, strings.Join(strings.Fields(statement), " "), err)
		}
	}
	exec(`SET LOCAL session_replication_role = replica`)
	exec(`INSERT INTO servers (
			id, tenant_id, owner_subject_id, lease_id, name, generation,
			lifecycle_state, desired_state, connection_state, health_state, decommissioned_at,
			environment_class, offering, provider_id, provider_target_ref,
			availability_owner, operations_owner, runtime_target_evidence_ref, runtime_target_observed_at
		) VALUES (
			$1, $2, 'owner-1', $3, $1, $4,
			$5, $6, 'pending', 'unknown', `+decommissioned+`,
			'local', 'substrate_vm', 'proxmox', $7,
			'customer', 'customer', 'runtime-lease:' || $3, clock_timestamp()
		)`, seed.serverID, tenantID, seed.leaseID, seed.serverGeneration, lifecycle, desired, targetRef)
	exec(`INSERT INTO techstack_vm_leases (
			id, tenant_id, subject_id, org_id, provider_id, engine_vm_id,
			desired_state, idempotency_key, lease_json, lease_revision,
			owner_subject_id, server_id, resource_generation_id, cancelled_at,
			valid_from, valid_until, renewed_at, created_at, updated_at
		) VALUES (
			$1, $2, 'owner-1', $2, 'proxmox', $3,
			'absent', $1 || '-idempotency',
			jsonb_strip_nulls(jsonb_build_object(
				'custody_class', NULLIF($4::text, ''),
				'billing_mode', 'local',
				'lifecycle_class', 'one_time',
				'recreate_policy', 'never',
				'resource', jsonb_build_object('provider_id', 'proxmox', 'engine_vm_id', $3::text),
				'metadata', jsonb_build_object(
					'substrate_server_id', $5::text, 'substrate_guest_id', $6::text, 'resource_generation_id', $8::text)
			)),
			1, 'owner-1', $7, $8::uuid, clock_timestamp(),
			clock_timestamp(), clock_timestamp() + interval '1 year',
			clock_timestamp(), clock_timestamp(), clock_timestamp()
		)`, seed.leaseID, tenantID, targetRef, custodyClass, substrateServerID, "9100", seed.serverID, seed.generation)
	exec(`INSERT INTO provider_operations (
			tenant_id, operation_id, lease_id, operation, idempotency_key,
			adapter_id, command_digest, command_json, ledger_revision,
			desired_spec_revision, status, phase, head_sequence,
			head_receipt_digest, requested_at, created_at, updated_at,
			provision_dispatch_mode
		) VALUES (
			$1, $2, $3, 'provision', $2 || '-idempotency', 'proxmox-substrate-v1',
			'sha256:' || repeat('b', 64),
			jsonb_build_object(
				'schema_version', 'techstack.provider-control-operation/v1',
				'execution_authority', 'techstack_provider_control',
				'runtime_server_generation', $4::bigint,
				'execution_profile', jsonb_build_object('provider_id', 'proxmox', 'credential_mode', 'worker_held'),
				'command', jsonb_build_object(
					'provider_id', 'proxmox', 'lease_revision', 1,
					'resource_generation_id', $5::text, 'runtime_server_id', $6::text
				)
			),
			1, 1, 'pending', 'requested', 1, $7,
			clock_timestamp(), clock_timestamp(), clock_timestamp(), 'provider_correlation'
		)`, tenantID, seed.operationID, seed.leaseID, seed.serverGeneration, seed.generation, seed.serverID, seed.receiptDigest)
	exec(`INSERT INTO provider_operation_receipts (
			tenant_id, operation_id, sequence, previous_receipt_digest,
			receipt_digest, status, phase, receipt_json, issued_at
		) VALUES ($1, $2, 1, NULL, $3, 'pending', 'requested', '{}'::jsonb, clock_timestamp())`,
		tenantID, seed.operationID, seed.receiptDigest)
	if !commercial {
		exec(`INSERT INTO substrate_guest_leases (
				tenant_id, lease_id, server_id, substrate_server_id, guest_id, slot_id,
				resource_generation_id, operation_id, request_digest
			) VALUES ($1, $2, $3, $4, $5, $6, $7::uuid, $8, 'sha256:' || repeat('d', 64))`,
			tenantID, seed.leaseID, seed.serverID, substrateServerID, guestID, "slot-"+key, seed.generation, guestOperation)
	}
	if reserved {
		exec(`INSERT INTO managed_runtime_capacity_reservations (
				tenant_id, owner_subject_id, provider_id, lease_id, resource_generation_id,
				reservation_mode, policy_source, policy_digest, reservation_origin
			) VALUES ($1, 'owner-1', 'ionos', $2, $3::uuid, 'quarantine',
				'migration_quarantine:managed-runtime-capacity/v1', 'sha256:' || repeat('e', 64), 'migration_quarantine')`,
			tenantID, seed.leaseID, seed.generation)
	}
	if shape == substrateTeardownCommercialReleased {
		exec(`INSERT INTO managed_runtime_capacity_release_facts (
				tenant_id, lease_id, resource_generation_id, server_id, server_generation,
				release_operation_id, release_authority, receipt_sequence, receipt_digest, released_at
			) VALUES ($1, $2, $3::uuid, $4, $5, $6, 'resource_free_teardown', 1, $7, clock_timestamp())`,
			tenantID, seed.leaseID, seed.generation, seed.serverID, seed.serverGeneration, seed.operationID, seed.receiptDigest)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return seed
}

// commitResourceFreeTerminalization inserts the no-dispatch terminalization as
// the runtime does, in the tenant's context, and commits it through the
// deferred commit fence.
func commitResourceFreeTerminalization(ctx context.Context, database *sql.DB, seed resourceFreeSubstrateTeardown) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, seed.tenantID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_operation_resource_free_terminalizations (
			tenant_id, operation_id, lease_id, lease_revision,
			server_id, server_generation, resource_generation_id,
			head_sequence, head_receipt_digest, authority, terminalized_at
		) VALUES ($1, $2, $3, 1, $4, $5, $6::uuid, 1, $7, 'no_dispatch_custody', clock_timestamp())
	`, seed.tenantID, seed.operationID, seed.leaseID, seed.serverID, seed.serverGeneration,
		seed.generation, seed.receiptDigest); err != nil {
		return err
	}
	return tx.Commit()
}
