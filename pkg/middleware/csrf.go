// Package middleware provides HTTP middleware components for kombifyHomelab.
package middleware

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/kombifyio/homelab/pkg/httpx"
)

const (
	// CSRFCookieName is the name of the cookie storing the CSRF token.
	CSRFCookieName = "csrf_token"

	// CSRFHeaderName is the HTTP header name for the CSRF token.
	CSRFHeaderName = "X-CSRF-Token"

	// csrfTokenLength is the byte length of generated tokens (64 hex chars).
	csrfTokenLength = 32

	// CSRFInvalidTokenCode is the machine-readable refusal of a failed
	// double-submit check. The SPA (app/src/lib/api/client.ts) drops its
	// cached token, fetches a fresh one and replays the request once on it; a
	// person therefore never meets a save that cannot succeed until reload.
	CSRFInvalidTokenCode = "csrf_token_invalid"
	// CSRFUnavailableCode names a token that could not be generated.
	CSRFUnavailableCode = "csrf_token_unavailable"
)

// CSRFMiddleware implements Double Submit Cookie CSRF protection.
// It generates a token stored in a cookie and requires the same token
// to be sent in a header for state-changing requests (POST, PUT, DELETE, PATCH).
type CSRFMiddleware struct {
	ignorePaths    []string // paths that don't need CSRF (e.g., /api/v1/health)
	ignorePrefixes []string // path prefixes that don't need CSRF (e.g., /api/v1/agent/)
	secure         bool     // use Secure cookie flag (should be true in production)
	cookiePath     string   // cookie path, defaults to "/"
	maxAge         int      // cookie max age in seconds, defaults to 12 hours
}

// CSRFConfig holds configuration for CSRF middleware.
type CSRFConfig struct {
	// Secure enables the Secure flag on the cookie (HTTPS only).
	// Should be true in production.
	Secure bool

	// IgnorePaths is a list of exact paths that bypass CSRF validation.
	// Useful for health checks, webhooks with their own auth, etc.
	IgnorePaths []string

	// IgnorePrefixes is a list of path prefixes that bypass CSRF validation.
	// Useful for agent gRPC endpoints or external integrations.
	IgnorePrefixes []string

	// CookiePath sets the path for the CSRF cookie.
	// Defaults to "/".
	CookiePath string

	// MaxAge is the cookie max age in seconds.
	// Defaults to 43200 (12 hours).
	MaxAge int
}

// DefaultCSRFConfig returns sensible defaults for CSRF protection.
func DefaultCSRFConfig() CSRFConfig {
	return CSRFConfig{
		Secure:         true,
		IgnorePaths:    []string{"/api/v1/health", "/api/health"},
		IgnorePrefixes: []string{},
		CookiePath:     "/",
		MaxAge:         43200, // 12 hours
	}
}

// NewCSRFMiddleware creates a new CSRF middleware with the given config.
func NewCSRFMiddleware(cfg CSRFConfig) *CSRFMiddleware {
	if cfg.CookiePath == "" {
		cfg.CookiePath = "/"
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = 43200
	}

	return &CSRFMiddleware{
		ignorePaths:    cfg.IgnorePaths,
		ignorePrefixes: cfg.IgnorePrefixes,
		secure:         cfg.Secure,
		cookiePath:     cfg.CookiePath,
		maxAge:         cfg.MaxAge,
	}
}

// Middleware returns the HTTP middleware handler.
func (c *CSRFMiddleware) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.handle(w, r, func() { next.ServeHTTP(w, r) })
	})
}

// HTTPXMiddleware returns the httpx router middleware equivalent of
// Middleware. It enforces CSRF only for unsafe cookie-auth browser requests and
// lets bearer/service-auth API calls keep their own authentication boundary.
func (c *CSRFMiddleware) HTTPXMiddleware() func(*httpx.Event) error {
	return func(e *httpx.Event) error {
		nextCalled := false
		c.handle(e.Response, e.Request, func() { nextCalled = true })
		if !nextCalled {
			return nil
		}
		return e.Next()
	}
}

func (c *CSRFMiddleware) handle(w http.ResponseWriter, r *http.Request, next func()) {
	// Skip for safe methods (GET, HEAD, OPTIONS, TRACE)
	if c.isSafeMethod(r.Method) {
		if err := c.ensureToken(w, r); err != nil {
			c.writeError(w, CSRFUnavailableCode, "failed to generate CSRF token", http.StatusInternalServerError)
			return
		}
		next()
		return
	}

	// Skip ignored paths
	if c.shouldIgnore(r.URL.Path) {
		next()
		return
	}

	if c.shouldBypassNonCookieAuth(r) {
		next()
		return
	}

	// Validate token for state-changing requests
	if !c.validateToken(r) {
		c.writeError(w, CSRFInvalidTokenCode, "invalid or missing CSRF token", http.StatusForbidden)
		return
	}

	next()
}

// isSafeMethod returns true for HTTP methods that don't require CSRF protection.
func (c *CSRFMiddleware) isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

// shouldIgnore returns true if the path should bypass CSRF validation.
func (c *CSRFMiddleware) shouldIgnore(path string) bool {
	// Check exact path matches
	for _, ignorePath := range c.ignorePaths {
		if path == ignorePath {
			return true
		}
	}

	// Check prefix matches
	for _, prefix := range c.ignorePrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}

	return false
}

func (c *CSRFMiddleware) shouldBypassNonCookieAuth(r *http.Request) bool {
	if IsEdgeAuthenticated(r.Context()) {
		return true
	}
	if strings.HasPrefix(strings.TrimSpace(r.Header.Get("Authorization")), "Bearer ") {
		return true
	}
	if strings.TrimSpace(r.Header.Get("X-Kombify-Service-Auth")) != "" {
		return true
	}
	return r.Header.Get("Cookie") == ""
}

// ensureToken ensures a CSRF token exists in the response.
// If no valid token cookie exists, it generates a new one.
// Returns error if token generation fails (indicates system entropy issue).
func (c *CSRFMiddleware) ensureToken(w http.ResponseWriter, r *http.Request) error {
	// Check if token already exists
	cookie, err := r.Cookie(CSRFCookieName)
	if err == nil && len(cookie.Value) == csrfTokenLength*2 {
		// Valid token exists, also set it in response header for frontend convenience
		w.Header().Set(CSRFHeaderName, cookie.Value)
		return nil
	}

	// Generate new token
	token, err := generateCSRFToken()
	if err != nil {
		return err
	}

	// Set cookie
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    token,
		Path:     c.cookiePath,
		MaxAge:   c.maxAge,
		HttpOnly: false, // Frontend needs to read this to send in header
		Secure:   c.secure,
		SameSite: http.SameSiteStrictMode,
	})

	// Also set in response header for immediate use
	w.Header().Set(CSRFHeaderName, token)
	return nil
}

// IssueCrossSiteCSRFCookie writes a fresh CSRF token cookie with
// SameSite=None; Secure. The middleware and CSRFTokenEndpoint keep an existing
// well-formed cookie, so a session that was established for a sanctioned
// cross-site portal embed carries this cookie on its later requests and the
// double-submit check works there as it does same-site. Secure is not optional:
// browsers reject SameSite=None cookies without it.
func IssueCrossSiteCSRFCookie(w http.ResponseWriter) error {
	return issueCrossSiteCSRFCookie(w, false)
}

// IssuePartitionedCSRFCookie keeps the local embed's double-submit token in
// the same browser partition as its session, even with third-party cookies blocked.
func IssuePartitionedCSRFCookie(w http.ResponseWriter) error {
	return issueCrossSiteCSRFCookie(w, true)
}

func issueCrossSiteCSRFCookie(w http.ResponseWriter, partitioned bool) error {
	token, err := generateCSRFToken()
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:        CSRFCookieName,
		Value:       token,
		Path:        "/",
		MaxAge:      43200,
		HttpOnly:    false,
		Secure:      true,
		SameSite:    http.SameSiteNoneMode,
		Partitioned: partitioned,
	})
	w.Header().Set(CSRFHeaderName, token)
	return nil
}

// validateToken validates that the CSRF token from the header matches the cookie.
func (c *CSRFMiddleware) validateToken(r *http.Request) bool {
	// Get token from cookie
	cookie, err := r.Cookie(CSRFCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}

	// Get token from header
	headerToken := r.Header.Get(CSRFHeaderName)
	if headerToken == "" {
		// Also check form field as fallback for traditional form submissions
		headerToken = r.FormValue("_csrf")
	}

	if headerToken == "" {
		return false
	}

	// Constant-time comparison to prevent timing attacks
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(headerToken)) == 1
}

// writeError writes the platform error envelope ({error:{code,message}}) so a
// client can act on the code instead of matching message text.
func (c *CSRFMiddleware) writeError(w http.ResponseWriter, code, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}

// generateCSRFToken generates a cryptographically secure random token.
// Returns an error if crypto/rand fails (indicates serious system issue).
func generateCSRFToken() (string, error) {
	b := make([]byte, csrfTokenLength)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("csrf: failed to generate random token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// CSRFTokenEndpoint returns an HTTP handler that generates and returns a CSRF token.
// This can be used as a dedicated endpoint for SPAs to obtain a token before mutations.
func CSRFTokenEndpoint(secure bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Check for existing token
		cookie, err := r.Cookie(CSRFCookieName)
		var token string
		if err == nil && len(cookie.Value) == csrfTokenLength*2 {
			token = cookie.Value
		} else {
			token, err = generateCSRFToken()
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]string{
					"error": "failed to generate CSRF token",
				})
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:     CSRFCookieName,
				Value:    token,
				Path:     "/",
				MaxAge:   43200,
				HttpOnly: false,
				Secure:   secure,
				SameSite: http.SameSiteStrictMode,
			})
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(CSRFHeaderName, token)
		json.NewEncoder(w).Encode(map[string]string{
			"token": token,
		})
	}
}
