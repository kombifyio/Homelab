// Package notifications proxies a Homelab user's notification center to the
// kombify-Notifications engine. Homelab's SvelteKit layer has no server-side
// identity, so the Go backend resolves the authenticated user and signs the
// engine call itself (EdDSA service JWT, iss kombify-homelab, aud
// kombify-notifications, scope notifications:*), forwarding the recipient as the
// auth0_user_id query param the engine expects (desk parity).
package notifications

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kombifyio/homelab/internal/gocommon/servicecall"
	"github.com/kombifyio/homelab/pkg/config"
	"github.com/kombifyio/homelab/pkg/serviceauth"
)

const (
	defaultEngineURL = "https://kombify-notifications-6rm24ekzwa-ey.a.run.app"
	issuer           = "kombify-homelab"
	audience         = "kombify-notifications"
	tokenTTL         = 5 * time.Minute
	requestTimeout   = 12 * time.Second
	maxResponseBytes = 1 << 20
)

// ErrNotConfigured is returned when signing or the destination is not configured.
// Handlers map it to 503.
var ErrNotConfigured = errors.New("notifications engine not configured")

// Engine is a thin, signed HTTP client for the notifications engine.
type Engine struct {
	baseURL       string
	secret        string
	signingKey    ed25519.PrivateKey
	signingKeyID  string
	signingKeyErr error
	http          *http.Client
}

// SigningKeyEnv holds the Ed25519 service key ("<kid>=<seed>") this client
// signs with when a deployment withholds the process-wide
// SERVICE_AUTH_ED25519_PRIVATE_KEY so no other client signs as homelab there.
// No admitted deployment withholds it; the name stays with the engine.
const SigningKeyEnv = "HOMELAB_NOTIFICATIONS_ED25519_PRIVATE_KEY"

// NewEngineFromEnv builds an Engine from NOTIFICATIONS_ENGINE_URL and
// SERVICE_AUTH_SECRET. Hosted editions default to the live origin; self-hosted
// editions require an explicit destination.
func NewEngineFromEnv() *Engine {
	base := strings.TrimSpace(os.Getenv("NOTIFICATIONS_ENGINE_URL"))
	if base == "" {
		mode, err := config.BootDeploymentModeFromEnv()
		if err == nil && mode.IsSaaS() {
			base = defaultEngineURL
		}
	}
	base = strings.TrimRight(base, "/")
	base = strings.TrimSuffix(base, "/v1")
	e := &Engine{
		baseURL: base,
		secret:  strings.TrimSpace(os.Getenv("SERVICE_AUTH_SECRET")),
		http:    &http.Client{Timeout: requestTimeout},
	}
	if raw := strings.TrimSpace(os.Getenv(SigningKeyEnv)); raw != "" {
		e.signingKeyID, e.signingKey, e.signingKeyErr = servicecall.ParseSigningKey(raw)
		if e.signingKeyErr != nil {
			e.signingKeyErr = fmt.Errorf("%s: %w", SigningKeyEnv, e.signingKeyErr)
		}
	}
	return e
}

// Configured reports whether a signing secret or Ed25519 key is present. A
// malformed engine key counts as configured so every call fails with its
// parse error instead of a silent 503.
func (e *Engine) Configured() bool {
	if e == nil || e.baseURL == "" {
		return false
	}
	return e.signingKey != nil || e.signingKeyErr != nil || serviceauth.CanSign(e.secret)
}

// Result is a passthrough of the engine's HTTP response.
type Result struct {
	Status int
	Body   []byte
}

// signToken mints the service JWT the engine accepts on the Authorization
// header: iss/aud in its allowlists, exp required, scope enforced per route.
// It is EdDSA under the engine key (SigningKeyEnv) when one is set, otherwise
// under the process Ed25519 service key, and HS256 with the shared secret only
// when neither exists.
func (e *Engine) signToken(scope string) (string, error) {
	if e.signingKeyErr != nil {
		return "", e.signingKeyErr
	}
	if e.signingKey == nil && !serviceauth.CanSign(e.secret) {
		return "", ErrNotConfigured
	}
	cfg, err := serviceauth.Apply(servicecall.Config{
		ServiceName:  "homelab",
		Target:       "notifications",
		Scope:        scope,
		Secret:       e.secret,
		TokenTTL:     tokenTTL,
		SigningKey:   e.signingKey,
		SigningKeyID: e.signingKeyID,
	})
	if err != nil {
		return "", err
	}
	return servicecall.IssueToken(cfg, "notifications", nil, "")
}

func (e *Engine) do(ctx context.Context, method, path, scope string, body []byte) (*Result, error) {
	if !e.Configured() {
		return nil, ErrNotConfigured
	}
	token, err := e.signToken(scope)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, e.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Kombify-Service-Auth", token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, err
	}
	return &Result{Status: resp.StatusCode, Body: respBody}, nil
}

func feedQuery(auth0UserID string, extra url.Values) string {
	v := url.Values{}
	v.Set("auth0_user_id", auth0UserID)
	for k, vals := range extra {
		for _, val := range vals {
			v.Add(k, val)
		}
	}
	return v.Encode()
}

// Feed returns the recipient's undismissed feed (newest first).
func (e *Engine) Feed(ctx context.Context, auth0UserID string, limit int) (*Result, error) {
	if limit < 1 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	q := feedQuery(auth0UserID, url.Values{"limit": []string{strconv.Itoa(limit)}})
	return e.do(ctx, http.MethodGet, "/v1/notifications/feed?"+q, "notifications:read", nil)
}

// MarkRead marks a feed item read.
func (e *Engine) MarkRead(ctx context.Context, auth0UserID, itemID string) (*Result, error) {
	path := "/v1/notifications/feed/" + url.PathEscape(itemID) + "/read?" + feedQuery(auth0UserID, nil)
	return e.do(ctx, http.MethodPost, path, "notifications:write", nil)
}

// Dismiss dismisses a feed item.
func (e *Engine) Dismiss(ctx context.Context, auth0UserID, itemID string) (*Result, error) {
	path := "/v1/notifications/feed/" + url.PathEscape(itemID) + "/dismiss?" + feedQuery(auth0UserID, nil)
	return e.do(ctx, http.MethodPost, path, "notifications:write", nil)
}

// MarkAllRead marks all of the recipient's items read.
func (e *Engine) MarkAllRead(ctx context.Context, auth0UserID string) (*Result, error) {
	return e.do(ctx, http.MethodPost, "/v1/notifications/feed/read-all?"+feedQuery(auth0UserID, nil), "notifications:write", nil)
}

// GetPreferences returns the recipient's topic-by-channel preference matrix.
func (e *Engine) GetPreferences(ctx context.Context, auth0UserID string) (*Result, error) {
	return e.do(ctx, http.MethodGet, "/v1/notifications/preferences?"+feedQuery(auth0UserID, nil), "notifications:read", nil)
}

// PutPreferences persists a preference matrix (locked channels are ignored engine-side).
func (e *Engine) PutPreferences(ctx context.Context, auth0UserID string, matrix []byte) (*Result, error) {
	return e.do(ctx, http.MethodPut, "/v1/notifications/preferences?"+feedQuery(auth0UserID, nil), "notifications:write", matrix)
}

// DispatchProduct sends one idempotent in-app product notification. It is the
// dispatch boundary used by Homelab's durable outbox worker.
func (e *Engine) DispatchProduct(ctx context.Context, event ProductEvent) (*DispatchResult, error) {
	channel := strings.TrimSpace(event.Channel)
	if channel == "" {
		channel = "in_app"
	}
	// Cloud resolves the tenant namespace to its organization identity.
	dispatch := map[string]any{
		"topic_slug":      event.Topic,
		"channel":         channel,
		"recipient":       map[string]string{"auth0_user_id": event.Auth0UserID},
		"payload":         event.Payload,
		"idempotency_key": event.IdempotencyKey,
	}
	if event.TenantID != "" {
		dispatch["tenant_id"] = event.TenantID
	}
	if sourceApp := strings.TrimSpace(event.SourceApp); sourceApp != "" {
		dispatch["source_app"] = sourceApp
		dispatch["event_key"] = strings.TrimSpace(event.EventKey)
	}
	for key, value := range map[string]string{
		"subject_ref": event.SubjectRef,
		"deep_link":   event.DeepLink,
		"group_key":   event.GroupKey,
		"priority":    event.Priority,
	} {
		if value = strings.TrimSpace(value); value != "" {
			dispatch[key] = value
		}
	}
	if actor := event.Actor; actor != nil && strings.TrimSpace(actor.Subject) != "" {
		dispatch["actor"] = map[string]string{
			"subject":      strings.TrimSpace(actor.Subject),
			"display_name": strings.TrimSpace(actor.DisplayName),
		}
	}
	if len(event.Detail) > 0 {
		dispatch["detail"] = event.Detail
	}
	body, err := json.Marshal(dispatch)
	if err != nil {
		return nil, err
	}
	result, err := e.do(ctx, http.MethodPost, "/v1/notifications/dispatch", "notifications:dispatch", body)
	if err != nil {
		return nil, err
	}
	if result.Status < 200 || result.Status >= 300 {
		var envelope struct {
			Error     string `json:"error"`
			ErrorCode string `json:"error_code"`
			Retryable bool   `json:"retryable"`
		}
		_ = json.Unmarshal(result.Body, &envelope)
		code := envelope.ErrorCode
		if code == "" {
			code = envelope.Error
		}
		retryable := envelope.Retryable || result.Status == http.StatusTooManyRequests || result.Status >= 500
		return nil, &DispatchError{StatusCode: result.Status, Code: code, Retryable: retryable}
	}
	var out DispatchResult
	if err := json.Unmarshal(result.Body, &out); err != nil {
		return nil, fmt.Errorf("notifications: decode dispatch response: %w", err)
	}
	return &out, nil
}
