package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	wire "github.com/kombifyio/stackkits/contracts/advancedcapability"

	"github.com/kombifyio/techstack/internal/advancedissuer"
	"github.com/kombifyio/techstack/internal/stackkitrelease"
	"github.com/kombifyio/techstack/pkg/stackkitcommand"
)

// StackKitLifecycleApplicationUpdate moves one StackKits-managed application to
// a newer upstream release (owner decision D-2026-11-12-06): the node runs
// `stackkit use-cases upgrade <use case> --to <version> --digest <digest>
// --capability <file> [--accept-untested] --apply --owner-approve --json` under
// the exact application-release.update capability, which checkpoints the
// application's data, moves only that application and rolls it back to the
// prior release and data when generate, apply or verify fails.
const StackKitLifecycleApplicationUpdate = "application_update"

// ApplicationUpdateResultField is the job result field that states what the
// node did: updated, rolled_back, rollback_failed or refused, with the CLI's
// reason code and the prior and target release.
const ApplicationUpdateResultField = "application_update_outcome"

// Application update outcomes as the StackKits CLI names them.
const (
	ApplicationUpdateUpdated        = "updated"
	ApplicationUpdateRolledBack     = "rolled_back"
	ApplicationUpdateRollbackFailed = "rollback_failed"
	ApplicationUpdateRefused        = "refused"
)

// ApplicationUpdateUnresolved is Techstack's own outcome when the node's
// answer is missing: the update may have run, rolled back or failed there.
const ApplicationUpdateUnresolved = "unresolved"

func applicationUpdateUnresolvedResult(req StackKitLifecycleRequest) map[string]interface{} {
	return map[string]interface{}{
		"schema_version": "techstack.stackkit-lifecycle-result/v1", "techstack_id": req.StackID, "operation": req.Operation,
		"agent_id": req.AgentID, "success": false, "service_action_receipt": StackKitServiceActionReceipt(req),
		ApplicationUpdateResultField: map[string]interface{}{"result": ApplicationUpdateUnresolved, "reason_code": "node_outcome_unknown"},
	}
}

// applicationUpdateCommandTimeout bounds the node command: the CLI bounds the
// whole update, rollback included, at 30 minutes.
const applicationUpdateCommandTimeout = 35 * time.Minute

func validateApplicationUpdateRequest(req *StackKitLifecycleRequest) error {
	if !req.OwnerApproved || req.DryRun || req.Offline {
		return fmt.Errorf("application_update requires explicit Owner approval and runs for real")
	}
	if req.ServiceID == "" || req.DurableJobID == "" {
		return fmt.Errorf("application_update requires its durable service action identity")
	}
	if !stackkitcommand.ValidApplicationUpdateTarget(req.UseCase, req.ApplicationVersion, req.ApplicationDigest) {
		return fmt.Errorf("application_update requires a use case slug, a release tag and a sha256 digest")
	}
	return nil
}

func applicationUpdateFieldsPresent(req StackKitLifecycleRequest) bool {
	return req.UseCase != "" || req.ApplicationVersion != "" || req.ApplicationDigest != "" || req.AcceptUntested
}

// requireApplicationUpdateRelease refuses the update before dispatch on a
// pinned StackKits release whose catalog does not dispatch
// application-release.update on a managed node.
func requireApplicationUpdateRelease(release stackkitrelease.Release) error {
	catalog, err := release.AdvancedOperations()
	if err == nil {
		err = catalog.Dispatchable(stackkitcommand.ApplicationUpdateOperation)
	}
	if err != nil {
		return fmt.Errorf("updating one application needs a StackKits release that dispatches %s; this installation pins %s: %w", stackkitcommand.ApplicationUpdateOperation, release.Receipt().Version, err)
	}
	return nil
}

// issueApplicationUpdateCapability mints the one application-release.update
// capability of this update: the deployment's StackKit stack and local Owner
// from its trust binding, and exactly the admitted use case, release tag,
// digest and tested or untested kind, valid for at most 15 minutes.
func issueApplicationUpdateCapability(ctx context.Context, issuer AdvancedIssuer, req StackKitLifecycleRequest) (advancedissuer.Capability, error) {
	if issuer == nil {
		return advancedissuer.Capability{}, advancedissuer.ErrUnavailable
	}
	binding, err := issuer.TrustBindingFor(ctx, req.TenantID, req.StackID)
	if err != nil {
		return advancedissuer.Capability{}, fmt.Errorf("managed deployment has no Advanced trust binding; run advanced_trust_import: %w", err)
	}
	stackID := firstNonEmpty(binding.StackID, req.StackKitInstanceID)
	if stackID == "" {
		return advancedissuer.Capability{}, fmt.Errorf("managed deployment has no StackKit stack id for its application update capability")
	}
	return issuer.Issue(ctx, advancedissuer.Request{
		TenantID: req.TenantID, DeploymentID: req.StackID, StackID: stackID, OwnerRef: binding.OwnerRef,
		Operations: []string{advancedissuer.OperationApplicationReleaseUpdate}, TTL: advancedissuer.DefaultTTL,
		ApplicationRelease: &wire.ApplicationRelease{
			ReleaseKind: stackkitcommand.ApplicationUpdateReleaseKind(req.AcceptUntested), TargetDigest: req.ApplicationDigest,
			TargetRelease: req.ApplicationVersion, UseCaseRef: req.UseCase,
		},
	})
}

// applicationUpdateOutcome projects the CLI's
// stackkit.application-update-result/v1 (inside the command result envelope)
// into the compact outcome the job reports. It never infers an outcome from
// stderr: without that document it returns nil.
func applicationUpdateOutcome(commandResult map[string]interface{}) map[string]interface{} {
	document := findApplicationUpdateResult(commandResult, 0)
	if document == nil {
		return nil
	}
	outcome := strings.TrimSpace(stringFromInterface(document["update_outcome"]))
	switch outcome {
	case ApplicationUpdateUpdated, ApplicationUpdateRolledBack, ApplicationUpdateRollbackFailed, ApplicationUpdateRefused:
	default:
		return nil
	}
	projected := map[string]interface{}{"result": outcome}
	for _, key := range []string{"reason_code", "operation_id", "workload_ref", "operation_state"} {
		if value := strings.TrimSpace(stringFromInterface(document[key])); value != "" {
			projected[key] = value
		}
	}
	if tested, ok := document["target_tested"].(bool); ok {
		projected["target_tested"] = tested
	}
	for _, key := range []string{"prior", "target"} {
		release, _ := document[key].(map[string]interface{})
		if version := strings.TrimSpace(stringFromInterface(release["version"])); version != "" {
			projected[key] = map[string]interface{}{"version": version, "digest": strings.TrimSpace(stringFromInterface(release["digest"]))}
		}
	}
	return projected
}

func findApplicationUpdateResult(value interface{}, depth int) map[string]interface{} {
	if depth > 4 {
		return nil
	}
	typed, ok := value.(map[string]interface{})
	if !ok {
		return nil
	}
	if stringFromInterface(typed["apiVersion"]) == stackkitcommand.ApplicationUpdateResultAPIVersion {
		return typed
	}
	for _, key := range []string{"data", "result"} {
		if found := findApplicationUpdateResult(typed[key], depth+1); found != nil {
			return found
		}
	}
	if output := strings.TrimSpace(stringFromInterface(typed["output"])); strings.HasPrefix(output, "{") && len(output) <= 1<<20 {
		var parsed map[string]interface{}
		if json.Unmarshal([]byte(output), &parsed) == nil {
			return findApplicationUpdateResult(parsed, depth+1)
		}
	}
	return nil
}
