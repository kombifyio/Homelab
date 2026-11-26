package middleware

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/kombifyio/homelab/pkg/auth/localcloud"
	"github.com/kombifyio/homelab/pkg/identity"
)

// ApplyLocalCloudFrame runs after browser identity resolution. A frame grant
// opens only the public shell or its synthetic owner's document; it never
// authenticates an API call or permits framing another signed-in account.
func ApplyLocalCloudFrame(h http.Header, r *http.Request) {
	if r.Method != http.MethodGet || strings.HasPrefix(r.URL.Path, "/api/") {
		return
	}
	if id := identity.FromContext(r.Context()); id != nil && id.IsAuthenticated() {
		if !localcloud.Owner(id.UserID, id.OrgID) {
			return
		}
	} else {
		if r.URL.Path != "/auth/sso" || len(r.URL.Query()["local_frame"]) != 1 {
			return
		}
		referrer, err := url.Parse(r.Referer())
		if err != nil || referrer.Scheme+"://"+referrer.Host != localcloud.Origin {
			return
		}
		if _, err := localcloud.Verify(r.URL.Query().Get("local_frame"), localcloud.FrameScope); err != nil {
			return
		}
	}
	mergeFrameAncestors(h, []string{localcloud.Origin})
	h.Set("Cache-Control", "private, no-store")
	h.Add("Vary", "Cookie, Referer")
}
