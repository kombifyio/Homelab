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
	providerexecutor "github.com/kombifyio/stackkits/contracts/providerexecutor/v1beta1"

	"github.com/kombifyio/homelab/internal/providerevidence"
)

// Sensitive migration boundary (kombify-StackKits-2w8s): migration 190 lets a
// Proxmox customer-substrate provision that FAILED before binding any guest
// terminalize through resource-free teardown, but only on a definitive Proxmox
// absence observation of its exact reserved guest recorded after the failure.
// Before it, the provision's provider_correlation dispatch guard stayed
// unsettled custody forever and the insert validator admitted a failed head
// only with an at-most-once operator decision. The observation goes through the
// real recorder exactly as internal/substrateruntime records an absent guest.
func TestIntegrationSubstrateFailedProvisionGuestAbsence(t *testing.T) {
	dsn := integrationDSN()
	if dsn == "" {
		t.Skip("HOMELAB_TEST_POSTGRES_URL not set; skipping Postgres integration test")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	target, admin, _ := newForwardBatchDatabase(t, ctx, dsn)
	pinApplyMigrationsBefore(t, ctx, target, "190_")

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	tenantID := "substrate-failed-tenant-" + suffix
	pinExec(t, ctx, admin, `INSERT INTO techstack_tenants (id, display_name, kind, status)
		VALUES ($1, $1, 'saas', 'active')`, tenantID)
	recorder, err := providerevidence.NewRecorder(target.DB)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	// The defect: through migration 189 the dispatch guard of a failed
	// substrate provision stays unsettled even after its reserved guest was
	// observed absent, so resource-free teardown can never be admitted.
	before := seedFailedSubstrateProvision(t, ctx, admin, tenantID, "before", failedSubstrateExact)
	recordReservedGuestAbsence(t, ctx, recorder, before, before.operationID, now)
	if count := countUnsettledDispatchGuards(t, ctx, target.DB, before); count != 1 {
		t.Fatalf("pre-190 unsettled dispatch guards = %d, want 1", count)
	}

	pinApplyMigration(t, ctx, target, "190_substrate_failed_provision_guest_absence.sql")

	if count := countUnsettledDispatchGuards(t, ctx, target.DB, before); count != 0 {
		t.Fatalf("proven failed provision unsettled dispatch guards = %d, want 0", count)
	}
	unproven := seedFailedSubstrateProvision(t, ctx, admin, tenantID, "unproven", failedSubstrateExact)
	if count := countUnsettledDispatchGuards(t, ctx, target.DB, unproven); count != 1 {
		t.Fatalf("unproven failed provision unsettled dispatch guards = %d, want 1", count)
	}

	// The exact proven failure terminalizes, tombstones its RuntimeServer
	// in the same transaction and commits through the commit fence without
	// any capacity release.
	exact := seedFailedSubstrateProvision(t, ctx, admin, tenantID, "exact", failedSubstrateExact)
	evidenceRef := recordReservedGuestAbsence(t, ctx, recorder, exact, exact.operationID, now)
	// The insert validator admits it under the runtime's row-level security,
	// in the tenant's scope (rolled back before the commit fence).
	if err := insertFailedSubstrateTerminalization(ctx, target.DB, exact, "substrate_guest_absent", evidenceRef); err != nil {
		t.Fatalf("exact failed substrate terminalization insert: %v", err)
	}
	if err := commitFailedSubstrateTerminalization(ctx, admin, exact, "substrate_guest_absent", evidenceRef); err != nil {
		t.Fatalf("exact failed substrate terminalization: %v", err)
	}
	var storedRef string
	var released bool
	if err := admin.QueryRowContext(ctx, `
		SELECT terminalization.absence_evidence_ref,
		       EXISTS (SELECT 1 FROM managed_runtime_capacity_release_facts AS release
		               WHERE release.tenant_id = terminalization.tenant_id
		                 AND release.lease_id = terminalization.lease_id)
		FROM provider_operation_resource_free_terminalizations AS terminalization
		WHERE terminalization.tenant_id = $1 AND terminalization.operation_id = $2`,
		tenantID, exact.operationID).Scan(&storedRef, &released); err != nil || storedRef != evidenceRef || released {
		t.Fatalf("exact terminalization ref=%q released=%t err=%v", storedRef, released, err)
	}

	// Everything short of that exact proof fails closed at insert. Without a
	// qualifying observation the dispatch guard is still unsettled custody;
	// with one, the authority branch checks the remaining custody.
	const (
		unsettledCustody = "conflicts with unsettled generation-wide provider custody"
		unprovenAbsence  = "substrate guest absence terminalization lacks a definitive absence observation"
		staleHead        = "is stale against its exact provision head"
	)
	refusedCases := []struct {
		name      string
		shape     failedSubstrateShape
		authority string
		message   string
		prove     func(failedSubstrateProvision) string
	}{
		{"no-observation", failedSubstrateExact, "substrate_guest_absent", unsettledCustody, func(seed failedSubstrateProvision) string {
			// An absence of an unrelated guest exists but proves nothing here.
			other := seedFailedSubstrateProvision(t, ctx, admin, tenantID, "no-observation-other", failedSubstrateExact)
			return recordReservedGuestAbsence(t, ctx, recorder, other, other.operationID, now)
		}},
		{"observed-before-failure", failedSubstrateExact, "substrate_guest_absent", unsettledCustody, func(seed failedSubstrateProvision) string {
			return recordReservedGuestAbsence(t, ctx, recorder, seed, seed.operationID, seed.failedAt.Add(-time.Minute))
		}},
		{"other-guest", failedSubstrateExact, "substrate_guest_absent", unsettledCustody, func(seed failedSubstrateProvision) string {
			other := seed
			other.guestRef = seed.substrateServerID + "/9199"
			return recordReservedGuestAbsence(t, ctx, recorder, other, seed.operationID, now)
		}},
		{"foreign-lease-observer", failedSubstrateExact, "substrate_guest_absent", unsettledCustody, func(seed failedSubstrateProvision) string {
			foreign := seedFailedSubstrateProvision(t, ctx, admin, tenantID, "foreign-observer", failedSubstrateExact)
			return recordReservedGuestAbsence(t, ctx, recorder, seed, foreign.operationID, now)
		}},
		{"live-claim", failedSubstrateLiveClaim, "substrate_guest_absent", unprovenAbsence, func(seed failedSubstrateProvision) string {
			return recordReservedGuestAbsence(t, ctx, recorder, seed, seed.operationID, now)
		}},
		{"unclassified-lease", failedSubstrateUnclassified, "substrate_guest_absent", unprovenAbsence, func(seed failedSubstrateProvision) string {
			return recordReservedGuestAbsence(t, ctx, recorder, seed, seed.operationID, now)
		}},
		{"substrate-with-reservation", failedSubstrateReserved, "substrate_guest_absent", unprovenAbsence, func(seed failedSubstrateProvision) string {
			return recordReservedGuestAbsence(t, ctx, recorder, seed, seed.operationID, now)
		}},
		{"pending-head", failedSubstratePending, "substrate_guest_absent", unprovenAbsence, func(seed failedSubstrateProvision) string {
			return recordReservedGuestAbsence(t, ctx, recorder, seed, seed.operationID, now)
		}},
		// Unchanged: a failed head is not admitted without dispatch custody
		// authority, observation or not.
		{"failed-no-dispatch", failedSubstrateExact, "no_dispatch_custody", staleHead, func(seed failedSubstrateProvision) string {
			recordReservedGuestAbsence(t, ctx, recorder, seed, seed.operationID, now)
			return ""
		}},
	}
	for _, refused := range refusedCases {
		seed := seedFailedSubstrateProvision(t, ctx, admin, tenantID, refused.name, refused.shape)
		ref := refused.prove(seed)
		err := insertFailedSubstrateTerminalization(ctx, target.DB, seed, refused.authority, ref)
		var refusal *pgconn.PgError
		if !errors.As(err, &refusal) || refusal.Code != "55000" ||
			!strings.Contains(refusal.Message, refused.message) {
			t.Fatalf("%s terminalization error = %v", refused.name, err)
		}
	}

	// The shape check binds the authority to its evidence reference.
	shape := seedFailedSubstrateProvision(t, ctx, admin, tenantID, "shape", failedSubstrateExact)
	recordReservedGuestAbsence(t, ctx, recorder, shape, shape.operationID, now)
	err = insertFailedSubstrateTerminalization(ctx, target.DB, shape, "substrate_guest_absent", "")
	var refusal *pgconn.PgError
	if !errors.As(err, &refusal) || refusal.Code != "23514" ||
		refusal.ConstraintName != "resource_free_terminalization_authority_shape" {
		t.Fatalf("substrate authority without evidence error = %v", err)
	}

	// Replaced fences keep the runtime boot posture; the shared predicate is no
	// runtime boundary.
	for _, boundary := range []struct {
		signature string
		definer   bool
	}{
		{"public.provider_resource_free_terminalization_validate_insert()", true},
		{"public.provider_resource_free_terminalization_validate_commit()", true},
		{"public.provider_control_count_unsettled_generation_dispatch_guards(text,uuid)", true},
		{"public.substrate_failed_provision_guest_absent(text,text,text)", false},
	} {
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
			WHERE function.oid = $1::regprocedure
		`, boundary.signature).Scan(&definer, &config, &publicExecute); err != nil {
			t.Fatal(err)
		}
		if definer != boundary.definer || publicExecute || config != "search_path=pg_catalog, public, pg_temp" {
			t.Fatalf("%s posture: definer=%t public_execute=%t config=%v",
				boundary.signature, definer, publicExecute, config)
		}
	}
}

type failedSubstrateShape int

const (
	failedSubstrateExact failedSubstrateShape = iota
	failedSubstrateLiveClaim
	failedSubstrateUnclassified
	failedSubstrateReserved
	failedSubstratePending
)

type failedSubstrateProvision struct {
	tenantID, leaseID, serverID, operationID, generation string
	substrateServerID, guestRef, headDigest              string
	headSequence, serverGeneration                       int64
	failedAt                                             time.Time
}

// seedFailedSubstrateProvision writes, with triggers and foreign keys
// suspended as a restore would, a Proxmox substrate provision that failed
// before binding any guest: its first-claim provider_correlation dispatch
// guard, its exact guest custody, and the cancelled lease and decommissioning
// RuntimeServer of an owner destroy.
func seedFailedSubstrateProvision(
	t *testing.T,
	ctx context.Context,
	admin *sql.DB,
	tenantID, name string,
	shape failedSubstrateShape,
) failedSubstrateProvision {
	t.Helper()
	key := name + "-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	seed := failedSubstrateProvision{
		tenantID: tenantID, leaseID: "lease-" + key, serverID: "server-" + key,
		operationID: "op-" + key, generation: uuid.NewString(),
		substrateServerID: "substrate-" + key,
		headDigest:        "sha256:" + strings.Repeat("f", 64), headSequence: 2,
		serverGeneration: 3, failedAt: time.Now().UTC().Add(-5 * time.Minute),
	}
	seed.guestRef = seed.substrateServerID + "/9100"
	requestedDigest := "sha256:" + strings.Repeat("a", 64)
	status, phase := "failed", "failed"
	if shape == failedSubstratePending {
		status, phase = "pending", "requested"
		seed.headDigest, seed.headSequence = requestedDigest, 1
	}
	custodyClass := "customer_substrate"
	if shape == failedSubstrateUnclassified {
		custodyClass = ""
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
			'decommissioning', 'absent', 'pending', 'unknown', NULL,
			'local', 'substrate_vm', 'proxmox', $5,
			'customer', 'customer', 'runtime-lease:' || $3, clock_timestamp()
		)`, seed.serverID, tenantID, seed.leaseID, seed.serverGeneration, seed.guestRef)
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
					'substrate_server_id', $5::text, 'substrate_guest_id', '9100', 'resource_generation_id', $7::text)
			)),
			1, 'owner-1', $6, $7::uuid, clock_timestamp(),
			clock_timestamp() - interval '1 hour', clock_timestamp() + interval '1 year',
			clock_timestamp(), clock_timestamp(), clock_timestamp()
		)`, seed.leaseID, tenantID, seed.guestRef, custodyClass, seed.substrateServerID, seed.serverID, seed.generation)
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
			1, 1, $7, $8, $9, $10,
			clock_timestamp(), clock_timestamp(), clock_timestamp(), 'provider_correlation'
		)`, tenantID, seed.operationID, seed.leaseID, seed.serverGeneration, seed.generation, seed.serverID,
		status, phase, seed.headSequence, seed.headDigest)
	exec(`INSERT INTO provider_operation_receipts (
			tenant_id, operation_id, sequence, previous_receipt_digest,
			receipt_digest, status, phase, receipt_json, issued_at
		) VALUES ($1, $2, 1, NULL, $3, 'pending', 'requested', '{}'::jsonb, $4)`,
		tenantID, seed.operationID, requestedDigest, seed.failedAt.Add(-5*time.Minute))
	if shape != failedSubstratePending {
		exec(`INSERT INTO provider_operation_receipts (
				tenant_id, operation_id, sequence, previous_receipt_digest,
				receipt_digest, status, phase, receipt_json, issued_at
			) VALUES ($1, $2, 2, $3, $4, 'failed', 'failed', '{}'::jsonb, $5)`,
			tenantID, seed.operationID, requestedDigest, seed.headDigest, seed.failedAt)
		exec(`INSERT INTO provider_provision_dispatch_guards (
				tenant_id, operation_id, lease_id, lease_revision, server_id,
				resource_generation_id, head_sequence, head_receipt_digest,
				capability_snapshot_hash, execution_profile_hash, dispatch_mode,
				adapter_manifest_hash, guard_origin, first_claim_token_digest,
				first_claim_owner, guarded_at
			) VALUES (
				$1, $2, $3, 1, $4, $5::uuid, 1, $6,
				'sha256:' || repeat('1', 64), 'sha256:' || repeat('2', 64), 'provider_correlation',
				'sha256:' || repeat('3', 64), 'first_claim', repeat('4', 64),
				'worker-1', $7
			)`, tenantID, seed.operationID, seed.leaseID, seed.serverID, seed.generation, requestedDigest,
			seed.failedAt.Add(-4*time.Minute))
	}
	if shape == failedSubstrateLiveClaim {
		exec(`INSERT INTO provider_operation_execution_claims (
				tenant_id, operation_id, head_sequence, head_receipt_digest,
				claim_token_digest, claim_owner, state, claimed_at, lease_expires_at, claim_access
			) VALUES ($1, $2, 2, $3, repeat('5', 64), 'worker-1', 'active',
				clock_timestamp(), clock_timestamp() + interval '10 minutes', 'read_only')`,
			tenantID, seed.operationID, seed.headDigest)
	}
	exec(`INSERT INTO substrate_guest_leases (
			tenant_id, lease_id, server_id, substrate_server_id, guest_id, slot_id,
			resource_generation_id, operation_id, request_digest
		) VALUES ($1, $2, $3, $4, 9100, $5, $6::uuid, $7, 'sha256:' || repeat('d', 64))`,
		tenantID, seed.leaseID, seed.serverID, seed.substrateServerID, "slot-"+key, seed.generation, seed.operationID)
	if shape == failedSubstrateReserved {
		exec(`INSERT INTO managed_runtime_capacity_reservations (
				tenant_id, owner_subject_id, provider_id, lease_id, resource_generation_id,
				reservation_mode, policy_source, policy_digest, reservation_origin
			) VALUES ($1, 'owner-1', 'ionos', $2, $3::uuid, 'quarantine',
				'migration_quarantine:managed-runtime-capacity/v1', 'sha256:' || repeat('e', 64), 'migration_quarantine')`,
			tenantID, seed.leaseID, seed.generation)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return seed
}

// recordReservedGuestAbsence records, through the real recorder, the node-local
// Proxmox list read that found the seed's reserved guest absent, under the
// given operation of the same lease and resource generation.
func recordReservedGuestAbsence(
	t *testing.T,
	ctx context.Context,
	recorder *providerevidence.Recorder,
	seed failedSubstrateProvision,
	operationID string,
	collectedAt time.Time,
) string {
	t.Helper()
	digest := func(fill string) string { return "sha256:" + strings.Repeat(fill, 64) }
	command := providerexecutor.Command{
		APIVersion: "homelab.provider-executor/v1beta1", OperationID: operationID,
		Operation: providerexecutor.OperationDecommission, TenantID: seed.tenantID,
		LeaseID: seed.leaseID, LeaseRevision: 1, RuntimeServerID: seed.serverID,
		ResourceGenerationID: seed.generation, IdempotencyKey: "idem-" + operationID,
		ProviderID: "proxmox", AdapterID: "proxmox-substrate-v1",
		ConnectionRef:  "provider-connection://substrate/worker-1/" + seed.leaseID,
		ConnectionHash: digest("1"), CapabilitySnapshotHash: digest("2"), ExecutionProfileHash: digest("3"),
		ResourceGraphHash: digest("4"), LedgerRevision: 5, CommandDigest: digest("5"),
		RequestedAt: time.Now().UTC(),
	}
	resource := providerexecutor.ResourceTarget{
		BindingID: "guest-" + seed.generation, Kind: "virtual_machine", NativeRef: seed.guestRef,
		OwnershipHash: digest("6"), Disposition: providerexecutor.DispositionDelete,
	}
	evidence, err := recorder.Record(ctx, command, resource, providerevidence.Observation{
		Endpoint: "proxmox-node-local:/qemu", StatusCode: 200,
		CollectedAt: collectedAt, AuthoritativeListAbsent: resource.NativeRef,
	})
	if err != nil {
		t.Fatalf("record reserved guest absence: %v", err)
	}
	return evidence.Ref
}

func countUnsettledDispatchGuards(t *testing.T, ctx context.Context, database *sql.DB, seed failedSubstrateProvision) int64 {
	t.Helper()
	tx := pinBeginInTenant(t, ctx, database, seed.tenantID)
	defer func() { _ = tx.Rollback() }()
	var count int64
	if err := tx.QueryRowContext(ctx, `SELECT provider_control_count_unsettled_generation_dispatch_guards($1, $2::uuid)`,
		seed.leaseID, seed.generation).Scan(&count); err != nil {
		t.Fatalf("count unsettled dispatch guards: %v", err)
	}
	return count
}

// insertFailedSubstrateTerminalization runs only the insert validator, in the
// tenant's context, and rolls back before the deferred commit fence.
func insertFailedSubstrateTerminalization(
	ctx context.Context,
	database *sql.DB,
	seed failedSubstrateProvision,
	authority, evidenceRef string,
) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, seed.tenantID); err != nil {
		return err
	}
	return execFailedSubstrateTerminalization(ctx, tx, seed, authority, evidenceRef)
}

// commitFailedSubstrateTerminalization inserts the terminalization, then
// tombstones the RuntimeServer as projectResourceFreeTeardownTx does (with its
// server-registry triggers suspended), and commits through the commit fence.
func commitFailedSubstrateTerminalization(
	ctx context.Context,
	admin *sql.DB,
	seed failedSubstrateProvision,
	authority, evidenceRef string,
) error {
	tx, err := admin.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, seed.tenantID); err != nil {
		return err
	}
	if err := execFailedSubstrateTerminalization(ctx, tx, seed, authority, evidenceRef); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE servers SET lifecycle_state = 'decommissioned', decommissioned_at = clock_timestamp()
		WHERE tenant_id = $1 AND id = $2`, seed.tenantID, seed.serverID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL session_replication_role = origin`); err != nil {
		return err
	}
	return tx.Commit()
}

func execFailedSubstrateTerminalization(
	ctx context.Context,
	tx *sql.Tx,
	seed failedSubstrateProvision,
	authority, evidenceRef string,
) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO provider_operation_resource_free_terminalizations (
			tenant_id, operation_id, lease_id, lease_revision,
			server_id, server_generation, resource_generation_id,
			head_sequence, head_receipt_digest, authority, absence_evidence_ref, terminalized_at
		) VALUES ($1, $2, $3, 1, $4, $5, $6::uuid, $7, $8, $9, NULLIF($10, ''), clock_timestamp())
	`, seed.tenantID, seed.operationID, seed.leaseID, seed.serverID, seed.serverGeneration,
		seed.generation, seed.headSequence, seed.headDigest, authority, evidenceRef)
	return err
}
