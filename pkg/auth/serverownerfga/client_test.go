package serverownerfga

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kombifyio/homelab/internal/gocommon/servicecall"
	"github.com/kombifyio/homelab/pkg/serverregistry"
)

const testServiceAuthSecret = "homelab-fga-owner-projection-test-secret-32"

func newTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	client, err := NewClient(ClientConfig{
		BaseURL: baseURL,
		ServiceAuthConfig: &servicecall.Config{
			ServiceName: CallerService,
			Target:      TargetService,
			Scope:       Scope,
			Secret:      testServiceAuthSecret,
			TokenTTL:    time.Minute,
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func TestClientProjectsOwnedAndUnownedWithVerifiedServiceAuth(t *testing.T) {
	var mu sync.Mutex
	var received []ProjectionRequest
	var observedScopes []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != EndpointPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		token := r.Header.Get(servicecall.HeaderServiceAuth)
		claims, err := servicecall.VerifyToken(token, testServiceAuthSecret, "")
		if err != nil {
			t.Errorf("VerifyToken failed: %v", err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if claims.Svc != CallerService || claims.Iss != "kombify-homelab" || claims.Aud != "kombify-administration" || claims.Scope != Scope {
			t.Errorf("unexpected service claims: %+v", claims)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		var req ProjectionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		received = append(received, req)
		observedScopes = append(observedScopes, claims.Scope)
		mu.Unlock()

		outcomes := make([]string, len(req.Servers))
		for i := range req.Servers {
			outcomes[i] = OutcomeApplied
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ProjectionResponse{
			OK:        true,
			Projected: len(req.Servers),
			Outcomes:  outcomes,
		})
	}))
	t.Cleanup(srv.Close)

	client := newTestClient(t, srv.URL)
	err := client.PublishServers(t.Context(), "usr:auth0|owner1", []serverregistry.ProjectedServer{
		{
			ServerID:          "srv-active",
			TenantID:          "usr:auth0|owner1",
			OwnerID:           "auth0|owner1",
			LifecycleState:    "active",
			AggregateRevision: 7,
		},
		{
			ServerID:          "srv-retired",
			TenantID:          "usr:auth0|owner1",
			OwnerID:           "auth0|owner1",
			LifecycleState:    "decommissioned",
			AggregateRevision: 12,
			Retired:           true,
		},
	})
	if err != nil {
		t.Fatalf("PublishServers: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 || len(received[0].Servers) != 2 {
		t.Fatalf("received batches = %+v", received)
	}
	if observedScopes[0] != Scope {
		t.Fatalf("scope = %q, want %q", observedScopes[0], Scope)
	}
	want := []ServerOwnerInput{
		{TenantID: "usr:auth0|owner1", ServerID: "srv-active", OwnerSub: "auth0|owner1", State: StateOwned, AggregateRevision: "7"},
		{TenantID: "usr:auth0|owner1", ServerID: "srv-retired", OwnerSub: "", State: StateUnowned, AggregateRevision: "12"},
	}
	if !slices.Equal(received[0].Servers, want) {
		t.Fatalf("projected payload = %+v, want %+v", received[0].Servers, want)
	}
}

func TestClientValidationFailsClosedBeforeNetwork(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	client := newTestClient(t, srv.URL)
	badInputs := []struct {
		name  string
		input ServerOwnerInput
		code  string
	}{
		{"empty tenant", ServerOwnerInput{TenantID: "", ServerID: "srv-1", OwnerSub: "u1", State: StateOwned, AggregateRevision: "1"}, "invalid_tenant_id"},
		{"b64 tenant prefix", ServerOwnerInput{TenantID: "b64_abc", ServerID: "srv-1", OwnerSub: "u1", State: StateOwned, AggregateRevision: "1"}, "invalid_tenant_id"},
		{"server id with slash", ServerOwnerInput{TenantID: "org_1", ServerID: "bad/id", OwnerSub: "u1", State: StateOwned, AggregateRevision: "1"}, "invalid_server_id"},
		{"server id with hash", ServerOwnerInput{TenantID: "org_1", ServerID: "bad#id", OwnerSub: "u1", State: StateOwned, AggregateRevision: "1"}, "invalid_server_id"},
		{"server id with colon", ServerOwnerInput{TenantID: "org_1", ServerID: "bad:id", OwnerSub: "u1", State: StateOwned, AggregateRevision: "1"}, "invalid_server_id"},
		{"server id with space", ServerOwnerInput{TenantID: "org_1", ServerID: "bad id", OwnerSub: "u1", State: StateOwned, AggregateRevision: "1"}, "invalid_server_id"},
		{"personal tenant mismatch", ServerOwnerInput{TenantID: "usr:auth0|alice", ServerID: "srv-1", OwnerSub: "auth0|bob", State: StateOwned, AggregateRevision: "1"}, "personal_tenant_owner_mismatch"},
		{"non-decimal revision", ServerOwnerInput{TenantID: "org_1", ServerID: "srv-1", OwnerSub: "auth0|alice", State: StateOwned, AggregateRevision: "-1"}, "invalid_aggregate_revision"},
		{"invalid state", ServerOwnerInput{TenantID: "org_1", ServerID: "srv-1", OwnerSub: "auth0|alice", State: "active", AggregateRevision: "1"}, "invalid_state"},
	}
	for _, tc := range badInputs {
		_, err := client.Project(t.Context(), []ServerOwnerInput{tc.input})
		var projErr *ProjectionError
		if !errors.As(err, &projErr) || projErr.ReasonCode != tc.code || projErr.IsRetryable() {
			t.Fatalf("%s: got err=%v, want non-retryable %q", tc.name, err, tc.code)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid inputs reached network %d times", calls)
	}
	if err := client.PublishServers(t.Context(), "org_1", []serverregistry.ProjectedServer{
		{ServerID: "srv-1", TenantID: "org_2", OwnerID: "auth0|alice", AggregateRevision: 1},
	}); err == nil {
		t.Fatal("cross-tenant server in batch must fail closed")
	}
}

func TestClientChunksBatchesAt200AndSurfacesErrors(t *testing.T) {
	var mu sync.Mutex
	var batchSizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ProjectionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		batchSizes = append(batchSizes, len(req.Servers))
		mu.Unlock()
		if len(req.Servers) > MaxBatchSize {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		outcomes := make([]string, len(req.Servers))
		for i := range outcomes {
			outcomes[i] = OutcomeReplayed
		}
		_ = json.NewEncoder(w).Encode(ProjectionResponse{OK: true, Projected: len(req.Servers), Outcomes: outcomes})
	}))
	t.Cleanup(srv.Close)

	client := newTestClient(t, srv.URL)
	inputs := make([]ServerOwnerInput, 205)
	for i := range inputs {
		inputs[i] = ServerOwnerInput{
			TenantID:          "org_large",
			ServerID:          fmt.Sprintf("srv-%03d", i),
			OwnerSub:          "auth0|owner1",
			State:             StateOwned,
			AggregateRevision: "3",
		}
	}
	resp, err := client.Project(t.Context(), inputs)
	if err != nil {
		t.Fatalf("Project 205 servers: %v", err)
	}
	if resp.Projected != 205 || len(resp.Outcomes) != 205 {
		t.Fatalf("combined response = %+v", resp)
	}
	mu.Lock()
	gotSizes := slices.Clone(batchSizes)
	mu.Unlock()
	if !slices.Equal(gotSizes, []int{200, 5}) {
		t.Fatalf("batch sizes = %v, want [200 5]", gotSizes)
	}

	// Verify 503 retryable, 409 non-retryable, and malformed 200 all fail closed.
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		retryable bool
		reason    string
	}{
		{"retryable 503", http.StatusServiceUnavailable, `{"ok":false,"error_code":"HOMELAB_SERVER_OWNER_PROJECTION_FAILED","reason_code":"fga_transport_unknown","retryable":true}`, true, "fga_transport_unknown"},
		{"conflict 409", http.StatusConflict, `{"ok":false,"error_code":"HOMELAB_SERVER_OWNER_PROJECTION_FAILED","reason_code":"aggregate_revision_conflict","retryable":false}`, false, "aggregate_revision_conflict"},
		{"mismatched 200 count", http.StatusOK, `{"ok":true,"projected":0,"outcomes":[]}`, true, "invalid_response_contract"},
		{"unknown 200 outcome", http.StatusOK, `{"ok":true,"projected":1,"outcomes":["unknown"]}`, true, "invalid_response_outcome"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(errSrv.Close)
			c := newTestClient(t, errSrv.URL)
			_, err := c.Project(t.Context(), inputs[:1])
			var projErr *ProjectionError
			if !errors.As(err, &projErr) || projErr.IsRetryable() != tc.retryable || projErr.ReasonCode != tc.reason {
				t.Fatalf("got err=%v, want retryable=%t reason=%q", err, tc.retryable, tc.reason)
			}
		})
	}
}

type fakeProjectionSource struct {
	tenants []string
	servers map[string][]serverregistry.ProjectedServer
}

func (f fakeProjectionSource) ListServerProjectionTenants(_ context.Context, after string, limit int) ([]string, error) {
	var out []string
	for _, tenant := range f.tenants {
		if tenant > after {
			out = append(out, tenant)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (f fakeProjectionSource) ListProjectedServers(_ context.Context, tenant string) ([]serverregistry.ProjectedServer, error) {
	return f.servers[tenant], nil
}

func TestPlatformProjectorAndEventDispatcherReconcileAndRetry(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	var delivered []ServerOwnerInput

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ProjectionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		attempts++
		attempt := attempts
		if attempt > 1 {
			delivered = append(delivered, req.Servers...)
		}
		mu.Unlock()
		if attempt == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"ok":false,"reason_code":"fga_transport_unknown","retryable":true}`))
			return
		}
		outcomes := make([]string, len(req.Servers))
		for i := range outcomes {
			outcomes[i] = OutcomeApplied
		}
		_ = json.NewEncoder(w).Encode(ProjectionResponse{OK: true, Projected: len(req.Servers), Outcomes: outcomes})
	}))
	t.Cleanup(srv.Close)

	client := newTestClient(t, srv.URL)
	var reported []error
	projector, err := serverregistry.NewPlatformProjector(serverregistry.PlatformProjectorConfig{
		Source: fakeProjectionSource{
			tenants: []string{"org_1"},
			servers: map[string][]serverregistry.ProjectedServer{
				"org_1": {{ServerID: "srv-1", TenantID: "org_1", OwnerID: "auth0|owner1", LifecycleState: "active", AggregateRevision: 2}},
			},
		},
		Publisher: client,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnError:   func(err error) { reported = append(reported, err) },
	})
	if err != nil {
		t.Fatalf("NewPlatformProjector: %v", err)
	}
	first, err := projector.ProjectOnce(t.Context())
	if err != nil || first.Failures != 1 || len(reported) != 1 {
		t.Fatalf("first pass = %+v err=%v reported=%d, want 1 logged failure", first, err, len(reported))
	}
	second, err := projector.ProjectOnce(t.Context())
	if err != nil || second.Failures != 0 || second.Servers != 1 {
		t.Fatalf("second retry pass = %+v err=%v, want 1 reconciled server", second, err)
	}

	// Verify EventDispatcher delivers asynchronously and stops cleanly.
	ctx, cancel := context.WithCancel(t.Context())
	dispatcher, err := NewEventDispatcher(EventDispatcherConfig{
		Projector: client,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewEventDispatcher: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		dispatcher.Run(ctx)
	}()
	dispatcher.Enqueue(ServerOwnerInput{
		TenantID:          "org_1",
		ServerID:          "srv-2",
		OwnerSub:          "",
		State:             StateUnowned,
		AggregateRevision: "9",
	})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(delivered)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 2 || delivered[1].ServerID != "srv-2" || delivered[1].State != StateUnowned {
		t.Fatalf("delivered events = %+v", delivered)
	}
}
