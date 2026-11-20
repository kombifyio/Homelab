// Package sessionreauth is the single response classifier for the
// "signature-valid session whose identity/tenant projection cannot resolve"
// class (owner rule 2026-08-12: tools recover sessions themselves, users are
// never asked to sign in again).
//
// When server-side re-projection (claims re-derivation, owner-tenant
// fallback, owner/demo bootstrap) cannot recover the session, every surface
// answers with the platform session renewal signal (workspace
// platform/standards/AUTHENTICATION.md §6, "Session renewal, never a sign-in
// error"): 401, header x-kombify-session-renewal: required, error_code
// session_renewal_required with reason session_reprojection and renew_url. It
// also clears the v2 session cookie, so the client's shared renewal layer
// re-mints the session and replays the request once. No route may hand-roll
// its own dead-end 500/403 for this class.
package sessionreauth

import (
	"strings"
	"sync"

	"github.com/kombifyio/homelab/internal/gocommon/authsession"

	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/logger"
)

// ReasonCode is the renewal reason this class carries on the platform
// session renewal signal (and the telemetry reason_code).
const ReasonCode = httpx.SessionRenewalReasonSessionReprojection

// RecoveredMessage is the structured-log message emitted when server-side
// re-projection repaired the session without any client involvement.
const RecoveredMessage = "session_reprojection_recovered"

var state struct {
	mu         sync.RWMutex
	cookieName string
	secure     bool
	counter    func(tenantID, outcome string)
}

// Configure sets the v2 session cookie parameters once at route wiring so
// Denial can clear the cookie with the same attributes the login flow set.
func Configure(cookieName string, secure bool) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.cookieName = cookieName
	state.secure = secure
}

// SetCounterHook installs the metrics sink (see routes.RegisterMetricsRoutes).
// The hook receives the tenant label and the outcome ("recovered" or
// "reauth_required") for homelab_auth_session_reprojections_total.
func SetCounterHook(hook func(tenantID, outcome string)) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.counter = hook
}

func snapshot() (string, bool, func(tenantID, outcome string)) {
	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.cookieName, state.secure, state.counter
}

// RecordRecovered emits the telemetry for an occurrence that server-side
// re-projection repaired in place: structured log, tenant-labeled counter,
// and an info-level Sentry event so incident rate spikes stay visible.
func RecordRecovered(e *httpx.Event, fromTenant, toTenant, rung string) {
	_, _, counter := snapshot()
	logger.Get().Warn(RecoveredMessage,
		"reason_code", ReasonCode,
		"from_tenant_id", fromTenant,
		"to_tenant_id", toTenant,
		"recovery_rung", rung,
		"path", requestPath(e),
	)
	if counter != nil {
		counter(tenantLabel(fromTenant), "recovered")
	}
	captureSentry(e, "info", RecoveredMessage, map[string]string{
		"reason_code":    ReasonCode,
		"from_tenant_id": tenantLabel(fromTenant),
		"to_tenant_id":   tenantLabel(toTenant),
		"recovery_rung":  rung,
	})
}

// Denial is the terminal classifier response: the session is signature-valid
// but unrecoverable server-side. It clears the v2 session cookie, emits the
// structured log + tenant-labeled counter + warning-level Sentry event, and
// returns the session renewal signal the shared client layer renews on.
func Denial(e *httpx.Event, tenantID string, cause error) *httpx.APIError {
	cookieName, secure, counter := snapshot()
	if e != nil && e.Response != nil && cookieName != "" {
		authsession.ClearSessionCookie(e.Response, cookieName, secure)
	}
	causeText := ""
	if cause != nil {
		causeText = cause.Error()
	}
	logger.Get().Warn(ReasonCode,
		"reason_code", ReasonCode,
		"tenant_id", tenantID,
		"path", requestPath(e),
		"error", causeText,
	)
	if counter != nil {
		counter(tenantLabel(tenantID), "reauth_required")
	}
	captureSentry(e, "warning", ReasonCode, map[string]string{
		"reason_code": ReasonCode,
		"tenant_id":   tenantLabel(tenantID),
	})
	return httpx.NewSessionRenewalError(ReasonCode, map[string]any{
		"reason_code": ReasonCode,
		"user_guidance": map[string]any{
			"title": "Renewing your session",
			"body":  "Your session is being renewed after a platform update. Nothing was changed and no data was lost; your action continues once the renewal completes.",
			"next_steps": []string{
				"Wait a moment while the session renews",
			},
		},
		"remediation":     "The client renews through renew_url (SSO re-entry re-mints the projection) and replays the request once.",
		"support_context": map[string]any{"feature_source": "v2-session/tenant-projection", "cost_bearing": false},
	})
}

func requestPath(e *httpx.Event) string {
	if e == nil || e.Request == nil || e.Request.URL == nil {
		return ""
	}
	return e.Request.URL.Path
}

func tenantLabel(tenantID string) string {
	if tenantID = strings.TrimSpace(tenantID); tenantID != "" {
		return tenantID
	}
	return "unknown"
}
