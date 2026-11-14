package routes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kombifyio/techstack/internal/gocommon/tenantdb"
	"github.com/kombifyio/techstack/pkg/httpx"
	"github.com/kombifyio/techstack/pkg/identity"
)

func TestRequireAuthAcceptsSignedEdgeIdentity(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/v1/discovery/networks", nil)
	req = req.WithContext(identity.NewContext(context.Background(), &identity.Identity{UserID: "api-key:techstack"}))
	e := &httpx.Event{
		Request:  req,
		Response: httptest.NewRecorder(),
	}

	got, err := requireAuth(e)
	if err != nil {
		t.Fatalf("requireAuth returned error: %v", err)
	}
	if got != "api-key:techstack" {
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

func TestAuthenticatedUserDetectsSignedEdgeAdminRole(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/v1/features", nil)
	req = req.WithContext(identity.NewContext(context.Background(), &identity.Identity{
		UserID: "api-key:techstack-admin",
		Roles:  []string{"operator", "admin"},
	}))
	e := &httpx.Event{
		Request:  req,
		Response: httptest.NewRecorder(),
	}

	got, isAdmin, ok := authenticatedUser(e)
	if !ok {
		t.Fatal("authenticatedUser rejected edge identity")
	}
	if got != "api-key:techstack-admin" {
		t.Fatalf("authenticatedUser returned %q, want edge identity user ID", got)
	}
	if !isAdmin {
		t.Fatal("authenticatedUser did not detect signed edge admin role")
	}
}

func TestAuthenticatedUserDetectsSignedEdgeGlobalAdminRole(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/v1/features", nil)
	req = req.WithContext(identity.NewContext(context.Background(), &identity.Identity{
		UserID: "api-key:techstack-global-admin",
		Roles:  []string{"global_admin"},
	}))
	e := &httpx.Event{
		Request:  req,
		Response: httptest.NewRecorder(),
	}

	got, isAdmin, ok := authenticatedUser(e)
	if !ok {
		t.Fatal("authenticatedUser rejected edge identity")
	}
	if got != "api-key:techstack-global-admin" {
		t.Fatalf("authenticatedUser returned %q, want edge identity user ID", got)
	}
	if !isAdmin {
		t.Fatal("authenticatedUser did not detect signed edge global_admin role")
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
