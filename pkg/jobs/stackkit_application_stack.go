package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kombifyio/techstack/internal/stackkitrelease"
	"github.com/kombifyio/techstack/pkg/api/agentpb"
	"github.com/kombifyio/techstack/pkg/controlplane"
	"github.com/kombifyio/techstack/pkg/grpcserver"
	"github.com/kombifyio/techstack/pkg/stackkitcommand"
)

// "Stack it" lifecycle operations: an existing container StackKits did not
// install is rebuilt as an own application (profile own-application-adopted/v1).
// They share the application lifecycle binding with the Home Assistant
// in-place profile but dispatch the application.stack.* catalog operations.
const (
	StackKitLifecycleApplicationStackPlan            = "application_stack_plan"
	StackKitLifecycleApplicationStack                = "application_stack"
	StackKitLifecycleApplicationStackVerify          = "application_stack_verify"
	StackKitLifecycleApplicationStackRestoreOriginal = "application_stack_restore_original"
	StackKitLifecycleApplicationStackReleaseOriginal = "application_stack_release_original"
)

// OwnApplicationAdoptionKey holds the projected Stack it record on the
// original container's service row; the Guard never supplies it.
const (
	OwnApplicationAdoptionKey      = "own_application_adoption"
	OwnApplicationAdoptionJobIDKey = "own_application_adoption_job_id"
)

var applicationStackAgentOperations = map[string]agentpb.StackKitOperation{
	StackKitLifecycleApplicationStackPlan:            agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_STACK_PLAN,
	StackKitLifecycleApplicationStack:                agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_STACK,
	StackKitLifecycleApplicationStackVerify:          agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_STACK_VERIFY,
	StackKitLifecycleApplicationStackRestoreOriginal: agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_STACK_RESTORE_ORIGINAL,
	StackKitLifecycleApplicationStackReleaseOriginal: agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_STACK_RELEASE_ORIGINAL,
}

// IsApplicationStackLifecycleOperation reports the Stack it operations.
func IsApplicationStackLifecycleOperation(op string) bool {
	_, ok := applicationStackAgentOperations[op]
	return ok
}

func applicationStackRead(op string) bool {
	return op == StackKitLifecycleApplicationStackPlan || op == StackKitLifecycleApplicationStackVerify
}

func validateApplicationStackRequest(req *StackKitLifecycleRequest) error {
	b := req.Application
	if b == nil || b.ServerID == "" || b.ExpectedServiceRevision <= 0 || req.ServiceID == "" || req.ServiceKey != "" || b.Profile != stackkitcommand.OwnApplicationAdoptedProfile || !stackkitcommand.ValidOwnApplicationName(b.OwnApplicationName) {
		return fmt.Errorf("Stack it requires a retained exact service/source binding and own application name")
	}
	if req.Operation == StackKitLifecycleApplicationStack && (b.ReviewedStack == nil || b.PlanJobID == "" || b.ReviewedStack.PlanDigest == "") {
		return fmt.Errorf("Stack it adopt requires the retained reviewed plan")
	}
	if applicationStackRead(req.Operation) == req.OwnerApproved {
		return fmt.Errorf("Stack it mutations, and only they, carry Owner approval")
	}
	return nil
}

func applicationStackCommand(req StackKitLifecycleRequest, release stackkitrelease.Release) (*agentpb.StackKitCommand, error) {
	c, err := stackKitLifecycleCommand(req.DurableJobID, req, release)
	if err != nil {
		return nil, err
	}
	c.ApplicationContainerId = req.Application.ContainerID
	c.OwnApplicationName = req.Application.OwnApplicationName
	if !applicationStackRead(req.Operation) {
		c.ApplicationOperationId = req.DurableJobID
	}
	if req.Operation == StackKitLifecycleApplicationStack {
		c.ApplicationPlanDigest = req.Application.ReviewedStack.PlanDigest
		// Stop, copy and checksum the data, start and hold the 2-minute
		// stability window: the write budget, not the default read budget.
		c.TimeoutSeconds = int32(stackKitWriteCommandTimeout / time.Second)
	}
	if req.Application.ReleasePin == nil || req.Application.ReleasePin.Version != c.Release.GetVersion() || req.Application.ReleasePin.ArchiveSha256 != c.Release.GetArchiveSha256() {
		return nil, fmt.Errorf("reviewed Stack it release pin changed")
	}
	return c, stackkitcommand.ValidateCommand(c)
}

// errApplicationStackIntentChanged: the receipt is retained, but newer service
// intent requires review before it is projected.
var errApplicationStackIntentChanged = errors.New("Stack it receipt retained but newer service intent requires review before projection")

// ApplicationStackFailureField is the structured failure of a Stack it job:
// the producer outcome (refused, rolled_back, rollback_failed, unhealthy) or
// Techstack's own reason code, and the code of the first refused plan row.
const ApplicationStackFailureField = "application_stack_failure"

// applicationStackFailed records a structured failure; the job error repeats
// it for logs only, clients read the field.
func applicationStackFailed(job *Job, result map[string]interface{}, outcome, reasonCode, rowCode string) error {
	if result == nil {
		result = map[string]interface{}{"success": false}
	}
	failure := map[string]interface{}{"outcome": outcome}
	if reasonCode != "" {
		failure["reason_code"] = reasonCode
	}
	if rowCode != "" {
		failure["row_code"] = rowCode
	}
	result[ApplicationStackFailureField] = failure
	job.replaceResult(result)
	return NewPermanentError(fmt.Errorf("Stack it %s: %s", outcome, firstNonEmpty(reasonCode, rowCode, "no_reason_code")))
}

func runApplicationStackJob(ctx context.Context, cfg StackKitLifecycleConfig, job *Job, q *Queue, req StackKitLifecycleRequest, release stackkitrelease.Release) error {
	head, err := cfg.Services.ReadServiceEventHead(ctx, req.TenantID, req.ServiceID)
	if err != nil {
		return NewPermanentError(err)
	}
	if head.Service.StackID != req.StackID || head.Service.ServerID != req.Application.ServerID || ApplicationContainerIdentity(head.Service) != req.Application.ContainerID {
		return applicationStackFailed(job, nil, "refused", "application_source_changed", "")
	}
	if head.Service.Metadata[OwnApplicationAdoptionJobIDKey] == job.ID {
		return nil
	}
	mutation := !applicationStackRead(req.Operation)
	if mutation && head.Service.MutationLock.Locked() {
		return applicationStackFailed(job, nil, "refused", "service_locked", "")
	}
	command, err := applicationStackCommand(req, release)
	if err != nil {
		return NewPermanentError(err)
	}
	var stacked stackkitcommand.ApplicationStackResult
	if raw := job.Snapshot().Result["application_result_proof"]; raw != nil {
		// A receipt stored before projection failed is reconciled, never
		// dispatched again: the CLI journals the operation ID.
		encoded, _ := json.Marshal(raw)
		var result agentpb.StackKitResult
		if err = json.Unmarshal(encoded, &result); err != nil {
			return NewPermanentError(err)
		}
		if err = stackkitcommand.ValidateResult(&result, command); err != nil {
			return NewPermanentError(err)
		}
		if stacked, err = stackkitcommand.ParseApplicationStackResult(command, &result); err != nil {
			return NewPermanentError(err)
		}
	} else {
		if mutation {
			if cfg.ApplicationAuthorize == nil {
				return NewConfigError(fmt.Errorf("application operate authority is unavailable"))
			}
			if err := cfg.ApplicationAuthorize(contextWithJobAuthorization(ctx, job), req.TenantID, req.OwnerID, req.Application.ServerID); err != nil {
				return NewPermanentError(fmt.Errorf("application operate authority denied: %w", err))
			}
		}
		if (req.Operation == StackKitLifecycleApplicationStackPlan || req.Operation == StackKitLifecycleApplicationStack) && head.Revision != req.Application.ExpectedServiceRevision && !applicationStackFence(head, req.Application) {
			return applicationStackFailed(job, nil, "refused", "application_source_changed", "")
		}
		job.setStep("stackkit_dispatch")
		q.UpdateProgress(job.ID, 10, "Dispatching Stack it "+req.Operation)
		result, sendErr := sendStackKitCommandBoundedForTenant(ctx, cfg.Sender, req.TenantID, req.AgentID, command)
		if sendErr != nil {
			return NewResourceError(sendErr)
		}
		if err = stackkitcommand.ValidateResult(result, command); err != nil {
			return NewPermanentError(err)
		}
		normalized, normalizeErr := normalizeStackKitLifecycleResult(req, result)
		if normalizeErr != nil {
			return NewPermanentError(normalizeErr)
		}
		stacked, err = stackkitcommand.ParseApplicationStackResult(command, result)
		if err == nil {
			normalized["application_stack"] = stackkitcommand.ApplicationStackResultMap(stacked)
		}
		normalized["application_binding"] = req.Application
		if !result.Success {
			if err == nil && stacked.Outcome != "" {
				return applicationStackFailed(job, normalized, stacked.Outcome, "", firstRefusedRow(stacked))
			}
			return applicationStackFailed(job, normalized, "refused", "application_stack_result_invalid", "")
		}
		if err != nil {
			return NewPermanentError(err)
		}
		normalized["application_result_proof"] = result
		job.replaceResult(normalized)
		syncer := q.executionSnapshotSyncer()
		if syncer == nil {
			return NewConfigError(fmt.Errorf("durable application receipt persistence is unavailable"))
		}
		if err = syncer(ctx, job.Snapshot()); err != nil {
			return NewResourceError(err)
		}
	}
	if applicationStackRead(req.Operation) {
		return nil
	}
	if err := projectApplicationStack(ctx, cfg, job.ID, req, stacked); err != nil {
		if errors.Is(err, errApplicationStackIntentChanged) {
			return applicationStackFailed(job, job.Snapshot().Result, "refused", "application_intent_changed", "")
		}
		return err
	}
	q.UpdateProgress(job.ID, 100, "Stack it receipt projected")
	return nil
}

func firstRefusedRow(r stackkitcommand.ApplicationStackResult) string {
	for _, row := range r.Rows {
		if row.Status == "refused" {
			return row.Code
		}
	}
	return ""
}

// applicationStackFence admits Guard observations between review and
// projection (the original stops during adoption) but no newer control-plane
// intent: same management and desired state, same Stack it record.
func applicationStackFence(head *controlplane.ServiceEventResult, binding *ApplicationLifecycleBinding) bool {
	jobID, _ := head.Service.Metadata[OwnApplicationAdoptionJobIDKey].(string)
	return !head.Service.MutationLock.Locked() && head.Revision >= binding.ExpectedServiceRevision &&
		ApplicationIntentRevision(head.Service) == binding.ExpectedIntentRevision &&
		head.Service.ManagementState == binding.ExpectedManagementState && jobID == binding.ExpectedAdoptionJobID
}

// OwnApplicationAdoption is the Stack it record projected onto the original
// container's service row.
type OwnApplicationAdoption struct {
	ProfileRef          string     `json:"profile_ref"`
	State               string     `json:"state"`
	Name                string     `json:"name"`
	WorkloadRef         string     `json:"workload_ref"`
	OriginalContainerID string     `json:"original_container_id"`
	PlanDigest          string     `json:"plan_digest,omitempty"`
	RetainUntil         *time.Time `json:"retain_until,omitempty"`
	JobID               string     `json:"job_id"`
	ProjectedAt         time.Time  `json:"projected_at"`
}

// ReadOwnApplicationAdoption returns the Stack it record of a service row.
func ReadOwnApplicationAdoption(service *controlplane.ServiceRuntime) (OwnApplicationAdoption, bool) {
	var record OwnApplicationAdoption
	if service == nil || service.Metadata[OwnApplicationAdoptionKey] == nil {
		return record, false
	}
	raw, _ := json.Marshal(service.Metadata[OwnApplicationAdoptionKey])
	if json.Unmarshal(raw, &record) != nil || record.ProfileRef != stackkitcommand.OwnApplicationAdoptedProfile || record.Name == "" {
		return record, false
	}
	return record, true
}

func projectApplicationStack(ctx context.Context, cfg StackKitLifecycleConfig, operationID string, req StackKitLifecycleRequest, r stackkitcommand.ApplicationStackResult) error {
	head, err := cfg.Services.ReadServiceEventHead(ctx, req.TenantID, req.ServiceID)
	if err != nil {
		return NewPermanentError(err)
	}
	if head.Service.StackID != req.StackID || head.Service.ServerID != req.Application.ServerID || ApplicationContainerIdentity(head.Service) != req.Application.ContainerID {
		return NewPermanentError(fmt.Errorf("Stack it source changed before receipt projection"))
	}
	if head.Service.Metadata[OwnApplicationAdoptionJobIDKey] == operationID {
		return nil
	}
	if !applicationStackFence(head, req.Application) {
		return NewPermanentError(errApplicationStackIntentChanged)
	}
	record, _ := ReadOwnApplicationAdoption(head.Service)
	record.ProfileRef = stackkitcommand.OwnApplicationAdoptedProfile
	record.Name = req.Application.OwnApplicationName
	record.WorkloadRef = "own-" + record.Name
	record.OriginalContainerID = req.Application.ContainerID
	record.JobID = operationID
	record.ProjectedAt = time.Now().UTC()
	switch req.Operation {
	case StackKitLifecycleApplicationStack:
		record.State, record.PlanDigest, record.RetainUntil = "stacked", r.PlanDigest, r.RetainUntil
	case StackKitLifecycleApplicationStackRestoreOriginal:
		record.State, record.RetainUntil = "restored", nil
	case StackKitLifecycleApplicationStackReleaseOriginal:
		record.State, record.RetainUntil = "released", nil
	default:
		return NewPermanentError(fmt.Errorf("Stack it %s projects nothing", req.Operation))
	}
	metadata := head.Service.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	encoded, _ := json.Marshal(record)
	var stored map[string]any
	_ = json.Unmarshal(encoded, &stored)
	metadata[OwnApplicationAdoptionKey] = stored
	metadata[OwnApplicationAdoptionJobIDKey] = operationID
	// The original row stays an observed container: the rebuilt own
	// application is a new container the Guard reports on its own.
	patch := controlplane.ServiceRuntime{ManagementState: head.Service.ManagementState, Metadata: metadata}
	_, err = cfg.Services.ApplyServiceEvent(ctx, controlplane.ServiceEvent{TenantID: req.TenantID, ServiceID: req.ServiceID, ExpectedRevision: &head.Revision, Authority: controlplane.ServiceEventAuthorityControlPlane, Source: "stackkit-application-stack", ReasonCode: req.Operation, EnforceIdentityBinding: true, Runtime: patch, Evidence: map[string]any{"job_id": operationID, "plan_digest": r.PlanDigest, "outcome": r.Outcome}})
	if err != nil {
		return NewResourceError(err)
	}
	return nil
}

// ApplicationStackReleaseForStack admits the stack's target release only when
// it dispatches every application.stack.* operation at the minimum version.
func ApplicationStackReleaseForStack(ctx context.Context, tenant, stack string, read bool) (*agentpb.StackKitReleasePin, error) {
	var release *stackkitrelease.Release
	var err error
	if read {
		release, err = CachedTargetStackKitReleaseForStack(ctx, tenant, stack)
	} else {
		release, err = TargetStackKitReleaseForStack(ctx, tenant, stack)
	}
	if err != nil {
		return nil, err
	}
	if release == nil {
		return nil, fmt.Errorf("published StackKits release is unavailable")
	}
	if version := release.Receipt().Version; !stackkitcommand.ReleaseAtLeast(version, stackkitcommand.MinApplicationStackRelease) {
		return nil, fmt.Errorf("Stack it needs StackKits %s or newer; this installation pins %s", stackkitcommand.MinApplicationStackRelease, version)
	}
	catalog, err := release.AdvancedOperations()
	if err != nil {
		return nil, err
	}
	for _, op := range stackkitcommand.ApplicationStackOperations {
		if err = catalog.Dispatchable(op); err != nil {
			return nil, err
		}
	}
	return grpcserver.StackKitReleasePinFor(*release), nil
}
