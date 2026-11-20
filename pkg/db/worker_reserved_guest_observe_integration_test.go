package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	providerexecutor "github.com/kombifyio/stackkits/contracts/providerexecutor/v1beta1"

	"github.com/kombifyio/homelab/internal/providercontrol"
	"github.com/kombifyio/homelab/internal/providerevidence"
	"github.com/kombifyio/homelab/internal/substrate"
	"github.com/kombifyio/homelab/internal/substrateruntime"
	"github.com/kombifyio/homelab/pkg/agentcontrol"
)

// Sensitive auth boundary (kombify-StackKits-4ab3): the Guard worker transport
// admits, without an execution claim, exactly one read: the provision "guest"
// observe of the reserved guest of a Proxmox customer-substrate provision that
// failed before binding, after teardown was requested, on its enabled substrate
// binding. Every other shape fails closed, and the claimed fence is not widened.
// The producer records the answer in the exact shape migration 190
// terminalizes on; a present guest or an unknown answer records nothing.
func TestIntegrationReservedGuestObserveFence(t *testing.T) {
	dsn := integrationDSN()
	if dsn == "" {
		t.Skip("HOMELAB_TEST_POSTGRES_URL not set; skipping Postgres integration test")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	target, admin, _ := newForwardBatchDatabase(t, ctx, dsn)
	pinApplyMigrationsBefore(t, ctx, target, "999_")

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	tenantID := "reserved-guest-tenant-" + suffix
	pinExec(t, ctx, admin, `INSERT INTO techstack_tenants (id, display_name, kind, status)
		VALUES ($1, $1, 'saas', 'active')`, tenantID)
	hub := agentcontrol.NewHub(target.DB)
	adapter, err := substrateruntime.NewAdapter(target.DB, hub)
	if err != nil {
		t.Fatal(err)
	}

	// The exact head: the Guard receives the claimless observe, answers that
	// the reserved guest is absent, and the producer records the observation
	// migration 190 admits a resource-free terminalization on.
	exact := seedReservedGuestProvision(t, ctx, admin, tenantID, "exact", reservedGuestExact)
	delivered := serveReservedGuestObserve(t, ctx, hub, exact, func(execution providerexecutor.WorkerExecution) providerexecutor.WorkerExecutionResult {
		return reservedGuestAnswer(execution, substrate.GuestObservation{ID: exact.guestID}, "")
	})
	if err := adapter.ObserveReservedGuestAbsence(ctx, exact.record()); err != nil {
		t.Fatalf("exact reserved guest absence: %v", err)
	}
	execution := <-delivered
	identity, err := substrate.ParseReservedGuestObserve(execution.InvocationID)
	if err != nil || identity.OperationID != exact.command.OperationID || identity.SubstrateServerID != exact.substrateServerID ||
		identity.GuestID != exact.guestID || execution.Phase != providerexecutor.WorkerPhaseObserve || execution.Stage != "guest" ||
		execution.WorkerID != exact.workerID || execution.Command.LeaseID != exact.leaseID ||
		execution.Command.ResourceGenerationID != exact.generation {
		t.Fatalf("delivered observe identity = %+v (%v), execution %+v", identity, err, execution)
	}
	var evidenceRef, bindingID, nativeRefHash, document string
	var collectedAt time.Time
	if err := admin.QueryRowContext(ctx, `SELECT evidence_ref, binding_id, native_ref_hash, document_json::text, collected_at
		FROM provider_absence_observations WHERE tenant_id = $1 AND operation_id = $2 AND provider_id = 'proxmox'`,
		tenantID, exact.command.OperationID).Scan(&evidenceRef, &bindingID, &nativeRefHash, &document, &collectedAt); err != nil {
		t.Fatalf("recorded observation: %v", err)
	}
	if bindingID != "guest-"+exact.generation || nativeRefHash != sha256Ref(exact.guestRef()) ||
		!strings.Contains(document, `["authoritative_list_absent", "`+exact.guestRef()+`"]`) || !collectedAt.After(exact.failedAt) {
		t.Fatalf("observation binding=%s native_ref_hash=%s collected=%s document=%s", bindingID, nativeRefHash, collectedAt, document)
	}
	if !reservedGuestProven(t, ctx, target.DB, exact) {
		t.Fatal("migration 190 does not accept the produced observation")
	}
	if err := insertFailedSubstrateTerminalization(ctx, target.DB, exact.failedSubstrate(), "substrate_guest_absent", evidenceRef); err != nil {
		t.Fatalf("terminalization on the produced observation: %v", err)
	}
	// Recorded once: a later cycle sends the Guard nothing.
	if err := adapter.ObserveReservedGuestAbsence(ctx, exact.record()); err != nil {
		t.Fatalf("recorded reserved guest absence again: %v", err)
	}

	// A present guest and an unknown answer record nothing: the head waits.
	for _, answer := range []struct {
		name        string
		observation substrate.GuestObservation
		code        string
		want        error
	}{
		{"present", substrate.GuestObservation{Present: true, ID: 9100, Name: "kombify-present", Status: "stopped"}, "", substrateruntime.ErrReservedGuestPresent},
		{"ownership-denied", substrate.GuestObservation{}, "substrate_ownership_denied", substrateruntime.ErrReservedGuestUnknown},
	} {
		seed := seedReservedGuestProvision(t, ctx, admin, tenantID, answer.name, reservedGuestExact)
		delivered := serveReservedGuestObserve(t, ctx, hub, seed, func(execution providerexecutor.WorkerExecution) providerexecutor.WorkerExecutionResult {
			return reservedGuestAnswer(execution, answer.observation, answer.code)
		})
		if err := adapter.ObserveReservedGuestAbsence(ctx, seed.record()); !errors.Is(err, answer.want) {
			t.Fatalf("%s answer error = %v, want %v", answer.name, err, answer.want)
		}
		<-delivered
		if reservedGuestProven(t, ctx, target.DB, seed) || countReservedGuestObservations(t, ctx, admin, seed) != 0 {
			t.Fatalf("%s answer recorded an absence", answer.name)
		}
	}
	queued := countProviderCommands(t, ctx, admin, tenantID)
	if queued != 3 {
		t.Fatalf("provider commands after the answered reads = %d, want 3", queued)
	}

	// The producer sends nothing while the failure settles or the Guard is
	// offline, and an observation that does not postdate the failure (it is
	// immutable, one per binding) is reported instead of trusted.
	recorder, err := providerevidence.NewRecorder(target.DB)
	if err != nil {
		t.Fatal(err)
	}
	for _, waiting := range []struct {
		name    string
		shape   reservedGuestShape
		message string
	}{
		{"unsettled", reservedGuestUnsettled, "is read once the failure has settled"},
		{"guard-offline", reservedGuestGuardOffline, "the substrate Guard is not connected"},
		{"stale-observation", reservedGuestExact, "does not postdate the failure"},
	} {
		seed := seedReservedGuestProvision(t, ctx, admin, tenantID, waiting.name, waiting.shape)
		if waiting.name == "stale-observation" {
			recordReservedGuestAbsence(t, ctx, recorder, seed.failedSubstrate(), seed.command.OperationID, seed.failedAt.Add(-time.Minute))
		}
		err := adapter.ObserveReservedGuestAbsence(ctx, seed.record())
		if !errors.Is(err, substrateruntime.ErrReservedGuestUnknown) || !strings.Contains(err.Error(), waiting.message) {
			t.Fatalf("%s: producer error = %v, want %q", waiting.name, err, waiting.message)
		}
	}
	if count := countProviderCommands(t, ctx, admin, tenantID); count != queued {
		t.Fatalf("waiting heads sent reads: %d commands, want %d", count, queued)
	}

	// Every other shape fails closed before anything is queued.
	const (
		notAdmitted = "reserved guest observe is not admitted"
		claimFence  = "provider worker execution is not admitted at the current lease and claim"
	)
	headShapes := []struct {
		name  string
		shape reservedGuestShape
	}{
		{"pending-head", reservedGuestPending},
		{"teardown-not-requested", reservedGuestNotCancelled},
		{"binding-disabled", reservedGuestBindingDisabled},
		{"binding-revision-moved", reservedGuestBindingMoved},
		{"binding-other-worker", reservedGuestBindingOtherWorker},
		{"bound-resource", reservedGuestBoundResource},
		{"no-guest-custody", reservedGuestNoCustody},
		{"unsettled", reservedGuestUnsettled},
		{"substrate-server-foreign-owner", reservedGuestForeignSubstrateOwner},
		{"not-customer-substrate", reservedGuestUnclassified},
		{"engine-ref-mismatch", reservedGuestEngineRefMismatch},
		{"terminalized", reservedGuestExact},
	}
	for _, refused := range headShapes {
		seed := seedReservedGuestProvision(t, ctx, admin, tenantID, refused.name, refused.shape)
		if refused.name == "terminalized" {
			ref := recordReservedGuestAbsence(t, ctx, recorder, seed.failedSubstrate(), seed.command.OperationID, time.Now())
			if err := commitFailedSubstrateTerminalization(ctx, admin, seed.failedSubstrate(), "substrate_guest_absent", ref); err != nil {
				t.Fatalf("terminalize: %v", err)
			}
		}
		expectWorkerRefusal(t, ctx, hub, refused.name, seed.observe(), notAdmitted)
	}
	shape := seedReservedGuestProvision(t, ctx, admin, tenantID, "execution-shape", reservedGuestExact)
	executionShapes := []struct {
		name    string
		mutate  func(*providerexecutor.WorkerExecution)
		message string
	}{
		{"submit-phase", func(e *providerexecutor.WorkerExecution) { e.Phase = providerexecutor.WorkerPhaseSubmit }, notAdmitted},
		{"create-stage", func(e *providerexecutor.WorkerExecution) {
			e.Phase, e.Stage = providerexecutor.WorkerPhaseSubmit, "create"
		}, notAdmitted},
		{"task-stage", func(e *providerexecutor.WorkerExecution) { e.Stage = "task" }, notAdmitted},
		{"task-ref", func(e *providerexecutor.WorkerExecution) { e.TaskRef = "UPID:node:0001:task" }, notAdmitted},
		{"prerequisite", func(e *providerexecutor.WorkerExecution) {
			e.Prerequisite = &providerexecutor.WorkerExecutionResult{InvocationID: "prior", CommandDigest: e.Command.CommandDigest}
		}, notAdmitted},
		{"other-guest", func(e *providerexecutor.WorkerExecution) {
			e.InvocationID = shape.observeIdentity(shape.substrateServerID, 9101).InvocationID()
		}, notAdmitted},
		{"other-substrate-server", func(e *providerexecutor.WorkerExecution) {
			e.InvocationID = shape.observeIdentity("substrate-other", shape.guestID).InvocationID()
		}, notAdmitted},
		{"other-operation", func(e *providerexecutor.WorkerExecution) {
			e.InvocationID = substrate.ReservedGuestObserve{OperationID: exact.command.OperationID, SubstrateServerID: shape.substrateServerID,
				GuestID: shape.guestID, Nonce: uuid.NewString()}.InvocationID()
		}, notAdmitted},
		{"non-canonical-nonce", func(e *providerexecutor.WorkerExecution) { e.InvocationID += "x" }, notAdmitted},
		{"stale-head", func(e *providerexecutor.WorkerExecution) {
			e.HeadSequence, e.HeadDigest = 1, reservedGuestRequestedDigest
		}, notAdmitted},
		{"other-worker", func(e *providerexecutor.WorkerExecution) { e.WorkerID = "worker-other" }, notAdmitted},
		{"decommission-command", func(e *providerexecutor.WorkerExecution) {
			e.Command = shape.resealed(t, func(c *providerexecutor.Command) {
				c.Operation, c.IdempotencyKey, c.DesiredSpecRef, c.DesiredSpecHash = providerexecutor.OperationDecommission, "native-decommission-"+shape.leaseID, "", ""
				c.Targets = []providerexecutor.ResourceTarget{{
					BindingID: "guest-" + shape.generation, Kind: "virtual_machine", NativeRef: shape.guestRef(),
					OwnershipHash: "sha256:" + strings.Repeat("6", 64), Disposition: providerexecutor.DispositionDelete,
				}}
			})
			e.Payload = nil
			e.InvocationID = substrate.ReservedGuestObserve{OperationID: e.Command.OperationID, SubstrateServerID: shape.substrateServerID,
				GuestID: shape.guestID, Nonce: uuid.NewString()}.InvocationID()
		}, notAdmitted},
		{"other-provider", func(e *providerexecutor.WorkerExecution) {
			e.Command = shape.resealed(t, func(c *providerexecutor.Command) { c.ProviderID = "ionos" })
		}, notAdmitted},
		{"other-tenant", func(e *providerexecutor.WorkerExecution) {
			e.Command = shape.resealed(t, func(c *providerexecutor.Command) { c.TenantID = "reserved-guest-other-" + suffix })
			e.InvocationID = substrate.ReservedGuestObserve{OperationID: e.Command.OperationID, SubstrateServerID: shape.substrateServerID,
				GuestID: shape.guestID, Nonce: uuid.NewString()}.InvocationID()
		}, notAdmitted},
		// The claimed fence is unchanged: the same read without the reserved
		// identity has no claim on a failed head.
		{"claimed-fence", func(e *providerexecutor.WorkerExecution) {
			e.InvocationID = shape.command.OperationID + "-guest-" + uuid.NewString()
		}, claimFence},
	}
	for _, refused := range executionShapes {
		execution := shape.observe()
		refused.mutate(&execution)
		expectWorkerRefusal(t, ctx, hub, refused.name, execution, refused.message)
	}
	if count := countProviderCommands(t, ctx, admin, tenantID); count != queued {
		t.Fatalf("refused reads queued commands: %d, want %d", count, queued)
	}

	// Delivery revalidates the same fence: a queued read whose binding was
	// disabled meanwhile is not handed to the Guard.
	revoked := seedReservedGuestProvision(t, ctx, admin, tenantID, "revoked", reservedGuestExact)
	sendCtx, stopSend := context.WithCancel(ctx)
	sent := make(chan error, 1)
	go func() {
		_, sendErr := hub.SendWorkerExecution(sendCtx, revoked.observe())
		sent <- sendErr
	}()
	deadline := time.Now().Add(30 * time.Second)
	for countProviderCommands(t, ctx, admin, tenantID) == queued {
		if time.Now().After(deadline) {
			t.Fatal("the admitted read was not queued")
		}
		time.Sleep(50 * time.Millisecond)
	}
	pinExec(t, ctx, admin, `UPDATE substrate_bindings SET enabled = false WHERE tenant_id = $1 AND server_id = $2`,
		tenantID, revoked.substrateServerID)
	if command, err := hub.PollWorkerExecution(ctx, tenantID, revoked.workerID); err == nil || command != nil ||
		!strings.Contains(err.Error(), notAdmitted) {
		t.Fatalf("delivery after the binding was disabled: command=%v err=%v", command, err)
	}
	stopSend()
	<-sent
}

type reservedGuestShape int

const (
	reservedGuestExact reservedGuestShape = iota
	reservedGuestPending
	reservedGuestNotCancelled
	reservedGuestBindingDisabled
	reservedGuestBindingMoved
	reservedGuestBindingOtherWorker
	reservedGuestBoundResource
	reservedGuestNoCustody
	reservedGuestUnsettled
	reservedGuestForeignSubstrateOwner
	reservedGuestUnclassified
	reservedGuestEngineRefMismatch
	reservedGuestGuardOffline
)

var reservedGuestRequestedDigest = "sha256:" + strings.Repeat("a", 64)

type reservedGuestProvision struct {
	tenantID, leaseID, serverID, generation, workerID string
	substrateServerID, headDigest                     string
	guestID                                           int
	headSequence, serverGeneration                    int64
	failedAt                                          time.Time
	payload                                           json.RawMessage
	command                                           providerexecutor.Command
	status                                            providerexecutor.Status
	phase                                             providerexecutor.Phase
}

func (seed reservedGuestProvision) guestRef() string {
	return seed.substrateServerID + "/" + strconv.Itoa(seed.guestID)
}

func (seed reservedGuestProvision) record() providercontrol.OperationRecord {
	return providercontrol.OperationRecord{Command: seed.command, Head: providerexecutor.Receipt{
		Sequence: uint64(seed.headSequence), ReceiptDigest: seed.headDigest,
		Status: seed.status, Phase: seed.phase,
	}}
}

func (seed reservedGuestProvision) observeIdentity(substrateServerID string, guestID int) substrate.ReservedGuestObserve {
	return substrate.ReservedGuestObserve{OperationID: seed.command.OperationID, SubstrateServerID: substrateServerID, GuestID: guestID, Nonce: uuid.NewString()}
}

// observe is the read the producer sends for this head.
func (seed reservedGuestProvision) observe() providerexecutor.WorkerExecution {
	return providerexecutor.WorkerExecution{
		Command: seed.command, InvocationID: seed.observeIdentity(seed.substrateServerID, seed.guestID).InvocationID(),
		WorkerID: seed.workerID, HeadSequence: uint64(seed.headSequence), HeadDigest: seed.headDigest,
		Phase: providerexecutor.WorkerPhaseObserve, Stage: "guest", Payload: seed.payload,
		ExpiresAt: time.Now().UTC().Add(30 * time.Second),
	}
}

// resealed is the seed's provision command changed by edit and sealed again.
func (seed reservedGuestProvision) resealed(t *testing.T, edit func(*providerexecutor.Command)) providerexecutor.Command {
	t.Helper()
	command := seed.command
	command.OperationID, command.CommandDigest, command.ResourceGraphHash = "", "", ""
	edit(&command)
	sealed, err := providerexecutor.SealCommand(command)
	if err != nil {
		t.Fatalf("reseal: %v", err)
	}
	return sealed
}

func (seed reservedGuestProvision) failedSubstrate() failedSubstrateProvision {
	return failedSubstrateProvision{
		tenantID: seed.tenantID, leaseID: seed.leaseID, serverID: seed.serverID, operationID: seed.command.OperationID,
		generation: seed.generation, substrateServerID: seed.substrateServerID, guestRef: seed.guestRef(),
		headDigest: seed.headDigest, headSequence: seed.headSequence, serverGeneration: seed.serverGeneration, failedAt: seed.failedAt,
	}
}

// seedReservedGuestProvision writes, with triggers and foreign keys suspended
// as a restore would, the provision shape of seedFailedSubstrateProvision with
// a sealed provision command, its retained desired spec and the enabled
// substrate binding of its Guard.
func seedReservedGuestProvision(
	t *testing.T,
	ctx context.Context,
	admin *sql.DB,
	tenantID, name string,
	shape reservedGuestShape,
) reservedGuestProvision {
	t.Helper()
	key := name + "-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	seed := reservedGuestProvision{
		tenantID: tenantID, leaseID: "lease-" + key, serverID: "server-" + key, generation: uuid.NewString(),
		workerID: "worker-" + key, substrateServerID: "substrate-" + key, guestID: 9100,
		headDigest: "sha256:" + strings.Repeat("f", 64), headSequence: 2, serverGeneration: 3,
		failedAt: time.Now().UTC().Add(-substrate.ReservedGuestSettle - 5*time.Minute), status: providerexecutor.StatusFailed, phase: "failed",
	}
	if shape == reservedGuestUnsettled {
		seed.failedAt = time.Now().UTC().Add(-time.Minute)
	}
	if shape == reservedGuestPending {
		seed.status, seed.phase = providerexecutor.StatusPending, "requested"
		seed.headDigest, seed.headSequence = reservedGuestRequestedDigest, 1
	}
	var spec any
	if err := json.Unmarshal([]byte(`{"action":"provision","guest":{"id":`+strconv.Itoa(seed.guestID)+
		`,"name":"kombify-`+key+`","operation_tag":"kombify-op-`+key+`","profile_id":"ubuntu-24.04"}}`), &spec); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	seed.payload = payload
	digest := func(fill string) string { return "sha256:" + strings.Repeat(fill, 64) }
	seed.command, err = providerexecutor.SealCommand(providerexecutor.Command{
		Operation: providerexecutor.OperationProvision, TenantID: tenantID, LeaseID: seed.leaseID, LeaseRevision: 1,
		RuntimeServerID: seed.serverID, ResourceGenerationID: seed.generation, IdempotencyKey: "native-provision-" + key,
		ProviderID: "proxmox", AdapterID: substrate.AdapterID,
		CustodyRef: "custody://substrate/" + seed.leaseID, CustodyHash: digest("1"),
		ConnectionRef: "provider-connection://substrate/" + seed.workerID + "/" + seed.leaseID, ConnectionHash: digest("2"),
		CapabilitySnapshotHash: digest("3"), ExecutionProfileHash: digest("4"), LedgerRevision: 1,
		DesiredSpecRef:  "desired-spec://providercontrol.kombify.io/" + seed.leaseID + "/1",
		DesiredSpecHash: sha256Ref(string(payload)),
		RequestedAt:     time.Now().UTC().Add(-15 * time.Minute).Truncate(time.Microsecond),
	})
	if err != nil {
		t.Fatalf("seal %s provision: %v", name, err)
	}
	commandJSON, err := json.Marshal(seed.command)
	if err != nil {
		t.Fatal(err)
	}
	bindingWorker, bindingRevision, bindingEnabled := seed.workerID, 1, true
	switch shape {
	case reservedGuestBindingDisabled:
		bindingEnabled = false
	case reservedGuestBindingMoved:
		bindingRevision = 2
	case reservedGuestBindingOtherWorker:
		bindingWorker = "worker-other"
	}
	cancelled := "clock_timestamp()"
	if shape == reservedGuestNotCancelled {
		cancelled = "NULL"
	}
	custodyClass, engineRef := "customer_substrate", seed.guestRef()
	switch shape {
	case reservedGuestUnclassified:
		custodyClass = ""
	case reservedGuestEngineRefMismatch:
		engineRef = seed.substrateServerID + "/9101"
	}
	substrateOwner, substrateConnection := "owner-1", "connected"
	switch shape {
	case reservedGuestForeignSubstrateOwner:
		substrateOwner = "owner-2"
	case reservedGuestGuardOffline:
		substrateConnection = "offline"
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
		)`, seed.serverID, tenantID, seed.leaseID, seed.serverGeneration, seed.guestRef())
	exec(`INSERT INTO techstack_vm_leases (
			id, tenant_id, subject_id, org_id, provider_id, engine_vm_id,
			desired_state, idempotency_key, lease_json, lease_revision,
			owner_subject_id, server_id, resource_generation_id, cancelled_at,
			valid_from, valid_until, renewed_at, created_at, updated_at
		) VALUES (
			$1, $2, 'owner-1', $2, 'proxmox', $3,
			'absent', $1 || '-idempotency',
			jsonb_strip_nulls(jsonb_build_object(
				'custody_class', NULLIF($8::text, ''),
				'billing_mode', 'local',
				'lifecycle_class', 'one_time',
				'recreate_policy', 'never',
				'resource', jsonb_build_object('provider_id', 'proxmox', 'engine_vm_id', $3::text),
				'metadata', jsonb_build_object(
					'substrate_server_id', $4::text, 'substrate_guest_id', $7::text,
					'substrate_binding_revision', '1', 'resource_generation_id', $6::text)
			)),
			1, 'owner-1', $5, $6::uuid, `+cancelled+`,
			clock_timestamp() - interval '1 hour', clock_timestamp() + interval '1 year',
			clock_timestamp(), clock_timestamp(), clock_timestamp()
		)`, seed.leaseID, tenantID, engineRef, seed.substrateServerID, seed.serverID, seed.generation, strconv.Itoa(seed.guestID), custodyClass)
	exec(`INSERT INTO provider_desired_spec_revisions (tenant_id, lease_id, revision, spec_ref, spec_digest, spec_json, created_at)
		VALUES ($1, $2, 1, $3, $4, $5::jsonb, clock_timestamp())`,
		tenantID, seed.leaseID, seed.command.DesiredSpecRef, seed.command.DesiredSpecHash, string(payload))
	exec(`INSERT INTO servers (
			id, tenant_id, owner_subject_id, worker_id, name,
			lifecycle_state, connection_state, health_state, last_heartbeat_at, metadata_json
		) VALUES (
			$1, $2, $3, $4, $1,
			'active', $5, 'healthy', clock_timestamp(), '{"server_node_role":"substrate"}'::jsonb
		)`, seed.substrateServerID, tenantID, substrateOwner, bindingWorker, substrateConnection)
	exec(`INSERT INTO substrate_bindings (tenant_id, server_id, worker_id, node, revision, config_json, enabled)
		VALUES ($1, $2, $3, 'pve', $4, '{"min_guest_id":9000,"max_guest_id":9999}'::jsonb, $5)`,
		tenantID, seed.substrateServerID, bindingWorker, bindingRevision, bindingEnabled)
	exec(`INSERT INTO provider_operations (
			tenant_id, operation_id, lease_id, operation, idempotency_key,
			adapter_id, command_digest, command_json, ledger_revision,
			desired_spec_revision, status, phase, head_sequence,
			head_receipt_digest, requested_at, created_at, updated_at,
			provision_dispatch_mode
		) VALUES (
			$1, $2, $3, 'provision', $4, 'proxmox-substrate-v1', $5,
			jsonb_build_object(
				'schema_version', 'techstack.provider-control-operation/v1',
				'execution_authority', 'techstack_provider_control',
				'runtime_server_generation', $6::bigint,
				'execution_profile', jsonb_build_object('provider_id', 'proxmox', 'credential_mode', 'worker_held'),
				'command', $7::jsonb
			),
			1, 1, $8, $9, $10, $11,
			clock_timestamp(), clock_timestamp(), clock_timestamp(), 'provider_correlation'
		)`, tenantID, seed.command.OperationID, seed.leaseID, seed.command.IdempotencyKey, seed.command.CommandDigest,
		seed.serverGeneration, string(commandJSON), string(seed.status), string(seed.phase), seed.headSequence, seed.headDigest)
	exec(`INSERT INTO provider_operation_receipts (
			tenant_id, operation_id, sequence, previous_receipt_digest,
			receipt_digest, status, phase, receipt_json, issued_at
		) VALUES ($1, $2, 1, NULL, $3, 'pending', 'requested', '{}'::jsonb, $4)`,
		tenantID, seed.command.OperationID, reservedGuestRequestedDigest, seed.failedAt.Add(-5*time.Minute))
	if shape != reservedGuestPending {
		exec(`INSERT INTO provider_operation_receipts (
				tenant_id, operation_id, sequence, previous_receipt_digest,
				receipt_digest, status, phase, receipt_json, issued_at
			) VALUES ($1, $2, 2, $3, $4, 'failed', 'failed', '{}'::jsonb, $5)`,
			tenantID, seed.command.OperationID, reservedGuestRequestedDigest, seed.headDigest, seed.failedAt)
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
			)`, tenantID, seed.command.OperationID, seed.leaseID, seed.serverID, seed.generation, reservedGuestRequestedDigest,
			seed.failedAt.Add(-4*time.Minute))
	}
	if shape == reservedGuestBoundResource {
		exec(`INSERT INTO provider_operation_resources (
				tenant_id, operation_id, binding_id, kind, native_ref, ownership_hash, disposition,
				observation, cleanup_state, first_receipt_sequence, last_receipt_sequence
			) VALUES ($1, $2, $3, 'virtual_machine', $4, 'sha256:' || repeat('6', 64), 'delete', 'unknown', 'required', 2, 2)`,
			tenantID, seed.command.OperationID, "guest-"+seed.generation, seed.guestRef())
	}
	if shape != reservedGuestNoCustody {
		exec(`INSERT INTO substrate_guest_leases (
				tenant_id, lease_id, server_id, substrate_server_id, guest_id, slot_id,
				resource_generation_id, operation_id, request_digest
			) VALUES ($1, $2, $3, $4, $5, $6, $7::uuid, $8, 'sha256:' || repeat('d', 64))`,
			tenantID, seed.leaseID, seed.serverID, seed.substrateServerID, seed.guestID, "slot-"+key, seed.generation, seed.command.OperationID)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return seed
}

// serveReservedGuestObserve plays one Guard cycle: it takes the next command
// delivered to the seed's worker and answers it as the substrate executor does.
func serveReservedGuestObserve(
	t *testing.T,
	ctx context.Context,
	hub *agentcontrol.Hub,
	seed reservedGuestProvision,
	answer func(providerexecutor.WorkerExecution) providerexecutor.WorkerExecutionResult,
) <-chan providerexecutor.WorkerExecution {
	delivered := make(chan providerexecutor.WorkerExecution, 1)
	go func() {
		defer close(delivered)
		for ctx.Err() == nil {
			command, err := hub.PollWorkerExecution(ctx, seed.tenantID, seed.workerID)
			if err != nil {
				t.Errorf("Guard poll: %v", err)
				return
			}
			if command == nil {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			if err := hub.SubmitWorkerExecutionResult(ctx, seed.tenantID, seed.workerID, answer(*command)); err != nil {
				t.Errorf("Guard result: %v", err)
			}
			delivered <- *command
			return
		}
	}()
	return delivered
}

func reservedGuestAnswer(execution providerexecutor.WorkerExecution, observation substrate.GuestObservation, code string) providerexecutor.WorkerExecutionResult {
	result := providerexecutor.WorkerExecutionResult{InvocationID: execution.InvocationID, CommandDigest: execution.Command.CommandDigest, Complete: true}
	if code != "" {
		result.ErrorCode = code
		return result
	}
	result.Succeeded = true
	result.Observation, _ = json.Marshal(observation)
	return result
}

func expectWorkerRefusal(t *testing.T, ctx context.Context, hub *agentcontrol.Hub, name string, execution providerexecutor.WorkerExecution, message string) {
	t.Helper()
	sendCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := hub.SendWorkerExecution(sendCtx, execution); err == nil || !strings.Contains(err.Error(), message) {
		t.Fatalf("%s: send error = %v, want %q", name, err, message)
	}
}

func reservedGuestProven(t *testing.T, ctx context.Context, database *sql.DB, seed reservedGuestProvision) bool {
	t.Helper()
	tx := pinBeginInTenant(t, ctx, database, seed.tenantID)
	defer func() { _ = tx.Rollback() }()
	var proven bool
	if err := tx.QueryRowContext(ctx, `SELECT substrate_failed_provision_guest_absent($1, $2, NULL)`,
		seed.tenantID, seed.command.OperationID).Scan(&proven); err != nil {
		t.Fatalf("migration 190 predicate: %v", err)
	}
	return proven
}

func countReservedGuestObservations(t *testing.T, ctx context.Context, admin *sql.DB, seed reservedGuestProvision) int {
	t.Helper()
	var count int
	if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM provider_absence_observations WHERE tenant_id = $1 AND operation_id = $2`,
		seed.tenantID, seed.command.OperationID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func countProviderCommands(t *testing.T, ctx context.Context, admin *sql.DB, tenantID string) int {
	t.Helper()
	var count int
	if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM typed_agent_commands WHERE tenant_id = $1 AND command_family = 'provider'`,
		tenantID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func sha256Ref(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}
