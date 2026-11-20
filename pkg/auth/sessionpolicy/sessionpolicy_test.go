package sessionpolicy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kombifyio/homelab/internal/gocommon/authsession"
)

// platform-jx5m6: the 7-day fixed session forced a new login every week; the
// owner rule is one login per device with an idle window only, so a session
// ends after a year without use and has no fixed end date (AUTHENTICATION.md
// §6, "doubt admits").
func TestBrowserSessionRenewsActiveAndNotIdleExpired(t *testing.T) {
	cfg := BrowserSessionConfig("homelab-test", []byte("0123456789abcdef0123456789abcdef"))
	mgr, err := authsession.NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	browser, err := NewBrowser(cfg, mgr, "homelab_session", true)
	if err != nil {
		t.Fatal(err)
	}
	token, err := mgr.Issue(authsession.Claims{Subject: "auth0|user-1", TenantID: "tenant-1"})
	if err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRecorder()
	authsession.SetSessionCookie(login, "homelab_session", token, true)
	browser.StampLogin(login.Header())
	loginCookies := login.Result().Cookies()

	renewAt := func(offset time.Duration) []*http.Cookie {
		browser.now = func() time.Time { return time.Now().Add(offset) }
		req := httptest.NewRequest(http.MethodGet, "/api/v2/whoami", nil)
		for _, c := range loginCookies {
			req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
		}
		rec := httptest.NewRecorder()
		browser.Renew(rec, req)
		return rec.Result().Cookies()
	}

	if got := renewAt(time.Hour); len(got) != 0 {
		t.Fatalf("renewed within the renewal interval: %v", got)
	}
	renewed := renewAt(25 * time.Hour)
	// The session and its companion cookie are issued together: the pair is
	// what the wallet's local-device confirmation binds.
	var session, anchor *http.Cookie
	for _, c := range renewed {
		switch c.Name {
		case "homelab_session":
			session = c
		case "homelab_session_origin":
			anchor = c
		}
	}
	if session == nil || anchor == nil {
		t.Fatalf("active session was not renewed as a pair: %v", renewed)
	}
	if got, want := session.MaxAge, int(BrowserSessionIdleLifetime/time.Second); got != want {
		t.Fatalf("renewed cookie Max-Age = %d, want %d", got, want)
	}
	if !session.HttpOnly || !session.Secure || session.SameSite != http.SameSiteLaxMode {
		t.Fatalf("renewed cookie lost its security flags: %+v", session)
	}
	if anchor.MaxAge < session.MaxAge {
		t.Fatalf("companion cookie Max-Age = %d, want it to keep up with the session (%d)", anchor.MaxAge, session.MaxAge)
	}
	claims, err := mgr.Verify(session.Value)
	if err != nil || claims.Subject != "auth0|user-1" || claims.TenantID != "tenant-1" {
		t.Fatalf("renewed token does not carry the session identity: %+v, %v", claims, err)
	}
	anchorClaims, err := browser.origins.Verify(anchor.Value)
	if err != nil || anchorClaims.Subject != "auth0|user-1" || anchorClaims.TenantID != "tenant-1" {
		t.Fatalf("renewed companion does not carry the session identity: %+v, %v", anchorClaims, err)
	}
	// A year without use ends the session; a year of use never does.
	if got := renewAt(BrowserSessionIdleLifetime + time.Hour); len(got) != 0 {
		t.Fatalf("idle-expired session was renewed: %v", got)
	}
}
