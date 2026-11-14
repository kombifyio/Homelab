package routes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kombifyio/techstack/internal/gocommon/denial"
	ksapi "github.com/kombifyio/techstack/pkg/api"
	"github.com/kombifyio/techstack/pkg/controlplane"
	"github.com/kombifyio/techstack/pkg/guardreach"
	"github.com/kombifyio/techstack/pkg/httpx"
	"github.com/kombifyio/techstack/pkg/jobs"
	"github.com/kombifyio/techstack/pkg/middleware"
	"github.com/kombifyio/techstack/pkg/ril/actioncontract"
	"github.com/kombifyio/techstack/pkg/ril/actions"
	"github.com/kombifyio/techstack/pkg/ril/primitives"
)

type GovernedActionWorkflow interface {
	Execute(context.Context, actions.BeginExecution) (*actions.GovernedCard, error)
}

type GovernedOperationWorkflow interface {
	ExecuteOperation(context.Context, actions.BeginExecution, string) (*actions.GovernedCard, error)
}

type RILDecisionAuthority interface {
	RILAuthority(context.Context, string, string, string, string, string, string, string) (guardreach.RILAuthorityResult, error)
}

type GovernedActionRouteConfig struct {
	DecisionAuthority RILDecisionAuthority
	Authority         actions.Authority
	Workflow          GovernedActionWorkflow
	Policy            InventoryPolicy
	// Servers is the owner-scoped server read: the card owner executes on
	// their own server without a per-server grant (authorizeOwnerServerOperate).
	Servers interface {
		GetServerRuntime(ctx context.Context, tenantID, serverID string) (*controlplane.ServerRuntime, error)
	}
	Now func() time.Time
	// Background turns the request's context into Techstack system work, so
	// an executor-bound execution outlives a cancelled request (an update runs
	// up to its rollback budget) instead of recording a false failure while
	// the node completes it.
	Background func(context.Context) context.Context
}

type governedActionHandler struct{ config GovernedActionRouteConfig }

func RegisterGovernedActionRoutes(r *httpx.Router, config GovernedActionRouteConfig) {
	if config.Authority == nil || config.Workflow == nil || config.Policy == nil || config.Servers == nil {
		panic("RegisterGovernedActionRoutes: authority, workflow, inventory policy, and server store required")
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	h := governedActionHandler{config: config}
	r.POST("/v1/ril/action-cards", h.create)
	r.GET("/v1/ril/action-cards", h.list)
	r.GET("/v1/ril/action-cards/{cardId}", h.get)
	r.GET("/v1/ril/action-cards/{cardId}/receipt", h.receipt)
	r.POST("/v1/ril/action-cards/{cardId}/decision", h.requestDecision)
	r.POST("/v1/ril/action-cards/{cardId}/approve", h.approve)
	r.POST("/v1/ril/action-cards/{cardId}/deny", h.deny)
	r.POST("/v1/ril/action-cards/{cardId}/execute", h.execute)
}

type createGovernedActionRequest struct {
	ID       string                 `json:"id,omitempty"`
	ServerID string                 `json:"server_id"`
	Title    string                 `json:"title"`
	Severity string                 `json:"severity,omitempty"`
	Action   actions.ActionTemplate `json:"action"`
}

func (h governedActionHandler) create(e *httpx.Event) error {
	scope, err := inventoryScopeFromEvent(e)
	if err != nil {
		return writeGovernedActionError(e, err)
	}
	var body createGovernedActionRequest
	if decodeErr := decodeGovernedActionJSON(e, &body); decodeErr != nil {
		return writeGovernedActionError(e, decodeErr)
	}
	// An agent creates an executor-bound card through its exact connector
	// binding for this card (execute requires that binding to carry the
	// primitive's grant scope); the managed service route creates its cards
	// without one. Either way the caller names only the target scope and the
	// input that selects the target: Create derives every target ref and the
	// derived inputs from fresh inventory and refuses a supplied one that
	// differs.
	if primitives.IsExecutorBound(body.Action.Primitive.ID) && body.Action.ConnectorBinding == nil {
		return writeGovernedActionError(e, actions.ErrExecutionAdmission)
	}
	card, err := h.config.Authority.Create(e.Request.Context(), actions.CreateGovernedCard{ID: body.ID, TenantID: scope.tenantID, OwnerSubjectID: scope.ownerID, ServerID: body.ServerID, Title: body.Title, Severity: body.Severity, Template: body.Action})
	if err != nil {
		return writeGovernedActionError(e, err)
	}
	return httpx.Success(e, http.StatusCreated, card)
}

func (h governedActionHandler) list(e *httpx.Event) error {
	if len(e.Request.URL.Query()) != 0 {
		return writeGovernedActionError(e, errGovernedActionRequest)
	}
	scope, err := inventoryScopeFromEvent(e)
	if err != nil {
		return writeGovernedActionError(e, err)
	}
	cards, err := h.config.Authority.List(e.Request.Context(), scope.tenantID, scope.ownerID)
	if err != nil {
		return writeGovernedActionError(e, err)
	}
	return httpx.Success(e, http.StatusOK, cards)
}

func (h governedActionHandler) get(e *httpx.Event) error {
	scope, err := inventoryScopeFromEvent(e)
	if err != nil {
		return writeGovernedActionError(e, err)
	}
	card, err := h.config.Authority.Get(e.Request.Context(), scope.tenantID, scope.ownerID, e.Request.PathValue("cardId"))
	if err != nil {
		return writeGovernedActionError(e, err)
	}
	return httpx.Success(e, http.StatusOK, cardView(card))
}

// receipt returns the typed step receipt of a terminal card: exactly one
// execution and its verification readback, cross-linked to the AI Platform
// approval receipt and Decision id. The Journey D evidence task reads it.
func (h governedActionHandler) receipt(e *httpx.Event) error {
	if len(e.Request.URL.Query()) != 0 {
		return writeGovernedActionError(e, errGovernedActionRequest)
	}
	scope, err := inventoryScopeFromEvent(e)
	if err != nil {
		return writeGovernedActionError(e, err)
	}
	reader, ok := h.config.Authority.(actions.StepReceiptReader)
	if !ok {
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Action receipt unavailable", nil)
	}
	receipt, err := reader.StepReceipt(e.Request.Context(), scope.tenantID, scope.ownerID, e.Request.PathValue("cardId"))
	if err != nil {
		return writeGovernedActionError(e, err)
	}
	e.Response.Header().Set("Cache-Control", "private, no-store")
	return httpx.Success(e, http.StatusOK, receipt)
}

// requestDecision files the card's one Gateway Decision ahead of execution;
// the Gateway answers it at once by the acting agent's equipment. Execute
// files the same Decision itself, so this call is never required. It never
// interprets request-body approval or identity as authority.
func (h governedActionHandler) requestDecision(e *httpx.Event) error {
	scope, err := inventoryScopeFromEvent(e)
	if err != nil {
		return writeGovernedActionError(e, err)
	}
	if h.config.DecisionAuthority == nil {
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Decision authority unavailable", nil)
	}
	cardID := strings.TrimSpace(e.Request.PathValue("cardId"))
	card, err := h.config.Authority.Get(e.Request.Context(), scope.tenantID, scope.ownerID, cardID)
	if err != nil {
		return writeGovernedActionError(e, err)
	}
	if err = authorizeOwnerServerOperate(e.Request.Context(), h.config.Policy, h.config.Servers, scope, card.ServerID); err != nil {
		return writeGovernedActionError(e, err)
	}
	result, err := h.config.DecisionAuthority.RILAuthority(e.Request.Context(), scope.ownerID, scope.tenantID, "prepare", cardID, "", "", "")
	if err != nil {
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Decision authority unavailable", nil)
	}
	return httpx.Success(e, http.StatusAccepted, result)
}

type actionDecisionRequest struct {
	AuditCorrelationID string `json:"audit_correlation_id"`
}

func (h governedActionHandler) approve(e *httpx.Event) error { return h.decision(e, true) }
func (h governedActionHandler) deny(e *httpx.Event) error    { return h.decision(e, false) }

func (h governedActionHandler) decision(e *httpx.Event, approve bool) error {
	scope, err := inventoryScopeFromEvent(e)
	if err != nil {
		return writeGovernedActionError(e, err)
	}
	var body actionDecisionRequest
	if decodeErr := decodeGovernedActionJSON(e, &body); decodeErr != nil {
		return writeGovernedActionError(e, decodeErr)
	}
	var card *actions.GovernedCard
	if approve {
		card, err = h.config.Authority.Approve(e.Request.Context(), scope.tenantID, scope.ownerID, e.Request.PathValue("cardId"), body.AuditCorrelationID, h.config.Now())
	} else {
		card, err = h.config.Authority.Deny(e.Request.Context(), scope.tenantID, scope.ownerID, e.Request.PathValue("cardId"), body.AuditCorrelationID, h.config.Now())
	}
	if err != nil {
		return writeGovernedActionError(e, err)
	}
	return httpx.Success(e, http.StatusOK, card)
}

type executeGovernedActionRequest struct {
	CompensationCardID string `json:"compensation_card_id,omitempty"`
	ExecutionID        string `json:"execution_id"`
	TraceID            string `json:"trace_id"`
	IdempotencyKey     string `json:"idempotency_key"`
}

func (h governedActionHandler) execute(e *httpx.Event) error {
	scope, err := inventoryScopeFromEvent(e)
	if err != nil {
		return writeGovernedActionError(e, err)
	}
	cardID := strings.TrimSpace(e.Request.PathValue("cardId"))
	card, err := h.config.Authority.Get(e.Request.Context(), scope.tenantID, scope.ownerID, cardID)
	if err != nil {
		return writeGovernedActionError(e, err)
	}
	if err := authorizeOwnerServerOperate(e.Request.Context(), h.config.Policy, h.config.Servers, scope, card.ServerID); err != nil {
		return writeGovernedActionError(e, err)
	}
	var body executeGovernedActionRequest
	if decodeErr := decodeGovernedActionJSON(e, &body); decodeErr != nil {
		return writeGovernedActionError(e, decodeErr)
	}
	input := actions.BeginExecution{TenantID: scope.tenantID, OwnerSubjectID: scope.ownerID, CardID: cardID, ExecutionID: body.ExecutionID, TraceID: body.TraceID, IdempotencyKey: body.IdempotencyKey, Now: h.config.Now(), ConnectorProjection: connectorProjectionFromEvent(e)}
	if card.Template.Primitive.OperationClass == "drift-correction" {
		operational, ok := h.config.Workflow.(GovernedOperationWorkflow)
		if !ok || strings.TrimSpace(body.CompensationCardID) == "" {
			return writeGovernedActionError(e, actions.ErrApprovalRequired)
		}
		card, err = operational.ExecuteOperation(e.Request.Context(), input, body.CompensationCardID)
	} else {
		if body.CompensationCardID != "" {
			return writeGovernedActionError(e, errGovernedActionRequest)
		}
		card, err = h.executeDetached(e.Request.Context(), card, input)
	}
	if err != nil {
		return writeGovernedActionError(e, err)
	}
	return httpx.Success(e, http.StatusOK, cardView(card))
}

// executeDetached runs an executor-bound card as system work bounded by the
// card run budget: the request waits for the result, but its cancellation
// never cancels the admitted execution, whose durable result stays on the card.
func (h governedActionHandler) executeDetached(ctx context.Context, card *actions.GovernedCard, input actions.BeginExecution) (*actions.GovernedCard, error) {
	if _, executorBound := primitives.For(card.Template.Primitive); !executorBound || h.config.Background == nil {
		return h.config.Workflow.Execute(ctx, input)
	}
	type result struct {
		card *actions.GovernedCard
		err  error
	}
	done := make(chan result, 1)
	run := h.config.Background(context.WithoutCancel(ctx))
	go func() {
		runCtx, cancel := context.WithTimeout(run, serviceActionCardRunBudget)
		defer cancel()
		executed, err := h.config.Workflow.Execute(runCtx, input)
		done <- result{card: executed, err: err}
	}()
	select {
	case out := <-done:
		return out.card, out.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

var errGovernedActionRequest = errors.New("invalid governed action request")

func decodeGovernedActionJSON(e *httpx.Event, destination any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(e.Response, e.Request.Body, rilaction.MaxRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return errGovernedActionRequest
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errGovernedActionRequest
	}
	return nil
}

func writeGovernedActionError(e *httpx.Event, err error) error {
	var notReadable *jobs.BackupNotReadableError
	switch {
	case errors.Is(err, actions.ErrCardNotFound):
		return httpx.NotFound(e, "Action card not found")
	case errors.Is(err, actions.ErrGrantRequired), errors.Is(err, actions.ErrConnectorBindingRequired), errors.Is(err, actions.ErrConnectorGrantInsufficient):
		return writeGovernedActionEntitlementDenial(e, err)
	case errors.As(err, &notReadable):
		return backupNotReadableError(e, notReadable)
	case errors.Is(err, actions.ErrApprovalRequired), errors.Is(err, actions.ErrExecutionAdmission), errors.Is(err, actions.ErrCardConflict), errors.Is(err, actions.ErrExecutionInProgress):
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "Action cannot transition", map[string]any{inventoryReasonCodeField: governedActionReason(err)})
	case errors.Is(err, errGovernedActionRequest):
		return httpx.BadRequest(e, "Invalid governed action request", map[string]any{inventoryReasonCodeField: "invalid_request"})
	default:
		if _, ok := err.(*inventoryError); ok {
			return writeInventoryHTTPError(e, err)
		}
		return httpx.Error(e, http.StatusInternalServerError, ksapi.ErrCodeInternal, "Governed action unavailable", map[string]any{inventoryReasonCodeField: "governed_action_unavailable"})
	}
}

// governedCardView is a card read with the person-facing guidance of a failed
// backup check (what is missing and how to set it up). The guidance is derived
// from the closed codes of the card's public evidence on every read; it is not
// stored and takes no part in any decision.
type governedCardView struct {
	*actions.GovernedCard
	UserGuidance *actions.UserGuidance `json:"user_guidance,omitempty"`
}

func cardView(card *actions.GovernedCard) any {
	if guidance := actions.GuidanceForCard(card); guidance != nil {
		return governedCardView{GovernedCard: card, UserGuidance: guidance}
	}
	return card
}

// backupNotReadableError refuses a backup check addressed to a server that
// makes no backup of its own, before any Decision, lease or claim exists. It
// names the server that does.
func backupNotReadableError(e *httpx.Event, refusal *jobs.BackupNotReadableError) error {
	title, body, nextSteps := refusal.Guidance()
	return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "This server keeps no backup to check",
		map[string]any{
			inventoryReasonCodeField: refusal.ReasonCode(),
			"retryable":              false,
			"user_guidance":          map[string]any{"title": title, "body": body, "next_steps": nextSteps},
		})
}

func writeGovernedActionEntitlementDenial(e *httpx.Event, err error) error {
	reason := governedActionReason(err)
	env := denial.Envelope{
		ErrorCode:        reason,
		ReasonCode:       reason,
		Capability:       "techstack.ril.execute",
		RequiredFeatures: []string{"techstack.ril.execute"},
		MissingFeatures:  []string{"techstack.ril.grant"},
		Retryable:        false,
		UserGuidance: denial.UserGuidance{
			Title: "This RIL action cannot run yet",
			Body:  "Techstack stopped before starting the durable workflow because the required execution grant is missing, expired, or insufficient.",
			NextSteps: []string{
				"Approve the action card with a live delegated grant.",
				"Retry only after the grant and connector binding are present.",
			},
		},
	}
	if errors.Is(err, actions.ErrConnectorBindingRequired) || errors.Is(err, actions.ErrConnectorGrantInsufficient) {
		env.MissingFeatures = []string{"techstack.ril.connector_binding"}
		env.UserGuidance.Title = "This RIL action needs a connector binding"
		env.UserGuidance.Body = "Techstack stopped before starting the durable workflow because the exact connector binding is missing or cannot mutate."
		env.UserGuidance.NextSteps = []string{
			"Bind the action card to an active connector grant.",
			"Retry the execute request through Gateway so the signed connector projection is present.",
		}
	}
	if validateErr := env.Validate(); validateErr != nil {
		return httpx.Error(e, http.StatusInternalServerError, ksapi.ErrCodeInternal, "Governed action unavailable", map[string]any{inventoryReasonCodeField: "governed_action_unavailable"})
	}
	raw, marshalErr := json.Marshal(env)
	if marshalErr != nil {
		return httpx.Error(e, http.StatusInternalServerError, ksapi.ErrCodeInternal, "Governed action unavailable", map[string]any{inventoryReasonCodeField: "governed_action_unavailable"})
	}
	var payload map[string]any
	if unmarshalErr := json.Unmarshal(raw, &payload); unmarshalErr != nil {
		return httpx.Error(e, http.StatusInternalServerError, ksapi.ErrCodeInternal, "Governed action unavailable", map[string]any{inventoryReasonCodeField: "governed_action_unavailable"})
	}
	payload["error"] = map[string]any{"code": env.ErrorCode, "message": env.UserGuidance.Body}
	return e.JSON(http.StatusForbidden, payload)
}

func governedActionReason(err error) string {
	switch {
	case errors.Is(err, actions.ErrApprovalRequired):
		return "approval_required"
	case errors.Is(err, actions.ErrGrantRequired):
		return "grant_required"
	case errors.Is(err, actions.ErrConnectorBindingRequired):
		return "connector_binding_required"
	case errors.Is(err, actions.ErrConnectorGrantInsufficient):
		return "connector_grant_insufficient"
	case errors.Is(err, actions.ErrExecutionInProgress):
		return "execution_in_progress"
	case errors.Is(err, actions.ErrExecutionAdmission):
		return "execution_admission_rejected"
	default:
		return "state_conflict"
	}
}

func connectorProjectionFromEvent(e *httpx.Event) *actions.ConnectorBindingProjection {
	if e == nil || e.Request == nil || !middleware.IsEdgeAuthenticated(e.Request.Context()) {
		return nil
	}
	header := e.Request.Header.Get
	grantID := header("X-Kombify-Connector-Grant-ID")
	bindingID := header("X-Kombify-Connector-Binding-ID")
	if grantID == "" && bindingID == "" {
		return nil
	}
	projection := &actions.ConnectorBindingProjection{
		GrantID:      grantID,
		BindingID:    bindingID,
		BindingHash:  header("X-Kombify-Connector-Binding-Hash"),
		ConnectorID:  header("X-Kombify-Connector-ID"),
		BindingScope: header("X-Kombify-Connector-Binding-Scope"),
		ResourceID:   header("X-Kombify-Connector-Resource-ID"),
		ServerID:     header("X-Kombify-Connector-Server-ID"),
		Status:       header("X-Kombify-Connector-Binding-Status"),
	}
	if rawScopes := strings.TrimSpace(header("X-Kombify-Connector-Scopes")); rawScopes != "" {
		projection.Scopes = strings.Split(rawScopes, ",")
	}
	return projection
}
