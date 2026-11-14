package httpx

import (
	"context"
	"net/http"
	"strings"

	"github.com/kombifyio/techstack/pkg/api"
)

// Session renewal signal (workspace platform/standards/AUTHENTICATION.md §6,
// "Session renewal, never a sign-in error"). A signed-in user whose session
// needs renewal never gets a sign-in error: the server answers with this
// signal before any side effect, and the web client's shared layer
// (app/src/lib/auth/session-renewal.svelte.ts) renews the session in place and
// replays the request once.
const (
	// SessionRenewalHeader is set to SessionRenewalHeaderValue on every
	// renewal response.
	SessionRenewalHeader      = "X-Kombify-Session-Renewal"
	SessionRenewalHeaderValue = "required"
	// SessionRenewalErrorCode is the machine code on the response body.
	SessionRenewalErrorCode api.ErrorCode = "session_renewal_required"
	// SessionRenewalURL is the same-origin renewal route the client opens in
	// a small window (or, as a last resort, the whole tab).
	SessionRenewalURL = "/auth/renew"
)

// Reasons a session needs renewal. They name the cause for telemetry and for
// the renewal route; none of them is worded to the user as a sign-in problem.
const (
	// SessionRenewalReasonSessionExpired: a session cookie was present but no
	// longer yields a session (expired, rotated key, unreadable).
	SessionRenewalReasonSessionExpired = "session_expired"
	// SessionRenewalReasonSessionReprojection: a signature-valid session whose
	// tenant projection could not be recovered server-side.
	SessionRenewalReasonSessionReprojection = "session_reprojection"
	// SessionRenewalReasonUpstreamBearerRejected: an upstream service
	// answered 401 for the user's bearer this request forwarded.
	SessionRenewalReasonUpstreamBearerRejected = "upstream_bearer_rejected"
)

type sessionRenewalContextKey struct{}

// WithSessionRenewalReason marks a request whose presented session no longer
// authenticates. Any 401 written for that request becomes the renewal signal
// instead of a plain authentication error; a request without the mark (a
// worker, an agent, an anonymous caller) keeps its ordinary 401.
func WithSessionRenewalReason(ctx context.Context, reason string) context.Context {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionRenewalContextKey{}, reason)
}

func sessionRenewalReasonFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	reason, _ := r.Context().Value(sessionRenewalContextKey{}).(string)
	return reason
}

// NewSessionRenewalError returns the APIError a handler or middleware returns
// for a session that needs renewal. Extra details are merged into the body's
// error details; the contract fields always win.
func NewSessionRenewalError(reason string, extra map[string]any) *APIError {
	details := map[string]any{}
	for key, value := range extra {
		details[key] = value
	}
	details["error_code"] = string(SessionRenewalErrorCode)
	details["reason"] = reason
	details["renew_url"] = SessionRenewalURL
	details["retryable"] = true
	return NewAPIError(http.StatusUnauthorized, SessionRenewalErrorCode, "Session renewal required", details)
}

// sessionRenewalResponse is the standard error envelope plus the contract's
// top-level fields, so both the Techstack client and any platform client read
// the same signal.
type sessionRenewalResponse struct {
	Error     api.ErrorDetail `json:"error"`
	ErrorCode api.ErrorCode   `json:"error_code"`
	Reason    string          `json:"reason"`
	RenewURL  string          `json:"renew_url"`
}

// sessionRenewalFor decides whether a 401 is the renewal signal and with
// which reason: an explicit renewal error, or any 401 for a request whose
// presented session lapsed.
func sessionRenewalFor(e *Event, status int, code api.ErrorCode, details any) (string, bool) {
	if status != http.StatusUnauthorized {
		return "", false
	}
	if code == SessionRenewalErrorCode {
		if fields, ok := details.(map[string]any); ok {
			if reason, ok := fields["reason"].(string); ok && strings.TrimSpace(reason) != "" {
				return reason, true
			}
		}
		return SessionRenewalReasonSessionExpired, true
	}
	if e == nil {
		return "", false
	}
	if reason := sessionRenewalReasonFromRequest(e.Request); reason != "" {
		return reason, true
	}
	return "", false
}

// writeSessionRenewal renders the signal. An explicit renewal error keeps its
// own details (guidance, telemetry codes); a marked 401 gets the contract
// fields only.
func writeSessionRenewal(e *Event, reason string, code api.ErrorCode, details any) error {
	if code != SessionRenewalErrorCode || details == nil {
		details = NewSessionRenewalError(reason, nil).Details
	}
	e.Response.Header().Set(SessionRenewalHeader, SessionRenewalHeaderValue)
	e.Response.Header().Set("Cache-Control", "no-store")
	return e.JSON(http.StatusUnauthorized, sessionRenewalResponse{
		Error: api.ErrorDetail{
			Code:    SessionRenewalErrorCode,
			Message: "Session renewal required",
			Details: details,
		},
		ErrorCode: SessionRenewalErrorCode,
		Reason:    reason,
		RenewURL:  SessionRenewalURL,
	})
}
