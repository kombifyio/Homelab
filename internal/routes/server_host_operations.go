package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kombifyio/homelab/pkg/agentcontrol"
	ksapi "github.com/kombifyio/homelab/pkg/api"
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/jobs"
	"github.com/kombifyio/homelab/pkg/ril/primitives"
	"github.com/kombifyio/homelab/pkg/stackkitcommand"
)

// Stable refusal reasons of the server host operation routes. Refusals the
// Guard raises itself (container_managed, container_protected,
// container_not_found, unit_not_found, unit_protected) pass through unchanged.
const (
	hostOpsReasonUnavailable       = "host_operations_unavailable"
	hostOpsReasonNotConnected      = "agent_not_connected"
	hostOpsReasonHeartbeatStale    = "heartbeat_stale"
	hostOpsReasonCapabilityMissing = "agent_capability_missing"
	hostOpsReasonNodeProtected     = "node_protected"
	hostOpsReasonUnitProtected     = "unit_protected"
	hostOpsReasonContainerProtect  = "container_protected"
	hostOpsReasonFailed            = "host_operation_failed"
)

// HostOperationsDispatcher runs the two read-only Guard host operations on a
// node's agent (jobs.HostOperationsDispatcher). The mutating ones are the
// governed host primitives of the action-card path only. While it is nil the
// routes are absent.
type HostOperationsDispatcher interface {
	ListProcesses(ctx context.Context, target jobs.HostOperationTarget, limit int32) (stackkitcommand.HostOperationData, error)
	ListContainers(ctx context.Context, target jobs.HostOperationTarget, limit int32) (stackkitcommand.HostOperationData, error)
}

// ServerHostOperationsRouteConfig wires the owner's host inspection and action
// routes under /api/v1/servers/{serverId}/host/.
type ServerHostOperationsRouteConfig struct {
	Dispatcher HostOperationsDispatcher
	// Workers reads the Guard facts: the capabilities the agent advertised.
	Workers controlplane.WorkerStore
	// Entitlements gates every route and the host cards; nil is the fail-closed hosted gate.
	Entitlements EntitlementGate
	// ControlPlaneMachineIDDigest is servermaintenance.MachineIDDigest of this
	// control plane's own host; "" when unknown.
	ControlPlaneMachineIDDigest string
	// ActionCards and Stacks are the governed action-card path every host
	// action (container and service-unit start, stop, restart) enters: the
	// same card, equipment-answered Decision, lease, dispatcher and evidence as
	// the managed service actions. Without them an action is unavailable.
	ActionCards *ServiceActionCards
	Stacks      controlplane.StackStore
}

type serverHostOperationsHandlers struct {
	dispatcher   HostOperationsDispatcher
	entitlements EntitlementGate
	eligibility  *HostOperationEligibility
	actionCards  *ServiceActionCards
	stacks       controlplane.StackStore
}

func newServerHostOperationsHandlers(cfg *ServerHostOperationsRouteConfig) *serverHostOperationsHandlers {
	if cfg == nil || cfg.Dispatcher == nil {
		return nil
	}
	if cfg.Workers == nil {
		panic("RegisterServerRuntimeRoutes: server host operations need the worker store")
	}
	return &serverHostOperationsHandlers{
		dispatcher: cfg.Dispatcher, entitlements: entitlementGateOrHosted(cfg.Entitlements),
		eligibility: &HostOperationEligibility{Workers: cfg.Workers, ControlPlaneMachineIDDigest: cfg.ControlPlaneMachineIDDigest, Entitlements: cfg.Entitlements},
		actionCards: cfg.ActionCards, stacks: cfg.Stacks,
	}
}

type serverHostProcessesResponse struct {
	ServerID  string                        `json:"server_id"`
	Total     int                           `json:"total"`
	Truncated bool                          `json:"truncated"`
	Processes []stackkitcommand.HostProcess `json:"processes"`
}

type serverHostContainersResponse struct {
	ServerID   string                          `json:"server_id"`
	Total      int                             `json:"total"`
	Truncated  bool                            `json:"truncated"`
	Containers []stackkitcommand.HostContainer `json:"containers"`
}

type serverHostActionRequest struct {
	Container string `json:"container,omitempty"`
	Unit      string `json:"unit,omitempty"`
	Action    string `json:"action"`
}

// hostOpsTarget is the owner's server with its Guard identity, after every
// admission check that does not depend on the request body.
type hostOpsTarget struct {
	server   controlplane.ServerRuntime
	tenantID string
	ownerID  string
}

// hostOpsEligibility is the node's own answer to "may a host operation run
// here": "" when eligible, otherwise the stable refusal reason.
func (h *serverHostOperationsHandlers) hostOpsEligibility(ctx context.Context, server controlplane.ServerRuntime, now time.Time, capability string) (string, error) {
	return h.eligibility.Reason(ctx, server, now, capability)
}

// hostOpsServer resolves the owner's server for one route. Reads and actions
// need ownership of the server in the tenant and the signed maintenance
// entitlement; an action also needs the Inventory operate grant on the server
// (the card's own admission repeats the node rules).
func (h serverRuntimeHandlers) hostOpsServer(e *httpx.Event, mutating bool, capability string) (*hostOpsTarget, error) {
	ops := h.hostOps
	capabilityScope := "homelab.servers.host.inspect"
	if mutating {
		capabilityScope = "homelab.servers.host.operate"
	}
	server, err := h.ownedServer(e, capabilityScope)
	if err != nil || server == nil {
		return nil, err
	}
	ownerID, _, _ := authenticatedUser(e)
	ctx := e.Request.Context()
	if !ops.entitlements.SignedGrant(ctx, ServerMaintenanceEntitlement) {
		return nil, writeEntitlementDenial(e, capabilityScope, ServerMaintenanceEntitlement)
	}
	if mutating {
		if authErr := authorizeOwnerServerOperate(ctx, h.policy, h.store, inventoryScope{tenantID: server.TenantID, ownerID: ownerID}, server.ID); authErr != nil {
			return nil, writeInventoryHTTPError(e, authErr)
		}
	}
	reason, reasonErr := ops.hostOpsEligibility(ctx, *server, h.now().UTC(), capability)
	if reasonErr != nil {
		return nil, httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Server host operations are unavailable", map[string]any{"reason_code": hostOpsReasonUnavailable})
	}
	if reason != "" {
		return nil, httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "This server cannot run host operations now", map[string]any{"reason_code": reason})
	}
	e.Response.Header().Set("Cache-Control", "private, no-store")
	return &hostOpsTarget{server: *server, tenantID: server.TenantID, ownerID: ownerID}, nil
}

func hostOpsLimit(e *httpx.Event) (int32, error) {
	raw := strings.TrimSpace(e.Request.URL.Query().Get("limit"))
	if raw == "" {
		return 0, nil
	}
	limit, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || limit < 1 || limit > stackkitcommand.HostListMaxLimit {
		return 0, httpx.BadRequest(e, "limit must be between 1 and "+strconv.Itoa(stackkitcommand.HostListMaxLimit), nil)
	}
	return int32(limit), nil
}

func (h serverRuntimeHandlers) hostProcesses(e *httpx.Event) error {
	target, err := h.hostOpsServer(e, false, stackkitcommand.HostInspectCapability)
	if err != nil || target == nil {
		return err
	}
	limit, err := hostOpsLimit(e)
	if err != nil {
		return err
	}
	data, runErr := h.hostOps.dispatcher.ListProcesses(e.Request.Context(), jobs.HostOperationTarget{TenantID: target.tenantID, AgentID: target.server.WorkerID}, limit)
	if runErr != nil {
		return writeHostOpsError(e, runErr)
	}
	if data.Processes == nil {
		data.Processes = []stackkitcommand.HostProcess{}
	}
	return httpx.Success(e, http.StatusOK, serverHostProcessesResponse{ServerID: target.server.ID, Total: data.Total, Truncated: data.Truncated, Processes: data.Processes})
}

func (h serverRuntimeHandlers) hostContainers(e *httpx.Event) error {
	target, err := h.hostOpsServer(e, false, stackkitcommand.HostInspectCapability)
	if err != nil || target == nil {
		return err
	}
	limit, err := hostOpsLimit(e)
	if err != nil {
		return err
	}
	data, runErr := h.hostOps.dispatcher.ListContainers(e.Request.Context(), jobs.HostOperationTarget{TenantID: target.tenantID, AgentID: target.server.WorkerID}, limit)
	if runErr != nil {
		return writeHostOpsError(e, runErr)
	}
	if data.Containers == nil {
		data.Containers = []stackkitcommand.HostContainer{}
	}
	return httpx.Success(e, http.StatusOK, serverHostContainersResponse{ServerID: target.server.ID, Total: data.Total, Truncated: data.Truncated, Containers: data.Containers})
}

// hostContainerAction starts, stops or restarts one unmanaged container.
func (h serverRuntimeHandlers) hostContainerAction(e *httpx.Event) error {
	return h.hostAction(e, agentOperationContainer)
}

// hostSystemdAction starts, stops or restarts one service unit.
func (h serverRuntimeHandlers) hostSystemdAction(e *httpx.Event) error {
	return h.hostAction(e, agentOperationSystemd)
}

const (
	agentOperationContainer = "container_action"
	agentOperationSystemd   = "systemd_action"
)

// hostAction is the shared admission of both mutating routes: authorization, a
// strict body without any approval flag, an Idempotency-Key that binds the retry
// to the first attempt, the protected-target refusals and then the one governed
// path: the host primitive of the object kind and verb as an action card with
// the equipment-answered Decision, exact lease and node proof.
func (h serverRuntimeHandlers) hostAction(e *httpx.Event, operation string) error {
	target, err := h.hostOpsServer(e, true, stackkitcommand.HostOperateCapability)
	if err != nil || target == nil {
		return err
	}
	request, err := decodeServerHostActionRequest(e, operation)
	if err != nil {
		return err
	}
	idempotencyKey := strings.TrimSpace(e.Request.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" || len(idempotencyKey) > 128 {
		return httpx.BadRequest(e, "A bounded Idempotency-Key is required", nil)
	}
	object, kind := request.Container, "container"
	if operation == agentOperationSystemd {
		object, kind = request.Unit, "unit"
	}
	switch {
	case operation == agentOperationSystemd && stackkitcommand.ProtectedHostUnit(object):
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "This unit belongs to the management path of the server", map[string]any{"reason_code": hostOpsReasonUnitProtected})
	case operation == agentOperationContainer && stackkitcommand.ProtectedHostContainer(object):
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "This container belongs to the management path of the server", map[string]any{"reason_code": hostOpsReasonContainerProtect})
	}
	profile, ok := primitives.ForHost(kind, request.Action)
	if !ok {
		return httpx.Reject(e, http.StatusBadRequest, ksapi.ErrCodeBadRequest, "Action must be start, stop or restart", nil)
	}
	slog.InfoContext(e.Request.Context(), "server_host_action_card",
		"server_id", target.server.ID, "tenant_id", target.tenantID, "owner_subject_id", target.ownerID,
		"primitive", profile.ID, "target", object)
	return h.executeHostActionCard(e, target, operation, profile, object, idempotencyKey)
}

// serverHostActionFields are the only accepted body keys, matched exactly, so a
// duplicate or differently cased key cannot change what the request means to
// the Edge and to this handler. There is no approval flag: the acting agent's
// equipment answers the card's Decision.
var serverHostActionFields = map[string]map[string]bool{
	agentOperationContainer: {"container": true, "action": true},
	agentOperationSystemd:   {"unit": true, "action": true},
}

func decodeServerHostActionRequest(e *httpx.Event, operation string) (serverHostActionRequest, error) {
	var request serverHostActionRequest
	raw, err := io.ReadAll(io.LimitReader(e.Request.Body, 1025))
	if err != nil || len(raw) > 1024 || !exactJSONObjectKeys(raw, serverHostActionFields[operation]) {
		return request, httpx.Reject(e, http.StatusBadRequest, ksapi.ErrCodeBadRequest, "Invalid request body", nil)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, httpx.Reject(e, http.StatusBadRequest, ksapi.ErrCodeBadRequest, "Invalid request body", nil)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return request, httpx.Reject(e, http.StatusBadRequest, ksapi.ErrCodeBadRequest, "Request body must contain one JSON document", nil)
	}
	request.Action = strings.ToLower(strings.TrimSpace(request.Action))
	request.Container = strings.TrimSpace(request.Container)
	request.Unit = strings.TrimSpace(request.Unit)
	if !stackkitcommand.ValidHostAction(request.Action) {
		return request, httpx.Reject(e, http.StatusBadRequest, ksapi.ErrCodeBadRequest, "Action must be start, stop or restart", nil)
	}
	if operation == agentOperationContainer && !stackkitcommand.ValidHostContainerRef(request.Container) {
		return request, httpx.Reject(e, http.StatusBadRequest, ksapi.ErrCodeBadRequest, "container must be a container name or id", nil)
	}
	if operation == agentOperationSystemd && !stackkitcommand.ValidHostUnit(request.Unit) {
		return request, httpx.Reject(e, http.StatusBadRequest, ksapi.ErrCodeBadRequest, "unit must be one .service unit name", nil)
	}
	return request, nil
}

// writeHostOpsError maps a dispatch outcome to the response. A Guard refusal
// changed nothing; a Guard failure may have changed the node; any other error
// is a dispatch problem (no agent, timeout, rejected result).
func writeHostOpsError(e *httpx.Event, err error) error {
	var refusal *jobs.HostOperationRefusal
	var failure *jobs.HostOperationFailure
	var missing *agentcontrol.MissingCapabilityError
	switch {
	case errors.As(err, &refusal):
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, refusal.Message, map[string]any{"reason_code": refusal.Code, "outcome": "refused"})
	case errors.As(err, &failure):
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, failure.Message, map[string]any{"reason_code": failure.Code, "outcome": "failed"})
	case errors.As(err, &missing):
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "The server agent does not support this host operation", map[string]any{"reason_code": hostOpsReasonCapabilityMissing})
	case errors.Is(err, jobs.ErrHostOperationsUnavailable):
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Server host operations are unavailable", map[string]any{"reason_code": hostOpsReasonUnavailable})
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "The server did not answer in time", map[string]any{"reason_code": hostOpsReasonNotConnected})
	default:
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "The host operation could not be completed", map[string]any{"reason_code": hostOpsReasonFailed})
	}
}
