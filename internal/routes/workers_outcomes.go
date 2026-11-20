package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/kombifyio/homelab/internal/routes/tenantguard"
	"github.com/kombifyio/homelab/pkg/agentcontrol"
	ksapi "github.com/kombifyio/homelab/pkg/api"
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/guardbinding"
	"github.com/kombifyio/homelab/pkg/httpx"
)

// outcomeResolutionStepUpMaxAge bounds how old the multi-factor sign-in behind
// an operator release of an unresolved outcome may be. Releasing the fence lets
// conflicting work run against a server whose state is unknown, so it needs the
// same fresh step-up as a reboot.
const outcomeResolutionStepUpMaxAge = 300 * time.Second

// WorkerOutcomeControl is the operator side of the typed command rendezvous.
type WorkerOutcomeControl interface {
	ListUnresolvedOutcomes(ctx context.Context, tenantID, agentID string) ([]agentcontrol.UnresolvedOutcome, error)
	ResolveOutcomeByOperator(ctx context.Context, req agentcontrol.OperatorResolution) (guardbinding.ResultReport, error)
}

// Guard side of the same rendezvous: operator resolutions handed to a Guard that
// still holds the command as unsettled.
type workerOutcomeNotices interface {
	OperatorResolutionsForTenant(ctx context.Context, tenantID, agentID string, known []string) ([]guardbinding.OperatorResolutionNotice, error)
}

var resolveOutcomeRequestFields = map[string]bool{"disposition": true, "expected_revision": true, "note": true}

type resolveOutcomeRequest struct {
	Disposition      string `json:"disposition"`
	ExpectedRevision int64  `json:"expected_revision"`
	Note             string `json:"note"`
}

type resolveOutcomeResponse struct {
	CommandID   string `json:"command_id"`
	Revision    int64  `json:"revision"`
	Disposition string `json:"disposition"`
	Actor       string `json:"actor"`
	Released    bool   `json:"fence_released"`
}

// ownedWorkerForOutcomes authenticates the operator and proves that the worker
// belongs to the caller inside the caller's tenant. A worker of another tenant
// or owner is indistinguishable from a missing one.
func (h workerRouteHandlers) ownedWorkerForOutcomes(e *httpx.Event, capability string) (ownerID, tenantID, workerID string, control WorkerOutcomeControl, err error) {
	ownerID, authErr := requireWorkerOwner(e)
	if authErr != nil || ownerID == "" {
		return "", "", "", nil, authErr
	}
	tenantID, tenantErr := tenantguard.TenantScope(requestExplicitTenantID(e), ownerID, capability)
	if tenantErr != nil {
		return "", "", "", nil, tenantErr
	}
	control, ok := h.typedControl.(WorkerOutcomeControl)
	if !ok {
		return "", "", "", nil, httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Durable Guard outcomes are unavailable", nil)
	}
	workerID = strings.TrimSpace(e.Request.PathValue("id"))
	worker, getErr := h.wst.GetWorker(e.Request.Context(), tenantID, workerID)
	if errors.Is(getErr, controlplane.ErrNotFound) || (getErr == nil && (worker == nil || worker.OwnerSubjectID != ownerID)) {
		return "", "", "", nil, httpx.NotFound(e, "Worker not found")
	}
	if getErr != nil {
		return "", "", "", nil, httpx.Error(e, http.StatusInternalServerError, ksapi.ErrCodeInternal, "Failed to read worker", nil)
	}
	return ownerID, tenantID, workerID, control, nil
}

func (h workerRouteHandlers) listUnresolvedOutcomes(e *httpx.Event) error {
	ownerID, tenantID, workerID, control, err := h.ownedWorkerForOutcomes(e, "homelab.workers.outcomes.list")
	if err != nil || ownerID == "" {
		return err
	}
	outcomes, listErr := control.ListUnresolvedOutcomes(e.Request.Context(), tenantID, workerID)
	if listErr != nil {
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Unresolved outcomes are unavailable", nil)
	}
	if outcomes == nil {
		outcomes = []agentcontrol.UnresolvedOutcome{}
	}
	return httpx.Success(e, http.StatusOK, map[string]any{"worker_id": workerID, "outcomes": outcomes})
}

// resolveOutcome releases the fence of an unresolved outcome_unknown with an
// explicit operator disposition. The actor is the authenticated subject, the
// person's step-up must be fresh (an agent acting for the person needs none)
// and the revision the operator saw must still be the current one.
func (h workerRouteHandlers) resolveOutcome(e *httpx.Event) error {
	ownerID, tenantID, workerID, control, err := h.ownedWorkerForOutcomes(e, "homelab.workers.outcomes.resolve")
	if err != nil || ownerID == "" {
		return err
	}
	agentID, admitted := freshPersonOrActingAgent(e.Request.Context(), h.entitlements, time.Now().UTC(), outcomeResolutionStepUpMaxAge)
	if !admitted {
		message, details := stepUpRefusal(e.Request.Context(), "A fresh multi-factor sign-in is required to release an unresolved outcome", "outcome_resolution_step_up_required")
		return httpx.Error(e, http.StatusForbidden, ksapi.ErrCodeForbidden, message, details)
	}
	raw, readErr := io.ReadAll(io.LimitReader(e.Request.Body, 4097))
	if readErr != nil || len(raw) > 4096 || !exactJSONObjectKeys(raw, resolveOutcomeRequestFields) {
		return httpx.BadRequest(e, "Invalid request body", nil)
	}
	var request resolveOutcomeRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return httpx.BadRequest(e, "Invalid request body", nil)
	}
	commandID := strings.TrimSpace(e.Request.PathValue("command_id"))
	report, resolveErr := control.ResolveOutcomeByOperator(e.Request.Context(), agentcontrol.OperatorResolution{
		TenantID: tenantID, AgentID: workerID, CommandID: commandID, ActorID: ownerID,
		Disposition: strings.ToLower(strings.TrimSpace(request.Disposition)), Note: request.Note, ExpectedRevision: request.ExpectedRevision,
	})
	if resolveErr != nil {
		var denial *guardbinding.Denial
		switch {
		case errors.As(resolveErr, &denial):
			status := http.StatusConflict
			if denial.Code == guardbinding.CodeOperatorNotAllowed {
				status = http.StatusBadRequest
			}
			return httpx.Error(e, status, ksapi.ErrCodeConflict, denial.Message, map[string]any{"reason": denial.Code, "guidance": denial.Guidance})
		case errors.Is(resolveErr, agentcontrol.ErrCommandNotPending):
			return httpx.NotFound(e, "Operation not found")
		default:
			return httpx.Error(e, http.StatusInternalServerError, ksapi.ErrCodeInternal, "Failed to resolve the outcome", nil)
		}
	}
	slog.Warn("guard_outcome_operator_resolution", "tenant_id", tenantID, "worker_id", workerID, "command_id", commandID,
		"revision", report.Revision, "disposition", report.Resolution, "actor", ownerID, "agent_id", agentID)
	return httpx.Success(e, http.StatusOK, resolveOutcomeResponse{
		CommandID: commandID, Revision: report.Revision, Disposition: report.Resolution, Actor: ownerID, Released: true,
	})
}
