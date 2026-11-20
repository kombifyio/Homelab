package routes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kombifyio/homelab/internal/gocommon/tenantdb"
	"github.com/kombifyio/homelab/internal/routes/tenantguard"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/identity"
)

func TestRequireAuthAcceptsSignedEdgeIdentity(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/v1/discovery/networks", nil)
	req = req.WithContext(identity.NewContext(context.Background(), &identity.Identity{UserID: "api-key:homelab"}))
	e := &httpx.Event{
		Request:  req,
		Response: httptest.NewRecorder(),
	}

	got, err := requireAuth(e)
	if err != nil {
		t.Fatalf("requireAuth returned error: %v", err)
	}
	if got != "api-key:homelab" {
		t.Fatalf("requireAuth returned %q, want edge identity user ID", got)
	}
}

func TestRequireAuthRejectsMissingIdentity(t *testing.T) {
	rec := httptest.NewRecorder()
	e := &httpx.Event{
		Request:  httptest.NewRequest("GET", "/api/v1/discovery/networks", nil),
		Response: rec,
	}

	got, err := requireAuth(e)
	if err == nil {
		t.Fatal("requireAuth must return an error once it has written the refusal")
	}
	if got != "" {
		t.Fatalf("requireAuth returned %q, want empty user ID", got)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("requireAuth status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestAuthenticatedUserDetectsSelfHostedOwnerRole(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/v1/features", nil)
	req = req.WithContext(identity.NewContext(context.Background(), &identity.Identity{
		UserID: "breakglass:local-owner",
		Roles:  []string{"operator", "admin"},
	}))
	e := &httpx.Event{
		Request:  req,
		Response: httptest.NewRecorder(),
	}

	got, isOwner, ok := authenticatedUser(e)
	if !ok {
		t.Fatal("authenticatedUser rejected identity")
	}
	if got != "breakglass:local-owner" {
		t.Fatalf("authenticatedUser returned %q, want identity user ID", got)
	}
	if !isOwner {
		t.Fatal("authenticatedUser did not detect the self-hosted owner")
	}
}

// A kombify platform role is never owner authority: not on a self-hosted
// installation beyond its own local "admin" membership, and not at all in the
// hosted product.
func TestAuthenticatedUserIgnoresPlatformRoles(t *testing.T) {
	platformRoles := []string{"super_admin", "global_admin", "developer", "manager"}
	for _, hosted := range []bool{false, true} {
		tenantguard.Configure(hosted)
		t.Cleanup(func() { tenantguard.Configure(false) })
		roleSets := [][]string{platformRoles}
		if hosted {
			roleSets = append(roleSets, []string{"admin"}, []string{"admin", "global_admin"})
		}
		for _, roles := range roleSets {
			req := httptest.NewRequest("GET", "/api/v1/features", nil)
			req = req.WithContext(identity.NewContext(context.Background(), &identity.Identity{
				UserID: "auth0|platform-user",
				Roles:  roles,
			}))
			e := &httpx.Event{Request: req, Response: httptest.NewRecorder()}

			_, isOwner, ok := authenticatedUser(e)
			if !ok {
				t.Fatal("authenticatedUser rejected identity")
			}
			if isOwner {
				t.Fatalf("hosted=%v roles=%v counted as instance owner", hosted, roles)
			}
		}
	}
}

// A lease read or write whose Edge-signed database context names another
// subject or organization than the tenant it asks for is the caller's missing
// reach. Every monthly-runtime route maps it to a structured 403, never to the
// 502 of a runtime failure.
func TestMonthlyRuntimeErrorRefusesTenantContextMismatch(t *testing.T) {
	mismatch := fmt.Errorf("lease read: %w", tenantdb.ErrContextMismatch)
	for name, mapper := range map[string]func(*httpx.Event) error{
		"operation error":    func(e *httpx.Event) error { return monthlyRuntimeError(e, mismatch) },
		"decommission error": func(e *httpx.Event) error { return monthlyRuntimeDecommissionError(e, nil, mismatch, "lease-1") },
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			e := &httpx.Event{Request: httptest.NewRequest(http.MethodGet, "/api/v1/monthly-runtimes/lease-1", nil), Response: rec}
			_ = mapper(e)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Error struct {
					Details map[string]any `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if got := body.Error.Details["reason_code"]; got != "tenant_context_mismatch" {
				t.Fatalf("reason_code = %v, want tenant_context_mismatch", got)
			}
		})
	}
}
