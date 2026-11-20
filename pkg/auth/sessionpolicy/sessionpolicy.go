// Package sessionpolicy owns Homelab browser-session policy.
//
// Gateway LOGIN-STANDARD "One login per device": a browser session lives as
// long as it is used. The signed session token lasts one idle window (one
// year) and is re-issued (sliding) on authenticated use, at most once per
// renewal interval. There is no fixed end date: only a year without use ends
// the session (workspace AUTHENTICATION.md §6, "doubt admits"; the one
// production tenant's refresh-token idle lifetime is 31,557,599 s and it has
// no absolute lifetime, kombify-Gateway #3405).
package sessionpolicy

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kombifyio/homelab/internal/gocommon/authsession"
)

const (
	// BrowserSessionIdleLifetime is the lifetime of one session token and the
	// Max-Age of its cookie: a session unused for this long ends. It equals
	// the one production tenant's Auth0 refresh-token idle lifetime
	// (31,557,599 s, kombify-Gateway #3405); the tenant has no absolute
	// lifetime, and neither does this session.
	BrowserSessionIdleLifetime = 31_557_599 * time.Second
	// BrowserSessionRenewInterval keeps renewal to about one Set-Cookie per
	// day instead of one per request.
	BrowserSessionRenewInterval = 24 * time.Hour

	// originCookieSuffix names the companion cookie of the session pair: the
	// second signed half the wallet's local-device confirmation binds and the
	// native handoff carries. It is re-issued with the session and never caps
	// it (there is no absolute end date).
	originCookieSuffix = "_origin"
	originAudienceTag  = ":session-origin"
)

// BrowserSessionConfig returns the shared authsession config with Homelab's
// product lifetime applied.
func BrowserSessionConfig(audience string, secret []byte) authsession.Config {
	return authsession.Config{
		Audience: audience,
		Secret:   secret,
		Lifetime: BrowserSessionIdleLifetime,
	}
}

// Browser renews Homelab browser sessions and keeps their companion cookie
// in step with the session token.
type Browser struct {
	sessions   *authsession.Manager
	origins    *authsession.Manager
	sessionCfg authsession.Config
	originCfg  authsession.Config
	cookieName string
	secure     bool
	now        func() time.Time
}

// NewBrowser binds the renewal policy to the session manager built from cfg
// (see [BrowserSessionConfig]). secure mirrors the login cookie's Secure flag.
func NewBrowser(cfg authsession.Config, sessions *authsession.Manager, cookieName string, secure bool) (*Browser, error) {
	sessionCfg := cfg
	if sessionCfg.Issuer == "" {
		// The V2 session compatibility constructor supplies this legacy issuer;
		// the origin manager below intentionally keeps the shared default.
		sessionCfg.Issuer = "homelab"
	}
	originCfg := cfg
	originCfg.Audience = cfg.Audience + originAudienceTag
	originCfg.Lifetime = BrowserSessionIdleLifetime
	origins, err := authsession.NewManager(originCfg)
	if err != nil {
		return nil, err
	}
	return &Browser{
		sessions:   sessions,
		origins:    origins,
		sessionCfg: sessionCfg,
		originCfg:  originCfg,
		cookieName: strings.TrimSpace(cookieName),
		secure:     secure,
		now:        time.Now,
	}, nil
}

// Transfer is the verified identity and expiry of a browser session and of its
// companion cookie. It contains no reusable cookie credential and may be held
// in a short-lived native-login ticket after the browser has completed the
// usual OIDC flow.
type Transfer struct {
	Claims        authsession.Claims
	SessionExpiry int64
	OriginExpiry  int64
}

func (b *Browser) VerifyTransfer(r *http.Request) (Transfer, error) {
	if b == nil || r == nil {
		return Transfer{}, errors.New("browser session unavailable")
	}
	c, err := r.Cookie(b.cookieName)
	if err != nil {
		return Transfer{}, err
	}
	claims, err := b.sessions.Verify(c.Value)
	if err != nil {
		return Transfer{}, err
	}
	a, err := r.Cookie(b.originCookieName())
	if err != nil {
		return Transfer{}, err
	}
	anchor, err := b.origins.Verify(a.Value)
	if err != nil || anchor.Subject != claims.Subject || anchor.TenantID != claims.TenantID ||
		anchor.Expires <= b.now().Unix() || claims.Expires <= b.now().Unix() {
		return Transfer{}, errors.New("browser session anchor invalid")
	}
	return Transfer{Claims: *claims, SessionExpiry: claims.Expires, OriginExpiry: anchor.Expires}, nil
}

// IssueTransfer signs a new cookie pair inside the transferred idle window;
// neither half outlives the transfer it comes from.
func (b *Browser) IssueTransfer(w http.ResponseWriter, t Transfer) error {
	if b == nil || w == nil {
		return errors.New("browser session unavailable")
	}
	remaining := min(t.SessionExpiry, t.OriginExpiry) - b.now().Unix()
	if remaining <= 0 || remaining > int64(BrowserSessionIdleLifetime/time.Second) {
		return errors.New("browser session expired")
	}
	token, err := issueBefore(b.sessionCfg, t.Claims, min(t.SessionExpiry, t.OriginExpiry))
	if err != nil {
		return err
	}
	originRemaining := t.OriginExpiry - b.now().Unix()
	if originRemaining <= 0 || originRemaining > int64(BrowserSessionIdleLifetime/time.Second) {
		return errors.New("browser session companion expired")
	}
	anchor, err := issueBefore(b.originCfg,
		authsession.Claims{Subject: t.Claims.Subject, TenantID: t.Claims.TenantID}, t.OriginExpiry)
	if err != nil {
		return err
	}
	c := &http.Cookie{Name: b.cookieName, Value: token, Path: "/", HttpOnly: true,
		Secure: b.secure, SameSite: http.SameSiteLaxMode, MaxAge: int(remaining),
		Expires: time.Unix(min(t.SessionExpiry, t.OriginExpiry), 0)}
	http.SetCookie(w, c)
	http.SetCookie(w, companion(c, b.originCookieName(), anchor, int(originRemaining)))
	return nil
}

// The shared signer accepts a relative lifetime and samples its own clock.
// Verify the minted expiry before returning so scheduling delays cannot move
// either transferred credential past the original absolute deadline.
func issueBefore(cfg authsession.Config, claims authsession.Claims, deadline int64) (string, error) {
	for range 3 {
		cfg.Lifetime = time.Until(time.Unix(deadline, 0))
		if cfg.Lifetime <= 0 {
			return "", errors.New("browser session expired")
		}
		manager, err := authsession.NewManager(cfg)
		if err != nil {
			return "", err
		}
		token, err := manager.Issue(claims)
		if err != nil {
			return "", err
		}
		issued, err := manager.Verify(token)
		if err != nil {
			return "", err
		}
		if issued.Expires <= deadline {
			return token, nil
		}
	}
	return "", errors.New("could not preserve browser session expiry")
}

func (b *Browser) originCookieName() string { return b.cookieName + originCookieSuffix }

// Renew re-issues the request's cookie pair when the session is older than the
// renewal interval and still inside its idle window. Auth endpoints (they own
// the cookie), cross-site requests (the SameSite=None preview embed) and
// sessions without their companion cookie are never renewed.
func (b *Browser) Renew(w http.ResponseWriter, r *http.Request) {
	if b == nil || w == nil || r == nil || r.URL == nil || b.cookieName == "" {
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/v2/auth/") ||
		strings.HasPrefix(r.URL.Path, "/api/v1/auth/") ||
		strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		return
	}
	cookie, err := r.Cookie(b.cookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return
	}
	claims, err := b.sessions.Verify(strings.TrimSpace(cookie.Value))
	if err != nil {
		return
	}
	now := b.now()
	if claims.Expires <= now.Unix() ||
		now.Sub(time.Unix(claims.IssuedAt, 0)) < BrowserSessionRenewInterval {
		return
	}
	origin, err := r.Cookie(b.originCookieName())
	if err != nil {
		return
	}
	anchor, err := b.origins.Verify(strings.TrimSpace(origin.Value))
	if err != nil || anchor.Subject != claims.Subject || anchor.TenantID != claims.TenantID {
		return
	}
	token, err := b.sessions.Issue(*claims)
	if err != nil {
		return
	}
	// The companion half slides with the session: the pair always names the
	// same session and nothing counts from the login any more.
	companionToken, err := b.origins.Issue(authsession.Claims{Subject: claims.Subject, TenantID: claims.TenantID})
	if err != nil {
		return
	}
	// #nosec G124 -- Secure follows the deployment policy of the login cookie;
	// HttpOnly and SameSite=Lax stay mandatory.
	c := &http.Cookie{
		Name:     b.cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   b.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(BrowserSessionIdleLifetime / time.Second),
	}
	http.SetCookie(w, c)
	http.SetCookie(w, companion(c, b.originCookieName(), companionToken, int(BrowserSessionIdleLifetime/time.Second)))
}

// StampLogin rewrites a login response's session Set-Cookie: it gets a
// Max-Age equal to the token's remaining idle window, and its companion cookie
// the same idle window. A cleared session cookie (logout) also clears the
// companion. Other cookies and attributes are kept.
func (b *Browser) StampLogin(h http.Header) {
	if b == nil || h == nil || b.cookieName == "" {
		return
	}
	lines := h.Values("Set-Cookie")
	if len(lines) == 0 {
		return
	}
	out := make([]string, 0, len(lines)+1)
	var extra *http.Cookie
	for _, line := range lines {
		c, err := http.ParseSetCookie(line)
		if err != nil || c.Name != b.cookieName {
			out = append(out, line)
			continue
		}
		if c.Value == "" || c.MaxAge < 0 {
			out = append(out, line)
			extra = companion(c, b.originCookieName(), "", -1)
			continue
		}
		claims, err := b.sessions.Verify(c.Value)
		if err != nil {
			out = append(out, line)
			continue
		}
		remaining := claims.Expires - b.now().Unix()
		if remaining <= 0 {
			out = append(out, line)
			continue
		}
		anchor, err := b.origins.Issue(authsession.Claims{Subject: claims.Subject, TenantID: claims.TenantID})
		if err != nil {
			out = append(out, line)
			continue
		}
		c.MaxAge = int(remaining)
		c.Expires = time.Time{}
		out = append(out, c.String())
		extra = companion(c, b.originCookieName(), anchor, int(BrowserSessionIdleLifetime/time.Second))
	}
	if extra != nil {
		out = append(out, extra.String())
	}
	h.Del("Set-Cookie")
	for _, line := range out {
		h.Add("Set-Cookie", line)
	}
}

func companion(session *http.Cookie, name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:        name,
		Value:       value,
		Path:        "/",
		HttpOnly:    true,
		Secure:      session.Secure,
		SameSite:    session.SameSite,
		Partitioned: session.Partitioned,
		MaxAge:      maxAge,
	}
}

// WrapLogin applies [Browser.StampLogin] to a login or logout handler's
// response before its headers are written.
func (b *Browser) WrapLogin(next http.Handler) http.Handler {
	if b == nil || next == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &stampWriter{ResponseWriter: w, browser: b}
		next.ServeHTTP(sw, r)
		sw.stamp()
	})
}

type stampWriter struct {
	http.ResponseWriter
	browser *Browser
	stamped bool
}

func (s *stampWriter) stamp() {
	if s.stamped {
		return
	}
	s.stamped = true
	s.browser.StampLogin(s.ResponseWriter.Header())
}

func (s *stampWriter) WriteHeader(code int) {
	s.stamp()
	s.ResponseWriter.WriteHeader(code)
}

func (s *stampWriter) Write(p []byte) (int, error) {
	s.stamp()
	return s.ResponseWriter.Write(p)
}

func (s *stampWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
