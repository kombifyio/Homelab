// Package serverownerfga projects canonical Homelab server-registry ownership
// (`server#owner` tuples) to Administration's monotonic FGA projection route
// (`POST /api/v1/fga/homelab-server-owners`).
package serverownerfga

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/kombifyio/homelab/internal/gocommon/servicecall"
	"github.com/kombifyio/homelab/pkg/serverregistry"
	"github.com/kombifyio/homelab/pkg/serviceauth"
)

const (
	EndpointPath             = "/api/v1/fga/homelab-server-owners"
	Scope                    = "entitlements:homelab-server-owners"
	CallerService            = "homelab"
	TargetService            = "administration"
	DefaultAdministrationURL = "https://admin.kombify.io"

	MaxBatchSize       = 200
	maxFGAObjectLength = 512
	maxResponseBytes   = 256 << 10
	defaultTimeout     = 10 * time.Second
	defaultTokenTTL    = 30 * time.Second

	StateOwned   = "owned"
	StateUnowned = "unowned"

	OutcomeApplied  = "applied"
	OutcomeReplayed = "replayed"
	OutcomeStale    = "stale"
)

var (
	revisionDecimalPattern = regexp.MustCompile(`^[0-9]{1,19}$`)
	// Matches Administration's SUBJECT_PATTERN exactly (`delegated-auth.ts`).
	subjectPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@|/\-]{0,254}$`)
)

// ServerOwnerInput is one item in POST /api/v1/fga/homelab-server-owners.
type ServerOwnerInput struct {
	TenantID          string `json:"tenantId"`
	ServerID          string `json:"serverId"`
	OwnerSub          string `json:"ownerSub"`
	State             string `json:"state"`
	AggregateRevision string `json:"aggregateRevision"`
}

// ProjectionRequest is the wire payload sent to Administration.
type ProjectionRequest struct {
	Servers []ServerOwnerInput `json:"servers"`
}

// ProjectionResponse is the verified 200 response from Administration.
type ProjectionResponse struct {
	OK        bool     `json:"ok"`
	Projected int      `json:"projected"`
	Outcomes  []string `json:"outcomes"`
}

// ProjectionError reports a structured failure from validation, transport, or
// Administration's FGA owner projection endpoint.
type ProjectionError struct {
	StatusCode int
	ErrorCode  string
	ReasonCode string
	Retryable  bool
	Message    string
	Err        error
}

func (e *ProjectionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *ProjectionError) Error() string {
	if e == nil {
		return "serverownerfga: projection failed"
	}
	parts := []string{"serverownerfga:"}
	if e.StatusCode > 0 {
		parts = append(parts, fmt.Sprintf("status=%d", e.StatusCode))
	}
	if e.ReasonCode != "" {
		parts = append(parts, "reason="+e.ReasonCode)
	}
	if e.ErrorCode != "" {
		parts = append(parts, "code="+e.ErrorCode)
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	return strings.Join(parts, " ")
}

func (e *ProjectionError) IsRetryable() bool {
	return e != nil && e.Retryable
}

// ClientConfig configures the Administration FGA server-owner projector client.
type ClientConfig struct {
	BaseURL           string
	ServiceAuthSecret string
	ServiceAuthConfig *servicecall.Config
	HTTPClient        *http.Client
	Timeout           time.Duration
}

// Client posts revisioned server-owner batches to Administration using a
// signed `homelab` -> `administration` service token.
type Client struct {
	endpoint string
	call     *servicecall.Client
	timeout  time.Duration
}

// NewClient constructs a fail-closed Administration FGA server-owner client.
func NewClient(cfg ClientConfig) (*Client, error) {
	base, err := normalizeBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	var callCfg servicecall.Config
	if cfg.ServiceAuthConfig != nil {
		callCfg = *cfg.ServiceAuthConfig
		if strings.TrimSpace(callCfg.ServiceName) == "" {
			callCfg.ServiceName = CallerService
		}
		if strings.TrimSpace(callCfg.Target) == "" {
			callCfg.Target = TargetService
		}
		if strings.TrimSpace(callCfg.Scope) == "" {
			callCfg.Scope = Scope
		}
		if callCfg.TokenTTL <= 0 {
			callCfg.TokenTTL = defaultTokenTTL
		}
	} else {
		if !serviceauth.CanSign(cfg.ServiceAuthSecret) {
			return nil, errors.New("serverownerfga: service auth signing key or secret required")
		}
		callCfg, err = serviceauth.Apply(servicecall.Config{
			ServiceName: CallerService,
			Target:      TargetService,
			Scope:       Scope,
			Secret:      cfg.ServiceAuthSecret,
			TokenTTL:    defaultTokenTTL,
		})
		if err != nil {
			return nil, fmt.Errorf("serverownerfga: configure service auth: %w", err)
		}
	}
	client, err := servicecall.NewClient(callCfg)
	if err != nil {
		return nil, fmt.Errorf("serverownerfga: build service client: %w", err)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	client.WithHTTPClient(httpClient)
	return &Client{
		endpoint: base + EndpointPath,
		call:     client,
		timeout:  timeout,
	}, nil
}

// InputFromProjectedServer derives the canonical FGA owner projection item from
// an accepted server-registry projection row.
func InputFromProjectedServer(server serverregistry.ProjectedServer) (ServerOwnerInput, error) {
	tenantID := strings.TrimSpace(server.TenantID)
	serverID := strings.TrimSpace(server.ServerID)
	ownerSub := strings.TrimSpace(server.OwnerID)
	state := StateOwned
	if server.Retired || strings.EqualFold(strings.TrimSpace(server.LifecycleState), string(serverregistry.LifecycleDecommissioned)) || ownerSub == "" {
		state = StateUnowned
		ownerSub = ""
	}
	if server.AggregateRevision < 0 {
		return ServerOwnerInput{}, &ProjectionError{
			ReasonCode: "invalid_aggregate_revision",
			Retryable:  false,
			Message:    "aggregateRevision must be non-negative",
		}
	}
	input := ServerOwnerInput{
		TenantID:          tenantID,
		ServerID:          serverID,
		OwnerSub:          ownerSub,
		State:             state,
		AggregateRevision: strconv.FormatInt(server.AggregateRevision, 10),
	}
	if err := ValidateInput(input); err != nil {
		return ServerOwnerInput{}, err
	}
	return input, nil
}

// ValidateInput enforces Administration's exact wire schema fail-closed before
// sending any network request.
func ValidateInput(input ServerOwnerInput) error {
	tenantID := strings.TrimSpace(input.TenantID)
	serverID := strings.TrimSpace(input.ServerID)
	ownerSub := strings.TrimSpace(input.OwnerSub)
	rev := strings.TrimSpace(input.AggregateRevision)

	if tenantID == "" || len(tenantID) > 512 || strings.HasPrefix(tenantID, "b64_") {
		return &ProjectionError{ReasonCode: "invalid_tenant_id", Retryable: false, Message: "invalid tenantId"}
	}
	if serverID == "" || len(serverID) > 200 || !validServerID(serverID) {
		return &ProjectionError{ReasonCode: "invalid_server_id", Retryable: false, Message: "invalid serverId"}
	}
	if len("server:"+fgaTenantSegment(tenantID)+"/"+serverID) > maxFGAObjectLength {
		return &ProjectionError{ReasonCode: "invalid_fga_object", Retryable: false, Message: "canonical FGA object exceeds limit"}
	}
	if !revisionDecimalPattern.MatchString(rev) {
		return &ProjectionError{ReasonCode: "invalid_aggregate_revision", Retryable: false, Message: "aggregateRevision must be a non-negative decimal integer"}
	}
	switch input.State {
	case StateOwned:
		if len(ownerSub) == 0 || len(ownerSub) > 255 || !subjectPattern.MatchString(ownerSub) {
			return &ProjectionError{ReasonCode: "invalid_owner_subject", Retryable: false, Message: "owned server requires a valid ownerSub"}
		}
		if strings.HasPrefix(tenantID, "usr:") && strings.TrimPrefix(tenantID, "usr:") != ownerSub {
			return &ProjectionError{ReasonCode: "personal_tenant_owner_mismatch", Retryable: false, Message: "personal tenant ownerSub must match tenant subject"}
		}
	case StateUnowned:
		if len(ownerSub) > 255 {
			return &ProjectionError{ReasonCode: "invalid_owner_subject", Retryable: false, Message: "ownerSub exceeds length limit"}
		}
	default:
		return &ProjectionError{ReasonCode: "invalid_state", Retryable: false, Message: "state must be owned or unowned"}
	}
	return nil
}

// Project reconciles one or more server-owner entries with Administration,
// chunking automatically at MaxBatchSize (200) and failing closed on any
// invalid input, non-200 status, or mismatched response payload.
func (c *Client) Project(ctx context.Context, servers []ServerOwnerInput) (ProjectionResponse, error) {
	if c == nil || c.call == nil {
		return ProjectionResponse{}, &ProjectionError{
			ReasonCode: "client_not_configured",
			Retryable:  false,
			Message:    "Administration FGA owner projection client is not configured",
		}
	}
	if len(servers) == 0 {
		return ProjectionResponse{OK: true, Projected: 0, Outcomes: []string{}}, nil
	}
	normalized := make([]ServerOwnerInput, len(servers))
	for i, item := range servers {
		item.TenantID = strings.TrimSpace(item.TenantID)
		item.ServerID = strings.TrimSpace(item.ServerID)
		item.OwnerSub = strings.TrimSpace(item.OwnerSub)
		item.AggregateRevision = strings.TrimSpace(item.AggregateRevision)
		if item.State == StateUnowned {
			item.OwnerSub = ""
		}
		if err := ValidateInput(item); err != nil {
			return ProjectionResponse{}, err
		}
		normalized[i] = item
	}

	combined := ProjectionResponse{
		OK:       true,
		Outcomes: make([]string, 0, len(normalized)),
	}
	for start := 0; start < len(normalized); start += MaxBatchSize {
		end := start + MaxBatchSize
		if end > len(normalized) {
			end = len(normalized)
		}
		batchResp, err := c.projectBatch(ctx, normalized[start:end])
		if err != nil {
			return ProjectionResponse{}, err
		}
		combined.Projected += batchResp.Projected
		combined.Outcomes = append(combined.Outcomes, batchResp.Outcomes...)
	}
	return combined, nil
}

func (c *Client) projectBatch(ctx context.Context, batch []ServerOwnerInput) (ProjectionResponse, error) {
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.call.PostJSON(reqCtx, c.endpoint, ProjectionRequest{Servers: batch}, nil)
	if err != nil {
		return ProjectionResponse{}, &ProjectionError{
			ReasonCode: "transport_error",
			Retryable:  true,
			Message:    err.Error(),
			Err:        err,
		}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if readErr != nil {
		return ProjectionResponse{}, &ProjectionError{
			StatusCode: resp.StatusCode,
			ReasonCode: "response_read_error",
			Retryable:  true,
			Message:    readErr.Error(),
			Err:        readErr,
		}
	}
	if len(raw) > maxResponseBytes {
		return ProjectionResponse{}, &ProjectionError{
			StatusCode: resp.StatusCode,
			ReasonCode: "response_too_large",
			Retryable:  true,
			Message:    "Administration response exceeded size limit",
		}
	}

	if resp.StatusCode != http.StatusOK {
		return ProjectionResponse{}, decodeErrorResponse(resp.StatusCode, raw)
	}

	var parsed ProjectionResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ProjectionResponse{}, &ProjectionError{
			StatusCode: resp.StatusCode,
			ReasonCode: "invalid_response_json",
			Retryable:  true,
			Message:    err.Error(),
		}
	}
	if !parsed.OK || parsed.Projected != len(batch) || len(parsed.Outcomes) != len(batch) {
		return ProjectionResponse{}, &ProjectionError{
			StatusCode: resp.StatusCode,
			ReasonCode: "invalid_response_contract",
			Retryable:  true,
			Message:    fmt.Sprintf("expected ok=true and %d outcomes, got ok=%t projected=%d outcomes=%d", len(batch), parsed.OK, parsed.Projected, len(parsed.Outcomes)),
		}
	}
	for _, outcome := range parsed.Outcomes {
		switch outcome {
		case OutcomeApplied, OutcomeReplayed, OutcomeStale:
		default:
			return ProjectionResponse{}, &ProjectionError{
				StatusCode: resp.StatusCode,
				ReasonCode: "invalid_response_outcome",
				Retryable:  true,
				Message:    fmt.Sprintf("unexpected projection outcome %q", outcome),
			}
		}
	}
	return parsed, nil
}

// PublishServers satisfies serverregistry.PlatformPublisher so PlatformProjector
// can run periodic full-aggregate FGA owner reconciliation across all tenants.
func (c *Client) PublishServers(ctx context.Context, tenantID string, servers []serverregistry.ProjectedServer) error {
	tenantID = strings.TrimSpace(tenantID)
	if len(servers) == 0 {
		return nil
	}
	inputs := make([]ServerOwnerInput, 0, len(servers))
	for _, server := range servers {
		if strings.TrimSpace(server.TenantID) != tenantID {
			return &ProjectionError{
				ReasonCode: "batch_tenant_mismatch",
				Retryable:  false,
				Message:    "projected server tenant does not match batch tenant",
			}
		}
		input, err := InputFromProjectedServer(server)
		if err != nil {
			// Skip malformed individual rows with a structured warning so one bad
			// server record never blocks reconciliation of the tenant's other servers.
			slog.Warn("homelab server-owner FGA projection skipped invalid server row",
				"tenant_id", tenantID,
				"server_id", strings.TrimSpace(server.ServerID),
				"error", err,
			)
			continue
		}
		inputs = append(inputs, input)
	}
	if len(inputs) == 0 {
		return nil
	}
	_, err := c.Project(ctx, inputs)
	return err
}

func decodeErrorResponse(status int, raw []byte) error {
	var body struct {
		ErrorCode  string `json:"error_code"`
		ReasonCode string `json:"reason_code"`
		Retryable  *bool  `json:"retryable"`
		Message    string `json:"message"`
	}
	_ = json.Unmarshal(raw, &body)
	retryable := status >= http.StatusInternalServerError || status == http.StatusTooManyRequests
	if body.Retryable != nil {
		retryable = *body.Retryable
	}
	reason := strings.TrimSpace(body.ReasonCode)
	if reason == "" {
		reason = fmt.Sprintf("http_%d", status)
	}
	return &ProjectionError{
		StatusCode: status,
		ErrorCode:  strings.TrimSpace(body.ErrorCode),
		ReasonCode: reason,
		Retryable:  retryable,
		Message:    strings.TrimSpace(body.Message),
	}
}

func normalizeBaseURL(raw string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return "", errors.New("serverownerfga: Administration base URL required")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("serverownerfga: Administration base URL must be a bare origin")
	}
	switch parsed.Scheme {
	case "https":
		return parsed.Scheme + "://" + parsed.Host, nil
	case "http":
		host := parsed.Hostname()
		if host != "127.0.0.1" && host != "localhost" {
			return "", errors.New("serverownerfga: non-loopback Administration URL must use https")
		}
		return parsed.Scheme + "://" + parsed.Host, nil
	default:
		return "", errors.New("serverownerfga: Administration URL scheme must be https")
	}
}

func validServerID(value string) bool {
	for _, r := range value {
		if unicode.IsSpace(r) || r == '/' || r == '#' || r == ':' {
			return false
		}
	}
	return true
}

func fgaTenantSegment(tenantID string) string {
	if !strings.ContainsAny(tenantID, ":#") {
		return tenantID
	}
	return "b64_" + base64.RawURLEncoding.EncodeToString([]byte(tenantID))
}
