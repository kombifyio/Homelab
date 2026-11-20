package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kombifyio/homelab/internal/routes/tenantguard"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/identity"
)

func TestRequireMetricsAccessAcceptsSelfHostedOwner(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req = req.WithContext(identity.NewContext(context.Background(), &identity.Identity{
		UserID: "api-key:metrics-scraper",
		Roles:  []string{"admin"},
	}))
	e := &httpx.Event{
		Request:  req,
		Response: httptest.NewRecorder(),
	}

	if err := requireMetricsAccess(e); err != nil {
		t.Fatalf("requireMetricsAccess returned error for the self-hosted owner: %v", err)
	}
}

func TestRequireMetricsAccessRejectsNonOwner(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req = req.WithContext(identity.NewContext(context.Background(), &identity.Identity{
		UserID: "api-key:tenant-user",
		Roles:  []string{"operator"},
	}))
	e := &httpx.Event{
		Request:  req,
		Response: rec,
	}

	// This asserted nil and so codified the bypass: the guard wrote 403, the
	// handler saw no error and served /metrics anyway. A rejection must be
	// reported as an error, not only as a recorder status.
	if err := requireMetricsAccess(e); err == nil {
		t.Fatalf("requireMetricsAccess must return an error for a non-admin, so the handler stops")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("requireMetricsAccess status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

// The hosted product has no instance owner: a platform role does not open the
// instance-wide metrics.
func TestRequireMetricsAccessRefusesPlatformRolesWhenHosted(t *testing.T) {
	tenantguard.Configure(true)
	t.Cleanup(func() { tenantguard.Configure(false) })
	for _, role := range []string{"admin", "super_admin", "global_admin", "developer", "manager"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		req = req.WithContext(identity.NewContext(context.Background(), &identity.Identity{
			UserID: "auth0|platform-" + role,
			Roles:  []string{role},
		}))
		e := &httpx.Event{Request: req, Response: rec}
		if err := requireMetricsAccess(e); err == nil {
			t.Fatalf("platform role %q must not open /metrics in the hosted product", role)
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("platform role %q status = %d, want %d", role, rec.Code, http.StatusForbidden)
		}
	}
}
