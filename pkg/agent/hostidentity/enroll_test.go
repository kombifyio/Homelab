package hostidentity_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kombifyio/techstack/pkg/agent/hostidentity"
	"github.com/kombifyio/techstack/pkg/guardidentity"
)

func TestEnrollmentDiscardsRejectedCapabilityButRetainsTransientRecovery(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		status      int
		replacement bool
	}{
		{"expired", http.StatusUnauthorized, false},
		{"rejected", http.StatusForbidden, false},
		{"transient", http.StatusServiceUnavailable, false},
		{"replacement-before-old-rejection", http.StatusUnauthorized, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			manager, err := hostidentity.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			const capability = "kpt1.expiring-fixture-capability"
			const replacement = "kpt1.replacement-fixture-capability"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scenario.replacement {
					// A second process can persist its new capability while the
					// old request is still awaiting the control-plane response.
					other, err := hostidentity.Open(manager.Dir())
					if err == nil {
						err = other.SaveBootstrap(hostidentity.Bootstrap{Server: "http://" + r.Host, Token: replacement})
					}
					if err != nil {
						t.Errorf("persist replacement: %v", err)
					}
				}
				w.WriteHeader(scenario.status)
			}))
			defer server.Close()
			residue := filepath.Join(t.TempDir(), "user-data")
			if err := os.WriteFile(residue, []byte("KOMBI_TOKEN="+capability+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err = hostidentity.Enroll(context.Background(), hostidentity.EnrollOptions{
				Manager: manager, Bootstrap: hostidentity.Bootstrap{Server: server.URL, Token: capability},
				HTTPClient: server.Client(), Backoff: []time.Duration{}, ResiduePaths: []string{residue},
			})
			if err == nil {
				t.Fatal("unsuccessful redemption was accepted")
			}
			transient := scenario.status == http.StatusServiceUnavailable
			if errors.Is(err, hostidentity.ErrRecoveryNeeded) == transient {
				t.Fatalf("wrong recovery classification: %v", err)
			}
			reopened, err := hostidentity.Open(manager.Dir())
			if err != nil {
				t.Fatal(err)
			}
			if reopened.CanResume(server.URL, capability) != transient {
				t.Fatal("restart did not preserve the appropriate capability recovery behavior")
			}
			bootstrap, err := reopened.LoadBootstrap()
			if err != nil || (bootstrap != nil) != (transient || scenario.replacement) {
				t.Fatalf("wrong retained bootstrap state: %v", err)
			}
			if scenario.replacement && bootstrap.Token != replacement {
				t.Fatal("old rejection destroyed the replacement capability")
			}
			remaining, err := os.ReadFile(residue)
			if err != nil || strings.Contains(string(remaining), capability) != transient {
				t.Fatalf("wrong cloud-init capability retention: %v", err)
			}
		})
	}
}

// A redemption that is answered but then refused at confirmation (the owner
// revoked the unconfirmed worker) leaves a capability that can never bind this
// host: neither the Guard's copy nor the cloud-init residue may survive it.
func TestEnrollmentRefusedAtConfirmationDiscardsTheCapability(t *testing.T) {
	manager, err := hostidentity.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const capability = "kpt1.revoked-before-confirmation-capability"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/workers/register":
			_, _ = w.Write([]byte(`{"data":{"worker_id":"worker-1","agent_token":"agent-credential","tenant_id":"tenant-1","accepted":true,` +
				`"heartbeat_url":"http://` + r.Host + `/api/v1/workers/worker-1/heartbeat"}}`))
		default:
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"guard_identity_revoked","details":{}}}`))
		}
	}))
	defer server.Close()
	residue := filepath.Join(t.TempDir(), "user-data")
	if err := os.WriteFile(residue, []byte("KOMBI_TOKEN="+capability+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = hostidentity.Enroll(context.Background(), hostidentity.EnrollOptions{
		Manager: manager, Bootstrap: hostidentity.Bootstrap{Server: server.URL, Token: capability},
		EnrollmentPath: filepath.Join(t.TempDir(), "agent-enrollment.json"),
		HTTPClient:     server.Client(), Backoff: []time.Duration{}, ResiduePaths: []string{residue},
	})
	if !errors.Is(err, hostidentity.ErrRevoked) {
		t.Fatalf("refused confirmation = %v, want revoked", err)
	}
	reopened, err := hostidentity.Open(manager.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap, err := reopened.LoadBootstrap(); err != nil || bootstrap != nil {
		t.Fatalf("the refused capability is still held by the Guard: %v", err)
	}
	if reopened.CanResume(server.URL, capability) {
		t.Fatal("a restart would redeem the refused capability again")
	}
	remaining, err := os.ReadFile(residue)
	if err != nil || strings.Contains(string(remaining), capability) {
		t.Fatalf("the refused capability survived in cloud-init residue: %v", err)
	}
}

// tenantRebindFixture is a Guard of the bare-subject legacy tenant
// google-oauth2|1 whose enrollment names the subject as owner and pins a
// binding key, with the same enrollment in the configured path and the state
// directory copy.
func tenantRebindFixture(t *testing.T, extra map[string]any) (*hostidentity.Manager, string, guardidentity.TenantRebindOffer) {
	t.Helper()
	stateDir := filepath.Join(t.TempDir(), "guard")
	manager, err := hostidentity.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	enrollment := map[string]any{
		"runtime_agent_id": "worker_1", "tenant_id": "google-oauth2|1", "owner_id": "google-oauth2|1",
		"agent_token":          "tsra.opaque.legacy",
		"heartbeat_url":        "https://techstack.example/api/v1/workers/worker_1/heartbeat",
		"binding_signing_keys": []any{map[string]any{"key_id": "aaaaaaaaaaaaaaaa", "public_key": "legacy"}},
		"channel_bootstrap":    map[string]any{"tenant_id": "google-oauth2|1", "owner_id": "google-oauth2|1"},
	}
	for key, value := range extra {
		enrollment[key] = value
	}
	raw, err := json.Marshal(enrollment)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "agent-enrollment.json")
	for _, target := range []string{path, filepath.Join(stateDir, hostidentity.EnrollmentFileName)} {
		if err := os.WriteFile(target, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	id, err := guardidentity.NewTenantRebindDirectiveID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	offer := guardidentity.TenantRebindOffer{
		Directive: guardidentity.TenantRebindDirective{
			ContractVersion: guardidentity.TenantRebindContractVersion, DirectiveID: id,
			FromTenantID: "google-oauth2|1", ToTenantID: "usr:google-oauth2|1", SubjectID: "google-oauth2|1",
			RuntimeAgentID: "worker_1", ServerID: "server_1", CredentialGeneration: 1,
			IssuedAt: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		},
		AgentToken:         "tsra.opaque.canonical",
		BindingSigningKeys: []guardidentity.TenantRebindPinnedKey{{KeyID: "bbbbbbbbbbbbbbbb", PublicKey: "canonical"}},
	}
	return manager, path, offer
}

func TestTenantRebindAppliesOnlyAfterProofAndMovesEveryEnrollmentCopyAndJournal(t *testing.T) {
	manager, path, offer := tenantRebindFixture(t, nil)
	if err := manager.StageTenantRebind(offer, "auth0|someone", "worker_1"); err == nil {
		t.Fatal("an offer for another tenant was stored")
	}
	if err := manager.StageTenantRebind(offer, "google-oauth2|1", "worker_1"); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if manager.StagedTenantRebindID() != offer.Directive.DirectiveID {
		t.Fatal("the stored offer is not acknowledged")
	}
	if receipt, err := manager.ApplyReadyTenantRebind(path, nil); err != nil || receipt != nil {
		t.Fatalf("an offer was applied before it was proven: %v %v", receipt, err)
	}
	if err := manager.MarkTenantRebindReady(offer.Directive.DirectiveID); err != nil || !manager.TenantRebindReady() {
		t.Fatalf("a proven offer did not become ready: %v", err)
	}
	select {
	case <-manager.TenantRebindReadyNotify():
	default:
		t.Fatal("readiness was not signalled")
	}
	var relabeled [2]string
	receipt, err := manager.ApplyReadyTenantRebind(path, func(from, to string) (int, error) {
		relabeled = [2]string{from, to}
		return 3, nil
	})
	if err != nil || receipt == nil || receipt.JournalEntries != 3 {
		t.Fatalf("apply: %v %v", receipt, err)
	}
	if relabeled != [2]string{"google-oauth2|1", "usr:google-oauth2|1"} {
		t.Fatalf("journal relabel = %v", relabeled)
	}
	for _, copy := range []string{path, filepath.Join(manager.Dir(), hostidentity.EnrollmentFileName)} {
		raw, err := os.ReadFile(copy)
		if err != nil {
			t.Fatal(err)
		}
		view, err := hostidentity.ParseEnrollment(raw)
		if err != nil || view.TenantID != "usr:google-oauth2|1" || view.AgentToken != "tsra.opaque.canonical" {
			t.Fatalf("%s: rewritten enrollment = %+v, %v", copy, view, err)
		}
		var document map[string]any
		_ = json.Unmarshal(raw, &document)
		if document["owner_id"] != "google-oauth2|1" || strings.Contains(string(raw), `"legacy"`) ||
			!strings.Contains(string(raw), `"canonical"`) {
			t.Fatalf("%s: owner or binding keys were not carried over: %s", copy, raw)
		}
	}
	if manager.StagedTenantRebindID() != "" {
		t.Fatal("the applied offer was kept")
	}
}

func TestTenantRebindRefusesAnEnrollmentWithAnotherLegacyTenantBinding(t *testing.T) {
	manager, path, offer := tenantRebindFixture(t, map[string]any{"household_tenant_id": "google-oauth2|1"})
	if err := manager.StageTenantRebind(offer, "google-oauth2|1", "worker_1"); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if err := manager.MarkTenantRebindReady(offer.Directive.DirectiveID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ApplyReadyTenantRebind(path, nil); !errors.Is(err, guardidentity.ErrTenantRebindRefused) {
		t.Fatalf("apply err = %v, want ErrTenantRebindRefused", err)
	}
	raw, _ := os.ReadFile(path)
	if view, _ := hostidentity.ParseEnrollment(raw); view.TenantID != "google-oauth2|1" {
		t.Fatal("a refused rewrite changed the enrollment")
	}
}
