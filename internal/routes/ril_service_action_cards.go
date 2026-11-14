package routes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	ksapi "github.com/kombifyio/techstack/pkg/api"
	"github.com/kombifyio/techstack/pkg/controlplane"
	"github.com/kombifyio/techstack/pkg/httpx"
	"github.com/kombifyio/techstack/pkg/jobs"
	"github.com/kombifyio/techstack/pkg/orchestrator"
	rilaction "github.com/kombifyio/techstack/pkg/ril/actioncontract"
	"github.com/kombifyio/techstack/pkg/ril/actions"
	"github.com/kombifyio/techstack/pkg/ril/primitives"
	"github.com/kombifyio/techstack/pkg/runtimehealth"
	"github.com/kombifyio/techstack/pkg/serverregistry"
	"github.com/kombifyio/techstack/pkg/serviceregistry"
)

// ServiceActionCards is the governed action-card path that every mutating
// managed service action enters (run_managed_service_action `start`, `stop`,
// `restart`, `update`, `backup`, adopted applications included): the same
// card, Decision, lease, dispatcher and evidence as /v1/ril/action-cards, with
// the managed route's exact replay semantics. It is the only write path of
// these actions.
type ServiceActionCards struct {
	Authority actions.Authority
	Launcher  ServiceActionCardLauncher
	// Lifecycle binds a lifecycle request to canonical stack, agent and node
	// authority (the orchestrator), so a card names the node the managed
	// command addresses.
	Lifecycle ServiceActionLifecycle
	// Background turns the admitted request's context into Techstack system
	// work for the execution that outlives the request (the Edge-signed
	// database context of a request is never reused after it).
	Background func(context.Context) context.Context
}

type ServiceActionCardLauncher interface {
	Admit(context.Context, actions.BeginExecution) (*actions.GovernedCard, bool, error)
	Execute(context.Context, actions.BeginExecution) (*actions.GovernedCard, error)
}

type ServiceActionLifecycle interface {
	PrepareStackKitLifecycleCommand(context.Context, jobs.StackKitLifecycleRequest) (jobs.StackKitLifecycleRequest, error)
}

// serviceActionCardRunBudget bounds one background card execution: an update
// runs its target and automatic rollback budget, a restart its verify.
const serviceActionCardRunBudget = 45 * time.Minute

var resolvedPlanHashPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// serviceActionPrimitives maps every mutating managed service action to its
// executor-bound RIL primitive. An adopted application's start, stop or
// restart is the same primitive; its card names the application's exact
// container (application-ref) and the node runs application control.
var serviceActionPrimitives = map[string]string{
	serviceActionStart: "start-service", serviceActionStop: "stop-service", serviceActionRestart: "restart-service",
	serviceActionUpdate: "update-service", serviceActionBackup: "run-backup",
}

// serviceActionUsesCard reports the managed actions that run as a governed
// action card: every agent-executed mutation. `logs` is a read job of the
// separate log request (requestLogs), and
// freeze/unfreeze are control-plane guardrails that never reach the node.
func serviceActionUsesCard(action string) bool {
	_, ok := serviceActionPrimitives[action]
	return ok
}

func serviceActionPrimitive(action string) primitives.Profile {
	profile, _ := primitives.Lookup(serviceActionPrimitives[action])
	return profile
}

// serviceActionCardIdentity derives the card and its one execution from the
// tenant, principal and Idempotency-Key, exactly like the managed durable job
// identity; the raw key is never persisted.
type serviceActionCardIdentity struct {
	card, execution, trace, idempotency string
}

func serviceActionCardIdentityFor(tenantID, ownerID, idempotencyKey string) serviceActionCardIdentity {
	card := jobs.StackKitServiceActionJobID(tenantID, ownerID, idempotencyKey)
	suffix := strings.TrimPrefix(card, "job-stackkit-service-")
	return serviceActionCardIdentity{
		card: card, execution: "exec-stackkit-service-" + suffix,
		trace: "trace-stackkit-service-" + suffix, idempotency: "idem-stackkit-service-" + suffix,
	}
}

func (id serviceActionCardIdentity) begin(tenantID, ownerID string, now time.Time) actions.BeginExecution {
	return actions.BeginExecution{TenantID: tenantID, OwnerSubjectID: ownerID, CardID: id.card, ExecutionID: id.execution, TraceID: id.trace, IdempotencyKey: id.idempotency, Now: now.UTC()}
}

// serviceActionEvidenceSink binds the card to the request fingerprint, so a
// different body under the same Idempotency-Key is a conflict.
func serviceActionEvidenceSink(digest string) string { return "evidence:service-action/" + digest }

func serviceActionCardStatus(status string) string {
	switch status {
	case "executing", "verifying":
		return "running"
	case "completed":
		return "completed"
	case "failed":
		return "failed"
	case "denied":
		return "cancelled"
	default:
		return "queued"
	}
}

// replayServiceActionCard answers a retry from the existing card before any
// target validation, like the job receipt replay. A card that was never
// admitted (still awaiting its Decision) falls through to the validated path,
// which admits that same card again.
func (h *serviceRuntimeHandlers) replayServiceActionCard(e *httpx.Event, tenantID, ownerID, idempotencyKey, digest string, service *controlplane.ServiceRuntime, action string) (bool, error) {
	if h.actionCards == nil || h.actionCards.Authority == nil || h.actionCards.Launcher == nil || h.actionCards.Background == nil {
		return true, httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Governed service actions are unavailable", nil)
	}
	identity := serviceActionCardIdentityFor(tenantID, ownerID, idempotencyKey)
	card, err := h.actionCards.Authority.Get(e.Request.Context(), tenantID, ownerID, identity.card)
	if errors.Is(err, actions.ErrCardNotFound) {
		return false, nil
	}
	if err != nil {
		return true, httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Failed to inspect service action replay", nil)
	}
	if card.Template.EvidenceSinkRef != serviceActionEvidenceSink(digest) || card.Template.Primitive.ID != serviceActionPrimitive(action).ID {
		return true, httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "Idempotency-Key was already used for a different request", nil)
	}
	if card.Status == "awaiting_approval" {
		return false, nil
	}
	if card.Status == "executing" || card.Status == "verifying" {
		// A same-key retry resumes an admitted execution whose background run
		// was lost (for example to a restart between admission and the
		// workflow start). The run identity is deterministic and
		// StartOrResumeRun adopts an existing run; a run still advancing in
		// this process holds its run lock, so the resume only waits for it.
		// The execution is still sent at most once: the ledger reservation
		// of this exact request is never taken over after its window (the
		// takeover window is the same request's own validity), and the
		// Gateway claim of the execution is one-use, so a resumed run after
		// a sent command reads back the reservation and never sends again.
		h.runServiceActionCard(e.Request.Context(), identity.begin(tenantID, ownerID, h.now()))
	}
	return true, serviceActionCardAccepted(e, service, action, identity, card)
}

// runServiceActionCard runs the admitted card through the launcher in the
// background, as Techstack system work that outlives the request.
func (h *serviceRuntimeHandlers) runServiceActionCard(ctx context.Context, input actions.BeginExecution) {
	h.actionCards.run(ctx, input)
}

// run executes the admitted card through the launcher in the background, as
// Techstack system work that outlives the request. Every governed entry (the
// managed service actions and the host actions) uses it.
func (c *ServiceActionCards) run(ctx context.Context, input actions.BeginExecution) {
	launcher := c.Launcher
	run := c.Background(context.WithoutCancel(ctx))
	go func() {
		runCtx, cancel := context.WithTimeout(run, serviceActionCardRunBudget)
		defer cancel()
		// The card holds the durable result; the launcher records any
		// failure on it.
		_, _ = launcher.Execute(runCtx, input)
	}()
}

// activeServiceMutationCard is the action-card half of the managed service
// concurrency guard: an executing or verifying start, stop or restart card of
// this service key, or an update or backup card anywhere on the stack. An
// empty serviceKey (an update or backup) matches every mutating card of the
// stack.
func (h *serviceRuntimeHandlers) activeServiceMutationCard(ctx context.Context, tenantID, stackID, serviceKey, excludeCardID string) (string, error) {
	if h.actionCards == nil || h.actionCards.Authority == nil {
		return "", errors.New("governed action cards are unavailable")
	}
	finder, ok := h.actionCards.Authority.(interface {
		ActiveServiceMutationCard(context.Context, string, string, string, string) (string, error)
	})
	if !ok {
		return "", errors.New("governed action-card concurrency authority is unavailable")
	}
	return finder.ActiveServiceMutationCard(ctx, tenantID, stackID, serviceKey, excludeCardID)
}

func serviceActionCardAccepted(e *httpx.Event, service *controlplane.ServiceRuntime, action string, identity serviceActionCardIdentity, card *actions.GovernedCard) error {
	return httpx.Success(e, http.StatusAccepted, serviceActionResponse{
		JobID: identity.execution, ActionCardID: card.ID, ServiceID: service.ID, Action: action, Status: serviceActionCardStatus(card.Status),
	})
}

// executeServiceActionCard derives the governed card of the managed action,
// creates it idempotently, admits it (the Gateway Decision answered by the
// acting agent's equipment, the lease and the fenced admission) and runs it
// through the same launcher as /v1/ril/action-cards in the background. The
// managed route's checks (lock, maintenance, freshness, allowed actions) ran
// before; the card's own admission repeats the protected-target refusals.
func (h *serviceRuntimeHandlers) executeServiceActionCard(e *httpx.Event, tenantID, ownerID, idempotencyKey, digest string, service *controlplane.ServiceRuntime, stack *controlplane.Stack, server *controlplane.ServerRuntime, action string) error {
	ctx := e.Request.Context()
	identity := serviceActionCardIdentityFor(tenantID, ownerID, idempotencyKey)
	if conflictID, err := h.activeServiceActionJob(ctx, tenantID, stack.ID, service.ID, ""); err != nil {
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Failed to inspect active service actions", nil)
	} else if conflictID != "" {
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "A service action is already active", map[string]any{"job_id": conflictID})
	}
	if conflictID, err := h.activeServiceMutationCard(ctx, tenantID, stack.ID, serviceActionCardGuardKey(service, action), identity.card); err != nil {
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Failed to inspect active service actions", nil)
	} else if conflictID != "" {
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "A service action is already active", map[string]any{"action_card_id": conflictID})
	}
	card, err := h.actionCards.Authority.Get(ctx, tenantID, ownerID, identity.card)
	if errors.Is(err, actions.ErrCardNotFound) {
		if action == serviceActionBackup {
			// A kit whose backups are in managed custody refuses here with
			// its guidance, before any card, Decision or lease exists.
			if _, prepareErr := h.actionCards.Lifecycle.PrepareStackKitLifecycleCommand(ctx, jobs.StackKitLifecycleRequest{
				StackID: stack.ID, TenantID: tenantID, OwnerID: ownerID, AgentID: server.WorkerID, Operation: jobs.StackKitLifecycleBackupRun, OwnerApproved: true,
			}); errors.Is(prepareErr, orchestrator.ErrStackKitBackupCustodyManaged) {
				return managedBackupCustodyError(e)
			}
		}
		template, templateErr := h.serviceActionCardTemplate(stack, service, action, digest)
		if templateErr != nil {
			return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "Service action target is unavailable", map[string]any{"reason_code": "service_action_target_unavailable"})
		}
		card, err = h.actionCards.Authority.Create(ctx, actions.CreateGovernedCard{
			ID: identity.card, TenantID: tenantID, OwnerSubjectID: ownerID, ServerID: server.ID,
			Title: fmt.Sprintf("%s %s", action, firstNonEmptyString(service.Name, serviceActionControlKey(service))), Severity: "info", Template: template,
		})
		if err != nil {
			// A concurrent exact retry may have created the same card.
			existing, getErr := h.actionCards.Authority.Get(ctx, tenantID, ownerID, identity.card)
			if getErr != nil {
				return writeGovernedActionError(e, err)
			}
			card = existing
		}
	} else if err != nil {
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Failed to inspect service action replay", nil)
	}
	if card.Template.EvidenceSinkRef != serviceActionEvidenceSink(digest) || card.Template.Primitive.ID != serviceActionPrimitive(action).ID {
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "Idempotency-Key was already used for a different request", nil)
	}
	input := identity.begin(tenantID, ownerID, h.now())
	admitted, _, err := h.actionCards.Launcher.Admit(ctx, input)
	if err != nil {
		return writeGovernedActionError(e, err)
	}
	if admitted.Status == "executing" || admitted.Status == "verifying" {
		h.runServiceActionCard(ctx, input)
	}
	return serviceActionCardAccepted(e, service, action, identity, admitted)
}

// serviceActionCardGuardKey is the service key a managed action's card
// conflicts on: the service itself for a start, stop or restart, the whole
// stack ("") for an update, which rewrites every service of the stack, or a
// backup, which snapshots all of them.
func serviceActionCardGuardKey(service *controlplane.ServiceRuntime, action string) string {
	if action == serviceActionUpdate || action == serviceActionBackup {
		return ""
	}
	return serviceActionControlKey(service)
}

// managedBackupCustodyError refuses a backup of a kit whose backups run on
// the managed backup schedule.
func managedBackupCustodyError(e *httpx.Event) error {
	return httpx.Error(e, http.StatusForbidden, ksapi.ErrCodeForbidden,
		"Backups for this StackKit run through the managed backup schedule",
		map[string]any{
			"reason_code": "managed_backup_custody",
			"retryable":   false,
			"user_guidance": map[string]any{
				"title":      "Backups for this stack are managed",
				"body":       "This StackKit keeps its backups in kombify-managed storage, so they run on the stack's backup schedule under your plan's backup entitlement.",
				"next_steps": []string{"Use the stack's backup schedule to run managed backups."},
			},
		})
}

// serviceActionCardTemplate is the card of one managed action as a caller
// names it: the runtime-instance scope and the service key the managed
// command carries, or for a backup the stack's backup contract. Card creation
// derives every target ref, an adopted application's exact container and the
// update's target release from fresh inventory (deriveExecutorBoundTarget),
// exactly as for a card created through /v1/ril/action-cards.
func (h *serviceRuntimeHandlers) serviceActionCardTemplate(stack *controlplane.Stack, service *controlplane.ServiceRuntime, action, digest string) (actions.ActionTemplate, error) {
	profile := serviceActionPrimitive(action)
	plan := stringFromAnyMap(stack.RuntimeSummary, jobs.RuntimeListenerPlanHashResultField)
	if !resolvedPlanHashPattern.MatchString(plan) {
		return actions.ActionTemplate{}, fmt.Errorf("stack has no applied resolved plan")
	}
	template := actions.ActionTemplate{
		StackID:          stack.ID,
		Primitive:        rilaction.PrimitiveBinding{ID: profile.ID, OperationClass: profile.OperationClass},
		ResolvedPlanHash: plan,
		Target:           rilaction.TargetBinding{Scope: profile.TargetScope},
		Inputs:           []rilaction.Input{primitives.Input(primitives.InputRuntimeInstanceRef, serviceActionControlKey(service))},
		EvidenceSinkRef:  serviceActionEvidenceSink(digest),
	}
	if profile.TargetScope == rilaction.TargetScopeModuleInstance {
		template.Inputs = []rilaction.Input{primitives.Input(primitives.InputBackupContractRef, stack.ID)}
	}
	return template, nil
}

// serviceActionNodeRef is the StackKits node the managed command addresses for
// this worker: the canonical lifecycle binding's node of the stack's kit.
func (h *serviceRuntimeHandlers) serviceActionNodeRef(ctx context.Context, tenantID, ownerID, stackID, workerID string) (string, error) {
	if h.actionCards == nil || h.actionCards.Lifecycle == nil {
		return "", fmt.Errorf("lifecycle authority unavailable")
	}
	request := jobs.StackKitLifecycleRequest{StackID: stackID, TenantID: tenantID, OwnerID: ownerID, AgentID: workerID, Operation: jobs.StackKitLifecycleVerify}
	prepared, err := h.actionCards.Lifecycle.PrepareStackKitLifecycleCommand(ctx, request)
	if err != nil {
		return "", err
	}
	if prepared.AgentID != workerID {
		return "", fmt.Errorf("lifecycle authority addresses another agent")
	}
	return jobs.StackKitNodeRefForAgent(prepared.StackKit, prepared.NodeRef)
}

// NewRILExecutorBoundInventory returns the fresh-inventory derivation and
// admission of an executor-bound card (actions.ExecutorBoundAdmission.Derive
// and .Target): derive fills a new card's target refs and derived inputs
// through either entry; admit validates them when the card is created and
// again when it begins.
func NewRILExecutorBoundInventory(cfg ServiceRuntimeRouteConfig) (
	derive func(context.Context, actions.ExecutorBoundTarget) (rilaction.TargetBinding, []rilaction.Input, error),
	admit func(context.Context, actions.ExecutorBoundTarget) error,
) {
	if cfg.Store == nil || cfg.Stacks == nil || cfg.Servers == nil || cfg.ActionCards == nil || cfg.HostEligibility == nil {
		panic("NewRILExecutorBoundInventory: service, stack and server stores, the action-card path and the host eligibility required")
	}
	h := &serviceRuntimeHandlers{store: cfg.Store, stacks: cfg.Stacks, servers: cfg.Servers, jobs: cfg.Jobs, now: cfg.Now, actionCards: cfg.ActionCards, stackKitRelease: cfg.StackKitReleaseVersion, hostEligibility: cfg.HostEligibility}
	if h.now == nil {
		h.now = func() time.Time { return time.Now().UTC() }
	}
	if h.stackKitRelease == nil {
		h.stackKitRelease = pinnedStackKitRelease()
	}
	return h.deriveExecutorBoundTarget, h.admitExecutorBoundTarget
}

// executorBoundServer is the owner's connected server of the card's stack.
func (h *serviceRuntimeHandlers) executorBoundServer(ctx context.Context, card actions.ExecutorBoundTarget) (*controlplane.ServerRuntime, error) {
	server, err := h.servers.GetServerRuntime(ctx, card.TenantID, card.ServerID)
	if err != nil || server == nil || server.ID != card.ServerID || server.OwnerSubjectID != card.OwnerSubjectID || server.StackID != card.Template.StackID {
		return nil, fmt.Errorf("server is not the owner's server of this stack")
	}
	if server.ConnectionState != string(serverregistry.ConnectionConnected) || strings.TrimSpace(server.WorkerID) == "" {
		return nil, fmt.Errorf("server agent is not connected")
	}
	return server, nil
}

// deriveExecutorBoundTarget derives a new card's target and inputs from fresh
// inventory; a caller selects only the service (runtime-instance-ref) of a
// runtime-instance primitive. stack: no narrower field. module-instance
// (check-backup, run-backup): the server and the stack's backup contract.
// runtime-instance: the server, the StackKits node its agent owns (the node
// the managed command addresses), its current worker as runtime instance and
// execution channel, the service key, for an adopted application its exact
// container (application-ref), and for an update the exact release the
// managed update targets.
func (h *serviceRuntimeHandlers) deriveExecutorBoundTarget(ctx context.Context, card actions.ExecutorBoundTarget) (rilaction.TargetBinding, []rilaction.Input, error) {
	server, err := h.executorBoundServer(ctx, card)
	if err != nil {
		return rilaction.TargetBinding{}, nil, err
	}
	stackID := card.Template.StackID
	if card.Profile.IsHost() {
		return h.deriveHostTarget(ctx, card, server)
	}
	switch card.Profile.TargetScope {
	case rilaction.TargetScopeStack:
		return rilaction.TargetBinding{Scope: rilaction.TargetScopeStack}, []rilaction.Input{}, nil
	case rilaction.TargetScopeModuleInstance:
		return rilaction.TargetBinding{Scope: rilaction.TargetScopeModuleInstance, SiteRef: server.ID, ModuleInstanceRef: primitives.BackupModuleInstanceRef(stackID)},
			[]rilaction.Input{primitives.Input(primitives.InputBackupContractRef, stackID)}, nil
	case rilaction.TargetScopeRuntimeInstance:
	default:
		return rilaction.TargetBinding{}, nil, fmt.Errorf("unsupported target scope")
	}
	service, err := h.serviceOnServer(ctx, card.TenantID, stackID, server, primitives.InputValue(card.Template.Inputs, primitives.InputRuntimeInstanceRef))
	if err != nil {
		return rilaction.TargetBinding{}, nil, err
	}
	nodeRef, err := h.serviceActionNodeRef(ctx, card.TenantID, card.OwnerSubjectID, stackID, server.WorkerID)
	if err != nil {
		return rilaction.TargetBinding{}, nil, err
	}
	inputs := []rilaction.Input{primitives.Input(primitives.InputRuntimeInstanceRef, serviceActionControlKey(service))}
	if card.Profile.ApplicationAction != "" && adoptedApplication(service) {
		inputs = []rilaction.Input{primitives.Input(primitives.InputApplicationRef, jobs.ApplicationContainerIdentity(service)), inputs[0]}
	}
	if card.Profile.ID == "update-service" {
		release := upgradeTargetRelease(h.stackKitRelease())
		if release == "" || release == "latest" {
			return rilaction.TargetBinding{}, nil, fmt.Errorf("the managed update release is unavailable")
		}
		inputs = append(inputs, primitives.Input(primitives.InputTargetReleaseRef, release))
	}
	return rilaction.TargetBinding{
		Scope: rilaction.TargetScopeRuntimeInstance, SiteRef: server.ID, NodeRef: nodeRef,
		RuntimeInstanceRef: server.WorkerID, ExecutionChannelRef: server.WorkerID,
	}, inputs, nil
}

// admitExecutorBoundTarget validates a card's target and inputs against fresh
// inventory. No host-level operation is reachable: a runtime-instance target
// is one service of the stack on this server, addressed through its current
// worker, and an adopted application is named by its exact container; a
// locked service, a locked application group, a locked stack (for an update)
// and an active server maintenance refuse, exactly as on the managed service
// route. A backup run refuses a managed-custody kit, an active maintenance
// and any other running mutation of the stack.
func (h *serviceRuntimeHandlers) admitExecutorBoundTarget(ctx context.Context, card actions.ExecutorBoundTarget) error {
	server, err := h.executorBoundServer(ctx, card)
	if err != nil {
		return err
	}
	if card.Profile.IsHost() {
		return h.admitHostTarget(ctx, card, server)
	}
	target, inputs := card.Template.Target, card.Template.Inputs
	switch card.Profile.TargetScope {
	case rilaction.TargetScopeStack:
		return nil
	case rilaction.TargetScopeModuleInstance:
		if target.SiteRef != server.ID || target.ModuleInstanceRef != primitives.BackupModuleInstanceRef(card.Template.StackID) ||
			primitives.InputValue(inputs, primitives.InputBackupContractRef) != card.Template.StackID {
			return fmt.Errorf("backup contract target differs from this stack on this server")
		}
		// A kit whose backups are in managed custody refuses the backup read
		// and run at dispatch (orchestrator ErrStackKitBackupCustodyManaged).
		// Prepare the exact lifecycle command the dispatcher sends now, so
		// such a card is refused before any Decision, lease or claim is issued.
		if h.actionCards == nil || h.actionCards.Lifecycle == nil {
			return fmt.Errorf("lifecycle authority unavailable")
		}
		operation := jobs.StackKitLifecycleBackupStatus
		if card.Profile.Mutation {
			operation = jobs.StackKitLifecycleBackupRun
		}
		prepared, err := h.actionCards.Lifecycle.PrepareStackKitLifecycleCommand(ctx, jobs.StackKitLifecycleRequest{
			StackID: card.Template.StackID, TenantID: card.TenantID, OwnerID: card.OwnerSubjectID, AgentID: server.WorkerID,
			Operation: operation, OwnerApproved: card.Profile.Mutation,
		})
		if err != nil {
			return fmt.Errorf("the backup %s is not admitted for this stack: %w", card.Profile.ID, err)
		}
		if prepared.AgentID != server.WorkerID {
			return fmt.Errorf("lifecycle authority addresses another agent")
		}
		if !card.Profile.Mutation {
			return nil
		}
		// One stack mutation at a time: a running update or service card of
		// this stack refuses the backup run, as does server maintenance.
		if conflictID, err := h.activeServiceMutationCard(ctx, card.TenantID, card.Template.StackID, "", card.CardID); err != nil || conflictID != "" {
			return fmt.Errorf("a service action is already active")
		}
		return h.refuseExecutorBoundMaintenance(ctx, card.TenantID, server)
	case rilaction.TargetScopeRuntimeInstance:
	default:
		return fmt.Errorf("unsupported target scope")
	}
	if target.SiteRef != server.ID || target.RuntimeInstanceRef != server.WorkerID || target.ExecutionChannelRef != server.WorkerID {
		return fmt.Errorf("runtime target is not this server's current worker")
	}
	nodeRef, err := h.serviceActionNodeRef(ctx, card.TenantID, card.OwnerSubjectID, card.Template.StackID, server.WorkerID)
	if err != nil || nodeRef != target.NodeRef {
		return fmt.Errorf("runtime target node is not the node the agent owns")
	}
	service, err := h.serviceOnServer(ctx, card.TenantID, card.Template.StackID, server, primitives.InputValue(inputs, primitives.InputRuntimeInstanceRef))
	if err != nil {
		return err
	}
	// An adopted application is named by its exact container on exactly
	// the power primitives; a managed service never carries one.
	adopted := adoptedApplication(service)
	container := primitives.InputValue(inputs, primitives.InputApplicationRef)
	if adopted != (container != "") || (adopted && (card.Profile.ApplicationAction == "" || container != jobs.ApplicationContainerIdentity(service))) {
		return fmt.Errorf("adopted application identity differs from the card")
	}
	if service.MutationLock.Locked() || len(serviceGroupLockSources(*service)) != 0 {
		return fmt.Errorf("service is locked")
	}
	if service.ObservedAt == nil || h.now().UTC().Sub(service.ObservedAt.UTC()) > runtimehealth.FreshHeartbeatWindow {
		return fmt.Errorf("service observation is stale")
	}
	// One service action at a time, across both paths: an unresolved managed
	// job of this service, or another executing restart of this service or
	// update of this stack, refuses.
	if conflictID, err := h.activeServiceActionJob(ctx, card.TenantID, card.Template.StackID, service.ID, ""); err != nil || conflictID != "" {
		return fmt.Errorf("a service action is already active")
	}
	guardKey := serviceActionControlKey(service)
	if card.Profile.ID == "update-service" {
		guardKey = ""
	}
	if conflictID, err := h.activeServiceMutationCard(ctx, card.TenantID, card.Template.StackID, guardKey, card.CardID); err != nil || conflictID != "" {
		return fmt.Errorf("a service action is already active")
	}
	switch card.Profile.ID {
	case "start-service", "stop-service", "restart-service":
		if !containsString(serviceRuntimeAllowedActions(service.Capabilities, false), card.Profile.ApplicationAction) {
			return fmt.Errorf("service does not allow %s", card.Profile.ApplicationAction)
		}
	case "update-service":
		release := upgradeTargetRelease(h.stackKitRelease())
		if release == "latest" || primitives.InputValue(inputs, primitives.InputTargetReleaseRef) != release {
			return fmt.Errorf("update target is not the managed update release")
		}
		rows, listErr := h.store.ListServiceRuntimes(ctx, card.TenantID, card.Template.StackID, "")
		if listErr != nil {
			return listErr
		}
		for i := range rows {
			if rows[i].MutationLock.Locked() {
				return fmt.Errorf("stack contains locked services")
			}
		}
	}
	return h.refuseExecutorBoundMaintenance(ctx, card.TenantID, server)
}

// refuseExecutorBoundMaintenance refuses a mutating card while the server is
// being rebooted or updated.
func (h *serviceRuntimeHandlers) refuseExecutorBoundMaintenance(ctx context.Context, tenantID string, server *controlplane.ServerRuntime) error {
	maintenance, ok := h.servers.(controlplane.ServerMaintenanceStore)
	if !ok {
		return fmt.Errorf("server maintenance state is unavailable")
	}
	if _, err := maintenance.ActiveServerMaintenanceJob(ctx, tenantID, controlplane.ServerMaintenanceScope{ServerID: server.ID, AgentID: server.WorkerID}); !errors.Is(err, controlplane.ErrNotFound) {
		if err != nil {
			return err
		}
		return fmt.Errorf("server maintenance is active")
	}
	return nil
}

// serviceOnServer is the one projected, server-bound service of the stack on
// this server whose control key is key.
func (h *serviceRuntimeHandlers) serviceOnServer(ctx context.Context, tenantID, stackID string, server *controlplane.ServerRuntime, key string) (*controlplane.ServiceRuntime, error) {
	if key == "" {
		return nil, fmt.Errorf("service key missing")
	}
	rows, err := h.store.ListServiceRuntimes(ctx, tenantID, stackID, "")
	if err != nil {
		return nil, err
	}
	servers, err := h.servers.ListServerRuntimesByTenant(ctx, tenantID, stackID)
	if err != nil {
		return nil, err
	}
	serverMap := map[string]*controlplane.ServerRuntime{}
	for i := range servers {
		serverMap[servers[i].ID] = &servers[i]
		if servers[i].NodeID != "" {
			serverMap[servers[i].NodeID] = &servers[i]
		}
	}
	var found *controlplane.ServiceRuntime
	for _, projected := range projectDeclaredServiceControls(rows, serverMap) {
		placement := serviceregistry.NormalizePlacement(projected.ServerID, projected.Placement)
		if projected.ServerID != server.ID || placement.TargetKind != serviceregistry.TargetKindServer || serviceActionControlKey(&projected) != key {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("service key is ambiguous on this server")
		}
		copy := projected
		found = &copy
	}
	if found == nil {
		return nil, fmt.Errorf("service is not on this server")
	}
	return found, nil
}
