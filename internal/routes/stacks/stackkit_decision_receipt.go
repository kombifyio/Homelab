package stacks

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/kombifyio/homelab/internal/gocommon/mcporiginauth"
	ksapi "github.com/kombifyio/homelab/pkg/api"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/jobs"
	"github.com/kombifyio/homelab/pkg/middleware"
)

// stackKitDecisionReceiptFeature is the feature flag of the decision receipt
// path (pkg/features). Off, the lifecycle route behaves exactly as before.
const stackKitDecisionReceiptFeature = "stackkit_decision_receipt"

// stackKitDecisionLedgerOperation maps the route's operation to the ledger's
// first-slice operation. Only these two operations of this route are in the
// first slice; every other operation keeps its approval unchanged.
//
//   - backup_restore_activate: the activation of a staged restore over the
//     live volumes (ledger backup_restore). backup_restore only stages into
//     an isolated area and backup_restore_recover resumes an admitted
//     activation through its own journal; both keep today's behaviour.
//   - advanced_change_set, and apply with a candidate_spec (which runs as
//     one): the change-set apply step (ledger advanced_change_set_apply).
//
// application_remove and installation_remove have no operation on this
// route: remove needs a workload_ref the route body does not take, and the
// installation removal runs through the stack teardown, not this route.
func stackKitDecisionLedgerOperation(operation string) (string, bool) {
	switch operation {
	case jobs.StackKitLifecycleBackupRestoreActivate:
		return "backup_restore", true
	case jobs.StackKitLifecycleAdvancedChangeSet:
		return "advanced_change_set_apply", true
	default:
		return "", false
	}
}

// stackKitDecisionReceiptEnabled reads the flag for this Owner; any failure
// to read it keeps the route on today's behaviour.
func (h crudRouteHandlers) stackKitDecisionReceiptEnabled(ctx context.Context, ownerID string) bool {
	if h.runtimeFeatures == nil {
		return false
	}
	enabled, err := h.runtimeFeatures.IsEnabled(ctx, stackKitDecisionReceiptFeature, ownerID)
	return err == nil && enabled
}

// stackKitDecisionPrincipal is the acting principal the decision is filed
// for (StackKits docs/DECISION-RECEIPTS.md, "Acting principal"): an agent
// the Gateway attested (agent token or on-behalf-of exchange, or the MCP
// origin proof) is agent_instance:<its instance id>; a direct human token is
// the Owner, person:<the Owner's platform subject>. The answer always comes
// from a person in the ledger, never from this route.
func stackKitDecisionPrincipal(ctx context.Context, ownerID string) (string, error) {
	if facts, verified := middleware.VerifiedStepUpFromContext(ctx); verified && facts.PrincipalType != "" && facts.PrincipalType != middleware.PrincipalTypeUser {
		agent := strings.TrimSpace(facts.AgentID)
		if agent == "" {
			return "", errors.New("the acting agent carries no attested agent instance")
		}
		return "agent_instance:" + agent, nil
	}
	if origin, marked := mcporiginauth.FromContext(ctx); marked && strings.TrimSpace(origin.AgentInstanceID) != "" {
		return "agent_instance:" + strings.TrimSpace(origin.AgentInstanceID), nil
	}
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return "", errors.New("the acting Owner is unknown")
	}
	return "person:" + ownerID, nil
}

// admitStackKitDecisionReceipt puts a first-slice request on the receipt
// path. The body's owner_approved no longer admits it: the job files the
// decision, waits for the person's answer and refuses to dispatch without a
// checked allowed receipt. An older node is refused here (409) instead of
// being sent a flag its CLI does not know.
func (h crudRouteHandlers) admitStackKitDecisionReceipt(e *httpx.Event, input *jobs.StackKitLifecycleRequest) error {
	if _, first := stackKitDecisionLedgerOperation(strings.ToLower(strings.TrimSpace(input.Operation))); !first {
		return nil
	}
	if !h.stackKitDecisionReceiptEnabled(e.Request.Context(), input.OwnerID) {
		return nil
	}
	// A dry run applies nothing, so it is outside the receipt gate: it would
	// ask the person for a confirmation that can never be used, and the node
	// refuses a receipt on a dry run. Restore activation already refuses a dry
	// run in its own validation; a dry-run change set is refused here.
	if input.DryRun {
		return httpx.BadRequest(e, "dry_run is not available for an operation that needs the Owner's signed confirmation")
	}
	principal, err := stackKitDecisionPrincipal(e.Request.Context(), input.OwnerID)
	if err != nil {
		return httpx.Error(e, http.StatusForbidden, ksapi.ErrCodeForbidden, "The acting principal of this confirmation is unknown", map[string]interface{}{
			"reason_code": "stackkit_decision_principal_unknown", "retryable": false,
		})
	}
	if strings.TrimSpace(input.AgentID) != "" {
		version, releaseErr := jobs.DecisionReceiptNodeRelease(e.Request.Context(), input.TenantID, input.StackID, input.AgentID, nil)
		if releaseErr != nil {
			return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "This server's StackKits release is unavailable", map[string]interface{}{
				"reason_code": "stackkit_decision_node_release_unavailable", "retryable": true,
			})
		}
		if !jobs.DecisionReceiptSupported(version) {
			return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "This server's StackKits release does not take a signed confirmation yet", map[string]interface{}{
				"reason_code": "stackkit_decision_node_unsupported", "retryable": false,
				"stackkits_release": version, "minimum_release": jobs.DecisionReceiptMinimumStackKitsRelease,
				"user_guidance": map[string]interface{}{
					"title":      "Update StackKits on this server first",
					"body":       "This operation needs your signed confirmation, which this server's StackKits release cannot check yet. Nothing was changed.",
					"next_steps": []string{"Approve the pending StackKits update for this server, then start the operation again."},
				},
			})
		}
	}
	input.OwnerApproved = true
	input.DecisionPrincipal = principal
	return nil
}
