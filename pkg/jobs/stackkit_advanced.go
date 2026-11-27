package jobs

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kombifyio/homelab/internal/advancedissuer"
	"github.com/kombifyio/homelab/internal/runtimeproduct/runtimeaction"
	"github.com/kombifyio/homelab/internal/stackkitrelease"
	"github.com/kombifyio/homelab/pkg/api/agentpb"
	"github.com/kombifyio/homelab/pkg/auth/stackkitdecision"
	"github.com/kombifyio/homelab/pkg/backupstore"
	"github.com/kombifyio/homelab/pkg/recoveryproof"
	"github.com/kombifyio/homelab/pkg/stackkitcommand"
)

// Internal step names of the Advanced lifecycle operations. They are not
// operator operations: the operator asks for advanced_change_set,
// drift_reconcile, rollback or restore_drill and Core dispatches the steps.
const (
	stackKitStepChangeSetCreate = "advanced_change_set_create"
	stackKitStepChangeSetApply  = "advanced_change_set_apply"
	stackKitStepDriftReconcile  = "advanced_drift_reconcile"
	stackKitStepRollback        = "advanced_rollback"
	stackKitStepRestoreDrill    = "advanced_restore_drill"
	// The managed kombify.me origin certificate (ADR-0047): the node returns a
	// CSR, Homelab has Cloudflare issue it, the node installs the result.
	stackKitStepOriginCertificateRequest = "origin_certificate_request"
	stackKitStepOriginCertificateInstall = "origin_certificate_install"

	// StackKitModeAdvanced is the stackkit_mode every managed deployment
	// records: Homelab dispatches Advanced Mode only.
	StackKitModeAdvanced = "advanced"
	// StackKitModeField is the deploy-result and stack-config mode key.
	StackKitModeField = "stackkit_mode"

	advancedResultField     = "advanced_result"
	advancedDenialField     = "advanced_denial"
	stackKitInstanceIDField = "stackkit_instance_id"
	resultModeField         = "mode"
	resultPlanHashField     = "plan_hash"
	resultSnapshotAnchorKey = "snapshot_anchor_id"
	resultActionField       = "action"
	driftStackRoleField     = "role"
	advancedDenialDetailKey = "detail"
	advancedOperationField  = "operation"
	advancedReasonCodeField = "reason_code"
)

// IsAdvancedStackKitLifecycleOperation reports the operator operations Core
// runs as capability-gated Advanced operations.
func IsAdvancedStackKitLifecycleOperation(operation string) bool {
	switch strings.ToLower(strings.TrimSpace(operation)) {
	case StackKitLifecycleAdvancedChangeSet, StackKitLifecycleDriftReconcile,
		StackKitLifecycleRollback, StackKitLifecycleRestoreDrill, StackKitLifecyclePublicTLSRepair:
		return true
	default:
		return false
	}
}

// AdvancedOperationDeniedError is a StackKits admission denial
// (stackkit.operation-denial/v1). The job fails with its reason code.
type AdvancedOperationDeniedError struct {
	Operation  string
	ReasonCode string
	Message    string
}

func (e *AdvancedOperationDeniedError) Error() string {
	return fmt.Sprintf("StackKits denied Advanced operation %s: %s", e.Operation, e.ReasonCode)
}

// AdvancedOperationUnavailableError reports an operation the pinned
// release's catalog does not list as available. Core refuses to dispatch it.
type AdvancedOperationUnavailableError struct {
	Operation string
	Err       error
}

func (e *AdvancedOperationUnavailableError) Error() string {
	return fmt.Sprintf("Advanced operation %s is not dispatchable: %v", e.Operation, e.Err)
}

func (e *AdvancedOperationUnavailableError) Unwrap() error { return e.Err }

// AdvancedPrerequisiteError reports a managed rollout whose Advanced
// prerequisite is missing: the pinned release's own operations catalog or the
// installation's Advanced issuer. Every Homelab-managed deployment runs
// Advanced Mode, so the rollout fails closed; there is no Standard fallback.
type AdvancedPrerequisiteError struct {
	Prerequisite string
	Err          error
}

func (e *AdvancedPrerequisiteError) Error() string {
	return fmt.Sprintf("managed rollout requires Advanced Mode, but %s is missing: %v", e.Prerequisite, e.Err)
}

func (e *AdvancedPrerequisiteError) Unwrap() error { return e.Err }

const (
	advancedPrerequisiteCatalog = "the pinned release's Advanced operations catalog"
	advancedPrerequisiteIssuer  = "the Advanced capability issuer"
)

// advancedFailureReason maps an Advanced failure to its structured reason.
func advancedFailureReason(err error) (string, string, bool) {
	var denied *AdvancedOperationDeniedError
	if errors.As(err, &denied) {
		return denied.ReasonCode, denied.Operation, true
	}
	var unavailable *AdvancedOperationUnavailableError
	if errors.As(err, &unavailable) {
		return "advanced_operation_unavailable", unavailable.Operation, true
	}
	var prerequisite *AdvancedPrerequisiteError
	if errors.As(err, &prerequisite) {
		return "advanced_prerequisite_missing", "", true
	}
	return "", "", false
}

// advancedDispatcher mints one capability per operator Advanced operation and
// sends it with each typed command of that operation. Capabilities are scoped
// to the deployment's StackKit stack id and the local Owner of its trust
// binding. StackKits binds a change set to the capability that created it
// (the record stores capabilityId and its digest, and apply or Advanced
// reconcile reject any other capability), so a change-set flow carries one
// capability allowing exactly its create and its apply or reconcile step.
type advancedDispatcher struct {
	cfg      StackKitLifecycleConfig
	sender   StackKitCommandSender
	issuer   AdvancedIssuer
	release  stackkitrelease.Release
	catalog  stackkitrelease.AdvancedOperationsCatalog
	binding  advancedissuer.TrustBinding
	stackID  string
	request  StackKitLifecycleRequest
	commands []*agentpb.StackKitCommand
}

func newAdvancedDispatcher(ctx context.Context, sender StackKitCommandSender, issuer AdvancedIssuer, release stackkitrelease.Release, request StackKitLifecycleRequest) (*advancedDispatcher, error) {
	if sender == nil {
		return nil, fmt.Errorf("typed StackKits dispatcher is not configured")
	}
	if issuer == nil {
		return nil, advancedissuer.ErrUnavailable
	}
	catalog, err := release.AdvancedOperations()
	if err != nil {
		return nil, err
	}
	binding, err := issuer.TrustBindingFor(ctx, request.TenantID, request.StackID)
	if err != nil {
		return nil, fmt.Errorf("managed deployment has no Advanced trust binding; run advanced_trust_import: %w", err)
	}
	if request.WizardExtend != nil && binding.StackID != request.StackKitInstanceID {
		return nil, fmt.Errorf("wizard extension differs from the enrolled Advanced instance")
	}
	if request.BackupRenewal != nil && binding.StackID != request.StackKitInstanceID {
		return nil, fmt.Errorf("backup renewal differs from enrolled Advanced instance")
	}
	stackID := firstNonEmpty(binding.StackID, request.StackKitInstanceID)
	if stackID == "" {
		return nil, fmt.Errorf("managed deployment has no StackKit stack id for its Advanced capability")
	}
	return &advancedDispatcher{
		sender: sender, issuer: issuer, release: release, catalog: catalog,
		binding: binding, stackID: stackID, request: request,
	}, nil
}

// require refuses every step the pinned release's catalog does not list as
// available before the first step is dispatched.
func (d *advancedDispatcher) require(steps ...string) ([]string, error) {
	operations := make([]string, 0, len(steps))
	for _, step := range steps {
		operation, err := stackKitLifecycleAgentOperation(step)
		if err != nil {
			return nil, err
		}
		catalogOperation := stackkitcommand.AdvancedCatalogOperation(operation)
		if dispatchErr := d.catalog.Dispatchable(catalogOperation); dispatchErr != nil {
			return nil, &AdvancedOperationUnavailableError{Operation: catalogOperation, Err: dispatchErr}
		}
		operations = append(operations, catalogOperation)
	}
	return operations, nil
}

// issue mints the one capability of an operator operation.
func (d *advancedDispatcher) issue(ctx context.Context, operations []string) (advancedissuer.Capability, error) {
	capability, err := d.issuer.Issue(ctx, advancedissuer.Request{
		TenantID: d.request.TenantID, DeploymentID: d.request.StackID, StackID: d.stackID,
		OwnerRef: d.binding.OwnerRef, Operations: operations, TTL: advancedissuer.DefaultTTL, BackupRenewal: d.request.BackupRenewal,
	})
	if err != nil {
		return advancedissuer.Capability{}, fmt.Errorf("issue Advanced capability for %s: %w", strings.Join(operations, ", "), err)
	}
	return capability, nil
}

// dispatch sends one Advanced step with the operator operation's capability
// and admits the typed evidence.
func (d *advancedDispatcher) dispatch(ctx context.Context, commandID, step string, capability advancedissuer.Capability, prepare func(*agentpb.StackKitCommand)) (stackkitcommand.AdvancedOperationResult, map[string]interface{}, error) {
	request := d.request
	request.Operation = step
	command, err := stackKitLifecycleCommand(commandID, request, d.release)
	if err != nil {
		return stackkitcommand.AdvancedOperationResult{}, nil, err
	}
	catalogOperation := stackkitcommand.AdvancedCatalogOperation(command.Operation)
	if dispatchErr := d.catalog.Dispatchable(catalogOperation); dispatchErr != nil {
		return stackkitcommand.AdvancedOperationResult{}, nil, &AdvancedOperationUnavailableError{Operation: catalogOperation, Err: dispatchErr}
	}
	if prepare != nil {
		prepare(command)
	}
	command.AdvancedCapability = capability.Raw
	if validateErr := stackkitcommand.ValidateCommand(command); validateErr != nil {
		return stackkitcommand.AdvancedOperationResult{}, nil, fmt.Errorf("build Advanced %s command: %w", catalogOperation, validateErr)
	}
	d.commands = append(d.commands, command)
	result, err := sendStackKitCommandBoundedForTenant(ctx, d.sender, d.request.TenantID, d.request.AgentID, command)
	if err != nil {
		return stackkitcommand.AdvancedOperationResult{}, nil, &typedStackKitOperationError{Operation: step, CommandID: commandID, Err: err}
	}
	normalized, err := normalizeStackKitLifecycleResult(request, result)
	if err != nil {
		return stackkitcommand.AdvancedOperationResult{}, nil, err
	}
	normalized["capability_id"] = capability.ID
	if !result.Success {
		if denial, denied := stackkitcommand.ParseOperationDenial(result); denied {
			normalized[advancedDenialField] = map[string]interface{}{
				advancedReasonCodeField: denial.ReasonCode, advancedOperationField: firstNonEmpty(denial.Operation, catalogOperation), advancedDenialDetailKey: denial.Message,
			}
			return stackkitcommand.AdvancedOperationResult{}, normalized, &AdvancedOperationDeniedError{
				Operation: catalogOperation, ReasonCode: denial.ReasonCode, Message: denial.Message,
			}
		}
		return stackkitcommand.AdvancedOperationResult{}, normalized, fmt.Errorf("StackKits %s failed with exit code %d: %s", catalogOperation, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	admitted, err := stackkitcommand.ParseAdvancedOperationResult(command, result)
	if err != nil {
		return stackkitcommand.AdvancedOperationResult{}, normalized, err
	}
	admitted.CommittedBaselineSpecJSON = nil
	if baseline, err := stackkitcommand.CommittedBaselineSpec(result); err == nil {
		admitted.CommittedBaselineSpecJSON = baseline
	}
	if d.request.BackupRenewal != nil {
		if err := verifyManagedDrillReceipt(admitted.Report, d.request.BackupRenewal, commandID, capability.ID); err != nil {
			return stackkitcommand.AdvancedOperationResult{}, normalized, err
		}
	}
	if step == stackKitStepRestoreDrill {
		proof, hasProof, proofErr := d.evaluateDispatchRecoveryProof(ctx, admitted, time.Now().UTC())
		if hasProof {
			normalized["recovery_proof"] = proof
		}
		if proofErr != nil {
			return stackkitcommand.AdvancedOperationResult{}, normalized, proofErr
		}
	}
	normalized[advancedResultField] = advancedResultMap(admitted)
	return admitted, normalized, nil
}

func (d *advancedDispatcher) evaluateDispatchRecoveryProof(
	ctx context.Context,
	admitted stackkitcommand.AdvancedOperationResult,
	now time.Time,
) (recoveryproof.Report, bool, error) {
	survivalRaw, err := selectBackupSurvivalReceipt(admitted.BackupSurvival, d.request.BackupSurvivalReceipt)
	if err != nil {
		return recoveryproof.Report{}, false, err
	}
	hasRecovery := len(admitted.ApplicationRecovery) > 0
	hasSurvival := len(survivalRaw) > 0
	if !hasRecovery && !hasSurvival {
		return recoveryproof.Report{}, false, nil
	}

	policy := recoveryproof.DefaultPolicy()
	if hasSurvival {
		trustedKey, keyErr := d.resolveBackupSurvivalOwnerKey(ctx)
		if keyErr != nil {
			return recoveryproof.Report{}, false, keyErr
		}
		policy.SurvivalPublicKey = trustedKey
		policy.RequireSurvivalSignature = true
	}

	if hasRecovery && !hasSurvival {
		proof, evalErr := recoveryproof.Evaluate(admitted.Report, nil, now, policy)
		if evalErr != nil {
			return recoveryproof.Report{}, false, evalErr
		}
		if proof.Recovery.Status != recoveryproof.StatusVerified {
			return proof, true, fmt.Errorf("application recovery receipt rejected (%s): %s", proof.Recovery.Status, proof.Recovery.Reason)
		}
		return proof, true, nil
	}

	if !hasRecovery && hasSurvival {
		survClaim, survErr := recoveryproof.ProjectSurvival(survivalRaw, now, policy)
		if survErr != nil {
			return recoveryproof.Report{}, false, survErr
		}
		proof := recoveryproof.Report{
			State:    recoveryproof.StateUnjoined,
			Recovery: recoveryproof.Claim{Status: recoveryproof.StatusAbsent, Reason: "no application recovery receipt"},
			Survival: survClaim,
			JoinGaps: []string{"recovery proof is absent"},
		}
		if survClaim.Status != recoveryproof.StatusVerified {
			return proof, true, fmt.Errorf("backup survival receipt rejected (%s): %s", survClaim.Status, survClaim.Reason)
		}
		if err := verifyExpectedSurvivalIdentity(d.request.NodeRef, d.request.WorkloadRef, survClaim); err != nil {
			proof.JoinGaps = append(proof.JoinGaps, err.Error())
			return proof, true, err
		}
		return proof, true, nil
	}

	recClaim, recErr := recoveryproof.ProjectRecovery(admitted.Report, now, policy)
	if recErr != nil {
		return recoveryproof.Report{}, false, recErr
	}
	if recClaim.Status != recoveryproof.StatusVerified {
		return recoveryproof.Report{
			State:    recoveryproof.StateUnjoined,
			Recovery: recClaim,
			Survival: recoveryproof.Claim{Status: recoveryproof.StatusAbsent},
			JoinGaps: []string{fmt.Sprintf("recovery proof is %s", recClaim.Status)},
		}, true, fmt.Errorf("application recovery receipt rejected (%s): %s", recClaim.Status, recClaim.Reason)
	}
	expectedNodeID, expectedWorkloadID, err := expectedRecoveryIdentity(d.request, recClaim)
	if err != nil {
		return recoveryproof.Report{
			State:    recoveryproof.StateUnjoined,
			Recovery: recClaim,
			Survival: recoveryproof.Claim{Status: recoveryproof.StatusAbsent},
			JoinGaps: []string{err.Error()},
		}, true, err
	}
	expectedTargetID := ""
	if d.request.BackupRenewal != nil && strings.TrimSpace(d.request.BackupRenewal.TargetRef) != "" {
		expectedTargetID = strings.TrimSpace(d.request.BackupRenewal.TargetRef)
	} else if delivery := managedBackupDeliveryOf(d.cfg.ManagedStackKitInventory); delivery != nil {
		nb, nbErr := delivery.NodeBinding(ctx, d.request.TenantID, d.request.StackID)
		switch {
		case nbErr == nil:
			expectedTargetID = strings.TrimSpace(nb.BackupTargetRef)
		case errors.Is(nbErr, backupstore.ErrNodeBindingMissing), errors.Is(nbErr, backupstore.ErrCustodyNotFound):
		default:
			return recoveryproof.Report{}, false, fmt.Errorf("resolve backup target binding: %w", nbErr)
		}
	}

	proof, evalErr := recoveryproof.EvaluateBound(admitted.Report, survivalRaw, recoveryproof.JoinBinding{
		NodeID:           expectedNodeID,
		WorkloadID:       expectedWorkloadID,
		TargetID:         expectedTargetID,
		SnapshotAnchorID: d.request.SnapshotAnchorID,
	}, now, policy)
	if evalErr != nil {
		return recoveryproof.Report{}, false, evalErr
	}
	if proof.Recovery.Status != recoveryproof.StatusVerified {
		return proof, true, fmt.Errorf("application recovery receipt rejected (%s): %s", proof.Recovery.Status, proof.Recovery.Reason)
	}
	if proof.Survival.Status != recoveryproof.StatusVerified {
		return proof, true, fmt.Errorf("backup survival receipt rejected (%s): %s", proof.Survival.Status, proof.Survival.Reason)
	}
	if proof.Joined == nil {
		return proof, true, fmt.Errorf("recovery and survival proofs could not be joined: %s", strings.Join(proof.JoinGaps, "; "))
	}
	return proof, true, nil
}

func selectBackupSurvivalReceipt(reportRaw, requestRaw []byte) ([]byte, error) {
	rep := bytes.TrimSpace(reportRaw)
	req := bytes.TrimSpace(requestRaw)
	if len(rep) == 0 {
		return req, nil
	}
	if len(req) == 0 {
		return rep, nil
	}
	if !bytes.Equal(rep, req) {
		var repCompact, reqCompact bytes.Buffer
		if json.Compact(&repCompact, rep) != nil || json.Compact(&reqCompact, req) != nil || !bytes.Equal(repCompact.Bytes(), reqCompact.Bytes()) {
			return nil, errors.New("backup survival receipt in report differs from request")
		}
	}
	return rep, nil
}

func (d *advancedDispatcher) resolveBackupSurvivalOwnerKey(ctx context.Context) (ed25519.PublicKey, error) {
	if d.cfg.BackupSurvivalOwnerKey != nil {
		key, err := d.cfg.BackupSurvivalOwnerKey(ctx, d.request.TenantID, d.request.StackID)
		if err != nil {
			return nil, fmt.Errorf("resolve node owner key for backup survival receipt: %w", err)
		}
		if len(key) != ed25519.PublicKeySize {
			return nil, errors.New("resolved node owner key for backup survival receipt is invalid")
		}
		return key, nil
	}
	if delivery := managedBackupDeliveryOf(d.cfg.ManagedStackKitInventory); delivery != nil {
		if provider, ok := delivery.(ManagedBackupOwnerKeyProvider); ok {
			raw, err := provider.NodeOwnerPublicKey(ctx, d.request.TenantID, d.request.StackID)
			if err != nil {
				return nil, fmt.Errorf("resolve node owner key for backup survival receipt: %w", err)
			}
			if len(raw) != ed25519.PublicKeySize {
				return nil, errors.New("resolved node owner key for backup survival receipt is invalid")
			}
			return ed25519.PublicKey(raw), nil
		}
	}
	return nil, errors.New("backup survival receipt cannot be verified without the node's trusted owner public key")
}

func expectedRecoveryIdentity(req StackKitLifecycleRequest, rec recoveryproof.Claim) (string, string, error) {
	var recNode, recWorkload string
	if rec.Subject != nil {
		recNode, recWorkload = rec.Subject.NodeID, rec.Subject.WorkloadID
	}
	reqNode := strings.TrimSpace(req.NodeRef)
	if reqNode != "" && recNode != "" && reqNode != recNode {
		return "", "", fmt.Errorf("application recovery nodeId %q differs from expected nodeId %q", recNode, reqNode)
	}
	reqWorkload := strings.TrimSpace(req.WorkloadRef)
	if reqWorkload != "" && recWorkload != "" && reqWorkload != recWorkload {
		return "", "", fmt.Errorf("application recovery workloadId %q differs from expected workloadId %q", recWorkload, reqWorkload)
	}
	return firstNonEmpty(reqNode, recNode), firstNonEmpty(reqWorkload, recWorkload), nil
}

func verifyExpectedSurvivalIdentity(expectedNode, expectedWorkload string, surv recoveryproof.Claim) error {
	expectedNode = strings.TrimSpace(expectedNode)
	expectedWorkload = strings.TrimSpace(expectedWorkload)
	if surv.Subject == nil {
		if expectedNode != "" || expectedWorkload != "" {
			return errors.New("backup survival receipt lacks subject identity")
		}
		return nil
	}
	if expectedNode != "" && surv.Subject.NodeID != expectedNode {
		return fmt.Errorf("backup survival nodeId %q differs from expected nodeId %q", surv.Subject.NodeID, expectedNode)
	}
	if expectedWorkload != "" && surv.Subject.WorkloadID != expectedWorkload {
		return fmt.Errorf("backup survival workloadId %q differs from expected workloadId %q", surv.Subject.WorkloadID, expectedWorkload)
	}
	return nil
}

func advancedResultMap(result stackkitcommand.AdvancedOperationResult) map[string]interface{} {
	out := map[string]interface{}{advancedOperationField: result.Operation, resultStatusField: result.Status}
	if result.ChangeSetID != "" {
		out["change_set_id"] = result.ChangeSetID
		out["change_set_sha256"] = result.ChangeSetSHA256
	}
	if len(result.Report) > 0 {
		var report interface{}
		if json.Unmarshal(result.Report, &report) == nil {
			out["report"] = report
		}
	}
	return out
}

// runAdvancedStackKitLifecycle runs one operator Advanced operation:
// advanced_change_set creates and applies a change set for the candidate
// StackSpec; drift_reconcile creates a change set for the candidate (the
// workspace StackSpec when none is given) and reconciles through it;
// rollback and restore_drill are one step each.
func runAdvancedStackKitLifecycle(ctx context.Context, cfg StackKitLifecycleConfig, release stackkitrelease.Release, jobID string, request StackKitLifecycleRequest) (map[string]interface{}, error) {
	dispatcher, err := newAdvancedDispatcher(ctx, cfg.Sender, cfg.AdvancedIssuer, release, request)
	if err != nil {
		return nil, err
	}
	dispatcher.cfg = cfg
	if err := revalidateWizardExecutionBinding(ctx, cfg.Sender, release, request, dispatcher.binding.OwnerRef, jobID+"-binding-plan"); err != nil {
		return nil, err
	}
	if request.Operation == StackKitLifecyclePublicTLSRepair {
		return repairPublicTLS(ctx, cfg, dispatcher, jobID, request, release)
	}
	steps := map[string][]string{
		StackKitLifecycleAdvancedChangeSet: {stackKitStepChangeSetCreate, stackKitStepChangeSetApply},
		StackKitLifecycleDriftReconcile:    {stackKitStepChangeSetCreate, stackKitStepDriftReconcile},
		StackKitLifecycleRollback:          {stackKitStepRollback},
		StackKitLifecycleRestoreDrill:      {stackKitStepRestoreDrill},
	}[request.Operation]
	if len(steps) == 0 {
		return nil, fmt.Errorf("unsupported Advanced lifecycle operation %q", request.Operation)
	}
	operations, err := dispatcher.require(steps...)
	if err != nil {
		return nil, err
	}
	capability, err := dispatcher.issue(ctx, operations)
	if err != nil {
		return nil, err
	}
	summary := map[string]interface{}{
		"schema_version": "homelab.stackkit-lifecycle-result/v1", "kit_deployment_id": request.StackID,
		stackKitInstanceIDField: request.StackKitInstanceID, advancedOperationField: request.Operation, "agent_id": request.AgentID,
		StackKitModeField: StackKitModeAdvanced,
		"capability_id":   capability.ID,
	}
	if request.Operation == StackKitLifecycleAdvancedChangeSet {
		reconciliation, err := reconcileManagedChangeSetAddresses(ctx, cfg.ManagedAddressAuthority, release, request)
		if err != nil {
			summary["success"] = false
			return summary, err
		}
		summary[ManagedAddressReconciliationResultField] = reconciliation
		// A changed route set needs a certificate for the new hostnames before
		// apply verifies TLS; the old certificate is revoked after the swap.
		certificate, certErr := reissueOriginCertificateForRoutes(ctx, cfg.OriginCertificates, dispatcher, jobID, reconciliation)
		if certificate != nil {
			summary[OriginCertificateResultField] = certificate
		}
		if certErr != nil {
			summary["success"] = false
			return summary, certErr
		}
	}
	var created stackkitcommand.AdvancedOperationResult
	stepResults := make([]interface{}, 0, len(steps))
	for index, step := range steps {
		commandID := jobID
		if len(steps) > 1 {
			commandID = jobID + "-" + step
		}
		receipt := ""
		if step == stackKitStepChangeSetApply && request.Operation == StackKitLifecycleAdvancedChangeSet && decisionReceiptRequired(request) {
			// The apply is the destructive step: nothing reaches it before a
			// checked allowed receipt binds exactly the change set the create
			// stored, its digest, the revision and the acting principal.
			track := cfg.decisionTrack
			if track.jobID == "" {
				track = decisionTracker{jobID: jobID}
			}
			if capability.ExpiresAt.IsZero() {
				summary["success"] = false
				return summary, decisionRefused("refused", "stackkit_decision_window_too_short", "", "the Advanced capability names no expiry, so the wait for the Owner cannot be bounded")
			}
			track.notAfter = capability.ExpiresAt
			admittedReceipt, decisionErr := requireDecisionReceipt(ctx, cfg, track, request, release, func(serverID string, principal stackkitdecision.Principal) (stackkitdecision.Facts, error) {
				return changeSetApplyDecisionFacts(serverID, created.ChangeSetID, created.ChangeSetSHA256, created.WorkspaceRevision, principal)
			})
			if decisionErr != nil {
				summary["success"] = false
				return summary, decisionErr
			}
			receipt = admittedReceipt.Compact
		}
		if step == stackKitStepChangeSetApply || step == stackKitStepDriftReconcile || step == stackKitStepRollback {
			outcome := map[string]interface{}{}
			if err := renewCustomerRegistryPull(ctx, cfg, commandID, request, release, outcome); err != nil {
				summary["success"] = false
				copyRegistryOutcomes(summary, outcome)
				return summary, err
			}
		}
		admitted, normalized, stepErr := dispatcher.dispatch(ctx, commandID, step, capability, func(command *agentpb.StackKitCommand) {
			if index > 0 {
				command.ChangeSetId, command.ChangeSetSha256 = created.ChangeSetID, created.ChangeSetSHA256
			}
			if receipt != "" && command.Operation == agentpb.StackKitOperation_STACKKIT_OPERATION_ADVANCED_CHANGE_SET_APPLY {
				command.DecisionReceipt, command.DecisionPrincipal = receipt, request.DecisionPrincipal
			}
		})
		if normalized != nil {
			stepResults = append(stepResults, normalized)
			summary["steps"] = stepResults
			for _, key := range []string{advancedResultField, advancedDenialField, "capability_id", "command_result", "events", "release", "stderr", "exit_code", "recovery_proof"} {
				if value, ok := normalized[key]; ok {
					summary[key] = value
				}
			}
		}
		if stepErr != nil {
			summary["success"] = false
			return summary, stepErr
		}
		if (step == stackKitStepChangeSetApply || step == stackKitStepDriftReconcile) && cfg.CommitSpecCanonical != nil && len(request.CandidateSpecJSON) > 0 {
			if err := cfg.CommitSpecCanonical(ctx, request, admitted.CommittedBaselineSpecJSON); err != nil {
				summary["success"] = false
				return summary, err
			}
		}
		if step == stackKitStepChangeSetCreate {
			if cfg.CommitRollbackTarget != nil {
				if err := cfg.CommitRollbackTarget(ctx, request, admitted.ChangeSetID, admitted.CommittedBaselineSpecJSON); err != nil {
					summary["success"] = false
					return summary, err
				}
			}
			created = admitted
			summary["change_set_id"], summary["change_set_sha256"] = admitted.ChangeSetID, admitted.ChangeSetSHA256
		}
	}
	summary["success"] = true
	return summary, nil
}

// advancedGenerationCandidate sets generation.target terramate on the
// managed rollout's candidate StackSpec. Every managed rollout generates the
// Terramate target from its first rollout; a pinned release without its own
// Advanced operations catalog cannot serve it and fails the rollout.
func advancedGenerationCandidate(candidate []byte, release stackkitrelease.Release) ([]byte, error) {
	if _, err := release.AdvancedOperations(); err != nil {
		return nil, &AdvancedPrerequisiteError{Prerequisite: advancedPrerequisiteCatalog, Err: err}
	}
	if len(candidate) == 0 {
		return nil, fmt.Errorf("managed rollout requires the candidate StackSpec for its Terramate generation")
	}
	return TerramateGenerationCandidate(candidate)
}

// TerramateGenerationCandidate sets generation.target terramate on a candidate
// StackSpec. A node generates the Terramate target from its first managed
// rollout and StackKits admits an Advanced change set only for that target,
// while the stored desired setup keeps the kit's default target. Every Day-2
// candidate (wizard extend, owner apply with a candidate) passes through here.
func TerramateGenerationCandidate(candidate []byte) ([]byte, error) {
	var document map[string]interface{}
	if decodeErr := json.Unmarshal(candidate, &document); decodeErr != nil {
		return nil, fmt.Errorf("decode Advanced candidate: %w", decodeErr)
	}
	generation := mapFromInterface(document["generation"])
	if generation == nil {
		generation = map[string]interface{}{}
	}
	generation["target"] = advancedGenerationTarget
	document["generation"] = generation
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode Advanced candidate: %w", err)
	}
	return encoded, nil
}

// runAdvancedStackKitLifecycleJob runs an operator Advanced operation as a
// lifecycle job. A restore drill first configures the owner-bound backup
// repository for the applied plan, which the drill's snapshot needs.
func runAdvancedStackKitLifecycleJob(ctx context.Context, cfg StackKitLifecycleConfig, job *Job, q *Queue, req StackKitLifecycleRequest, release stackkitrelease.Release) error {
	var managedAdmission map[string]interface{}
	if req.Operation == StackKitLifecycleRestoreDrill && req.StackKit != DefaultBasementKitRef {
		var err error
		req, managedAdmission, err = prepareManagedRestore(ctx, cfg, job, req, release)
		if err != nil {
			return NewPermanentError(err)
		}
	}
	if req.Operation == StackKitLifecycleRestoreDrill && req.BackupRenewal == nil {
		job.setStep("stackkit_backup_configure")
		q.UpdateProgress(job.ID, 20, "Configuring the owner-bound backup repository for the restore drill")
		err := configureStackKitBackupForDrill(ctx, cfg.Sender, job.ID, req, release)
		if err != nil {
			return NewPermanentError(err)
		}
	}
	job.setStep("stackkit_advanced_" + req.Operation)
	q.UpdateProgress(job.ID, 40, "Dispatching StackKits Advanced "+req.Operation)
	commandScope := job.ID
	if req.Operation == StackKitLifecyclePublicTLSRepair {
		// Each queue attempt is a new pair of node commands: an at-most-once
		// command id is never dispatched twice.
		job.mu.RLock()
		commandScope = fmt.Sprintf("%s-a%d", job.ID, job.Attempts)
		job.mu.RUnlock()
	}
	cfg.decisionTrack = jobDecisionTracker(job, q)
	summary, err := runAdvancedStackKitLifecycle(ctx, cfg, release, commandScope, req)
	if summary != nil {
		if managedAdmission != nil {
			summary[ManagedBackupAdmissionField] = managedAdmission
		}
		job.replaceStackKitLifecycleResult(req, summary)
	}
	if err != nil {
		if errors.Is(err, advancedissuer.ErrUnavailable) {
			return NewConfigError(err)
		}
		var unavailable *AdvancedOperationUnavailableError
		if req.Operation == StackKitLifecyclePublicTLSRepair && !errors.As(err, &unavailable) {
			// Cloudflare issuance and node install failures are transient: the
			// queue retries this job with backoff instead of giving up.
			return NewTransientError(err)
		}
		return NewPermanentError(err)
	}
	q.UpdateProgress(job.ID, 100, "StackKits Advanced "+req.Operation+" completed")
	return nil
}

func configureStackKitBackupForDrill(ctx context.Context, sender StackKitCommandSender, jobID string, req StackKitLifecycleRequest, release stackkitrelease.Release) error {
	configure := req
	configure.Operation = StackKitLifecycleBackupConfigure
	planHash := ""
	if configure.StackKitInstanceID != "" {
		var err error
		if planHash, err = planStackKitLifecycleApply(ctx, sender, jobID, configure, release); err != nil {
			return fmt.Errorf("bind restore drill to the node plan: %w", err)
		}
	}
	command, err := stackKitLifecycleCommand(jobID+"-backup_configure", configure, release)
	if err != nil {
		return err
	}
	command.ExpectedPlanHash = planHash
	result, err := sendStackKitCommandBoundedForTenant(ctx, sender, req.TenantID, req.AgentID, command)
	if err != nil {
		return fmt.Errorf("restore-drill backup configuration dispatch failed: %w", err)
	}
	if err := requireSuccessfulNativeBackupStep("backup configure", result); err != nil {
		return err
	}
	return stackkitcommand.ParseBackupConfigurationEvidence(command, result)
}

// driftReportMap projects a stackkit.drift-report/v1 for job results: the
// combined status and one entry per Terramate stack.
func driftReportMap(report stackkitcommand.DriftReport) map[string]interface{} {
	stacks := make([]interface{}, 0, len(report.Stacks))
	for _, stack := range report.Stacks {
		entry := map[string]interface{}{
			stackIDField: stack.StackID, driftStackRoleField: stack.Role, "module_ref": stack.ModuleRef,
			resultStatusField: stack.Status, "runtime_root": stack.RuntimeRoot,
		}
		if stack.PlanExit != nil {
			entry["plan_exit_code"] = *stack.PlanExit
		}
		stacks = append(stacks, entry)
	}
	return map[string]interface{}{
		resultStatusField: report.Status, resultModeField: report.Mode, "generation_target": report.GenerationTarget,
		"has_drift": report.HasDrift, "stacks": stacks,
	}
}

// requireAdvancedRestoreDrill fails a rollout whose drill cannot run as the
// Advanced restore.drill of the pinned release: a missing issuer, catalog or
// available restore.drill entry is an error. There is no native staged
// restore fallback.
func (r *deployRollout) requireAdvancedRestoreDrill(release stackkitrelease.Release) error {
	if r.cfg.AdvancedIssuer == nil {
		return &AdvancedPrerequisiteError{Prerequisite: advancedPrerequisiteIssuer, Err: advancedissuer.ErrUnavailable}
	}
	catalog, err := release.AdvancedOperations()
	if err != nil {
		return &AdvancedPrerequisiteError{Prerequisite: advancedPrerequisiteCatalog, Err: err}
	}
	drill := stackkitcommand.AdvancedCatalogOperation(agentpb.StackKitOperation_STACKKIT_OPERATION_ADVANCED_RESTORE_DRILL)
	if dispatchErr := catalog.Dispatchable(drill); dispatchErr != nil {
		return &AdvancedOperationUnavailableError{Operation: drill, Err: dispatchErr}
	}
	return nil
}

// runAdvancedRestoreDrill runs the rollout's restore drill as the Advanced
// restore.drill operation of the pinned release.
func (r *deployRollout) runAdvancedRestoreDrill(ctx context.Context, request StackKitLifecycleRequest, release stackkitrelease.Release, planHash string) (map[string]interface{}, error) {
	request.Operation = StackKitLifecycleRestoreDrill
	request.OwnerApproved = true
	cfg := r.cfg.StackKitLifecycle
	cfg.Sender = r.cfg.StackKitCommander
	cfg.AdvancedIssuer = r.cfg.AdvancedIssuer
	if cfg.ManagedStackKitInventory == nil {
		cfg.ManagedStackKitInventory = r.cfg.ManagedStackKitInventory
	}
	summary, err := runAdvancedStackKitLifecycle(ctx, cfg, release, r.job.ID+"-restore-drill", request)
	if err != nil {
		return nil, err
	}
	advanced, _ := summary[advancedResultField].(map[string]interface{})
	report, _ := advanced["report"].(map[string]interface{})
	out := map[string]interface{}{
		resultActionField:       string(runtimeaction.ActionRestoreDrill),
		resultStatusField:       string(runtimeaction.StatusVerified),
		resultModeField:         "advanced-restore-drill",
		stackKitInstanceIDField: request.StackKitInstanceID,
		resultPlanHashField:     planHash,
		"drill_id":              report["drillId"],
		resultSnapshotAnchorKey: report["anchorId"],
		"restore_result_id":     report["restoreResultId"],
		"capability_id":         summary["capability_id"],
	}
	if proof, ok := summary["recovery_proof"]; ok {
		out["recovery_proof"] = proof
	}
	return out, nil
}
