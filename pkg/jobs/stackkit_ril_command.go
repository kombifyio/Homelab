package jobs

import (
	"context"
	"fmt"

	"github.com/kombifyio/techstack/internal/stackkitrelease"
	"github.com/kombifyio/techstack/pkg/api/agentpb"
	"github.com/kombifyio/techstack/pkg/controlplane"
	"github.com/kombifyio/techstack/pkg/stackkitcommand"
)

// The RIL executor-bound primitives (internal/rilactionexecution) send the one
// typed StackKits command the managed lifecycle job sends for the same
// operation, so the action-card path and the managed service path share one
// write path. These are the job path's own builder, sender, normalizer and
// upgrade bookkeeping; the job path itself calls them unchanged.

// StackKitLifecycleCommand builds the typed StackKits command of one prepared
// lifecycle request (stackKitLifecycleCommand).
func StackKitLifecycleCommand(commandID string, req StackKitLifecycleRequest, release stackkitrelease.Release) (*agentpb.StackKitCommand, error) {
	return stackKitLifecycleCommand(commandID, req, release)
}

// SendStackKitCommandBoundedForTenant sends one command to the agent through
// the tenant-scoped transport within the command's own timeout, addressed to
// the release the node runs.
func SendStackKitCommandBoundedForTenant(ctx context.Context, sender StackKitCommandSender, tenantID, agentID string, command *agentpb.StackKitCommand) (*agentpb.StackKitResult, error) {
	return sendStackKitCommandBoundedForTenant(ctx, sender, tenantID, agentID, command)
}

// StackKitUpgradeOutcome is the `result` of the upgrade outcome
// (UpgradeOutcomeResultField) the normalized lifecycle result of an upgrade
// states: "updated", "returned_to_previous_release", "rollback_failed" or
// "failed". It is "" when the node reported no upgrade transaction.
func StackKitUpgradeOutcome(req StackKitLifecycleRequest, result *agentpb.StackKitResult) string {
	normalized, err := normalizeStackKitLifecycleResult(req, result)
	if err != nil {
		return ""
	}
	outcome, _ := normalized[UpgradeOutcomeResultField].(map[string]interface{})
	value, _ := outcome["result"].(string)
	return value
}

// ApprovedStackKitsUpgradeTarget is the only release an approved upgrade moves
// a node to: the published release.
func ApprovedStackKitsUpgradeTarget(requested string, release stackkitrelease.Release) (string, error) {
	return approvedStackKitsUpgradeTarget(requested, release)
}

// RecordStackKitsTarget records an approved upgrade's outcome as the node's
// StackKits target, as the upgrade job does.
func RecordStackKitsTarget(ctx context.Context, recorder controlplane.WorkerStackKitsTargetRecorder, executionID string, req StackKitLifecycleRequest, result *agentpb.StackKitResult) (map[string]interface{}, error) {
	return recordStackKitsTarget(ctx, recorder, executionID, req, result)
}

// StackKitFailureReasonCode is the typed StackKits failure reason of a
// result, or "" when the result carries none.
func StackKitFailureReasonCode(result *agentpb.StackKitResult) string {
	return typedStackKitFailureFromResult(result).ReasonCode
}

// RILLifecycleEffects is the managed lifecycle job's own aggregate capture and
// projection around a typed StackKits command, for the RIL executor-bound
// service primitives: a start or stop captures and projects the declared
// service intent, and an adopted application's power control binds the
// retained application binding and projects its verified receipt, exactly as
// the job path does. Only the orchestrator constructs it, from the lifecycle
// configuration it registers the job handler with.
type RILLifecycleEffects struct{ cfg StackKitLifecycleConfig }

// NewRILLifecycleEffects returns the effects of one lifecycle configuration.
func NewRILLifecycleEffects(cfg StackKitLifecycleConfig) RILLifecycleEffects {
	return RILLifecycleEffects{cfg: cfg}
}

// StackKitServiceIntent is the captured declared control group of one start
// or stop, projected after the node's verified service evidence.
type StackKitServiceIntent struct{ captured []controlplane.ServiceRuntime }

// DeclaredServiceControlID returns the one declared, managed StackKits
// service of the stack on this server whose service key is serviceKey: the
// control the managed start or stop anchors its intent on.
func (e RILLifecycleEffects) DeclaredServiceControlID(ctx context.Context, tenantID, stackID, serverID, serviceKey string) (string, error) {
	if e.cfg.Services == nil {
		return "", fmt.Errorf("service aggregate authority is unavailable")
	}
	rows, err := e.cfg.Services.ListServiceRuntimes(ctx, tenantID, stackID, serverID)
	if err != nil {
		return "", err
	}
	id := ""
	for _, row := range rows {
		if row.TenantID != tenantID || row.StackID != stackID || row.ServerID != serverID || row.ServiceKey != serviceKey || !declaredServiceControlMember(row, serviceKey) {
			continue
		}
		if id != "" {
			return "", fmt.Errorf("declared service control is ambiguous")
		}
		id = row.ID
	}
	if id == "" {
		return "", fmt.Errorf("declared service control is unavailable")
	}
	return id, nil
}

// CaptureServiceIntent captures the declared control group of a prepared
// start or stop (captureDeclaredServiceIntent); any other operation captures
// nothing.
func (e RILLifecycleEffects) CaptureServiceIntent(ctx context.Context, req StackKitLifecycleRequest) (StackKitServiceIntent, error) {
	captured, err := captureDeclaredServiceIntent(ctx, e.cfg, req)
	return StackKitServiceIntent{captured: captured}, err
}

// ProjectServiceIntent admits the node's service evidence of a successful
// start or stop and projects its desired state onto the captured group
// (projectDeclaredServiceIntent); executionID marks the projection.
func (e RILLifecycleEffects) ProjectServiceIntent(ctx context.Context, executionID string, req StackKitLifecycleRequest, intent StackKitServiceIntent, command *agentpb.StackKitCommand, result *agentpb.StackKitResult) error {
	evidence, err := stackkitcommand.ParseServiceActionEvidence(command, result)
	if err != nil {
		return err
	}
	return projectDeclaredServiceIntent(ctx, e.cfg, executionID, req, intent.captured, evidence)
}

// ApplicationControlBinding binds an adopted application's power action to
// its current aggregate head: the one service of the stack on this server
// whose exact container is containerID, and the retained application binding
// the managed path builds (RetainedApplicationBinding), with the action and
// the release pin of the exact release the command addresses, which must
// admit application control.
func (e RILLifecycleEffects) ApplicationControlBinding(ctx context.Context, tenantID, stackID, serverID, containerID, action string, release stackkitrelease.Release) (string, *ApplicationLifecycleBinding, error) {
	if e.cfg.Services == nil {
		return "", nil, fmt.Errorf("application aggregate authority is unavailable")
	}
	rows, err := e.cfg.Services.ListServiceRuntimes(ctx, tenantID, stackID, serverID)
	if err != nil {
		return "", nil, err
	}
	serviceID := ""
	for i := range rows {
		if rows[i].TenantID != tenantID || rows[i].StackID != stackID || rows[i].ServerID != serverID || ApplicationContainerIdentity(&rows[i]) != containerID {
			continue
		}
		if serviceID != "" {
			return "", nil, fmt.Errorf("adopted application container is ambiguous")
		}
		serviceID = rows[i].ID
	}
	if serviceID == "" {
		return "", nil, fmt.Errorf("adopted application is not on this server")
	}
	head, err := e.cfg.Services.ReadServiceEventHead(ctx, tenantID, serviceID)
	if err != nil {
		return "", nil, err
	}
	if head.Service == nil || head.Service.StackID != stackID || head.Service.ServerID != serverID || ApplicationContainerIdentity(head.Service) != containerID {
		return "", nil, fmt.Errorf("application source identity changed")
	}
	if head.Service.MutationLock.Locked() {
		return "", nil, fmt.Errorf("application is locked")
	}
	pin, err := applicationAdoptionPin(func() (*stackkitrelease.Release, error) { return &release, nil })
	if err != nil {
		return "", nil, err
	}
	binding := RetainedApplicationBinding(head)
	binding.Action = action
	binding.ReleasePin = pin
	return serviceID, binding, nil
}

// RetainedApplicationBinding is the exact application binding of a service
// aggregate head: its container, server, revision and intent.
func RetainedApplicationBinding(head *controlplane.ServiceEventResult) *ApplicationLifecycleBinding {
	jobID, _ := head.Service.Metadata["application_adoption_job_id"].(string)
	return &ApplicationLifecycleBinding{ContainerID: ApplicationContainerIdentity(head.Service), ServerID: head.Service.ServerID,
		ExpectedServiceRevision: head.Revision, ExpectedIntentRevision: ApplicationIntentRevision(head.Service),
		ExpectedManagementState: head.Service.ManagementState, ExpectedDesiredState: head.Service.DesiredState, ExpectedAdoptionJobID: jobID}
}

// ApplicationLifecycleCommand builds the typed command of one prepared
// application lifecycle request (applicationCommand): its command and
// operation id are req.DurableJobID.
func ApplicationLifecycleCommand(req StackKitLifecycleRequest, release stackkitrelease.Release) (*agentpb.StackKitCommand, error) {
	return applicationCommand(req, release)
}

// ProjectApplicationControl admits the node's verified application receipt
// and projects it onto the service aggregate under the retained binding's
// fence (projectApplicationReceipt); executionID marks the projection.
func (e RILLifecycleEffects) ProjectApplicationControl(ctx context.Context, executionID string, req StackKitLifecycleRequest, command *agentpb.StackKitCommand, result *agentpb.StackKitResult) error {
	if e.cfg.Services == nil {
		return fmt.Errorf("application aggregate authority is unavailable")
	}
	evidence, err := stackkitcommand.ParseApplicationEvidence(command, result)
	if err != nil {
		return err
	}
	return projectApplicationReceipt(ctx, e.cfg, executionID, req, evidence)
}

// StackKitBackupRunEvidence admits the node's evidence of a successful
// out-of-band backup run (stackKitBackupEvidence).
func StackKitBackupRunEvidence(command *agentpb.StackKitCommand, result *agentpb.StackKitResult) error {
	_, err := stackKitBackupEvidence(command, result)
	return err
}
