package guardidentity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kombifyio/techstack/pkg/pairingtoken"
)

func TestAppliancePairingUsesAccountAuthorityAndCurrentCapability(t *testing.T) {
	token, _, err := pairingtoken.Generate("test-appliance-tenant")
	if err != nil {
		t.Fatal(err)
	}
	for _, expires := range []time.Time{time.Now().Add(time.Minute), time.Now().Add(-time.Minute)} {
		t.Run(expires.Format(time.RFC3339Nano), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer test-account-access" {
					t.Error("pairing mint lost the account authority")
				}
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"token": token, "expires_at": expires}})
			}))
			defer server.Close()
			got, err := AppliancePairing(context.Background(), server.Client(), server.URL, "test-account-access", "mini-pc")
			if expires.After(time.Now()) {
				if err != nil || got != token {
					t.Fatalf("current capability rejected: %v", err)
				}
			} else if err == nil || got != "" {
				t.Fatal("expired capability admitted")
			}
		})
	}
}

func TestAppliancePairingNeverRedirectsTheAccountCredential(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("credential followed a redirect") }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	if got, err := AppliancePairing(context.Background(), origin.Client(), origin.URL, "test-account-access", "mini-pc"); err == nil || got != "" {
		t.Fatal("redirect admitted as a pairing")
	}
}
