package routes

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/kombifyio/homelab/pkg/fulllocal"
	"github.com/kombifyio/homelab/pkg/logger"
)

// The catalogued product event of a server bound to an account
// (internal/standards-enforcement/posthog-event-catalog.json in the workspace).
// It is the "server connected" step of the workspace funnel (AP16.5,
// scripts/warehouse.mjs funnel), so it is sent where the binding happens: a
// completed pairing redemption (the embedded Homelab setup and `stackkit
// connect` on a server that already runs the public package) and an owner's
// Connect request, whether or not anyone opens a dashboard afterwards.
//
// kombify-go-common/posthog is not used: its event-name rule requires an
// underscore after the colon and refuses this catalogued name.
const serverConnectedEvent = "homelab:connected"

const defaultAnalyticsHost = "https://e.kombify.io"

// The funnel counts accounts by their Auth0 subject (<connection>|<id>), the
// identity every other product event carries; an owner of another shape (a
// local or service principal) is not an account and sends nothing.
var auth0Subject = regexp.MustCompile(`^[a-z0-9-]+\|[A-Za-z0-9_.:-]{1,128}$`)

// captureServerConnected records the binding of one server to its owner. It is
// best-effort product analytics, never a condition of the enrollment: it runs
// only in the hosted (SaaS) edition with a project key, never under Full
// Local, sends no host, address, name or token, and never blocks or fails the
// request.
func captureServerConnected(ownerSubject, leaseID string) {
	owner := strings.TrimSpace(ownerSubject)
	key := strings.TrimSpace(os.Getenv("PUBLIC_POSTHOG_KEY"))
	edition := strings.ToLower(firstNonEmptyEnv("PUBLIC_KOMBIFY_EDITION", "KOMBIFY_EDITION"))
	if !auth0Subject.MatchString(owner) || !strings.HasPrefix(key, "phc_") || !strings.HasPrefix(edition, "saas") || fulllocal.Active() {
		return
	}
	connectionClass := "self_hosted"
	if strings.TrimSpace(leaseID) != "" {
		connectionClass = "managed_vps"
	}
	properties := map[string]any{
		"connection_class":        connectionClass,
		"service":                 "kombify-homelab",
		"product":                 "kombify-homelab",
		"surface":                 "homelab",
		"env":                     analyticsEnvironment(firstNonEmptyEnv("PUBLIC_POSTHOG_ENVIRONMENT", "PUBLIC_POSTHOG_ENV")),
		"edition":                 edition,
		"correlation_id":          "srv_" + randomHex(8),
		"$process_person_profile": false,
	}
	payload := map[string]any{"api_key": key, "event": serverConnectedEvent, "distinct_id": owner, "properties": properties}
	go postServerConnected(analyticsHost(os.Getenv("PUBLIC_POSTHOG_HOST")), payload)
}

func postServerConnected(host string, payload map[string]any) {
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/capture/", bytes.NewReader(body))
	if err != nil {
		return
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		logger.Default().Warn("server_connected_capture_failed", "reason", "transport")
		return
	}
	_ = response.Body.Close()
	if response.StatusCode >= 300 {
		logger.Default().Warn("server_connected_capture_failed", "status", response.StatusCode)
	}
}

// analyticsHost mirrors the browser client (app/src/lib/analytics/posthog.ts):
// the configured origin, with the PostHog-managed hosts routed through the
// first-party proxy.
func analyticsHost(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return defaultAnalyticsHost
	}
	hostname := strings.ToLower(parsed.Hostname())
	if hostname == "posthog.com" || strings.HasSuffix(hostname, ".posthog.com") {
		return defaultAnalyticsHost
	}
	return parsed.Scheme + "://" + parsed.Host
}

// analyticsEnvironment uses the browser client's names, so both producers file
// events under the same environment.
func analyticsEnvironment(raw string) string {
	switch value := strings.ToLower(strings.TrimSpace(raw)); value {
	case "", "dev":
		return "development"
	case "prod":
		return "production"
	default:
		return value
	}
}

func randomHex(n int) string {
	buffer := make([]byte, n)
	if _, err := rand.Read(buffer); err != nil {
		return "0"
	}
	return hex.EncodeToString(buffer)
}
