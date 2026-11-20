package stacks

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	ksapi "github.com/kombifyio/homelab/pkg/api"
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/jobs"
	"github.com/kombifyio/homelab/pkg/orchestrator"
	"github.com/kombifyio/homelab/pkg/specv2"
)

// WizardExistingServers adapts the existing canonical server assignment
// authority without creating another inventory or deployment executor.
// A request with a SpecNodeID joins the server to that node of an existing
// deployment; the server must be bare or already bound to exactly that node.
type WizardExistingServers interface {
	Resolve(context.Context, WizardExistingServerRequest) error
	Assign(context.Context, WizardExistingServerRequest) error
}

type WizardExistingServerRequest struct {
	TenantID string
	OwnerID  string
	StackID  string
	ServerID string
	// SpecNodeID is the StackSpec node a joining server becomes; empty for
	// a first installation.
	SpecNodeID string
}

// WizardMultiNodeJoin is the pinned-release gate for an enrolled server
// joining a deployment: the release must declare multi-node join for the kit.
type WizardMultiNodeJoin interface {
	RequireMultiNodeJoin(kit string) error
}

// wizardJoinBaseSpecKey holds the pre-join StackSpec, and wizardJoinRequestsKey
// the change sets prepared from it, in unfinished ledger entries of an
// existing-server join.
const (
	wizardJoinBaseSpecKey = "join_base_spec"
	wizardJoinRequestsKey = "join_requests"
)

func (h wizardRunHandlers) runWizardExistingServer(e *httpx.Event, run *wizardRunState, projection *specv2.Projection, normalized normalizedCreateStackRequest) error {
	if run.key == "" {
		return httpx.BadRequest(e, "Installing on an existing server requires an Idempotency-Key", nil)
	}
	if run.request.Managed != nil || run.request.Remote != nil || run.request.Substrate != nil {
		return httpx.BadRequest(e, "An existing enrolled server cannot request provisioning or reconnection", nil)
	}
	// Owner-bootstrap options are a generic map on the older create contract.
	// Refuse provider fields there too, so this route cannot admit a provider
	// request hidden outside the explicit managed section.
	policy := runtimePolicyConfigFromRequest(normalized)
	if hasManagedRuntimeFields(policy, runtimeFieldsFromConfig(policy)) {
		return httpx.BadRequest(e, "An existing enrolled server cannot request a managed runtime", nil)
	}
	if h.cfg.ExistingServers == nil || h.crud.orch == nil {
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Existing-server installation authority is unavailable", nil)
	}
	target := WizardExistingServerRequest{
		TenantID: run.tenantID, OwnerID: run.ownerID,
		StackID:  deterministicStackID(run.tenantID, run.ownerID, run.key),
		ServerID: strings.TrimSpace(run.request.Intent.Server.ExistingServerID),
	}
	if err := h.cfg.ExistingServers.Resolve(e.Request.Context(), target); err != nil {
		return writeWizardExistingServerError(e, err)
	}
	if err := h.crud.requireWalletCreateRight(e, run.ownerID); err != nil {
		return err
	}
	if err := h.crud.requireWalletForCreate(e, run.ownerID, normalized); err != nil {
		return err
	}
	if handled := h.ensureWizardHomelab(e, run, run.request.Intent.Name); handled {
		return nil
	}
	normalized.HomelabID = run.homelab.ID
	normalized.StackSpecV2 = projection.Spec
	normalized.UserConfig["stackkit"] = run.request.Intent.KitAssignment.KitSlug
	stack, err := h.crud.persistStackWithRequestHash(e, run.ownerID, run.tenantID, normalized, run.requestHash)
	if err != nil || stack == nil {
		return err
	}
	if err := h.persistWizardHomelabIntent(e.Request.Context(), run, stack.Id, projection); err != nil {
		return h.failWizardIntentProjection(e, run, stack.Id, projection, err)
	}
	if err := h.crud.applyCreateOwnerBootstrap(e, run.ownerID, run.tenantID, stack, normalized); err != nil {
		return err
	}
	access, err := h.crud.issueCreateOwnerSpecAccess(e, stack, run.ownerID, run.tenantID, normalized)
	if err != nil {
		return err
	}
	// Re-read immediately before CAS: another installation may have claimed
	// this host while the pinned CLI validated the requested configuration.
	if err := h.cfg.ExistingServers.Assign(e.Request.Context(), target); err != nil {
		h.recordWizardRunFailure(e.Request.Context(), run, controlplane.WizardRun{StackID: stack.Id}, "existing_server_assignment_failed")
		return writeWizardExistingServerError(e, err)
	}
	jobSpec := createStackJobSpec(normalized)
	jobSpec[stackConfigKeySpecV2] = projection.Spec
	jobID, err := h.crud.orch.ProvisionStackWithOptions(stack.Id, jobSpec, orchestrator.ProvisionStackOptions{
		RequestContext: e.Request.Context(), TenantID: run.tenantID, OwnerID: run.ownerID,
		StackName: stack.Name, IdempotencyKey: "wizard-existing-server:" + run.key,
		ExistingServerID: target.ServerID, AutoDeploy: true, OwnerSpecBootstrap: ownerSpecRuntimeBootstrap(access),
	})
	if err != nil {
		h.recordWizardRunFailure(e.Request.Context(), run, controlplane.WizardRun{StackID: stack.Id}, "existing_server_deploy_failed")
		return writeWizardExistingServerError(e, err)
	}
	return h.finishWizardFound(e, run, wizardFoundOutcome{
		stack: stack, name: stack.Name, serverID: target.ServerID, jobID: jobID,
		autoDeploy: true, projection: projection, ownerSpecAccess: access,
	})
}

// admitWizardJoinExistingServer admits an enrolled server joining an existing
// deployment before anything is read or written for the join: the pinned
// release must declare multi-node join for the deployment's kit, and the
// deployment must run its first validated setup, so the new node is rolled
// out through the Advanced change-set lifecycle like any other extension.
func (h wizardRunHandlers) admitWizardJoinExistingServer(e *httpx.Event, run *wizardRunState, stack *controlplane.Stack) (bool, error) {
	if run.key == "" {
		return true, httpx.BadRequest(e, "Joining an existing server requires an Idempotency-Key", nil)
	}
	if run.request.Managed != nil || run.request.Remote != nil || run.request.Substrate != nil {
		return true, httpx.BadRequest(e, "An existing enrolled server cannot request provisioning or reconnection", nil)
	}
	if h.cfg.ExistingServers == nil || h.cfg.MultiNodeJoin == nil || h.crud.orch == nil {
		return true, httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Existing-server join authority is unavailable", nil)
	}
	stored, _ := stack.Config[stackConfigKeySpecV2].(map[string]any)
	// A resumed join already saved its node; a change set that failed since
	// leaves the deployment in error, and the retry still has to finish it.
	resuming := run.priorFailed != nil && run.priorFailed.Result[wizardJoinBaseSpecKey] != nil
	if (!resuming && strings.TrimSpace(stack.Status) != "running") || stored == nil || specv2.RequireCanonicalV2(stored) != nil {
		return true, httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict,
			"The deployment must finish its first setup before a server joins it", wizardRunDetails(
				"wizard_join_existing_server_requires_running", false, "Finish the first setup",
				"A connected server can join this deployment once its first validated setup runs; nothing was changed."))
	}
	if err := h.cfg.MultiNodeJoin.RequireMultiNodeJoin(wizardDeploymentKit(stack, stored)); err != nil {
		return true, httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict,
			"The pinned StackKits release does not support adding a server to this deployment", wizardRunDetails(
				"wizard_join_multi_node_unsupported", false, "Not supported yet",
				"This StackKit release cannot take another server into this deployment yet. Set the server up as a new deployment instead; nothing was changed."))
	}
	if locked, err := h.crud.lockedStackServices(e, run.tenantID, stack.ID, jobs.StackKitLifecycleAdvancedChangeSet); err != nil {
		return true, err
	} else if len(locked) > 0 {
		return true, httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "Stack contains locked services", map[string]any{"reason": "service_locked", "locked_service_ids": locked})
	}
	if err := h.crud.refuseDuringServerMaintenance(e, run.tenantID, stack.ID, ""); err != nil {
		return true, err
	}
	return false, nil
}

// resolveWizardJoinExistingServer checks the server against the node the join
// projected, before the node is persisted: owned by this owner in this tenant,
// unleased, approved, connected through Guard and bare (or already this
// node, on a same-key retry). It also fixes the pre-join StackSpec the change
// set is prepared from.
func (h wizardRunHandlers) resolveWizardJoinExistingServer(e *httpx.Event, run *wizardRunState, stack *controlplane.Stack, base map[string]any, projection *specv2.Projection, persisted bool) bool {
	if strings.TrimSpace(projection.NodeID) == "" {
		_ = httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict,
			"The join projected no new node", wizardRunDetails(
				"wizard_join_existing_server_no_node", false, "Not supported yet",
				"This StackKit release did not add a node for the server; nothing was changed."))
		return true
	}
	if !persisted {
		run.joinBaseSpec = base
	} else if prior, _ := run.priorFailed.Result[wizardJoinBaseSpecKey].(map[string]any); prior != nil {
		run.joinBaseSpec = prior
		if raw, ok := run.priorFailed.Result[wizardJoinRequestsKey]; ok {
			encoded, _ := json.Marshal(raw)
			if err := json.Unmarshal(encoded, &run.joinRequests); err != nil {
				_ = wizardExtendUnavailable(e, "The accepted change of this join is unreadable")
				return true
			}
		}
	} else {
		_ = httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict,
			"The joined server's earlier setup can no longer be resumed", wizardRunDetails(
				"wizard_join_existing_server_resume_unavailable", false, "Start again",
				"The node of this attempt is saved, but its starting point is not. Remove that node and add the server again."))
		return true
	}
	if err := h.cfg.ExistingServers.Resolve(e.Request.Context(), h.wizardJoinServerTarget(run, stack, projection)); err != nil {
		_ = writeWizardExistingServerError(e, err)
		return true
	}
	return false
}

// finishWizardJoinExistingServer binds the server to the persisted node and
// rolls the change out through the Advanced change-set lifecycle: one
// prepared change set per node the join changes, the joined node included.
// No pairing is minted: the server is already enrolled and connected.
func (h wizardRunHandlers) finishWizardJoinExistingServer(e *httpx.Event, run *wizardRunState, stack *controlplane.Stack, projection *specv2.Projection, persisted bool) error {
	ctx := e.Request.Context()
	outcome := controlplane.WizardRun{StackID: stack.ID, NodeID: projection.NodeID}
	target := h.wizardJoinServerTarget(run, stack, projection)
	if err := h.cfg.ExistingServers.Assign(ctx, target); err != nil {
		h.recordWizardRunFailure(ctx, run, outcome, "existing_server_assignment_failed")
		return writeWizardExistingServerError(e, err)
	}
	if run.joinRequests == nil {
		requests, err := h.crud.orch.PrepareWizardExtend(ctx, run.tenantID, run.ownerID, stack.ID, run.runID, run.requestHash, run.joinBaseSpec, projection.Spec)
		if err != nil {
			h.recordWizardRunFailure(ctx, run, outcome, "join_change_set_unavailable")
			return wizardExtendUnavailable(e, err.Error())
		}
		// The prepared change sets are recorded before the first dispatch, so
		// a retry replays exactly these jobs.
		run.joinRequests = append([]jobs.StackKitLifecycleRequest{}, requests...)
		if err := h.writeWizardRunLedger(ctx, run, outcome, "failed", "join_change_set_pending", map[string]any{}); err != nil {
			return wizardExtendUnavailable(e, "The accepted change could not be recorded; retry with the same Idempotency-Key")
		}
	}
	jobIDs := make([]string, 0, len(run.joinRequests))
	for _, request := range run.joinRequests {
		jobID, err := h.crud.orch.EnqueueStackKitLifecycle(ctx, request)
		if err != nil {
			h.recordWizardRunFailure(ctx, run, outcome, "join_change_set_dispatch_failed")
			return wizardExtendUnavailable(e, "The joined server's change could not be dispatched; retry with the same Idempotency-Key")
		}
		jobIDs = append(jobIDs, jobID)
		h.linkLayoutExecution(e, run, jobID)
	}
	state, primaryJobID := wizardRunStateAccepted, ""
	if len(jobIDs) > 0 {
		state, primaryJobID = wizardRunStateProvisioning, jobIDs[0]
	}
	data := map[string]any{
		"run_kind":                 run.effectiveKind,
		"requested_run_kind":       run.request.Intent.RunKind,
		"coerced":                  run.coerced(),
		"homelab_id":               run.homelab.ID,
		"kit_assignment_mode":      wizardRunKindJoin,
		"kit_slug":                 wizardRunKitSlug(run.request, projection),
		creationStackIDField:       stack.ID,
		"server_id":                target.ServerID,
		"node_id":                  projection.NodeID,
		creationNameField:          stack.Name,
		wizardRunStateField:        state,
		"job_ids":                  jobIDs,
		wizardRunExistingServerIDs: append([]string(nil), run.existingServerIDs...),
		creationOperationsURLField: homelabDashboardURL(),
	}
	if primaryJobID != "" {
		data[creationJobIDField] = primaryJobID
	}
	if persisted {
		data["idempotent_replay"] = true
	}
	if run.layoutRecommendationID != "" {
		data[wizardRunLayoutRecommendationKey] = run.layoutRecommendationID
	}
	addWizardProjectionFields(data, projection, h.cfg.ReleaseVersion)
	outcome.JobID = primaryJobID
	if err := h.recordWizardRun(ctx, run, outcome, data); err != nil {
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Wizard outcome could not be recorded; retry with the same Idempotency-Key", nil)
	}
	response := map[string]any{"run_id": run.runID}
	for key, value := range data {
		response[key] = value
	}
	return httpx.Success(e, http.StatusAccepted, response)
}

func (h wizardRunHandlers) wizardJoinServerTarget(run *wizardRunState, stack *controlplane.Stack, projection *specv2.Projection) WizardExistingServerRequest {
	return WizardExistingServerRequest{
		TenantID: run.tenantID, OwnerID: run.ownerID, StackID: stack.ID,
		ServerID:   strings.TrimSpace(run.request.Intent.Server.ExistingServerID),
		SpecNodeID: projection.NodeID,
	}
}

// wizardDeploymentKit is the kit of a deployment: its StackSpec's kit, else
// the kit its legacy configuration names.
func wizardDeploymentKit(stack *controlplane.Stack, spec map[string]any) string {
	if kit, ok := spec["kit"].(map[string]any); ok {
		if slug, _ := kit["slug"].(string); strings.TrimSpace(slug) != "" {
			return strings.TrimSpace(slug)
		}
	}
	return wizardLegacyMigrationTarget("", stack.Config, spec)
}

func writeWizardExistingServerError(e *httpx.Event, err error) error {
	if errors.Is(err, controlplane.ErrNotFound) {
		return httpx.NotFound(e, "Existing server not found")
	}
	if errors.Is(err, controlplane.ErrConflict) || errors.Is(err, orchestrator.ErrNoAssignedWorkers) {
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict,
			"The selected server must be owned, unassigned, approved and currently connected through Guard", wizardRunDetails(
				"wizard_existing_server_not_admitted", true, "Server not ready", "Refresh the server's Guard connection and select an available server."))
	}
	return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable,
		"Existing-server installation could not be admitted; retry with the same Idempotency-Key", wizardRunDetails(
			"wizard_existing_server_unavailable", true, "Installation unavailable", "The server was retained. Retry this installation with the same attempt key."))
}
