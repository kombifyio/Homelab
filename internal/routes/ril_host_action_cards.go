package routes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kombifyio/homelab/pkg/agentcontrol"
	ksapi "github.com/kombifyio/homelab/pkg/api"
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/demoguard"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/jobs"
	rilaction "github.com/kombifyio/homelab/pkg/ril/actioncontract"
	"github.com/kombifyio/homelab/pkg/ril/actions"
	"github.com/kombifyio/homelab/pkg/ril/primitives"
	"github.com/kombifyio/homelab/pkg/runtimehealth"
	"github.com/kombifyio/homelab/pkg/servermaintenance"
	"github.com/kombifyio/homelab/pkg/serverregistry"
	"github.com/kombifyio/homelab/pkg/stackkitcommand"
)

// HostOperationEligibility is the one answer to "may a Guard host operation
// run on this server now". The owner's host routes (first line) and the
// executor-bound card admission of the six host primitives (second line, at
// card creation and again when the card begins) use the same rules: the node
// must be a connected, current Guard that is not the control plane or a
// protected demo or substrate node, and must advertise the capability the
// operation needs.
type HostOperationEligibility struct {
	// Workers reads the Guard facts: the capabilities the agent advertised.
	Workers controlplane.WorkerStore
	// ControlPlaneMachineIDDigest is servermaintenance.MachineIDDigest of this
	// control plane's own host; "" when unknown.
	ControlPlaneMachineIDDigest string
	// Entitlements gates host actions: the Edge-signed
	// `homelab.servers.maintenance` grant of the request that creates the
	// card, whichever entry (the owner's host route or /v1/ril/action-cards)
	// creates it. Nil is the fail-closed hosted gate.
	Entitlements EntitlementGate
}

// Reason is "" when eligible, otherwise the stable refusal reason.
func (e *HostOperationEligibility) Reason(ctx context.Context, server controlplane.ServerRuntime, now time.Time, capability string) (string, error) {
	if e == nil || e.Workers == nil {
		return "", errors.New("host operation eligibility is unavailable")
	}
	switch {
	case stringFromAnyMap(server.Metadata, "server_node_role") == "substrate", demoguard.IsProtectedLease(server.LeaseID), e.isControlPlaneHost(server):
		return hostOpsReasonNodeProtected, nil
	case serverregistry.LifecycleState(strings.TrimSpace(server.LifecycleState)) != serverregistry.LifecycleActive ||
		strings.TrimSpace(server.WorkerID) == "" || !serverregistry.MutationsAllowed(server.ConnectionState):
		return hostOpsReasonNotConnected, nil
	case server.LastHeartbeatAt == nil || now.Sub(server.LastHeartbeatAt.UTC()) > runtimehealth.FreshHeartbeatWindow:
		return hostOpsReasonHeartbeatStale, nil
	}
	worker, err := e.Workers.GetWorker(ctx, server.TenantID, strings.TrimSpace(server.WorkerID))
	if errors.Is(err, controlplane.ErrNotFound) {
		return hostOpsReasonNotConnected, nil
	}
	if err != nil {
		return "", err
	}
	if !containsString(serverCapabilitiesFromWorker(*worker).stackKitCapabilities, capability) {
		return hostOpsReasonCapabilityMissing, nil
	}
	return "", nil
}

// isControlPlaneHost compares the agent-reported machine-id digest with this
// control plane's own. Either side unknown is not a match.
func (e *HostOperationEligibility) isControlPlaneHost(server controlplane.ServerRuntime) bool {
	reported := strings.TrimSpace(stringFromAnyMap(server.Metadata, servermaintenance.MetadataMachineIDDigest))
	configured := strings.TrimSpace(e.ControlPlaneMachineIDDigest)
	return configured != "" && reported != "" && reported == configured
}

// hostObject is the validated object of a host primitive: a container name or
// id, or one .service unit. A protected container or unit is never valid, so
// no card, Decision or lease exists for it.
func hostObject(profile primitives.Profile, object string) (string, error) {
	switch profile.HostObject {
	case "container":
		if !stackkitcommand.ValidHostContainerRef(object) || stackkitcommand.ProtectedHostContainer(object) {
			return "", fmt.Errorf("the container is not an admitted host target")
		}
	case "unit":
		if !stackkitcommand.ValidHostUnit(object) || stackkitcommand.ProtectedHostUnit(object) {
			return "", fmt.Errorf("the unit is not an admitted host target")
		}
	default:
		return "", fmt.Errorf("not a host primitive")
	}
	return object, nil
}

// deriveHostTarget is the card target of a host primitive: the server, the
// StackKits node its agent owns and its current worker as runtime instance and
// execution channel (the node the Guard command addresses), and the one
// object the caller names, validated. Nothing about the object is invented:
// the Guard resolves a container to its full id and refuses a managed or
// protected target again before it acts.
func (h *serviceRuntimeHandlers) deriveHostTarget(ctx context.Context, card actions.ExecutorBoundTarget, server *controlplane.ServerRuntime) (rilaction.TargetBinding, []rilaction.Input, error) {
	object, err := hostObject(card.Profile, primitives.InputValue(card.Template.Inputs, card.Profile.HostInput()))
	if err != nil {
		return rilaction.TargetBinding{}, nil, err
	}
	// The commercial grant is a per-request Edge decision: it is checked where
	// the card is created, with the request that creates it. A card that began
	// later runs as system work and carries no request grant to re-check.
	if !entitlementGateOrHosted(h.hostEligibility.Entitlements).SignedGrant(ctx, ServerMaintenanceEntitlement) {
		return rilaction.TargetBinding{}, nil, fmt.Errorf("host operations need the %s entitlement", ServerMaintenanceEntitlement)
	}
	if err := h.hostEligible(ctx, server); err != nil {
		return rilaction.TargetBinding{}, nil, err
	}
	nodeRef, err := h.serviceActionNodeRef(ctx, card.TenantID, card.OwnerSubjectID, card.Template.StackID, server.WorkerID)
	if err != nil {
		return rilaction.TargetBinding{}, nil, err
	}
	return rilaction.TargetBinding{
			Scope: rilaction.TargetScopeRuntimeInstance, SiteRef: server.ID, NodeRef: nodeRef,
			RuntimeInstanceRef: server.WorkerID, ExecutionChannelRef: server.WorkerID,
		}, []rilaction.Input{
			primitives.Input(card.Profile.HostInput(), object),
		}, nil
}

// admitHostTarget validates a host card's target and input against fresh
// inventory when the card is created and again when it begins: the server's
// current worker and node, the validated unprotected object, the node's
// eligibility and capability, no active server maintenance and no other
// running mutation of the stack.
func (h *serviceRuntimeHandlers) admitHostTarget(ctx context.Context, card actions.ExecutorBoundTarget, server *controlplane.ServerRuntime) error {
	target, inputs := card.Template.Target, card.Template.Inputs
	if target.SiteRef != server.ID || target.RuntimeInstanceRef != server.WorkerID || target.ExecutionChannelRef != server.WorkerID {
		return fmt.Errorf("runtime target is not this server's current worker")
	}
	if _, err := hostObject(card.Profile, primitives.InputValue(inputs, card.Profile.HostInput())); err != nil {
		return err
	}
	if err := h.hostEligible(ctx, server); err != nil {
		return err
	}
	nodeRef, err := h.serviceActionNodeRef(ctx, card.TenantID, card.OwnerSubjectID, card.Template.StackID, server.WorkerID)
	if err != nil || nodeRef != target.NodeRef {
		return fmt.Errorf("runtime target node is not the node the agent owns")
	}
	if conflictID, err := h.activeServiceMutationCard(ctx, card.TenantID, card.Template.StackID, "", card.CardID); err != nil || conflictID != "" {
		return fmt.Errorf("a service action is already active")
	}
	return h.refuseExecutorBoundMaintenance(ctx, card.TenantID, server)
}

func (h *serviceRuntimeHandlers) hostEligible(ctx context.Context, server *controlplane.ServerRuntime) error {
	reason, err := h.hostEligibility.Reason(ctx, *server, h.now().UTC(), stackkitcommand.HostOperateCapability)
	if err != nil {
		return fmt.Errorf("host operation eligibility is unavailable")
	}
	if reason != "" {
		return fmt.Errorf("host operations are not available on this server: %s", reason)
	}
	return nil
}

// hostActionDigest binds a host card to its exact request, so a different
// server, verb or object under the same Idempotency-Key is a conflict.
func hostActionDigest(serverID, primitiveID, object string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{"server-host-action/v1", serverID, primitiveID, object}, "\x00")))
	return hex.EncodeToString(sum[:])
}

type serverHostActionResponse struct {
	ServerID     string `json:"server_id"`
	ActionCardID string `json:"action_card_id"`
	// JobID is the card's execution identity, as for the managed service
	// actions; the result is the card's evidence (get_action_card).
	JobID     string `json:"job_id"`
	Operation string `json:"operation"`
	Primitive string `json:"primitive"`
	Action    string `json:"action"`
	Target    string `json:"target"`
	Status    string `json:"status"`
}

// executeHostActionCard derives the governed card of one host action, creates
// it idempotently, admits it (the Gateway Decision answered by the acting
// agent's equipment, the lease and the fenced admission) and runs it through
// the same launcher as /v1/ril/action-cards and the managed service actions
// in the background. It is the only write path of a host action.
func (h serverRuntimeHandlers) executeHostActionCard(e *httpx.Event, target *hostOpsTarget, operation string, profile primitives.Profile, object, idempotencyKey string) error {
	ops := h.hostOps
	cards := ops.actionCards
	if cards == nil || cards.Authority == nil || cards.Launcher == nil || cards.Background == nil || ops.stacks == nil {
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Governed host actions are unavailable", map[string]any{"reason_code": hostOpsReasonUnavailable})
	}
	ctx := e.Request.Context()
	server := target.server
	identity := serviceActionCardIdentityFor(target.tenantID, target.ownerID, idempotencyKey)
	digest := hostActionDigest(server.ID, profile.ID, object)
	accepted := func(status string) error {
		return httpx.Success(e, http.StatusAccepted, serverHostActionResponse{
			ServerID: server.ID, ActionCardID: identity.card, JobID: identity.execution, Operation: operation,
			Primitive: profile.ID, Action: profile.HostAction, Target: object, Status: status,
		})
	}
	card, err := cards.Authority.Get(ctx, target.tenantID, target.ownerID, identity.card)
	switch {
	case errors.Is(err, actions.ErrCardNotFound):
		stack, stackErr := ops.stacks.GetStack(ctx, target.tenantID, server.StackID)
		if stackErr != nil || stack == nil {
			return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "This server has no stack to govern a host action", map[string]any{"reason_code": "host_action_stack_unavailable"})
		}
		plan := stringFromAnyMap(stack.RuntimeSummary, jobs.RuntimeListenerPlanHashResultField)
		if !resolvedPlanHashPattern.MatchString(plan) {
			return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "This server's stack has no applied plan to govern a host action", map[string]any{"reason_code": "host_action_plan_unavailable"})
		}
		card, err = cards.Authority.Create(ctx, actions.CreateGovernedCard{
			ID: identity.card, TenantID: target.tenantID, OwnerSubjectID: target.ownerID, ServerID: server.ID,
			Title: fmt.Sprintf("%s %s", profile.HostAction, object), Severity: "info",
			Template: actions.ActionTemplate{
				StackID: stack.ID, Primitive: rilaction.PrimitiveBinding{ID: profile.ID, OperationClass: profile.OperationClass},
				ResolvedPlanHash: plan, Target: rilaction.TargetBinding{Scope: profile.TargetScope},
				Inputs:          []rilaction.Input{primitives.Input(profile.HostInput(), object)},
				EvidenceSinkRef: serviceActionEvidenceSink(digest),
			},
		})
		if err != nil {
			// A concurrent exact retry may have created the same card.
			existing, getErr := cards.Authority.Get(ctx, target.tenantID, target.ownerID, identity.card)
			if getErr != nil {
				return writeGovernedActionError(e, err)
			}
			card = existing
		}
	case err != nil:
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Failed to inspect host action replay", nil)
	}
	if card.Template.EvidenceSinkRef != serviceActionEvidenceSink(digest) || card.Template.Primitive.ID != profile.ID {
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "Idempotency-Key was already used for a different request", nil)
	}
	input := identity.begin(target.tenantID, target.ownerID, h.now())
	if card.Status != "awaiting_approval" {
		// An exact retry of a card that already ran or is running. A still
		// executing one resumes its run; it is never sent a second time (the
		// ledger reservation and the one-use claim).
		if card.Status == "executing" || card.Status == "verifying" {
			cards.run(ctx, input)
		}
		return accepted(serviceActionCardStatus(card.Status))
	}
	admitted, _, err := cards.Launcher.Admit(ctx, input)
	if err != nil {
		return writeHostActionAdmissionError(e, err)
	}
	if admitted.Status == "executing" || admitted.Status == "verifying" {
		cards.run(ctx, input)
	}
	return accepted(serviceActionCardStatus(admitted.Status))
}

// writeHostActionAdmissionError maps an admission refusal; a missing agent
// capability keeps its stable reason.
func writeHostActionAdmissionError(e *httpx.Event, err error) error {
	var missing *agentcontrol.MissingCapabilityError
	if errors.As(err, &missing) {
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "The server agent does not support this host operation", map[string]any{"reason_code": hostOpsReasonCapabilityMissing})
	}
	return writeGovernedActionError(e, err)
}
