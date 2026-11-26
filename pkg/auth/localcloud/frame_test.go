package localcloud_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kombifyio/homelab/pkg/auth/localcloud"
	"github.com/kombifyio/homelab/pkg/identity"
	"github.com/kombifyio/homelab/pkg/middleware"
)

func TestLocalFrameDoesNotTrustLoopbackReferrerOrUnsignedGrant(t *testing.T) {
	for _, referrer := range []string{localcloud.Origin + "/dashboard", "http://localhost:5174/", "https://untrusted.example/"} {
		for _, subject := range []string{"", "auth0|real"} {
			r := httptest.NewRequest(http.MethodGet, "https://homelab.kombify.io/auth/sso?local_frame=unsigned", nil)
			r.Header.Set("Referer", referrer)
			if subject != "" {
				r = r.WithContext(identity.NewContext(r.Context(), &identity.Identity{UserID: subject, OrgID: "usr:" + subject}))
			}
			h := http.Header{"Content-Security-Policy": []string{"frame-ancestors 'self' https://app.kombify.io"}}
			middleware.ApplyLocalCloudFrame(h, r)
			if strings.Contains(h.Get("Content-Security-Policy"), "localhost") || len(h.Values("Set-Cookie")) != 0 {
				t.Fatalf("unsigned frame admission: subject=%q referrer=%q", subject, referrer)
			}
		}
	}
}

func TestLocalFrameUsesOnlyAuthenticatedSyntheticOwnerAfterExchange(t *testing.T) {
	for _, tc := range []struct {
		subject, tenant, path string
		allowed               bool
	}{
		{localcloud.Subject, "usr:" + localcloud.Subject, "/dashboard", true},
		{localcloud.Subject, "org_real", "/dashboard", false},
		{"auth0|real", "usr:auth0|real", "/dashboard", false},
		{localcloud.Subject, "usr:" + localcloud.Subject, "/api/v2/whoami", false},
	} {
		r := httptest.NewRequest(http.MethodGet, "https://homelab.kombify.io"+tc.path, nil)
		r = r.WithContext(identity.NewContext(r.Context(), &identity.Identity{UserID: tc.subject, OrgID: tc.tenant}))
		h := http.Header{"Content-Security-Policy": []string{"frame-ancestors 'self' https://app.kombify.io"}}
		middleware.ApplyLocalCloudFrame(h, r)
		if strings.Contains(h.Get("Content-Security-Policy"), localcloud.Origin) != tc.allowed {
			t.Fatalf("owner admission: %+v CSP=%s", tc, h.Get("Content-Security-Policy"))
		}
		if len(h.Values("Set-Cookie")) != 0 {
			t.Fatal("frame permission created a credential")
		}
	}
}
