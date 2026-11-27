package hostidentity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kombifyio/homelab/pkg/guardidentity"
)

func establishAgainst(t *testing.T, bound bool, handler http.HandlerFunc) (*Client, Mode, error) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	manager, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	if bound {
		if err := manager.SetState(StateBound, ""); err != nil {
			t.Fatal(err)
		}
	}
	client, err := NewClient(ClientConfig{
		Manager: manager, AgentToken: "credential",
		Endpoints: Endpoints{TokenURL: server.URL + "/token", BindURL: server.URL + "/bind", RotateURL: server.URL + "/rotate"},
	})
	if err != nil {
		t.Fatal(err)
	}
	mode, err := client.Establish(context.Background())
	return client, mode, err
}

// A Guard that holds a bound key is never talked back onto its bearer
// credential by a control plane that claims to know no host identity.
func TestBoundGuardRefusesToFallBackToBearerWhenIdentityIsReportedUnsupported(t *testing.T) {
	_, mode, err := establishAgainst(t, true, func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) })
	if !errors.Is(err, ErrDowngradeRefused) || mode != ModeLegacy {
		t.Fatalf("Establish = %v, %v; want ErrDowngradeRefused", mode, err)
	}
	// A server that never bound a key keeps the compatibility behavior.
	if _, _, err := establishAgainst(t, false, func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) }); err != nil {
		t.Fatalf("unbound Guard against a control plane without identity = %v", err)
	}
}

// A key the owner has not approved yet is not an error: the Guard keeps
// reporting on its bearer credential and looks again.
func TestGuardWaitsForOwnerApprovalOfItsKeyInsteadOfFailing(t *testing.T) {
	bound := false
	client, mode, err := establishAgainst(t, false, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/bind":
			bound = true
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"data":{"identity_state":"upgrade_pending"}}`))
		case bound:
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"` + guardidentity.CodeIdentityUpgradePending + `"}}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"guard_proof_invalid","details":{"reason":"` + guardidentity.ReasonIdentityUnbound + `"}}}`))
		}
	})
	if err != nil || mode != ModeLegacy || client.Mode() != ModeLegacy || !client.pendingApproval.Load() {
		t.Fatalf("Establish = %v, %v (pending=%v)", mode, err, client.pendingApproval.Load())
	}
}

// After the control plane revokes the identity the Guard destroys every secret
// that could authenticate the server: the key, the staged rotation key, a
// persisted pairing capability and the enrollment files that carry the worker
// credential. The record stays, revoked, so a restart idles.
func TestDecommissionDestroysEveryCredentialOfARevokedGuard(t *testing.T) {
	dir := t.TempDir()
	manager, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.StageRotation(); err != nil {
		t.Fatal(err)
	}
	if err := manager.SaveBootstrap(Bootstrap{Server: "https://homelab.example", Token: "kpt1.capability-that-must-not-linger"}); err != nil {
		t.Fatal(err)
	}
	enrollment := filepath.Join(t.TempDir(), "agent-enrollment.json")
	for _, path := range []string{enrollment, filepath.Join(dir, EnrollmentFileName)} {
		if err := os.WriteFile(path, []byte(`{"agent_token":"tsra.secret"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.SetState(StateRevoked, "guard_identity_revoked"); err != nil {
		t.Fatal(err)
	}

	if err := manager.Decommission(enrollment); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(dir, "identity.key"), filepath.Join(dir, "identity.key.next"), filepath.Join(dir, "bootstrap.json"),
		filepath.Join(dir, EnrollmentFileName), enrollment,
	} {
		if info, statErr := os.Stat(path); statErr == nil && info.Size() > 0 {
			t.Fatalf("%s still holds %d bytes", filepath.Base(path), info.Size())
		}
	}
	if record := manager.Record(); record.State != StateRevoked || record.KeyThumbprint != "" || manager.Key() != nil {
		t.Fatalf("record after decommission = %+v", record)
	}
	if err := manager.Decommission(enrollment); err != nil {
		t.Fatalf("repeating the decommission failed: %v", err)
	}
}

// The Guard owns its enrollment file but not the root-owned directory that
// holds it, so a revoked Guard cannot unlink it: it must still leave nothing
// readable behind.
func TestDecommissionEmptiesAnEnrollmentFileInAReadOnlyDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	manager, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	etc := t.TempDir()
	enrollment := filepath.Join(etc, "agent-enrollment.json")
	if err := os.WriteFile(enrollment, []byte(`{"agent_token":"tsra.secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(etc, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(etc, 0o700) })
	if err := manager.Decommission(enrollment); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(enrollment); err == nil && info.Size() > 0 {
		t.Fatalf("the enrollment file still holds %d bytes", info.Size())
	}
}

// A replayed or stray capability never degrades a confirmed binding: the
// redemption is refused before the control plane is asked, and the state stays
// bound (finding Q-10).
func TestEnrollLeavesABoundGuardBound(t *testing.T) {
	asked := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked = true
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	manager, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetState(StateBound, ""); err != nil {
		t.Fatal(err)
	}
	enrollment := filepath.Join(dir, "agent-enrollment.json")
	if err := os.WriteFile(enrollment, []byte(`{"agent_token":"credential"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Enroll(context.Background(), EnrollOptions{
		Manager: manager, Bootstrap: Bootstrap{Server: server.URL, Token: "consumed-capability"}, EnrollmentPath: enrollment,
	})
	if !errors.Is(err, ErrAlreadyBound) {
		t.Fatalf("enroll on a bound Guard: err = %v, want ErrAlreadyBound", err)
	}
	if asked {
		t.Fatal("a bound Guard sent its capability to the control plane")
	}
	if state := manager.Record().State; state != StateBound {
		t.Fatalf("state after a refused enroll = %q, want %q", state, StateBound)
	}
}

// A rotation the control plane accepted but whose answer was lost (a gateway
// error after the commit) must not cost the Guard its only valid key: once the
// overlap for the old key ends, the Guard still authenticates with the new one.
func TestRotationWithLostAnswerKeepsTheAcceptedKey(t *testing.T) {
	var (
		mu       sync.Mutex
		current  string
		previous string
		tokenURL string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			proof, err := guardidentity.VerifyProof(r.Header.Get(guardidentity.ProofHeader), guardidentity.ProofExpectation{Method: r.Method, URLs: []string{tokenURL}})
			if err != nil || (proof.Thumbprint != current && proof.Thumbprint != previous) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":"guard_proof_invalid","details":{"reason":"key_mismatch"}}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"access_token":"access","expires_in":600}}`))
		case "/rotate":
			var body struct {
				NewPublicKey guardidentity.JWK `json:"new_public_key"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			next, _ := body.NewPublicKey.Thumbprint()
			previous, current = current, next
			w.WriteHeader(http.StatusBadGateway)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	tokenURL = server.URL + "/token"

	manager, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	current, _ = guardidentity.PublicJWK(manager.Key()).Thumbprint()
	if err := manager.SetState(StateBound, ""); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		Manager: manager, AgentToken: "credential",
		Endpoints: Endpoints{TokenURL: tokenURL, BindURL: server.URL + "/bind", RotateURL: server.URL + "/rotate"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if mode, err := client.Establish(context.Background()); err != nil || mode != ModeHostKey {
		t.Fatalf("Establish = %v, %v", mode, err)
	}
	if err := client.Rotate(context.Background()); err == nil {
		t.Fatal("a rotation answered with a gateway error reported success")
	}

	mu.Lock()
	previous = "" // the overlap for the old key ends
	mu.Unlock()
	client.dropToken()
	if mode, err := client.Establish(context.Background()); err != nil || mode != ModeHostKey {
		t.Fatalf("Establish after the overlap = %v, %v; the accepted key was lost", mode, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if held, _ := guardidentity.PublicJWK(manager.Key()).Thumbprint(); held != current {
		t.Fatal("the Guard does not hold the key the control plane accepted")
	}
}

// After a control-plane restart or secret change rejects a cached access token
// with guard_proof_invalid (access_token_invalid) or guard_token_expired, the
// Guard drops the stale token, re-exchanges once, and retries a replayable
// request without entering a 401 loop. A non-replayable request still drops the
// stale token so the next request succeeds, and a persistent 401 stops after
// one retry (platform-0oscgv).
func TestTransportRecoversInOneRetryAfterServerRestartInvalidatesToken(t *testing.T) {
	var (
		activeToken atomic.Value
		exchanges   atomic.Int32
		calls       atomic.Int32
		rejectCode  atomic.Value
	)
	activeToken.Store("tsga.before-restart.sig")
	rejectCode.Store("invalid")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			n := exchanges.Add(1)
			tok := "tsga.token-" + string(rune('a'+n)) + ".sig"
			activeToken.Store(tok)
			_, _ = w.Write([]byte(`{"data":{"access_token":"` + tok + `","expires_in":600}}`))
			return
		}
		calls.Add(1)
		want := "DPoP " + activeToken.Load().(string)
		if r.Header.Get("Authorization") != want || rejectCode.Load().(string) == "always_invalid" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"` + guardidentity.CodeProofInvalid + `","details":{"reason":"` + guardidentity.ReasonTokenInvalid + `"}}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
	}))
	t.Cleanup(server.Close)

	manager, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetState(StateBound, ""); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		Manager: manager, AgentToken: "credential",
		Endpoints: Endpoints{TokenURL: server.URL + "/token", BindURL: server.URL + "/bind", RotateURL: server.URL + "/rotate"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if mode, err := client.Establish(context.Background()); err != nil || mode != ModeHostKey {
		t.Fatalf("Establish = %v, %v", mode, err)
	}
	httpClient := &http.Client{Transport: client.Transport(nil)}

	// Simulate server restart: previously issued token is no longer accepted.
	activeToken.Store("tsga.post-restart-mismatch.sig")
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+"/heartbeat", strings.NewReader(`{"seq":1}`))
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("heartbeat after restart: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat after restart = %d, want 200 after one re-exchange", resp.StatusCode)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("heartbeat attempts = %d, want 2 (1 rejected + 1 retry)", got)
	}
	if got := exchanges.Load(); got != 2 {
		t.Fatalf("token exchanges = %d, want 2 (initial + 1 recovery)", got)
	}

	// Non-replayable request still drops the rejected token so the next request succeeds.
	activeToken.Store("tsga.second-restart-mismatch.sig")
	nonReplayReq, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+"/heartbeat", struct{ *strings.Reader }{strings.NewReader(`{"seq":2}`)})
	nonReplayReq.GetBody = nil
	resp, err = httpClient.Do(nonReplayReq)
	if err != nil {
		t.Fatalf("non-replayable heartbeat: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("non-replayable heartbeat = %d, want 401", resp.StatusCode)
	}
	followUp, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+"/heartbeat", strings.NewReader(`{"seq":3}`))
	resp, err = httpClient.Do(followUp)
	if err != nil {
		t.Fatalf("follow-up heartbeat: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("follow-up after non-replayable 401 = %d, want 200", resp.StatusCode)
	}

	// Persistent 401 stops after at most one retry (no loop).
	calls.Store(0)
	rejectCode.Store("always_invalid")
	loopReq, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+"/heartbeat", strings.NewReader(`{"seq":4}`))
	resp, err = httpClient.Do(loopReq)
	if err != nil {
		t.Fatalf("persistent 401 heartbeat: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("persistent 401 status = %d, want 401", resp.StatusCode)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("persistent 401 attempts = %d, want exactly 2 (no loop)", got)
	}
}
