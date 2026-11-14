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
	"github.com/kombifyio/techstack/pkg/api/agentpb"
	"github.com/kombifyio/techstack/pkg/controlplane"
	"github.com/kombifyio/techstack/pkg/stackkitcommand"
)

// StackKitLifecycleOwnApplication runs one own-application lifecycle step
// (StackKits own-application.*) for an application StackKits runs as an own
// application, for example one rebuilt by "Stack it". A read (status, logs or
// the dry run of a mutation) changes nothing; an execution carries the review
// digest of its dry run and the own-application.lifecycle capability Techstack
// issues for exactly that application, action, operation and review.
const StackKitLifecycleOwnApplication = "own_application"

// Result fields of an own-application job.
const (
	// OwnApplicationResultField holds the projected node answer.
	OwnApplicationResultField = "own_application"
	// OwnApplicationStatusKey holds the last projected status on the row.
	OwnApplicationStatusKey = "own_application_status"
)

// OwnApplicationRequest is one own-application step. Step is the catalog
// operation after "own-application."; ReviewJobID names the read job whose
// dry run an execution carries out.
type OwnApplicationRequest struct {
	Name          string `json:"name"`
	Step          string `json:"step"`
	ServerID      string `json:"server_id"`
	Action        string `json:"action,omitempty"`
	ImageTag      string `json:"image_tag,omitempty"`
	AllowMajor    bool   `json:"allow_major,omitempty"`
	ResolvedImage string `json:"resolved_image,omitempty"`
	Checkpoint    string `json:"checkpoint,omitempty"`
	ReviewDigest  string `json:"review_digest,omitempty"`
	ReviewJobID   string `json:"review_job_id,omitempty"`
}

func validateOwnApplicationRequest(req *StackKitLifecycleRequest) error {
	o := req.OwnApplication
	if o == nil || !stackkitcommand.ValidOwnApplicationName(o.Name) || o.ServerID == "" || req.ServiceID == "" || req.ServiceKey != "" || req.DurableJobID == "" || req.Application != nil {
		return fmt.Errorf("own application lifecycle requires its application name, server and durable service action identity")
	}
	if !stackkitcommand.ValidOwnApplicationOperationID(req.DurableJobID) {
		return fmt.Errorf("own application lifecycle durable identity is not an operation ID")
	}
	backup := o.Step == stackkitcommand.OwnApplicationBackup
	read := stackkitcommand.OwnApplicationStepRead(o.Step)
	if !backup && !read && !stackkitcommand.OwnApplicationStepExecutes(o.Step) {
		return fmt.Errorf("own application step %q is not dispatched", o.Step)
	}
	if read == req.OwnerApproved || req.DryRun || req.Offline {
		return fmt.Errorf("own application executions, and only they, carry Owner approval")
	}
	if stackkitcommand.OwnApplicationStepExecutes(o.Step) && (o.ReviewJobID == "" || o.ReviewDigest == "") {
		return fmt.Errorf("own application execution requires its reviewed dry run")
	}
	return nil
}

func ownApplicationCommand(req StackKitLifecycleRequest, release stackkitrelease.Release, capability []byte) (*agentpb.StackKitCommand, error) {
	c, err := stackKitLifecycleCommand(req.DurableJobID, req, release)
	if err != nil {
		return nil, err
	}
	o := req.OwnApplication
	c.OwnApplicationName = o.Name
	c.DryRun, c.ServiceKey = false, ""
	if o.Step == stackkitcommand.OwnApplicationBackup {
		c.Operation = agentpb.StackKitOperation_STACKKIT_OPERATION_OWN_APPLICATION_BACKUP
		c.ApplicationOperationId = req.DurableJobID
		c.TimeoutSeconds = int32(stackKitWriteCommandTimeout / time.Second)
		return c, stackkitcommand.ValidateCommand(c)
	}
	c.OwnApplicationOperation = "own-application." + o.Step
	c.OwnApplicationImageTag, c.OwnApplicationAllowMajor, c.OwnApplicationCheckpoint = o.ImageTag, o.AllowMajor, o.Checkpoint
	switch o.Step {
	case stackkitcommand.OwnApplicationReview:
		c.OwnApplicationAction = o.Action
	case stackkitcommand.OwnApplicationUpdate:
		c.OwnApplicationResolvedImage = o.ResolvedImage
	}
	if stackkitcommand.OwnApplicationStepExecutes(o.Step) {
		c.ApplicationOperationId = req.DurableJobID
		c.OwnApplicationReviewDigest = o.ReviewDigest
		c.AdvancedCapability = append([]byte(nil), capability...)
		// A tag update stops, checkpoints, recreates, holds a stability
		// window and may roll image and data back: the write budget.
		c.TimeoutSeconds = int32(stackKitWriteCommandTimeout / time.Second)
	} else {
		c.TimeoutSeconds = int32(stackKitReadCommandTimeout / time.Second)
	}
	return c, stackkitcommand.ValidateCommand(c)
}

// issueOwnApplicationCapability mints the one own-application.lifecycle
// capability of an execution: this application, action, operation ID (the
// durable job) and review digest, for at most 15 minutes.
func issueOwnApplicationCapability(ctx context.Context, issuer AdvancedIssuer, req StackKitLifecycleRequest) (advancedissuer.Capability, error) {
	if issuer == nil {
		return advancedissuer.Capability{}, advancedissuer.ErrUnavailable
	}
	binding, err := issuer.TrustBindingFor(ctx, req.TenantID, req.StackID)
	if err != nil {
		return advancedissuer.Capability{}, fmt.Errorf("managed deployment has no Advanced trust binding; run advanced_trust_import: %w", err)
	}
	stackID := firstNonEmpty(binding.StackID, req.StackKitInstanceID)
	if stackID == "" {
		return advancedissuer.Capability{}, fmt.Errorf("managed deployment has no StackKit stack id for its own application capability")
	}
	o := req.OwnApplication
	return issuer.Issue(ctx, advancedissuer.Request{
		TenantID: req.TenantID, DeploymentID: req.StackID, StackID: stackID, OwnerRef: binding.OwnerRef,
		Operations: []string{advancedissuer.OperationOwnApplicationLifecycle}, TTL: advancedissuer.DefaultTTL,
		OwnApplicationLifecycle: &wire.OwnApplicationLifecycle{
			Action: stackkitcommand.OwnApplicationCapabilityAction(o.Step), Name: o.Name, OperationID: req.DurableJobID, ReviewDigest: o.ReviewDigest,
		},
	})
}

// OwnApplicationFailedError is a node answer that changed nothing or did not
// finish: the job fails with its structured outcome and reason code.
type OwnApplicationFailedError struct {
	Step       string
	Outcome    string
	ReasonCode string
}

func (e *OwnApplicationFailedError) Error() string {
	return fmt.Sprintf("own application %s %s: %s", e.Step, e.Outcome, firstNonEmpty(e.ReasonCode, "no_reason_code"))
}

func ownApplicationFailure(result map[string]interface{}, step, outcome, reason string) map[string]interface{} {
	if result == nil {
		result = map[string]interface{}{}
	}
	result["success"] = false
	result[OwnApplicationResultField] = map[string]interface{}{"operation": "own-application." + step, "outcome": outcome, "reason_code": reason}
	return result
}

func runOwnApplicationJob(ctx context.Context, cfg StackKitLifecycleConfig, job *Job, q *Queue, req StackKitLifecycleRequest, release stackkitrelease.Release) error {
	if cfg.Services == nil {
		return NewConfigError(fmt.Errorf("application aggregate authority is unavailable"))
	}
	o := req.OwnApplication
	head, err := cfg.Services.ReadServiceEventHead(ctx, req.TenantID, req.ServiceID)
	if err != nil {
		return NewPermanentError(err)
	}
	if head.Service.StackID != req.StackID || head.Service.ServerID != o.ServerID {
		return NewPermanentError(fmt.Errorf("own application source identity changed"))
	}
	base := map[string]interface{}{
		"schema_version": "techstack.stackkit-lifecycle-result/v1", "techstack_id": req.StackID, "operation": req.Operation,
		"agent_id": req.AgentID, "service_action_receipt": StackKitServiceActionReceipt(req),
	}
	read := stackkitcommand.OwnApplicationStepRead(o.Step)
	if !read && head.Service.MutationLock.Locked() {
		job.replaceResult(ownApplicationFailure(base, o.Step, "refused", "service_locked"))
		return NewPermanentError(&OwnApplicationFailedError{Step: o.Step, Outcome: "refused", ReasonCode: "service_locked"})
	}
	catalog, err := release.AdvancedOperations()
	if err == nil {
		err = catalog.Dispatchable("own-application." + o.Step)
	}
	if err != nil {
		job.replaceResult(ownApplicationFailure(base, o.Step, "refused", "own_application_unavailable"))
		return NewPermanentError(fmt.Errorf("the pinned StackKits release does not dispatch own-application.%s: %w", o.Step, err))
	}
	var capability []byte
	if !read {
		if cfg.ApplicationAuthorize == nil {
			return NewConfigError(fmt.Errorf("application operate authority is unavailable"))
		}
		if err := cfg.ApplicationAuthorize(contextWithJobRequestAuthority(ctx, job), req.TenantID, req.OwnerID, o.ServerID); err != nil {
			job.replaceResult(ownApplicationFailure(base, o.Step, "denied", "application_operate_denied"))
			return NewPermanentError(fmt.Errorf("application operate authority denied: %w", err))
		}
		if stackkitcommand.OwnApplicationStepExecutes(o.Step) {
			issued, issueErr := issueOwnApplicationCapability(ctx, cfg.AdvancedIssuer, req)
			if issueErr != nil {
				job.replaceResult(ownApplicationFailure(base, o.Step, "refused", "own_application_capability_unavailable"))
				return NewConfigError(fmt.Errorf("issue the own-application.lifecycle capability: %w", issueErr))
			}
			capability = issued.Raw
			base["capability_id"] = issued.ID
		}
	}
	command, err := ownApplicationCommand(req, release, capability)
	if err != nil {
		return NewPermanentError(err)
	}
	job.setStep("stackkit_dispatch")
	q.UpdateProgress(job.ID, 10, "Dispatching own-application."+o.Step)
	result, sendErr := sendStackKitCommandBoundedForTenant(ctx, cfg.Sender, req.TenantID, req.AgentID, command)
	if sendErr != nil {
		if read {
			return NewResourceError(sendErr)
		}
		// The node may have started or finished the step: never retried, never
		// inferred; status tells the owner what the node holds.
		job.replaceResult(ownApplicationFailure(base, o.Step, "unresolved", "node_outcome_unknown"))
		return NewPermanentError(fmt.Errorf("the own application %s outcome on the node is unknown: %w", o.Step, sendErr))
	}
	if err = stackkitcommand.ValidateResult(result, command); err != nil {
		return NewPermanentError(err)
	}
	normalized, err := normalizeStackKitLifecycleResult(req, result)
	if err != nil {
		return NewPermanentError(err)
	}
	for key, value := range base {
		if _, set := normalized[key]; !set {
			normalized[key] = value
		}
	}
	answer, parseErr := stackkitcommand.ParseOwnApplicationResult(command, result)
	if parseErr != nil {
		normalized = ownApplicationFailure(normalized, o.Step, "unresolved", "node_result_invalid")
		job.replaceResult(normalized)
		return NewPermanentError(parseErr)
	}
	normalized[OwnApplicationResultField] = stackkitcommand.OwnApplicationResultMap(answer)
	// The raw node output (logs included) is kept only as its bounded,
	// cleaned projection above, never verbatim.
	if commandResult, ok := normalized["command_result"].(map[string]interface{}); ok {
		normalized["command_result"] = map[string]interface{}{
			"schemaVersion": commandResult["schemaVersion"], "command": commandResult["command"], "status": commandResult["status"],
		}
	}
	// Logs are returned to the owner once; they are never stored on the row.
	if o.Step != stackkitcommand.OwnApplicationLogs && o.Step != stackkitcommand.OwnApplicationBackup {
		if err := projectOwnApplicationStatus(ctx, cfg, job.ID, req, answer); err != nil {
			// The answer stands; only the row projection is behind.
			normalized["status_projection"] = "own_application_status_not_projected"
		}
	}
	failed := !result.Success || answer.Outcome == "refused" || answer.Outcome == "pending" || answer.Outcome == "denied" || answer.Outcome == "rollback_failed" ||
		(answer.Outcome == "rolled_back" && o.Step == stackkitcommand.OwnApplicationUpdate)
	if failed {
		normalized["success"] = false
		job.replaceResult(normalized)
		return NewPermanentError(&OwnApplicationFailedError{Step: o.Step, Outcome: answer.Outcome, ReasonCode: answer.ReasonCode})
	}
	job.replaceResult(normalized)
	q.UpdateProgress(job.ID, 100, "own-application."+o.Step+" "+answer.Outcome)
	return nil
}

// OwnApplicationStatus is the projection of the last signed node answer on
// the own application's row: its recorded state, image, the observed
// containers and the retained data checkpoint.
type OwnApplicationStatus struct {
	Name       string                                    `json:"name"`
	State      string                                    `json:"state,omitempty"`
	Image      string                                    `json:"image,omitempty"`
	Containers []stackkitcommand.OwnApplicationContainer `json:"containers,omitempty"`
	Observed   bool                                      `json:"observed"`
	Checkpoint *stackkitcommand.OwnApplicationCheckpoint `json:"checkpoint,omitempty"`
	JobID      string                                    `json:"job_id"`
	At         time.Time                                 `json:"at"`
}

// ReadOwnApplicationStatus returns the projected status of a row.
func ReadOwnApplicationStatus(service *controlplane.ServiceRuntime) (OwnApplicationStatus, bool) {
	var status OwnApplicationStatus
	if service == nil || service.Metadata[OwnApplicationStatusKey] == nil {
		return status, false
	}
	raw, _ := json.Marshal(service.Metadata[OwnApplicationStatusKey])
	if json.Unmarshal(raw, &status) != nil || status.Name == "" {
		return status, false
	}
	return status, true
}

func projectOwnApplicationStatus(ctx context.Context, cfg StackKitLifecycleConfig, jobID string, req StackKitLifecycleRequest, answer stackkitcommand.OwnApplicationResult) error {
	if answer.State == "" && answer.Checkpoint == nil && len(answer.Containers) == 0 {
		return nil
	}
	head, err := cfg.Services.ReadServiceEventHead(ctx, req.TenantID, req.ServiceID)
	if err != nil {
		return err
	}
	status, _ := ReadOwnApplicationStatus(head.Service)
	status.Name, status.JobID, status.At = req.OwnApplication.Name, jobID, time.Now().UTC()
	if answer.State != "" {
		status.State = answer.State
	}
	if answer.Image != "" {
		status.Image = answer.Image
	}
	if answer.Checkpoint != nil || answer.State != "" {
		// The signed record names the checkpoint; no checkpoint means none. A
		// tag update result names no time: keep the time already known for
		// the same checkpoint, else the time Techstack learned of it.
		previous := status.Checkpoint
		status.Checkpoint = answer.Checkpoint
		if checkpoint := status.Checkpoint; checkpoint != nil && checkpoint.At == nil {
			at := time.Now().UTC()
			if previous != nil && previous.OperationID == checkpoint.OperationID && previous.At != nil {
				at = *previous.At
			}
			checkpoint.At = &at
		}
	}
	if req.OwnApplication.Step == stackkitcommand.OwnApplicationStatus {
		status.Containers, status.Observed = answer.Containers, answer.Outcome == "observed"
	}
	metadata := map[string]any{}
	for key, value := range head.Service.Metadata {
		metadata[key] = value
	}
	encoded, _ := json.Marshal(status)
	var stored map[string]any
	_ = json.Unmarshal(encoded, &stored)
	metadata[OwnApplicationStatusKey] = stored
	patch := controlplane.ServiceRuntime{ManagementState: head.Service.ManagementState, Metadata: metadata}
	_, err = cfg.Services.ApplyServiceEvent(ctx, controlplane.ServiceEvent{
		TenantID: req.TenantID, ServiceID: req.ServiceID, ExpectedRevision: &head.Revision, Authority: controlplane.ServiceEventAuthorityControlPlane,
		Source: "stackkit-own-application", ReasonCode: "own_application_" + strings.ReplaceAll(req.OwnApplication.Step, ".", "_"), EnforceIdentityBinding: true,
		Runtime: patch, Evidence: map[string]any{"job_id": jobID, "outcome": answer.Outcome},
	})
	return err
}
