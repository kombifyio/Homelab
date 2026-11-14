package controlplane

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestMemoryWorkerCredentialCASSurvivesStaleHeartbeatAndGuardSatellite(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store.SetNow(func() time.Time { return now })
	initial := WorkerCredentialState{
		TokenSHA256:       digestCharacter("a"),
		IdempotencySHA256: digestCharacter("b"),
		RequestSHA256:     digestCharacter("c"),
		Generation:        1,
	}
	resources := workerCredentialResources(initial)
	resources["observation"] = "initial"
	if _, err := store.UpsertWorkerHeartbeat(t.Context(), Worker{
		ID: "runtime-1", TenantID: "tenant-1", OwnerSubjectID: "owner-1",
		Resources: resources,
	}); err != nil {
		t.Fatal(err)
	}
	next := WorkerCredentialState{
		TokenSHA256:       digestCharacter("d"),
		IdempotencySHA256: digestCharacter("e"),
		RequestSHA256:     digestCharacter("f"),
		Generation:        2,
	}
	if _, err := store.CompareAndSwapWorkerCredential(t.Context(), WorkerCredentialCAS{
		TenantID: "tenant-1", WorkerID: "runtime-1", Expected: initial, Next: next,
	}); err != nil {
		t.Fatal(err)
	}

	staleResources := workerCredentialResources(initial)
	staleResources["observation"] = "heartbeat"
	if _, err := store.UpsertWorkerHeartbeat(t.Context(), Worker{
		ID: "runtime-1", TenantID: "tenant-1", OwnerSubjectID: "owner-1",
		Resources: staleResources,
	}); err != nil {
		t.Fatal(err)
	}
	assertMemoryWorkerCredentialState(t, store, next, "heartbeat")

	store.mu.Lock()
	store.servers["server-1"] = ServerRuntime{
		ID: "server-1", TenantID: "tenant-1", Generation: 1,
		SourceAuthority: ServerEventAuthorityGuard, SourceID: "runtime-1",
		SourceEpoch: "epoch-1", SourceSequence: 1, SourceObservedAt: &now,
		InventoryRevision: 1, LifecycleState: "active",
	}
	store.mu.Unlock()
	staleResources["observation"] = "guard"
	result, err := store.ApplyGuardInventorySatellites(t.Context(), GuardInventorySatelliteProjection{
		TenantID: "tenant-1", ServerID: "server-1", Generation: 1,
		SourceID: "runtime-1", SourceEpoch: "epoch-1", SourceSequence: 1,
		SourceObservedAt: now, InventoryRevision: 1,
		Worker: Worker{
			ID: "runtime-1", TenantID: "tenant-1", OwnerSubjectID: "owner-1",
			Resources: staleResources,
		},
		RILServer: RILServer{
			ID: "server-1", TenantID: "tenant-1", Status: "healthy", LastSeenAt: &now,
		},
	})
	if err != nil || result == nil || !result.Applied {
		t.Fatalf("apply Guard satellites: result=%+v err=%v", result, err)
	}
	assertMemoryWorkerCredentialState(t, store, next, "guard")
}

func TestPostgresWorkerCredentialCASUsesOneTenantWorkerUpdate(t *testing.T) {
	t.Parallel()

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewPostgresStore(db)
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	expected := WorkerCredentialState{
		TokenSHA256:       digestCharacter("a"),
		IdempotencySHA256: digestCharacter("b"),
		RequestSHA256:     digestCharacter("c"),
		Generation:        1,
	}
	next := WorkerCredentialState{
		TokenSHA256:       digestCharacter("d"),
		IdempotencySHA256: digestCharacter("e"),
		RequestSHA256:     digestCharacter("f"),
		Generation:        2,
	}

	mock.ExpectBegin()
	expectTenantGUC(mock, "tenant-1")
	mock.ExpectQuery(
		"(?s)"+regexp.QuoteMeta("UPDATE workers")+".*"+
			regexp.QuoteMeta("WHERE tenant_id = $1 AND id = $2")+".*"+
			regexp.QuoteMeta("resources_json->>'credential_generation'")+".*"+
			regexp.QuoteMeta("resources_json->>'agent_token_sha256'")+".*"+
			regexp.QuoteMeta("resources_json->>'enrollment_idempotency_sha256'")+".*"+
			regexp.QuoteMeta("resources_json->>'enrollment_request_sha256'"),
	).WithArgs(
		"tenant-1", "runtime-1", "1", expected.TokenSHA256,
		expected.IdempotencySHA256, expected.RequestSHA256, sqlmock.AnyArg(),
	).WillReturnRows(workerRows().AddRow(
		"runtime-1", "tenant-1", nil, "stack-1", "node-1", nil,
		"ubuntu", "amd64", nil, "connected", true, now, now,
		4, 8192, 100, nil, false, false, "27", "runtime", "local",
		`{}`, "owner-1", `{"server_id":"server-1"}`,
		`{"agent_token_sha256":"`+next.TokenSHA256+`","enrollment_idempotency_sha256":"`+next.IdempotencySHA256+`","enrollment_request_sha256":"`+next.RequestSHA256+`","credential_generation":2}`,
		now, now,
	))
	mock.ExpectCommit()

	worker, err := store.CompareAndSwapWorkerCredential(context.Background(), WorkerCredentialCAS{
		TenantID: "tenant-1", WorkerID: "runtime-1", Expected: expected, Next: next,
	})
	if err != nil {
		t.Fatalf("CompareAndSwapWorkerCredential: %v", err)
	}
	state, err := WorkerCredentialStateFromWorker(*worker)
	if err != nil || !workerCredentialStateEqual(state, next) {
		t.Fatalf("returned state = %+v err=%v", state, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func assertMemoryWorkerCredentialState(
	t *testing.T,
	store *MemoryStore,
	want WorkerCredentialState,
	wantObservation string,
) {
	t.Helper()
	worker, err := store.GetWorker(t.Context(), "tenant-1", "runtime-1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := WorkerCredentialStateFromWorker(*worker)
	if err != nil || !workerCredentialStateEqual(got, want) {
		t.Fatalf("credential state = %+v want=%+v err=%v", got, want, err)
	}
	if worker.Resources["observation"] != wantObservation {
		t.Fatalf("ordinary resources were not updated: %#v", worker.Resources)
	}
}

func digestCharacter(character string) string {
	out := ""
	for len(out) < 64 {
		out += character
	}
	return out
}

// Generation 0 carries command authority only for a key-bound Guard that last
// proved its bound host key; unreadable state never does (kombify-StackKits-4czp).
func TestCommandAuthorityGenerationAdmitsGenerationZeroOnlyWithBoundHostKey(t *testing.T) {
	worker := func(binding string, generation any) Worker {
		resources := map[string]any{}
		if generation != nil {
			resources[WorkerCredentialGenerationResourceKey] = generation
		}
		return Worker{ID: "w", TenantID: "t", Capabilities: map[string]any{"identity_binding": binding}, Resources: resources}
	}
	identity := func(state string) func() (*WorkerIdentity, error) {
		return func() (*WorkerIdentity, error) { return &WorkerIdentity{TenantID: "t", WorkerID: "w", State: state}, nil }
	}
	foreign := func() (*WorkerIdentity, error) {
		return &WorkerIdentity{TenantID: "other", WorkerID: "w", State: WorkerIdentityBound}, nil
	}
	for _, tc := range []struct {
		name     string
		worker   Worker
		identity func() (*WorkerIdentity, error)
		want     int64
		admitted bool
	}{
		{"key-bound, never rotated", worker(IdentityBindingHostKey, nil), identity(WorkerIdentityBound), 0, true},
		{"key-bound, explicit zero", worker(IdentityBindingHostKey, "0"), identity(WorkerIdentityBound), 0, true},
		{"legacy bearer at zero", worker(IdentityBindingLegacyBearer, nil), identity(WorkerIdentityBound), 0, false},
		{"revoked identity", worker(IdentityBindingHostKey, nil), identity(WorkerIdentityRevoked), 0, false},
		{"upgrade pending", worker(IdentityBindingHostKey, nil), identity(WorkerIdentityUpgradePending), 0, false},
		{"unconfirmed key", worker(IdentityBindingHostKey, nil), identity(WorkerIdentityIssuedUnconfirmed), 0, false},
		{"identity of another tenant", worker(IdentityBindingHostKey, nil), foreign, 0, false},
		{"no identity reader", worker(IdentityBindingHostKey, nil), nil, 0, false},
		{"opaque credential", worker(IdentityBindingLegacyBearer, 3), nil, 3, true},
		{"unreadable state", worker(IdentityBindingHostKey, -1), identity(WorkerIdentityBound), -1, false},
	} {
		generation, admitted := CommandAuthorityGeneration(tc.worker, tc.identity)
		if generation != tc.want || admitted != tc.admitted {
			t.Errorf("%s: got (%d, %v), want (%d, %v)", tc.name, generation, admitted, tc.want, tc.admitted)
		}
	}
}
