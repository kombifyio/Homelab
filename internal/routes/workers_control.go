package routes

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kombifyio/techstack/pkg/agentcontrol"
	ksapi "github.com/kombifyio/techstack/pkg/api"
	"github.com/kombifyio/techstack/pkg/api/agentpb"
	"github.com/kombifyio/techstack/pkg/controlplane"
	"github.com/kombifyio/techstack/pkg/guardbinding"
	"github.com/kombifyio/techstack/pkg/guardidentity"
	"github.com/kombifyio/techstack/pkg/httpx"
)

// maxTypedResultBytes bounds one typed result submission: the StackKits result
// limits (16 MiB result plus 16 MiB events, base64 encoded) with headroom.
const maxTypedResultBytes int64 = 64 << 20

const guardCommandPollTimeout = 10 * time.Second

type WorkerTypedControl interface {
	Poll(context.Context, string, []string) (*agentpb.StackKitCommand, bool, error)
	SubmitResult(string, *agentpb.StackKitResult) error
}

type tenantWorkerTypedControl interface {
	PollForTenant(context.Context, string, string, []string) (*agentpb.StackKitCommand, bool, error)
	SubmitResultForTenant(context.Context, string, string, *agentpb.StackKitResult) error
}

// boundTenantWorkerTypedControl is the binding-aware rendezvous: commands carry
// their admission binding and outcomes are committed as acknowledged revisions.
type boundTenantWorkerTypedControl interface {
	PollBoundForTenant(context.Context, agentcontrol.PollOptions) (*agentcontrol.Offer, bool, error)
	SubmitReportForTenant(context.Context, agentcontrol.ReportRequest) (guardbinding.Acknowledgement, error)
}

type workerControlRequest struct {
	RuntimeAgentID string                  `json:"runtime_agent_id"`
	TenantID       string                  `json:"tenant_id"`
	OwnerID        string                  `json:"owner_id"`
	StackID        string                  `json:"stack_id"`
	LeaseID        string                  `json:"lease_id"`
	ServerID       string                  `json:"server_id"`
	Capabilities   []string                `json:"capabilities,omitempty"`
	Result         *agentpb.StackKitResult `json:"result,omitempty"`
	// CommandID names the command of a report that carries no result.
	CommandID string `json:"command_id,omitempty"`
	// KnownCommands lists the command IDs a binding-aware Guard holds in its
	// durable journal; they are never offered again.
	KnownCommands []string `json:"known_commands,omitempty"`
	// Report is the revisioned outcome of a binding-aware Guard. Its absence
	// marks a legacy Guard result.
	Report *guardbinding.ResultReport `json:"report,omitempty"`
	// HeldPredecessorCommands lists the prior commands of the same tenant whose
	// unacknowledged outcome the Guard holds from an earlier worker binding.
	HeldPredecessorCommands []guardbinding.PredecessorCommand `json:"held_predecessor_commands,omitempty"`
	// PredecessorWorkerID and PredecessorRecovery deliver one such outcome under
	// the signed, result-only recovery statement the control plane issued.
	PredecessorWorkerID string                            `json:"predecessor_worker_id,omitempty"`
	PredecessorRecovery *guardbinding.PredecessorRecovery `json:"predecessor_recovery,omitempty"`
}

// credentialGeneration is the generation of the credential that authenticated
// the request; a worker without credential state is generation zero.
func credentialGeneration(authCtx runtimeAgentAuthContext) int64 {
	if authCtx.Worker == nil {
		return 0
	}
	state, err := controlplane.WorkerCredentialStateFromWorker(*authCtx.Worker)
	if err != nil {
		return 0
	}
	return state.Generation
}

// knownCredentialGeneration is credentialGeneration, or -1 when no credential
// state could be read. A key-bound Guard that was never rotated holds a real
// generation 0, so 0 alone cannot tell the two apart.
func knownCredentialGeneration(authCtx runtimeAgentAuthContext) int64 {
	if authCtx.Worker == nil {
		return -1
	}
	state, err := controlplane.WorkerCredentialStateFromWorker(*authCtx.Worker)
	if err != nil {
		return -1
	}
	return state.Generation
}

func (request workerControlRequest) authRequest(id string) workerInventoryRequest {
	return workerInventoryRequest{
		RuntimeAgentID: firstNonEmpty(request.RuntimeAgentID, id),
		TenantID:       request.TenantID, OwnerID: request.OwnerID, StackID: request.StackID,
		LeaseID: request.LeaseID, ServerID: request.ServerID,
	}
}

func (h workerRouteHandlers) nextTypedCommand(e *httpx.Event) error {
	if h.typedControl == nil {
		return httpx.Error(e, http.StatusServiceUnavailable, "UNAVAILABLE", "Typed HTTPS agent control is unavailable", nil)
	}
	id := strings.TrimSpace(e.Request.PathValue("id"))
	var request workerControlRequest
	if id == "" || json.NewDecoder(e.Request.Body).Decode(&request) != nil {
		return httpx.BadRequest(e, "Invalid typed control poll", nil)
	}
	authCtx, ok := h.authenticateRuntimeAgent(e, id, request.authRequest(id))
	if !ok {
		return nil
	}
	if authCtx.Worker != nil && authCtx.Worker.Type == "substrate" {
		// This endpoint dispatches StackKit commands only. Hypervisors never
		// become StackKit execution targets, even with a forged capability list.
		e.Response.WriteHeader(http.StatusNoContent)
		return nil
	}
	if notices, ok := h.typedControl.(workerOutcomeNotices); ok && len(request.KnownCommands) > 0 {
		// An operator resolution reaches the Guard before any new command: its
		// journal fence must release first, or the next mutation would be
		// refused as fenced by work the operator already settled.
		resolutions, noticeErr := notices.OperatorResolutionsForTenant(e.Request.Context(), authCtx.TenantID, authCtx.RuntimeAgentID, request.KnownCommands)
		if noticeErr != nil {
			return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Operator resolutions are unavailable", nil)
		}
		if len(resolutions) > 0 {
			return httpx.Success(e, http.StatusOK, map[string]any{"resolutions": resolutions})
		}
	}
	if recovery := h.predecessorRecoveryFor(e.Request.Context(), authCtx, request.HeldPredecessorCommands); recovery != nil {
		// Like a resolution, the recovery statement comes before new work: it
		// lets the Guard deliver the held outcomes that still fence this server.
		return httpx.Success(e, http.StatusOK, map[string]any{"predecessor_recovery": recovery})
	}
	refreshID := h.pendingInventoryRefresh(e.Request.Context(), authCtx)
	pollCtx, cancel := context.WithTimeout(e.Request.Context(), guardCommandPollTimeout)
	defer cancel()
	var command *agentpb.StackKitCommand
	var binding *guardbinding.Binding
	var found bool
	var err error
	if bound, ok := h.typedControl.(boundTenantWorkerTypedControl); ok {
		var offer *agentcontrol.Offer
		offer, found, err = bound.PollBoundForTenant(pollCtx, agentcontrol.PollOptions{
			TenantID: authCtx.TenantID, AgentID: authCtx.RuntimeAgentID, Capabilities: request.Capabilities,
			CurrentGeneration: credentialGeneration(authCtx), KnownCommands: request.KnownCommands,
			// A bearer-only worker may observe but is sent no mutating command.
			ObserveOnly: authCtx.IdentityBinding != controlplane.IdentityBindingHostKey,
		})
		if found {
			command, binding = offer.Command, offer.Binding
			if offer.Legacy {
				slog.Info("guard_legacy_command_dispatch", "tenant_id", authCtx.TenantID, "runtime_agent_id", authCtx.RuntimeAgentID, "command_id", command.GetCommandId())
			}
		}
	} else if authCtx.IdentityBinding != controlplane.IdentityBindingHostKey {
		// A control without the observe-only poll cannot tell observation from
		// mutation, so a bearer-only worker gets nothing from it.
		denyGuard(e, guardidentity.CodeIdentityUpgradeRequired, "")
		return nil
	} else if scoped, ok := h.typedControl.(tenantWorkerTypedControl); ok {
		command, found, err = scoped.PollForTenant(pollCtx, authCtx.TenantID, authCtx.RuntimeAgentID, request.Capabilities)
	} else {
		command, found, err = h.typedControl.Poll(pollCtx, authCtx.RuntimeAgentID, request.Capabilities)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			if refreshID != "" && e.Request.Context().Err() == nil {
				return httpx.Success(e, http.StatusOK, map[string]any{"inventory_refresh": refreshID})
			}
			e.Response.WriteHeader(http.StatusNoContent)
			return nil
		}
		return httpx.Error(e, http.StatusConflict, "CAPABILITY_REQUIRED", err.Error(), nil)
	}
	if !found {
		if refreshID != "" {
			return httpx.Success(e, http.StatusOK, map[string]any{"inventory_refresh": refreshID})
		}
		e.Response.WriteHeader(http.StatusNoContent)
		return nil
	}
	payload := map[string]any{"command": command}
	if refreshID != "" {
		payload["inventory_refresh"] = refreshID
	}
	if binding != nil {
		binding.ServerID = authCtx.ServerID
		signer, err := h.bindingSignerFor(e.Request.Context(), authCtx.TenantID, authCtx.RuntimeAgentID)
		if err == nil && signer != nil {
			err = signer.Sign(binding)
		}
		if err != nil {
			slog.Error("guard_binding_unsigned", "tenant_id", authCtx.TenantID, "runtime_agent_id", authCtx.RuntimeAgentID, "error", err)
			return httpx.Error(e, http.StatusServiceUnavailable, "BINDING_UNSIGNED", "The command binding could not be signed", nil)
		}
		payload["binding"] = binding
	}
	return httpx.Success(e, http.StatusOK, payload)
}

func (h workerRouteHandlers) pendingInventoryRefresh(ctx context.Context, authCtx runtimeAgentAuthContext) string {
	readCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	serverID := firstNonEmpty(authCtx.ServerID, runtimeServerIDForWorker(authCtx.RuntimeAgentID))
	server, err := h.serverStore.GetServerRuntime(readCtx, authCtx.TenantID, serverID)
	if err != nil {
		if !errors.Is(err, controlplane.ErrNotFound) {
			slog.Warn("guard_inventory_refresh_read_failed", "tenant_id", authCtx.TenantID, "server_id", serverID, "error", err)
		}
		return ""
	}
	if server == nil || server.WorkerID != authCtx.RuntimeAgentID || server.StackID != authCtx.StackID {
		return ""
	}
	return controlplane.PendingServerInventoryRefresh(*server, time.Now())
}

func (h workerRouteHandlers) submitTypedCommandResult(e *httpx.Event) error {
	if h.typedControl == nil {
		return httpx.Error(e, http.StatusServiceUnavailable, "UNAVAILABLE", "Typed HTTPS agent control is unavailable", nil)
	}
	id := strings.TrimSpace(e.Request.PathValue("id"))
	var request workerControlRequest
	if id == "" || json.NewDecoder(http.MaxBytesReader(e.Response, e.Request.Body, maxTypedResultBytes)).Decode(&request) != nil ||
		(request.Result == nil && (request.Report == nil || request.Report.State == guardbinding.ReportTerminal)) {
		return httpx.BadRequest(e, "Invalid typed control result", nil)
	}
	authCtx, ok := h.authenticateRuntimeAgent(e, id, request.authRequest(id))
	if !ok {
		return nil
	}
	if bound, ok := h.typedControl.(boundTenantWorkerTypedControl); ok && request.Report != nil {
		return h.submitTypedReport(e, bound, authCtx, request)
	}
	if strings.TrimSpace(request.PredecessorWorkerID) != "" {
		// A predecessor outcome is accepted only as a bound revision; it never
		// falls through to the legacy path of the current worker.
		return writeGuardDenial(e, guardbinding.Denyf(guardbinding.CodePredecessorScope, "a predecessor outcome needs the binding contract"))
	}
	if request.Result == nil {
		return httpx.BadRequest(e, "Invalid typed control result", nil)
	}
	var submitErr error
	if scoped, ok := h.typedControl.(tenantWorkerTypedControl); ok {
		submitErr = scoped.SubmitResultForTenant(e.Request.Context(), authCtx.TenantID, authCtx.RuntimeAgentID, request.Result)
	} else {
		submitErr = h.typedControl.SubmitResult(authCtx.RuntimeAgentID, request.Result)
	}
	if err := submitErr; err != nil {
		var denial *guardbinding.Denial
		if errors.As(err, &denial) {
			return writeGuardDenial(e, denial)
		}
		if errors.Is(err, agentcontrol.ErrCommandNotPending) || errors.Is(err, agentcontrol.ErrResultRejected) {
			return httpx.Error(e, http.StatusConflict, "CONFLICT", "Typed command result does not match a pending command", nil)
		}
		logTypedOutcomeFailure(e, "workers.commands.result", id, authCtx, request.Result.GetCommandId(), err)
		return httpx.Error(e, http.StatusInternalServerError, "INTERNAL", "Failed to accept typed command result", nil)
	}
	return httpx.Success(e, http.StatusAccepted, map[string]any{"command_id": request.Result.GetCommandId(), "accepted": true})
}

// submitTypedReport commits one revisioned outcome and answers only after the
// control-plane transaction committed it.
func (h workerRouteHandlers) submitTypedReport(e *httpx.Event, control boundTenantWorkerTypedControl, authCtx runtimeAgentAuthContext, request workerControlRequest) error {
	commandID := strings.TrimSpace(request.CommandID)
	if resultID := strings.TrimSpace(request.Result.GetCommandId()); commandID == "" {
		commandID = resultID
	} else if resultID != "" && resultID != commandID {
		return writeGuardDenial(e, guardbinding.Denyf(guardbinding.CodeBindingMismatch, "result names another command"))
	}
	predecessor := strings.TrimSpace(request.PredecessorWorkerID)
	if predecessor != "" {
		if denial := h.admitPredecessorDelivery(e.Request.Context(), authCtx, commandID, predecessor, request.PredecessorRecovery); denial != nil {
			slog.Warn("guard_predecessor_delivery_refused", "tenant_id", authCtx.TenantID, "runtime_agent_id", authCtx.RuntimeAgentID,
				"predecessor_worker_id", predecessor, "command_id", commandID, "reason", denial.Code)
			return writeGuardDenial(e, denial)
		}
	}
	ack, err := control.SubmitReportForTenant(e.Request.Context(), agentcontrol.ReportRequest{
		TenantID: authCtx.TenantID, AgentID: authCtx.RuntimeAgentID, CommandID: commandID,
		Generation: credentialGeneration(authCtx), Report: *request.Report, Result: request.Result,
		GenerationUnknown:  knownCredentialGeneration(authCtx) < 0,
		PredecessorAgentID: predecessor,
	})
	if err == nil && predecessor != "" {
		slog.Info("guard_predecessor_outcome_recovered", "tenant_id", authCtx.TenantID, "runtime_agent_id", authCtx.RuntimeAgentID,
			"predecessor_worker_id", predecessor, "command_id", commandID, "revision", request.Report.Revision)
	}
	if err != nil {
		var denial *guardbinding.Denial
		switch {
		case errors.As(err, &denial):
			return writeGuardDenial(e, denial)
		case errors.Is(err, agentcontrol.ErrCommandNotPending):
			return writeGuardDenial(e, guardbinding.Denyf(guardbinding.CodeCommandNotPending, "the command is not awaiting this report"))
		case errors.Is(err, agentcontrol.ErrResultRejected):
			return writeGuardDenial(e, guardbinding.Denyf(guardbinding.CodeResultRejected, "the report was rejected"))
		default:
			logTypedOutcomeFailure(e, "workers.commands.report", e.Request.PathValue("id"), authCtx, commandID, err,
				"report_state", request.Report.State, "report_revision", request.Report.Revision, "predecessor_worker_id", predecessor)
			return httpx.Error(e, http.StatusInternalServerError, ksapi.ErrCodeInternal, "Failed to commit typed command outcome", nil)
		}
	}
	return httpx.Success(e, http.StatusAccepted, ack)
}

// logTypedOutcomeFailure names why the control plane could not commit a
// Guard's typed result or report, before the 500 answers it: the Sentry event
// of the response carries the cause, and one structured line names the
// command, the worker, the credential generation (-1 when unreadable) and the
// error class. A PostgreSQL refusal adds its SQLSTATE, constraint and table,
// never the failing row; the error text is the redacted, bounded cause that
// httpx.RecordDependencyFailure keeps. Request payloads, results and proofs
// are never logged. A client that left first is logged as a warning.
func logTypedOutcomeFailure(e *httpx.Event, operation, workerID string, authCtx runtimeAgentAuthContext, commandID string, err error, extra ...any) {
	httpx.RecordDependencyFailure(e, operation, err)
	ctx := e.Request.Context()
	failure, _ := httpx.DependencyFailureFrom(ctx)
	attrs := []any{
		"operation", operation,
		"tenant_id", authCtx.TenantID,
		"worker_id", boundedLogID(workerID),
		"runtime_agent_id", authCtx.RuntimeAgentID,
		"command_id", boundedLogID(commandID),
		"credential_generation", knownCredentialGeneration(authCtx),
		"error_class", failure.Class,
		"error", failure.Message,
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		attrs = append(attrs, "sqlstate", pgErr.Code, "constraint", pgErr.ConstraintName, "table", pgErr.TableName)
	}
	level := slog.LevelError
	if failure.Class == "canceled" || httpx.ClientGone(ctx) {
		level = slog.LevelWarn
	}
	slog.Log(ctx, level, "guard_typed_outcome_commit_failed", append(attrs, extra...)...)
}

// boundedLogID keeps a caller-supplied identifier to the 128 bytes a valid
// command ID can have, so an oversized one cannot flood the log line.
func boundedLogID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 128 {
		return strings.ToValidUTF8(value[:128], "") + "…"
	}
	return value
}

// writeGuardDenial renders a structured, machine-readable refusal. The Guard
// treats these statuses as permanent: the same report will not succeed later.
func writeGuardDenial(e *httpx.Event, denial *guardbinding.Denial) error {
	status, code := http.StatusConflict, ksapi.ErrCodeConflict
	switch denial.Code {
	case guardbinding.CodeStaleGeneration, guardbinding.CodeAuthorityExpired:
		status, code = http.StatusForbidden, ksapi.ErrCodeForbidden
	case guardbinding.CodeResultDigestMismatch, guardbinding.CodeBindingMismatch:
		status, code = http.StatusBadRequest, ksapi.ErrCodeBadRequest
	}
	return httpx.Error(e, status, code, denial.Message, map[string]any{"reason": denial.Code, "guidance": denial.Guidance})
}
