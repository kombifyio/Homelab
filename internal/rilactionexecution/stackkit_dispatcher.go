package rilactionexecution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kombifyio/homelab/internal/stackkitrelease"
	"github.com/kombifyio/homelab/pkg/agentcontrol"
	"github.com/kombifyio/homelab/pkg/api/agentpb"
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/grpcserver"
	"github.com/kombifyio/homelab/pkg/guardreach"
	"github.com/kombifyio/homelab/pkg/jobs"
	"github.com/kombifyio/homelab/pkg/ril/actioncontract"
	"github.com/kombifyio/homelab/pkg/ril/primitives"
	"github.com/kombifyio/homelab/pkg/stackkitcommand"
)

const (
	// verifyExecutorRef is the stack-scoped verifier's evidence identity.
	verifyExecutorRef = "stackkits-pinned-cli-verify-v1"
	// managedServiceExecutorRef is the catalog executor of the managed
	// service primitives (check-backup, run-backup, start-service,
	// stop-service, restart-service, update-service).
	managedServiceExecutorRef = "stackkits-managed-service-executor-v1"
	// guardHostExecutorRef is the catalog executor of the host primitives (the
	// Guard itself runs the one container or service-unit verb).
	guardHostExecutorRef = "stackkits-guard-host-executor-v1"
	// reattachGrace bounds the wait for an already dispatched command's
	// durable outcome beyond its own timeout.
	reattachGrace = 2 * time.Minute
)

// StackKitDispatcherConfig binds the executor-bound primitives to the typed
// StackKits command channel. The request cannot select a binary, command,
// provider, or transport: each primitive profile sends exactly the one typed
// command its catalog Guard operation names.
type StackKitDispatcherConfig struct {
	Sender       jobs.StackKitCommandSender
	Now          func() time.Time
	Authority    DispatchAuthority
	ProofGateway ProofGateway
	// Lifecycle prepares a managed lifecycle request against canonical stack,
	// agent and node authority, exactly as the managed service path enqueues
	// it (the orchestrator).
	Lifecycle LifecyclePreparer
	// StackKitsTargets records an update's outcome as the node's StackKits
	// target, as the managed upgrade job does.
	StackKitsTargets controlplane.WorkerStackKitsTargetRecorder
}

// LifecyclePreparer binds a lifecycle request to canonical authority without
// reserving a job, and returns the managed lifecycle job's own aggregate
// capture and projection (start/stop service intent, adopted application
// control receipts).
type LifecyclePreparer interface {
	PrepareStackKitLifecycleCommand(context.Context, jobs.StackKitLifecycleRequest) (jobs.StackKitLifecycleRequest, error)
	RILLifecycleEffects() jobs.RILLifecycleEffects
}

// StackKitDispatcher is the one dispatcher of the executor-bound primitives,
// keyed by primitive profile.
type StackKitDispatcher struct {
	sender       jobs.StackKitCommandSender
	now          func() time.Time
	authority    DispatchAuthority
	proofGateway ProofGateway
	lifecycle    LifecyclePreparer
	targets      controlplane.WorkerStackKitsTargetRecorder
}

func NewStackKitDispatcher(config StackKitDispatcherConfig) (*StackKitDispatcher, error) {
	if config.Sender == nil || config.Authority == nil || config.ProofGateway == nil || config.Lifecycle == nil || config.StackKitsTargets == nil {
		return nil, fmt.Errorf("rilactionexecution: typed sender, dispatch authority, Gateway proof authority, lifecycle preparation and StackKits target store are required")
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	return &StackKitDispatcher{sender: config.Sender, now: config.Now, authority: config.Authority, proofGateway: config.ProofGateway, lifecycle: config.Lifecycle, targets: config.StackKitsTargets}, nil
}

// DispatchAuthority re-checks and binds the approved owner/server/lifecycle
// tuple under lock immediately before the dispatch and releases the lock
// before the send; it never derives a worker from the target.
type DispatchAuthority interface {
	OwnerFor(context.Context, rilaction.Request, string) (string, error)
	IssuedLease(context.Context, rilaction.Request, string) (guardreach.RILAuthorityResult, error)
	WithWorker(context.Context, rilaction.Request, string, *agentpb.RILExecutionProof, func(context.Context, string) error) error
}
type ProofGateway interface {
	RILAuthority(context.Context, string, string, string, string, string, string, string) (guardreach.RILAuthorityResult, error)
}

type tenantCommandSender interface {
	SendStackKitCommandForTenant(context.Context, string, string, *agentpb.StackKitCommand) (*agentpb.StackKitResult, error)
}

type tenantCommandAwaiter interface {
	AwaitStackKitCommandForTenant(context.Context, string, string) (*agentpb.StackKitResult, error)
}

// dispatch is what one proof-bearing send produced.
type dispatch struct {
	command  *agentpb.StackKitCommand
	request  jobs.StackKitLifecycleRequest
	release  stackkitrelease.Release
	worker   string
	result   *agentpb.StackKitResult
	enqueued bool
	// effects and intent carry the managed job's aggregate capture of a
	// start or stop to its projection after the node's evidence.
	effects jobs.RILLifecycleEffects
	intent  jobs.StackKitServiceIntent
}

// outcome is the closed public result of one primitive.
type outcome struct {
	succeeded bool
	observed  bool
	checks    []rilaction.VerificationCheck
	codes     []string
	recovery  rilaction.RecoveryEvidence
}

// Execute sends the primitive's one typed command only after the exact
// release qualifies that primitive and the admitted lifecycle authority still
// authorizes the current worker.
func (d *StackKitDispatcher) Execute(ctx context.Context, request rilaction.Request, admissionDigest string) (rilaction.Evidence, error) {
	profile, ok := primitives.For(request.Primitive)
	if !ok || !profile.Admits(request.Target, request.Inputs) {
		return rilaction.Evidence{}, fmt.Errorf("rilactionexecution: unsupported governed primitive profile")
	}
	// The execution instant is the start of the dispatch, inside the admitted
	// authority window; a long-running effect still reports when it began.
	executedAt := d.now().UTC()
	scoped, ok := d.sender.(tenantCommandSender)
	if !ok {
		return d.failedEvidence(request, profile, executedAt, fmt.Errorf("RIL execution requires tenant-scoped transient Guard transport"))
	}
	if err := agentcontrol.ValidateRILQueueCustody(); err != nil {
		return d.failedEvidence(request, profile, executedAt, err)
	}
	owner, err := d.authority.OwnerFor(ctx, request, admissionDigest)
	if err != nil {
		return d.failedEvidence(request, profile, executedAt, err)
	}
	issued, err := d.authority.IssuedLease(ctx, request, admissionDigest)
	if err != nil || !issued.Allowed || issued.Lease == "" || len(issued.Receipt) == 0 || len(issued.Effect) == 0 {
		return d.failedEvidence(request, profile, executedAt, fmt.Errorf("gateway issue denied or unavailable: %v", err))
	}
	claimed, err := d.proofGateway.RILAuthority(ctx, owner, request.TenantID, "claim", request.ActionCardID, issued.Lease, request.ExecutionID, admissionDigest)
	if err != nil || !claimed.Allowed || claimed.Claim == "" || claimed.ExecutionID != request.ExecutionID || claimed.Fence != admissionDigest || claimed.LeaseID == "" {
		return d.failedEvidence(request, profile, executedAt, fmt.Errorf("gateway claim denied or mismatched: %v", err))
	}
	proof := &agentpb.RILExecutionProof{LeaseJws: issued.Lease, ClaimJws: claimed.Claim, ReceiptJson: issued.Receipt, EffectJson: issued.Effect}
	proof.ProofDigest, err = stackkitcommand.RILProofDigest(proof)
	if err != nil {
		return d.failedEvidence(request, profile, executedAt, err)
	}
	var sent dispatch
	err = d.authority.WithWorker(ctx, request, admissionDigest, proof, func(ctx context.Context, worker string) error {
		release, err := jobs.TargetStackKitReleaseForStack(ctx, request.TenantID, request.StackID)
		if err != nil {
			return err
		}
		if release == nil {
			return fmt.Errorf("StackKits release unavailable")
		}
		if !release.RILPrimitiveSupported(profile.ID) || release.RILPrimitiveHash(profile.ID) != request.Primitive.ContractHash {
			return fmt.Errorf("pinned StackKits release lacks the qualified %s primitive", profile.ID)
		}
		prepared, err := d.command(ctx, request, profile, owner, worker, *release)
		if err != nil {
			return err
		}
		command := prepared.command
		command.RilExecutionProof = proof
		jobs.AddressNodeStackKitRelease(ctx, request.TenantID, worker, command)
		if err := stackkitcommand.ValidateCommand(command); err != nil {
			return err
		}
		sent = prepared
		sent.enqueued = true
		sent.result, err = scoped.SendStackKitCommandForTenant(ctx, request.TenantID, worker, command)
		return err
	})
	if err != nil && sent.enqueued && sent.result == nil && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		// The admitted authority window bounds the dispatch, not a running
		// effect: a command a Guard already received keeps running, and its
		// durable outcome is read back here (a command no Guard received was
		// withdrawn and reads back as failed).
		sent.result, err = d.reattach(ctx, request.TenantID, sent.command)
	}
	if err == nil {
		err = stackkitcommand.ValidateResult(sent.result, sent.command)
	}
	result := d.outcome(ctx, request, profile, sent, err)
	evidence, evidenceErr := d.evidence(request, profile, executedAt, result)
	if evidenceErr != nil {
		return rilaction.Evidence{}, evidenceErr
	}
	if err == nil && !result.succeeded {
		err = fmt.Errorf("StackKits %s did not succeed", profile.ID)
	}
	return evidence, err
}

// command builds the primitive's one typed command. The verifier keeps its
// pinned stack-scoped command; the managed service primitives send the very
// command the managed service path builds for the same operation (for an
// adopted application: its application control command), after the managed
// job's own aggregate capture. A host primitive sends the Guard's own host
// command for exactly its verb and object, addressed to the node the effect
// names.
func (d *StackKitDispatcher) command(ctx context.Context, request rilaction.Request, profile primitives.Profile, owner, worker string, release stackkitrelease.Release) (dispatch, error) {
	out := dispatch{release: release, worker: worker}
	if profile.IsHost() {
		out.command = &agentpb.StackKitCommand{CommandId: request.ExecutionID, Operation: profile.GuardOperation,
			HostTarget: primitives.InputValue(request.Inputs, profile.HostInput()), HostAction: profile.HostAction,
			LocalNodeRef: request.Target.NodeRef, TimeoutSeconds: int32(jobs.HostActionCommandTimeout / time.Second),
			Release: grpcserver.StackKitReleasePinFor(release)}
		return out, nil
	}
	operation := profile.GuardOperationFor(request.Inputs)
	if operation == agentpb.StackKitOperation_STACKKIT_OPERATION_VERIFY {
		if !release.VerifyExpectedPlanSupported() {
			return out, fmt.Errorf("pinned StackKits release lacks the qualified plan verifier")
		}
		out.command = &agentpb.StackKitCommand{CommandId: request.ExecutionID, Operation: agentpb.StackKitOperation_STACKKIT_OPERATION_VERIFY,
			WorkingDirectory: "/opt/stackkit", SpecPath: "stack-spec.yaml", OutputDirectory: "deploy", TimeoutSeconds: 240,
			Release: grpcserver.StackKitReleasePinFor(release), Offline: true, ExpectedPlanHash: request.ResolvedPlanHash}
		return out, nil
	}
	out.effects = d.lifecycle.RILLifecycleEffects()
	lifecycleRequest := jobs.StackKitLifecycleRequest{
		StackID: request.StackID, TenantID: request.TenantID, OwnerID: owner, AgentID: worker,
		OwnerApproved: profile.Mutation,
	}
	serverID := request.Target.SiteRef
	serviceKey := primitives.InputValue(request.Inputs, primitives.InputRuntimeInstanceRef)
	switch operation {
	case agentpb.StackKitOperation_STACKKIT_OPERATION_SERVICE_START, agentpb.StackKitOperation_STACKKIT_OPERATION_SERVICE_STOP:
		lifecycleRequest.Operation = jobs.StackKitLifecycleServiceStart
		if operation == agentpb.StackKitOperation_STACKKIT_OPERATION_SERVICE_STOP {
			lifecycleRequest.Operation = jobs.StackKitLifecycleServiceStop
		}
		lifecycleRequest.ServiceKey = serviceKey
		// The managed start or stop anchors its declared intent on the
		// service's control; its execution is the durable identity.
		serviceID, err := out.effects.DeclaredServiceControlID(ctx, request.TenantID, request.StackID, serverID, serviceKey)
		if err != nil {
			return out, err
		}
		lifecycleRequest.ServiceID, lifecycleRequest.DurableJobID, lifecycleRequest.ServiceActionDigest = serviceID, request.ExecutionID, executionDigest(request)
	case agentpb.StackKitOperation_STACKKIT_OPERATION_SERVICE_RESTART:
		lifecycleRequest.Operation = jobs.StackKitLifecycleServiceRestart
		lifecycleRequest.ServiceKey = serviceKey
	case agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_CONTROL:
		// An adopted application's power action is application control of
		// its exact container, bound to the retained application binding of
		// its current aggregate head and the release this command addresses.
		serviceID, binding, err := out.effects.ApplicationControlBinding(ctx, request.TenantID, request.StackID, serverID,
			primitives.InputValue(request.Inputs, primitives.InputApplicationRef), profile.ApplicationAction, release)
		if err != nil {
			return out, err
		}
		lifecycleRequest.Operation = jobs.StackKitLifecycleApplicationControl
		lifecycleRequest.ServiceKey = "smart-home"
		lifecycleRequest.Application = binding
		lifecycleRequest.ServiceID, lifecycleRequest.DurableJobID, lifecycleRequest.ServiceActionDigest = serviceID, request.ExecutionID, executionDigest(request)
	case agentpb.StackKitOperation_STACKKIT_OPERATION_UPGRADE:
		lifecycleRequest.Operation = jobs.StackKitLifecycleUpgrade
		lifecycleRequest.TargetRelease = primitives.InputValue(request.Inputs, primitives.InputTargetReleaseRef)
		approved, err := jobs.ApprovedStackKitsUpgradeTarget(lifecycleRequest.TargetRelease, release)
		if err != nil || approved != lifecycleRequest.TargetRelease {
			return out, fmt.Errorf("the update targets %s, not the published StackKits release", lifecycleRequest.TargetRelease)
		}
	case agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_STATUS, agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_RUN:
		lifecycleRequest.Operation = jobs.StackKitLifecycleBackupStatus
		if operation == agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_RUN {
			lifecycleRequest.Operation = jobs.StackKitLifecycleBackupRun
		}
		if primitives.InputValue(request.Inputs, primitives.InputBackupContractRef) != request.StackID {
			return out, fmt.Errorf("the backup contract names another stack")
		}
	default:
		return out, fmt.Errorf("unsupported Guard operation %s", operation)
	}
	prepared, err := d.lifecycle.PrepareStackKitLifecycleCommand(ctx, lifecycleRequest)
	if err != nil {
		return out, err
	}
	out.request = prepared
	if prepared.AgentID != worker || prepared.StackID != request.StackID {
		return out, fmt.Errorf("prepared lifecycle request addresses another agent or stack")
	}
	if out.intent, err = out.effects.CaptureServiceIntent(ctx, prepared); err != nil {
		return out, err
	}
	if operation == agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_CONTROL {
		// Its command and operation id are the execution (DurableJobID).
		out.command, err = jobs.ApplicationLifecycleCommand(prepared, release)
	} else {
		out.command, err = jobs.StackKitLifecycleCommand(request.ExecutionID, prepared, release)
	}
	if err != nil {
		return out, err
	}
	if operation == agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_STATUS || operation == agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_RUN {
		// The backup read and run bind the card's exact resolved plan.
		out.command.ExpectedPlanHash = request.ResolvedPlanHash
	}
	return out, nil
}

// executionDigest is the durable request fingerprint of a managed request
// the execution identifies (the lifecycle request's ServiceActionDigest).
func executionDigest(request rilaction.Request) string {
	sum := sha256.Sum256([]byte(request.TenantID + "\x00" + request.ActionCardID + "\x00" + request.ExecutionID))
	return hex.EncodeToString(sum[:])
}

func (d *StackKitDispatcher) reattach(ctx context.Context, tenantID string, command *agentpb.StackKitCommand) (*agentpb.StackKitResult, error) {
	awaiter, ok := d.sender.(tenantCommandAwaiter)
	if !ok || command == nil {
		return nil, fmt.Errorf("RIL command outcome cannot be read back")
	}
	timeout := time.Duration(command.TimeoutSeconds)*time.Second + reattachGrace
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return awaiter.AwaitStackKitCommandForTenant(waitCtx, tenantID, command.CommandId)
}

// outcome derives the closed public result of the primitive from the node's
// typed result, reusing the managed lifecycle result normalizer.
func (d *StackKitDispatcher) outcome(ctx context.Context, request rilaction.Request, profile primitives.Profile, sent dispatch, sendErr error) outcome {
	ok := sendErr == nil && sent.result != nil && sent.result.Success
	observed := sendErr == nil && sent.result != nil
	if profile.IsHost() {
		return hostOutcome(profile, sent, sendErr)
	}
	reason := ""
	if observed && !ok {
		reason = jobs.StackKitFailureReasonCode(sent.result)
	}
	switch profile.GuardOperationFor(request.Inputs) {
	case agentpb.StackKitOperation_STACKKIT_OPERATION_VERIFY:
		if ok {
			if _, err := stackkitcommand.ParseVerifyPlanEvidence(sent.command, sent.result); err != nil {
				ok = false
			}
		}
		return closedOutcome(ok, false, "stackkit-verify", "stackkit-verify", reason, rilaction.RecoveryEvidence{Kind: "none", Status: "not-required"})
	case agentpb.StackKitOperation_STACKKIT_OPERATION_SERVICE_START, agentpb.StackKitOperation_STACKKIT_OPERATION_SERVICE_STOP,
		agentpb.StackKitOperation_STACKKIT_OPERATION_SERVICE_RESTART:
		return d.serviceOutcome(ctx, request, profile, sent, ok, observed, reason)
	case agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_CONTROL:
		return d.applicationOutcome(ctx, request, profile, sent, ok, observed, reason)
	case agentpb.StackKitOperation_STACKKIT_OPERATION_UPGRADE:
		return d.updateOutcome(ctx, request, sent, ok, observed, reason)
	case agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_STATUS:
		return backupStatusOutcome(sent, ok, observed, reason)
	case agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_RUN:
		// As on the managed path, a successful run is admitted only with the
		// node's backup run evidence (snapshot anchor bound to the plan).
		if ok && jobs.StackKitBackupRunEvidence(sent.command, sent.result) != nil {
			return closedOutcome(false, true, "backup-run", "backup-run", "", rilaction.RecoveryEvidence{Kind: "manual", Status: "manual-required"})
		}
		return closedOutcome(ok, observed, "backup-run", "backup-run", reason, rilaction.RecoveryEvidence{Kind: "manual", Status: "manual-required"})
	default:
		return closedOutcome(false, false, "primitive-dispatch", "primitive-dispatch", "", rilaction.RecoveryEvidence{Kind: "none", Status: "not-required"})
	}
}

// backupStatusOutcome is the closed result of the backup check, the same for
// every kit: the check passes only when the node's repository is ready and an
// authenticated snapshot receipt exists, and the closed codes name what it
// read (repository, newest snapshot, newest staged restore). A node that has
// no backup to read answers with its typed reason (backup_not_configured,
// backup_source_unavailable), which stays on the failed card as a code; the
// check changed nothing, so there is nothing to recover. No path, repository
// detail or credential enters the evidence.
func backupStatusOutcome(sent dispatch, ok, observed bool, reason string) outcome {
	none := rilaction.RecoveryEvidence{Kind: "none", Status: "not-required"}
	if !ok {
		return closedOutcome(false, observed, "backup-status", "backup-check", reason, none)
	}
	status, err := stackkitcommand.ParseBackupStatusEvidence(sent.command, sent.result)
	if err != nil {
		return closedOutcome(false, true, "backup-status", "backup-check", "", none)
	}
	codes := []string{"backup-snapshot-" + status.Snapshot, "backup-restore-" + status.StagedRestore}
	if !status.RepositoryReady {
		return withCodes(outcome{observed: true, recovery: none, checks: []rilaction.VerificationCheck{{ID: "backup-status", Status: "failed"}}},
			append(codes, "backup-check-failed", "backup-repository-not-ready"))
	}
	codes = append(codes, "backup-repository-ready")
	if status.Snapshot != "recorded" {
		return withCodes(outcome{observed: true, recovery: none, checks: []rilaction.VerificationCheck{{ID: "backup-status", Status: "failed"}}},
			append(codes, "backup-check-failed"))
	}
	return withCodes(outcome{succeeded: true, observed: true, recovery: none, checks: []rilaction.VerificationCheck{{ID: "backup-status", Status: "passed"}}},
		append(codes, "backup-check-passed"))
}

// hostOutcome is the closed result of a host primitive: the Guard's own
// receipt (it reads the state back and answers success only when the verb's
// target state was reached), or the Guard's stable refusal or failure code.
// A refusal changed nothing; a failure may have.
func hostOutcome(profile primitives.Profile, sent dispatch, sendErr error) outcome {
	name := hostPrimitiveName(profile)
	failed := rilaction.RecoveryEvidence{Kind: "manual", Status: "manual-required"}
	if sendErr != nil || sent.result == nil {
		return closedOutcome(false, false, name, name, "", failed)
	}
	data, err := jobs.ParseHostOperationResult(sent.command, sent.result)
	if err == nil && data.Receipt != nil {
		return withCodes(outcome{succeeded: true, observed: true, recovery: rilaction.RecoveryEvidence{Kind: "none", Status: "not-required"},
			checks: []rilaction.VerificationCheck{{ID: name, Status: "passed"}, {ID: "guard-readback", Status: "passed"}}}, []string{name + "-succeeded"})
	}
	var refusal *jobs.HostOperationRefusal
	var failure *jobs.HostOperationFailure
	codes := []string{name + "-failed"}
	switch {
	case errors.As(err, &refusal):
		codes = append(codes, "guard-refused-"+strings.ReplaceAll(refusal.Code, "_", "-"))
	case errors.As(err, &failure):
		codes = append(codes, "guard-failed-"+strings.ReplaceAll(failure.Code, "_", "-"))
	}
	result := outcome{observed: true, recovery: failed, checks: []rilaction.VerificationCheck{{ID: name, Status: "failed"}}}
	if refusal != nil {
		// A refusal changed nothing: there is nothing to recover.
		result.recovery = rilaction.RecoveryEvidence{Kind: "none", Status: "not-required"}
	}
	return withCodes(result, codes)
}

// hostPrimitiveName is the closed check and summary name of a host primitive.
func hostPrimitiveName(profile primitives.Profile) string {
	return "host-" + profile.HostObject + "-" + profile.HostAction
}

// updateOutcome maps the node's upgrade transaction (the job result's
// jobs.UpgradeOutcomeResultField, when the normalizer reports one) into the
// closed result:
// an update that went back to the previous release recovered by itself; any
// other failure needs a person. The approved upgrade's outcome is recorded as
// the node's StackKits target exactly as the managed upgrade job records it.
func (d *StackKitDispatcher) updateOutcome(ctx context.Context, request rilaction.Request, sent dispatch, ok, observed bool, reason string) outcome {
	failed := rilaction.RecoveryEvidence{Kind: "manual", Status: "manual-required"}
	if !observed {
		return closedOutcome(false, false, "stackkit-upgrade", "service-update", reason, failed)
	}
	codes := []string{}
	result := "failed"
	if ok {
		result = "updated"
	}
	if value := jobs.StackKitUpgradeOutcome(sent.request, sent.result); value != "" {
		result = value
	}
	if _, err := jobs.RecordStackKitsTarget(ctx, d.targets, request.ExecutionID, sent.request, sent.result); err != nil {
		codes = append(codes, "stackkit-target-unrecorded")
	}
	switch {
	case ok && result == "updated":
		return withCodes(outcome{succeeded: true, observed: true, recovery: rilaction.RecoveryEvidence{Kind: "none", Status: "not-required"},
			checks: []rilaction.VerificationCheck{{ID: "stackkit-upgrade", Status: "passed"}}}, append(codes, "service-update-succeeded"))
	case result == "returned_to_previous_release":
		return withCodes(outcome{observed: true, recovery: rilaction.RecoveryEvidence{Kind: "manual", Status: "recovered"},
			checks: []rilaction.VerificationCheck{{ID: "previous-release-restored", Status: "passed"}, {ID: "stackkit-upgrade", Status: "failed"}}}, append(codes, "service-update-returned-to-previous-release"))
	default:
		failure := closedOutcome(false, true, "stackkit-upgrade", "service-update", reason, failed)
		if result == "rollback_failed" {
			codes = append(codes, "service-update-rollback-failed")
		}
		return withCodes(failure, append(failure.codes, codes...))
	}
}

// serviceOutcome is the closed result of a managed start, stop or restart.
// As on the managed path, a start or restart is verified by StackKits before
// the measured state counts as changed (the verify reads only; it runs outside
// the admitted dispatch lock and carries no proof), and a start or stop
// projects its declared intent after the node's service evidence.
func (d *StackKitDispatcher) serviceOutcome(ctx context.Context, request rilaction.Request, profile primitives.Profile, sent dispatch, ok, observed bool, reason string) outcome {
	action := "service-" + profile.ApplicationAction
	failed := rilaction.RecoveryEvidence{Kind: "manual", Status: "manual-required"}
	if !ok {
		return closedOutcome(false, observed, action, action, reason, failed)
	}
	checks := []rilaction.VerificationCheck{{ID: action, Status: "passed"}}
	codes := []string{action + "-succeeded"}
	succeeded := true
	if sent.command.Operation != agentpb.StackKitOperation_STACKKIT_OPERATION_SERVICE_STOP {
		verified := d.verifyAfter(ctx, sent)
		checks = append(checks, rilaction.VerificationCheck{ID: "stackkit-verify", Status: checkStatus(verified)})
		if !verified {
			succeeded = false
			codes = append(codes, "stackkit-verify-failed")
		}
	}
	if sent.command.Operation != agentpb.StackKitOperation_STACKKIT_OPERATION_SERVICE_RESTART {
		// The successful host effect is retained even when the aggregate
		// refuses a newer intent; Guard stays the observation authority.
		projected := sent.effects.ProjectServiceIntent(ctx, request.ExecutionID, sent.request, sent.intent, sent.command, sent.result) == nil
		checks = append(checks, rilaction.VerificationCheck{ID: "service-intent-projection", Status: checkStatus(projected)})
		if !projected {
			succeeded = false
			codes = append(codes, "service-intent-unprojected")
		}
	}
	result := outcome{succeeded: succeeded, observed: true, checks: checks, recovery: rilaction.RecoveryEvidence{Kind: "none", Status: "not-required"}}
	if !succeeded {
		result.recovery = failed
	}
	return withCodes(result, codes)
}

// applicationOutcome is the closed result of an adopted application's power
// action: the node's verified application receipt is projected onto the
// service aggregate under the retained binding's fence, as the managed
// application control job does.
func (d *StackKitDispatcher) applicationOutcome(ctx context.Context, request rilaction.Request, profile primitives.Profile, sent dispatch, ok, observed bool, reason string) outcome {
	action := "application-" + profile.ApplicationAction
	failed := rilaction.RecoveryEvidence{Kind: "manual", Status: "manual-required"}
	if !ok {
		return closedOutcome(false, observed, action, action, reason, failed)
	}
	projected := sent.effects.ProjectApplicationControl(ctx, request.ExecutionID, sent.request, sent.command, sent.result) == nil
	result := outcome{succeeded: projected, observed: true, recovery: rilaction.RecoveryEvidence{Kind: "none", Status: "not-required"},
		checks: []rilaction.VerificationCheck{{ID: action, Status: "passed"}, {ID: "application-receipt-projection", Status: checkStatus(projected)}}}
	codes := []string{action + "-succeeded"}
	if !projected {
		result.recovery = failed
		codes = append(codes, "application-receipt-unprojected")
	}
	return withCodes(result, codes)
}

func (d *StackKitDispatcher) verifyAfter(ctx context.Context, sent dispatch) bool {
	verifyRequest := sent.request
	verifyRequest.Operation = jobs.StackKitLifecycleVerify
	verifyRequest.OwnerApproved = false
	verifyRequest.ServiceID, verifyRequest.DurableJobID, verifyRequest.ServiceActionDigest = "", "", ""
	command, err := jobs.StackKitLifecycleCommand(sent.command.CommandId+"-verify", verifyRequest, sent.release)
	if err != nil {
		return false
	}
	result, err := jobs.SendStackKitCommandBoundedForTenant(ctx, d.sender, verifyRequest.TenantID, sent.worker, command)
	return err == nil && stackkitcommand.ValidateResult(result, command) == nil && result.Success
}

func closedOutcome(succeeded, observed bool, check, summary, reason string, recovery rilaction.RecoveryEvidence) outcome {
	result := outcome{succeeded: succeeded, observed: observed, recovery: recovery,
		checks: []rilaction.VerificationCheck{{ID: check, Status: checkStatus(succeeded)}}}
	if succeeded {
		result.recovery = rilaction.RecoveryEvidence{Kind: "none", Status: "not-required"}
		if summary == "stackkit-verify" || summary == "backup-check" {
			return withCodes(result, []string{summary + "-passed"})
		}
		return withCodes(result, []string{summary + "-succeeded"})
	}
	codes := []string{summary + "-failed"}
	if reason != "" {
		// A typed StackKits reason (for example why a backup check failed)
		// is a closed code; raw output never enters public evidence.
		codes = append(codes, "stackkit-"+strings.ReplaceAll(reason, "_", "-"))
	}
	return withCodes(result, codes)
}

func withCodes(result outcome, codes []string) outcome {
	unique := map[string]bool{}
	result.codes = result.codes[:0:0]
	for _, code := range codes {
		if code != "" && !unique[code] {
			unique[code] = true
			result.codes = append(result.codes, code)
		}
	}
	sort.Strings(result.codes)
	sort.Slice(result.checks, func(i, j int) bool { return result.checks[i].ID < result.checks[j].ID })
	return result
}

func checkStatus(passed bool) string {
	if passed {
		return "passed"
	}
	return "failed"
}

func (d *StackKitDispatcher) failedEvidence(request rilaction.Request, profile primitives.Profile, executedAt time.Time, cause error) (rilaction.Evidence, error) {
	recovery := rilaction.RecoveryEvidence{Kind: "none", Status: "not-required"}
	evidence, err := d.evidence(request, profile, executedAt, closedOutcome(false, false, dispatchCheck(profile), dispatchSummary(profile), "", recovery))
	if err != nil {
		return rilaction.Evidence{}, err
	}
	return evidence, cause
}

// dispatchCheck and dispatchSummary name the closed check and summary of a
// primitive that failed before its command reached a node.
func dispatchCheck(profile primitives.Profile) string {
	if profile.IsHost() {
		return hostPrimitiveName(profile)
	}
	switch profile.GuardOperation {
	case agentpb.StackKitOperation_STACKKIT_OPERATION_VERIFY:
		return "stackkit-verify"
	case agentpb.StackKitOperation_STACKKIT_OPERATION_UPGRADE:
		return "stackkit-upgrade"
	case agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_RUN:
		return "backup-run"
	case agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_STATUS:
		return "backup-status"
	default:
		return "service-" + profile.ApplicationAction
	}
}

func dispatchSummary(profile primitives.Profile) string {
	if profile.IsHost() {
		return hostPrimitiveName(profile)
	}
	switch profile.GuardOperation {
	case agentpb.StackKitOperation_STACKKIT_OPERATION_VERIFY:
		return "stackkit-verify"
	case agentpb.StackKitOperation_STACKKIT_OPERATION_UPGRADE:
		return "service-update"
	case agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_RUN:
		return "backup-run"
	case agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_STATUS:
		return "backup-check"
	default:
		return "service-" + profile.ApplicationAction
	}
}

func verificationKind(profile primitives.Profile) string {
	if profile.IsHost() {
		return "stackkit-" + hostPrimitiveName(profile)
	}
	switch profile.GuardOperation {
	case agentpb.StackKitOperation_STACKKIT_OPERATION_VERIFY:
		return "stackkit-pinned-cli-verify"
	case agentpb.StackKitOperation_STACKKIT_OPERATION_UPGRADE:
		return "stackkit-service-update"
	case agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_RUN:
		return "stackkit-backup-run"
	case agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_STATUS:
		return "stackkit-backup-check"
	default:
		return "stackkit-service-" + profile.ApplicationAction
	}
}

func executorRef(profile primitives.Profile) string {
	if profile.IsHost() {
		return guardHostExecutorRef
	}
	if profile.GuardOperation == agentpb.StackKitOperation_STACKKIT_OPERATION_VERIFY {
		return verifyExecutorRef
	}
	return managedServiceExecutorRef
}

//nolint:goconst // Evidence status literals are the closed public rilaction wire vocabulary.
func (d *StackKitDispatcher) evidence(request rilaction.Request, profile primitives.Profile, executedAt time.Time, result outcome) (rilaction.Evidence, error) {
	requestDigest, err := rilaction.ComputeRequestDigest(request)
	if err != nil {
		return rilaction.Evidence{}, err
	}
	executor := executorRef(profile)
	evidenceID, err := rilaction.ComputeEvidenceID(requestDigest, executor)
	if err != nil {
		return rilaction.Evidence{}, err
	}
	targetRef, err := rilaction.TargetReference(request)
	if err != nil {
		return rilaction.Evidence{}, err
	}
	status, check := "failed", "failed"
	if result.succeeded {
		status, check = "succeeded", "passed"
	}
	evidence := rilaction.Evidence{
		APIVersion: rilaction.EvidenceAPIVersionV1, EvidenceID: evidenceID, EvidenceSinkRef: request.EvidenceSinkRef,
		ActionCardID: request.ActionCardID, ExecutionID: request.ExecutionID, TraceID: request.TraceID,
		TenantID: request.TenantID, StackID: request.StackID, PrimitiveID: request.Primitive.ID,
		PrimitiveContractHash: request.Primitive.ContractHash, ResolvedPlanHash: request.ResolvedPlanHash,
		RequestDigest: requestDigest, ExecutorRef: executor, TargetRef: targetRef, Status: status,
		Verification: rilaction.VerificationEvidence{Kind: verificationKind(profile), Status: check, RuntimeStateObserved: result.observed, Checks: result.checks},
		Recovery:     result.recovery, SummaryCodes: result.codes,
		EvaluatedAt: executedAt.Format(time.RFC3339Nano),
	}
	if !result.succeeded {
		evidence.ProtectedDiagnosticRef = "diagnostic:" + request.ExecutionID
	}
	if err := rilaction.ValidateEvidenceForRequest(request, evidence); err != nil {
		return rilaction.Evidence{}, err
	}
	return evidence, nil
}

var _ Dispatcher = (*StackKitDispatcher)(nil)
