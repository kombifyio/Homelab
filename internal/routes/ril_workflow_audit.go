package routes

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	ksapi "github.com/kombifyio/homelab/pkg/api"
	"github.com/kombifyio/homelab/pkg/httpx"
	rilaction "github.com/kombifyio/homelab/pkg/ril/actioncontract"
	"github.com/kombifyio/homelab/pkg/ril/actions"
	"github.com/kombifyio/homelab/pkg/ril/workflow"
)

const workflowAuditLimit = 500

type WorkflowAuditReader interface {
	GetRun(context.Context, string) (*workflow.Run, error)
	ListSteps(context.Context, string) ([]*workflow.Step, error)
	ListAudit(context.Context, string, string, int) ([]workflow.AuditEvent, error)
}

type workflowAuditHandler struct {
	reader   WorkflowAuditReader
	receipts actions.StepReceiptReader
}

// RegisterWorkflowAuditRoutes exposes the sanitized durable run/step history
// to the authenticated owner. Inputs, outputs, context, idempotency keys, raw
// errors, and connector material never cross this boundary. A run of a
// governed action card that reached a terminal state also carries the card's
// typed step receipt when receipts is configured.
func RegisterWorkflowAuditRoutes(r *httpx.Router, reader WorkflowAuditReader, receipts actions.StepReceiptReader) {
	if reader == nil {
		panic("RegisterWorkflowAuditRoutes: reader required")
	}
	h := workflowAuditHandler{reader: reader, receipts: receipts}
	r.GET("/v1/ril/workflow-runs/{runId}", h.get)
}

func (h workflowAuditHandler) get(e *httpx.Event) error {
	if len(e.Request.URL.Query()) != 0 {
		return httpx.BadRequest(e, "Workflow audit query parameters are not supported", map[string]any{inventoryReasonCodeField: "invalid_request"})
	}
	scope, err := inventoryScopeFromEvent(e)
	if err != nil {
		return writeInventoryHTTPError(e, err)
	}
	runID := strings.TrimSpace(e.Request.PathValue("runId"))
	ctx := workflow.WithTenant(e.Request.Context(), scope.tenantID)
	run, err := h.reader.GetRun(ctx, runID)
	if err != nil {
		return writeWorkflowAuditError(e, err)
	}
	if run.OwnerID != scope.ownerID || workflowRunTenant(run) != scope.tenantID {
		return httpx.NotFound(e, "Workflow run not found")
	}
	steps, err := h.reader.ListSteps(ctx, runID)
	if err != nil {
		return writeWorkflowAuditError(e, err)
	}
	events, err := h.reader.ListAudit(ctx, scope.tenantID, runID, workflowAuditLimit)
	if err != nil {
		return writeWorkflowAuditError(e, err)
	}
	receipt, err := h.cardReceipt(ctx, scope.tenantID, scope.ownerID, run.CardID)
	if err != nil {
		return writeWorkflowAuditError(e, err)
	}
	e.Response.Header().Set("Cache-Control", "private, no-store")
	return httpx.Success(e, http.StatusOK, workflowAuditResponse{
		Run:     projectWorkflowRun(run),
		Steps:   projectWorkflowSteps(steps),
		Events:  events,
		Receipt: receipt,
	})
}

// cardReceipt returns the typed step receipt of the run's card, or nil while
// the card has none (no card, not terminal yet, or no persisted approval).
func (h workflowAuditHandler) cardReceipt(ctx context.Context, tenantID, ownerID, cardID string) (*rilaction.StepReceipt, error) {
	if h.receipts == nil || strings.TrimSpace(cardID) == "" {
		return nil, nil
	}
	receipt, err := h.receipts.StepReceipt(ctx, tenantID, ownerID, cardID)
	if errors.Is(err, actions.ErrCardNotFound) || errors.Is(err, actions.ErrCardConflict) {
		return nil, nil
	}
	return receipt, err
}

type workflowRunAudit struct {
	RunID       string             `json:"run_id"`
	Type        workflow.RunType   `json:"type"`
	Status      workflow.RunStatus `json:"status"`
	CurrentStep int                `json:"current_step"`
	ServerID    string             `json:"server_id,omitempty"`
	CardID      string             `json:"card_id,omitempty"`
	StartedAt   *time.Time         `json:"started_at,omitempty"`
	FinishedAt  *time.Time         `json:"finished_at,omitempty"`
	CreatedAt   time.Time          `json:"created_at"`
	UpdatedAt   time.Time          `json:"updated_at"`
}

type workflowStepAudit struct {
	StepIndex  int                 `json:"step_index"`
	Name       string              `json:"name"`
	Status     workflow.StepStatus `json:"status"`
	Attempt    int                 `json:"attempt"`
	StartedAt  *time.Time          `json:"started_at,omitempty"`
	FinishedAt *time.Time          `json:"finished_at,omitempty"`
}

type workflowAuditResponse struct {
	Run     workflowRunAudit       `json:"run"`
	Steps   []workflowStepAudit    `json:"steps"`
	Events  []workflow.AuditEvent  `json:"events"`
	Receipt *rilaction.StepReceipt `json:"receipt,omitempty"`
}

func workflowRunTenant(run *workflow.Run) string {
	if run == nil {
		return ""
	}
	return run.TenantID
}

func projectWorkflowRun(run *workflow.Run) workflowRunAudit {
	return workflowRunAudit{
		RunID: run.RunID, Type: run.Type, Status: run.Status, CurrentStep: run.CurrentStep,
		ServerID: run.ServerID, CardID: run.CardID, StartedAt: run.StartedAt,
		FinishedAt: run.FinishedAt, CreatedAt: run.Created, UpdatedAt: run.Updated,
	}
}

func projectWorkflowSteps(steps []*workflow.Step) []workflowStepAudit {
	result := make([]workflowStepAudit, 0, len(steps))
	for _, step := range steps {
		if step == nil {
			continue
		}
		result = append(result, workflowStepAudit{
			StepIndex: step.StepIndex, Name: step.Name, Status: step.Status,
			Attempt: step.Attempt, StartedAt: step.StartedAt, FinishedAt: step.FinishedAt,
		})
	}
	return result
}

func writeWorkflowAuditError(e *httpx.Event, err error) error {
	if errors.Is(err, workflow.ErrNotFound) {
		return httpx.NotFound(e, "Workflow run not found")
	}
	return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeInternal, "Workflow audit unavailable", map[string]any{inventoryReasonCodeField: "workflow_audit_unavailable"})
}
