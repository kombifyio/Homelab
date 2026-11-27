package routes

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kombifyio/homelab/internal/privatefs"
	"github.com/kombifyio/homelab/pkg/agent/hostidentity"
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/pairingtoken"
)

// The Guard enrolls against the real routes over HTTP, survives a lost
// redemption response, drops its pairing capability once the binding is
// confirmed, and from then on every request it signs is accepted while the same
// bearer credential without the key is not.
func TestGuardAgentEnrollmentSurvivesLostResponseAndSignsRequests(t *testing.T) {
	store := controlplane.NewMemoryStore()
	raw, hash, err := pairingtoken.Generate("tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	if _, err := store.UpsertPairingToken(t.Context(), controlplane.PairingToken{
		ID: "agent-pairing", TenantID: "tenant-1", OwnerSubjectID: "owner-1", StackID: "stack-1",
		TokenHash: hash, Status: "active", ExpiresAt: &expires,
	}); err != nil {
		t.Fatal(err)
	}
	router := httpx.NewRouter()
	RegisterWorkerRoutesWithStore(router, WorkerRouteConfig{Store: store, Servers: store})

	// The first redemption is processed by the control plane but its response
	// never reaches the Guard.
	var dropped atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/workers/register" && dropped.CompareAndSwap(false, true) {
			router.ServeHTTP(httptest.NewRecorder(), r)
			connection, _, hijackErr := w.(http.Hijacker).Hijack()
			if hijackErr == nil {
				_ = connection.Close()
			}
			return
		}
		router.ServeHTTP(w, r)
	}))
	defer server.Close()

	stateDir := filepath.Join(t.TempDir(), "state")
	enrollmentPath := filepath.Join(stateDir, "agent-enrollment.json")
	manager, err := hostidentity.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	result, err := hostidentity.Enroll(t.Context(), hostidentity.EnrollOptions{
		Manager: manager, EnrollmentPath: enrollmentPath, ResiduePaths: []string{}, Backoff: []time.Duration{time.Millisecond, time.Millisecond},
		Bootstrap: hostidentity.Bootstrap{Server: server.URL, Token: raw, Hostname: "node-1", OS: "linux", Arch: "x86_64"},
	})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if !dropped.Load() || result.Mode != hostidentity.ModeHostKey || !result.Accepted {
		t.Fatalf("enrollment result = %+v (response dropped: %v)", result, dropped.Load())
	}
	if record := manager.Record(); record.State != hostidentity.StateBound {
		t.Fatalf("local state = %s, want bound", record.State)
	}
	// Confirmation discards the capability in place (an empty file, so a
	// concurrent replacement is never unlinked); what matters is that it can no
	// longer be loaded and redeemed.
	if capability, err := manager.LoadBootstrap(); err != nil || capability != nil {
		t.Fatalf("the pairing capability survived confirmation: %+v, %v", capability, err)
	}
	enrollmentRoot, err := privatefs.Open(filepath.Dir(enrollmentPath))
	if err != nil {
		t.Fatalf("admit enrollment custody: %v", err)
	}
	defer enrollmentRoot.Close()

	enrollment, err := privatefs.ReadFile(enrollmentRoot, filepath.Base(enrollmentPath))
	if err != nil {
		t.Fatal(err)
	}
	view, err := hostidentity.ParseEnrollment(enrollment)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := hostidentity.NewClient(hostidentity.ClientConfig{
		Manager: manager, Endpoints: view.Endpoints, AgentToken: view.AgentToken, TenantID: view.TenantID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if mode, err := identity.Establish(t.Context()); err != nil || mode != hostidentity.ModeHostKey {
		t.Fatalf("establish = %v, %v", mode, err)
	}
	heartbeat := func(client *http.Client) int {
		body := `{"source_epoch":"agent-e2e","source_sequence":1,"observed_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}`
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, view.HeartbeatURL, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+view.AgentToken)
		req.Header.Set("X-Kombify-Tenant-ID", view.TenantID)
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		return response.StatusCode
	}
	if status := heartbeat(&http.Client{Transport: identity.Transport(nil)}); status != http.StatusOK {
		t.Fatalf("signed heartbeat = %d", status)
	}
	// Every request carries its own proof: a second one is accepted too, and
	// the bearer credential without the key is refused.
	if status := heartbeat(&http.Client{Transport: identity.Transport(nil)}); status != http.StatusOK {
		t.Fatalf("second signed heartbeat = %d", status)
	}
	if status := heartbeat(http.DefaultClient); status != http.StatusUnauthorized {
		t.Fatalf("bearer-only heartbeat = %d, want 401", status)
	}
}

// A Guard killed after the enrollment response was persisted but before the
// binding was confirmed resumes at confirmation: it neither redeems the
// capability again nor loses the credential it already holds.
func TestGuardAgentResumesAtConfirmationWithoutRedeemingAgain(t *testing.T) {
	store := controlplane.NewMemoryStore()
	raw, hash, err := pairingtoken.Generate("tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	if _, err := store.UpsertPairingToken(t.Context(), controlplane.PairingToken{
		ID: "resume-pairing", TenantID: "tenant-1", OwnerSubjectID: "owner-1", StackID: "stack-1",
		TokenHash: hash, Status: "active", ExpiresAt: &expires,
	}); err != nil {
		t.Fatal(err)
	}
	router := httpx.NewRouter()
	RegisterWorkerRoutesWithStore(router, WorkerRouteConfig{Store: store, Servers: store})
	var registrations atomic.Int32
	var confirmationDown atomic.Bool
	confirmationDown.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/workers/register":
			registrations.Add(1)
		case strings.HasSuffix(r.URL.Path, "/identity/token") && confirmationDown.Load():
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		router.ServeHTTP(w, r)
	}))
	defer server.Close()

	manager, err := hostidentity.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	options := hostidentity.EnrollOptions{
		Manager: manager, EnrollmentPath: filepath.Join(t.TempDir(), "agent-enrollment.json"), ResiduePaths: []string{},
		Backoff:   []time.Duration{time.Millisecond},
		Bootstrap: hostidentity.Bootstrap{Server: server.URL, Token: raw, Hostname: "node-1", OS: "linux", Arch: "x86_64"},
	}
	if _, err := hostidentity.Enroll(t.Context(), options); err == nil {
		t.Fatal("enrollment succeeded while the control plane refused confirmation")
	}
	if record := manager.Record(); record.State != hostidentity.StateRedeeming || record.EnrollmentAttemptID != record.AttemptID {
		t.Fatalf("interrupted state = %+v, want redeeming with the response persisted", record)
	}
	if _, err := os.Stat(filepath.Join(manager.Dir(), "bootstrap.json")); err != nil {
		t.Fatalf("the capability must survive until the binding is confirmed: %v", err)
	}

	confirmationDown.Store(false)
	reopened, err := hostidentity.Open(manager.Dir())
	if err != nil {
		t.Fatal(err)
	}
	options.Manager = reopened
	result, err := hostidentity.Enroll(t.Context(), options)
	if err != nil || result.Mode != hostidentity.ModeHostKey {
		t.Fatalf("resumed enrollment = %+v, %v", result, err)
	}
	if registrations.Load() != 1 {
		t.Fatalf("the capability was redeemed %d times, want once", registrations.Load())
	}
	if capability, err := reopened.LoadBootstrap(); err != nil || capability != nil {
		t.Fatalf("the capability survived confirmation: %+v, %v", capability, err)
	}
}

// The Guard rotates its key through the real endpoint: the control plane
// accepts the new key proved by both keys, and the Guard keeps authenticating
// with the new key after it switched.
func TestGuardAgentKeyRotationKeepsTheWorkerAuthenticated(t *testing.T) {
	store := controlplane.NewMemoryStore()
	raw, hash, err := pairingtoken.Generate("tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	if _, err := store.UpsertPairingToken(t.Context(), controlplane.PairingToken{
		ID: "rotation-pairing", TenantID: "tenant-1", OwnerSubjectID: "owner-1", StackID: "stack-1",
		TokenHash: hash, Status: "active", ExpiresAt: &expires,
	}); err != nil {
		t.Fatal(err)
	}
	router := httpx.NewRouter()
	RegisterWorkerRoutesWithStore(router, WorkerRouteConfig{Store: store, Servers: store})
	server := httptest.NewServer(router)
	defer server.Close()

	manager, err := hostidentity.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	enrollmentPath := filepath.Join(t.TempDir(), "agent-enrollment.json")
	if _, err := hostidentity.Enroll(t.Context(), hostidentity.EnrollOptions{
		Manager: manager, EnrollmentPath: enrollmentPath, ResiduePaths: []string{},
		Bootstrap: hostidentity.Bootstrap{Server: server.URL, Token: raw, Hostname: "node-1", OS: "linux", Arch: "x86_64"},
	}); err != nil {
		t.Fatal(err)
	}
	enrollment, _ := os.ReadFile(enrollmentPath)
	view, _ := hostidentity.ParseEnrollment(enrollment)
	identity, err := hostidentity.NewClient(hostidentity.ClientConfig{
		Manager: manager, Endpoints: view.Endpoints, AgentToken: view.AgentToken, TenantID: view.TenantID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identity.Establish(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := manager.Record().KeyThumbprint
	if err := identity.Rotate(t.Context()); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if after := manager.Record().KeyThumbprint; after == before || manager.PendingRotation() != nil {
		t.Fatalf("rotation did not switch the key: %q -> %q (pending %v)", before, after, manager.PendingRotation() != nil)
	}
	body := `{"source_epoch":"rotation-e2e","source_sequence":1,"observed_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}`
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, view.HeartbeatURL, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+view.AgentToken)
	req.Header.Set("X-Kombify-Tenant-ID", view.TenantID)
	response, err := (&http.Client{Transport: identity.Transport(nil)}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat after rotation = %d", response.StatusCode)
	}
}

// An enrolled Guard recovers transparently within one retry after the control
// plane restarts with a new access-token signing secret, without re-enrolling
// or entering a 401 loop (platform-0oscgv).
func TestGuardAgentRecoversAcrossControlPlaneSecretRestartWithoutReenrolling(t *testing.T) {
	store := controlplane.NewMemoryStore()
	raw, hash, err := pairingtoken.Generate("tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	if _, err := store.UpsertPairingToken(t.Context(), controlplane.PairingToken{
		ID: "restart-pairing", TenantID: "tenant-1", OwnerSubjectID: "owner-1", StackID: "stack-1",
		TokenHash: hash, Status: "active", ExpiresAt: &expires,
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOMELAB_WORKER_AGENT_TOKEN_SECRET", "secret-before-control-plane-restart")
	routerBefore := httpx.NewRouter()
	RegisterWorkerRoutesWithStore(routerBefore, WorkerRouteConfig{Store: store, Servers: store})

	t.Setenv("HOMELAB_WORKER_AGENT_TOKEN_SECRET", "secret-after-control-plane-restart!")
	routerAfter := httpx.NewRouter()
	RegisterWorkerRoutesWithStore(routerAfter, WorkerRouteConfig{Store: store, Servers: store})

	var restarted atomic.Bool
	var heartbeatAttempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/heartbeat") && restarted.Load() {
			heartbeatAttempts.Add(1)
		}
		if restarted.Load() {
			routerAfter.ServeHTTP(w, r)
			return
		}
		routerBefore.ServeHTTP(w, r)
	}))
	defer server.Close()

	manager, err := hostidentity.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	enrollmentPath := filepath.Join(t.TempDir(), "agent-enrollment.json")
	if _, err := hostidentity.Enroll(t.Context(), hostidentity.EnrollOptions{
		Manager: manager, EnrollmentPath: enrollmentPath, ResiduePaths: []string{},
		Bootstrap: hostidentity.Bootstrap{Server: server.URL, Token: raw, Hostname: "node-1", OS: "linux", Arch: "x86_64"},
	}); err != nil {
		t.Fatal(err)
	}
	enrollment, _ := os.ReadFile(enrollmentPath)
	view, _ := hostidentity.ParseEnrollment(enrollment)
	identity, err := hostidentity.NewClient(hostidentity.ClientConfig{
		Manager: manager, Endpoints: view.Endpoints, AgentToken: view.AgentToken, TenantID: view.TenantID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identity.Establish(t.Context()); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: identity.Transport(nil)}
	sendHeartbeat := func(seq string) int {
		body := `{"source_epoch":"restart-e2e","source_sequence":` + seq + `,"observed_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}`
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, view.HeartbeatURL, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+view.AgentToken)
		req.Header.Set("X-Kombify-Tenant-ID", view.TenantID)
		resp, doErr := client.Do(req)
		if doErr != nil {
			t.Fatal(doErr)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if status := sendHeartbeat("1"); status != http.StatusOK {
		t.Fatalf("heartbeat before restart = %d", status)
	}

	// Simulate control-plane restart with a fresh signing secret.
	restarted.Store(true)
	if status := sendHeartbeat("2"); status != http.StatusOK {
		t.Fatalf("heartbeat after restart = %d, want 200 after single re-exchange", status)
	}
	if got := heartbeatAttempts.Load(); got != 2 {
		t.Fatalf("post-restart heartbeat attempts = %d, want 2 (1 rejected + 1 retry)", got)
	}
	if status := sendHeartbeat("3"); status != http.StatusOK {
		t.Fatalf("subsequent heartbeat after recovery = %d, want 200", status)
	}
	if got := heartbeatAttempts.Load(); got != 3 {
		t.Fatalf("subsequent heartbeat triggered extra retries: attempts = %d, want 3", got)
	}
	if state := manager.Record().State; state != hostidentity.StateBound {
		t.Fatalf("Guard identity state = %q, want %q", state, hostidentity.StateBound)
	}
}
