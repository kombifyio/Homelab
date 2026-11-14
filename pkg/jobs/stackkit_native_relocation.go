package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	wire "github.com/kombifyio/stackkits/contracts/advancedcapability"

	"github.com/kombifyio/techstack/internal/advancedissuer"
	"github.com/kombifyio/techstack/internal/stackkitrelease"
	"github.com/kombifyio/techstack/pkg/api/agentpb"
	"github.com/kombifyio/techstack/pkg/stackkitcommand"
)

// Workload relocation engines. The Coolify engine moves PaaS projects with
// `stackkit relocate`; the native engine moves an installed use case or
// individual applications with the StackKits application transfer.
const (
	RelocationEngineCoolify = "coolify"
	RelocationEngineNative  = "native"
)

// TransferConfigurationOperation is the source-side Advanced catalog entry
// that projects a selection's portable public configuration. A pinned release
// must list it, in addition to the receive operation, before a native move
// dispatches anything.
const TransferConfigurationOperation = "application-transfer.configuration"

// TransferReceiveIssuer issues the application-transfer.receive capability of
// one receiving step: the exact receiving desired intent and the one source
// Owner of the move. The installation's Advanced issuer provides it through the
// pinned StackKits contract that carries the receive scope.
type TransferReceiveIssuer interface {
	IssueTransferReceive(ctx context.Context, request advancedissuer.Request, scope wire.TransferReceive) (advancedissuer.Capability, error)
}

// errTransferStepUnanswered marks a step whose node answer never arrived: its
// effect is unknown and the step is never repeated.
var errTransferStepUnanswered = errors.New("the node's answer to the transfer step did not arrive")

// nativeRelocation carries one native move through its steps.
type nativeRelocation struct {
	cfg     StackKitLifecycleConfig
	job     *Job
	q       *Queue
	req     WorkloadRelocationRequest
	release stackkitrelease.Release
	receive TransferReceiveIssuer
	outcome map[string]interface{}
	summary map[string]interface{}
}

func validateApplicationTransferStepRequest(req *StackKitLifecycleRequest) error {
	if req.DryRun || req.Offline || len(req.CandidateSpecJSON) != 0 || len(req.InventoryJSON) != 0 || req.ExpectedSpecHash != "" {
		return fmt.Errorf("application transfer takes no dry run, offline mode, candidate, Inventory or spec hash")
	}
	request, err := stackkitcommand.DecodeApplicationTransferRequest(req.ApplicationTransferJSON)
	if err != nil {
		return err
	}
	if request.Mutating() != req.OwnerApproved {
		return fmt.Errorf("application transfer step %s requires Owner approval exactly when it changes the node", request.Step)
	}
	return nil
}

// transferStep is the lifecycle request of one native transfer step on node.
func (req WorkloadRelocationRequest) transferStep(node WorkloadRelocationNode, request stackkitcommand.ApplicationTransferRequest, capability []byte) (StackKitLifecycleRequest, error) {
	encoded, err := request.Encode()
	if err != nil {
		return StackKitLifecycleRequest{}, err
	}
	step := req.step(node, stackKitStepApplicationTransfer)
	step.OwnerApproved = request.Mutating()
	step.ApplicationTransferJSON, step.ApplicationTransferCapability = encoded, capability
	return step, nil
}

// dispatch runs one step and returns its document. A failed step still
// returns the document the CLI wrote (for example the scope a retirement
// awaits) together with the error.
func (m *nativeRelocation) dispatch(ctx context.Context, name string, node WorkloadRelocationNode, request stackkitcommand.ApplicationTransferRequest, capability []byte) (json.RawMessage, error) {
	request.APIVersion = stackkitcommand.ApplicationTransferRequestVersion
	step, err := m.req.transferStep(node, request, capability)
	if err != nil {
		return nil, err
	}
	var command *agentpb.StackKitCommand
	result, commandID, dispatchErr := dispatchTypedStackKitOperation(ctx, m.cfg.Sender, m.job.ID+"-"+name, step, m.release, func(c *agentpb.StackKitCommand) { command = c })
	if result == nil || command == nil {
		return nil, errors.Join(errTransferStepUnanswered, dispatchErr)
	}
	evidence, parseErr := stackkitcommand.ParseApplicationTransferEvidence(command, result)
	if parseErr != nil {
		return nil, &typedStackKitOperationError{Operation: stackKitStepApplicationTransfer + "." + request.Step, CommandID: commandID, Err: errors.Join(dispatchErr, parseErr)}
	}
	if dispatchErr != nil {
		return evidence.Document, &typedStackKitOperationError{Operation: stackKitStepApplicationTransfer + "." + request.Step, CommandID: commandID, Err: dispatchErr}
	}
	return evidence.Document, nil
}

func (m *nativeRelocation) checkpoint(ctx context.Context) error {
	m.job.replaceResult(m.summary)
	syncer := m.q.executionSnapshotSyncer()
	if syncer == nil {
		return fmt.Errorf("durable relocation checkpoint is unavailable")
	}
	return syncer(ctx, m.job.Snapshot())
}

func (m *nativeRelocation) finish(err error, configuration bool) error {
	m.summary["success"] = err == nil
	m.job.replaceResult(m.summary)
	if err == nil {
		return nil
	}
	if configuration {
		return NewConfigError(err)
	}
	return NewPermanentError(err)
}

// issueReceive mints the receive capability of one step on the destination.
func (m *nativeRelocation) issueReceive(ctx context.Context, step, candidateSpecHash string) ([]byte, error) {
	dest := m.req.Destination
	capability, err := m.receive.IssueTransferReceive(ctx, advancedissuer.Request{
		TenantID: m.req.TenantID, DeploymentID: dest.StackID, StackID: dest.StackKitInstanceID, OwnerRef: dest.OwnerRef,
		Operations: []string{wire.OperationApplicationTransferReceive}, TTL: time.Duration(wire.TransferReceiveMaxLifetimeSeconds) * time.Second,
	}, wire.TransferReceive{CandidateSpecHash: candidateSpecHash, SourceOwnerRef: m.req.Source.OwnerRef, Step: step})
	if err != nil {
		return nil, &AdvancedPrerequisiteError{Prerequisite: "the application-transfer.receive capability", Err: err}
	}
	return capability.Raw, nil
}

// issueRetirement mints the retire capability for the exact scope a node
// reported, after Core checked every field it can reconstruct itself.
func (m *nativeRelocation) issueRetirement(ctx context.Context, node WorkloadRelocationNode, scope wire.TransferRetirement) ([]byte, error) {
	if m.cfg.AdvancedIssuer == nil {
		return nil, advancedissuer.ErrUnavailable
	}
	capability, err := m.cfg.AdvancedIssuer.Issue(ctx, advancedissuer.Request{
		TransferRetirement: &scope, TenantID: m.req.TenantID, DeploymentID: node.StackID, StackID: node.StackKitInstanceID, OwnerRef: node.OwnerRef,
		Operations: []string{wire.OperationApplicationTransferRetire}, TTL: 15 * time.Minute,
	})
	if err != nil {
		return nil, err
	}
	return capability.Raw, nil
}

// transferDocuments are the signed documents a native move carries.
type transferDocuments struct {
	admission  json.RawMessage
	receipt    json.RawMessage
	outcome    json.RawMessage
	plan       string // receiving plan hash the destination admitted
	workloads  []string
	nonce      string
	destFP     string
	sourceFP   string
	transferID string
	archive    string
	operation  string
	// portableConfiguration is the source projection the receiving candidate
	// was authored from; its digest binds the destination admission and the
	// activated outcome.
	portableConfiguration       map[string]stackkitcommand.ApplicationTransferWorkloadConfiguration
	portableConfigurationDigest string
}

func runNativeWorkloadRelocation(ctx context.Context, cfg StackKitLifecycleConfig, job *Job, q *Queue, req WorkloadRelocationRequest, release stackkitrelease.Release) error {
	req.TransferID = job.ID
	m := &nativeRelocation{cfg: cfg, job: job, q: q, req: req, release: release}
	m.outcome = map[string]interface{}{
		"engine": RelocationEngineNative, "use_case": req.UseCase, "workloads": req.Workloads,
		"source_stack_id": req.Source.StackID, "destination_stack_id": req.Destination.StackID,
		"source_agent_id": req.Source.AgentID, "destination_agent_id": req.Destination.AgentID,
		"release_version": release.Receipt().Version, "release_archive_sha256": release.Receipt().ArchiveSHA256,
	}
	m.summary = map[string]interface{}{
		"schema_version": "techstack.workload-relocation-result/v1", WorkloadRelocationResultField: m.outcome,
		"relocation_request_sha256": job.Snapshot().Result["relocation_request_sha256"],
	}
	// The pinned release must list the configuration projection and this
	// installation must issue the receive grant; otherwise nothing is dispatched.
	operation := TransferConfigurationOperation
	catalog, err := release.AdvancedOperations()
	if err == nil {
		if err = catalog.Dispatchable(TransferConfigurationOperation); err == nil {
			operation = wire.OperationApplicationTransferReceive
			err = catalog.Dispatchable(operation)
		}
	}
	if err != nil {
		m.outcome["reason_code"], m.outcome["state"] = "stackkits_release_too_old", "preflight_failed"
		return m.finish(&AdvancedOperationUnavailableError{Operation: operation, Err: err}, true)
	}
	receive, ok := cfg.AdvancedIssuer.(TransferReceiveIssuer)
	if !ok || cfg.AdvancedIssuer == nil {
		m.outcome["reason_code"], m.outcome["state"] = "advanced_prerequisite_missing", "preflight_failed"
		return m.finish(&AdvancedPrerequisiteError{Prerequisite: "the application-transfer.receive issuer", Err: advancedissuer.ErrUnavailable}, true)
	}
	m.receive = receive

	// 1. The source projects its selected applications' portable public
	// configuration: public settings and effective module profiles only. Secret
	// material moves solely inside the existing sealed archive custody.
	job.setStep("stackkit_transfer_configuration")
	q.UpdateProgress(job.ID, 5, "Reading the settings of the applications to move")
	exported, err := m.dispatch(ctx, "configuration", req.Source, stackkitcommand.ApplicationTransferRequest{Step: stackkitcommand.TransferStepConfiguration, UseCase: req.UseCase, Workloads: req.Workloads}, nil)
	if err != nil {
		m.outcome["state"] = "preflight_failed"
		return m.finish(err, false)
	}
	configuration, err := stackkitcommand.DecodeApplicationTransferConfiguration(exported)
	if err == nil && len(req.Workloads) != 0 && !sameWorkloads(configuration.Workloads, req.Workloads) {
		err = fmt.Errorf("the source portable configuration names other applications than the selection")
	}
	if err != nil {
		m.outcome["state"] = "preflight_failed"
		return m.finish(err, false)
	}
	m.outcome["portable_configuration_digest"], m.outcome["configured_workloads"] = configuration.PortableConfigurationDigest, configuration.Workloads
	if err := m.checkpoint(ctx); err != nil {
		m.outcome["state"] = "preflight_failed"
		return m.finish(err, false)
	}

	// 2. The destination authors the receiving candidate from that
	// configuration (nothing changes).
	job.setStep("stackkit_transfer_candidate")
	q.UpdateProgress(job.ID, 10, "Preparing the destination server for the move")
	candidateRequest := stackkitcommand.ApplicationTransferRequest{Step: stackkitcommand.TransferStepCandidate, PortableConfiguration: exported}
	if req.UseCase != "" {
		candidateRequest.UseCase = req.UseCase
	} else {
		candidateRequest.Workloads = req.Workloads
	}
	authored, err := m.dispatch(ctx, "candidate", req.Destination, candidateRequest, nil)
	if err != nil {
		m.outcome["state"] = "preflight_failed"
		return m.finish(err, false)
	}
	var candidate struct {
		APIVersion       string          `json:"apiVersion"`
		SpecHash         string          `json:"specHash"`
		PreviousSpecHash string          `json:"previousSpecHash"`
		Outcome          string          `json:"outcome"`
		RuntimeApplied   bool            `json:"runtimeApplied"`
		PreviousSpec     json.RawMessage `json:"previousCanonicalSpec"`
		CandidateSpec    json.RawMessage `json:"candidate_spec"`
	}
	if json.Unmarshal(authored, &candidate) != nil || candidate.APIVersion != "stackkit.intent-change/v1" || candidate.Outcome != "candidate" || candidate.RuntimeApplied ||
		!strings.HasPrefix(candidate.SpecHash, "sha256:") || !strings.HasPrefix(candidate.PreviousSpecHash, "sha256:") || len(candidate.CandidateSpec) == 0 {
		m.outcome["state"] = "preflight_failed"
		return m.finish(fmt.Errorf("the destination did not author a receiving candidate"), false)
	}
	added, err := addedTransferWorkloads(candidate.PreviousSpec, candidate.CandidateSpec)
	if err == nil && len(req.Workloads) != 0 && !sameWorkloads(added, req.Workloads) {
		err = fmt.Errorf("the application belongs to a use case with other applications; move the whole use case")
	}
	if err == nil && !sameWorkloads(added, configuration.Workloads) {
		err = fmt.Errorf("the receiving candidate adds other applications than the source portable configuration names")
	}
	if err != nil {
		m.outcome["state"] = "preflight_failed"
		return m.finish(err, false)
	}
	m.outcome["received_workloads"] = added

	// 3. The destination persists and generates the candidate under its admit
	// grant and issues the signed destination admission.
	docs := transferDocuments{operation: job.ID, portableConfiguration: configuration.Configuration, portableConfigurationDigest: configuration.PortableConfigurationDigest}
	capability, err := m.issueReceive(ctx, "admit", candidate.SpecHash)
	if err != nil {
		m.outcome["state"], m.outcome["reason_code"] = "preflight_failed", "advanced_prerequisite_missing"
		return m.finish(err, true)
	}
	job.setStep("stackkit_transfer_admit")
	docs.admission, err = m.dispatch(ctx, "admit", req.Destination, stackkitcommand.ApplicationTransferRequest{Step: stackkitcommand.TransferStepTarget, CandidateSpec: candidate.CandidateSpec, ExpectedSpecHash: candidate.PreviousSpecHash, SourceOwner: req.Source.OwnerRef, PortableConfigurationDigest: configuration.PortableConfigurationDigest}, capability)
	if err != nil {
		m.outcome["state"] = "preflight_failed"
		return m.finish(err, false)
	}
	var admission struct {
		APIVersion  string `json:"apiVersion"`
		Destination struct {
			OwnerRef                    string `json:"ownerRef"`
			SigningFingerprint          string `json:"signingFingerprint"`
			StackID                     string `json:"stackId"`
			NodeRef                     string `json:"nodeRef"`
			PlanHash                    string `json:"planHash"`
			Nonce                       string `json:"nonce"`
			PortableConfigurationDigest string `json:"portableConfigurationDigest"`
		} `json:"destination"`
	}
	if json.Unmarshal(docs.admission, &admission) != nil || admission.APIVersion != "stackkit.application-transfer-destination/v1" ||
		admission.Destination.OwnerRef != req.Destination.OwnerRef || admission.Destination.StackID != req.Destination.StackKitInstanceID ||
		admission.Destination.NodeRef != req.Destination.NodeRef || admission.Destination.PortableConfigurationDigest != configuration.PortableConfigurationDigest {
		m.outcome["state"] = "preflight_failed"
		return m.finish(errors.Join(fmt.Errorf("the destination admission differs from the admitted destination custody"), m.withdraw(ctx, admission.Destination.Nonce)), false)
	}
	docs.plan, docs.nonce, docs.destFP = admission.Destination.PlanHash, admission.Destination.Nonce, admission.Destination.SigningFingerprint
	m.outcome["destination_plan_hash"], m.outcome["admission_nonce"] = docs.plan, docs.nonce
	if err := m.checkpoint(ctx); err != nil {
		m.outcome["state"] = "preflight_failed"
		return m.finish(errors.Join(err, m.withdraw(ctx, docs.nonce)), false)
	}

	// 3. The source exports the selection to the admitted destination and
	// holds it (migrate): from here on the source stays stopped with its data
	// retained until the destination outcome is verified.
	sourcePlan, err := planStackKitLifecycleApply(ctx, cfg.Sender, job.ID+"-source", req.step(req.Source, StackKitLifecyclePlan), release)
	if err != nil {
		m.outcome["state"] = "preflight_failed"
		return m.finish(errors.Join(err, m.withdraw(ctx, docs.nonce)), false)
	}
	job.setStep("stackkit_transfer_export")
	q.UpdateProgress(job.ID, 30, "Stopping and exporting the applications on the source server")
	export := stackkitcommand.ApplicationTransferRequest{Step: stackkitcommand.TransferStepExport, UseCase: req.UseCase, Workloads: req.Workloads, OperationID: docs.operation,
		DestinationAdmission: docs.admission, DestinationOwner: req.Destination.OwnerRef, DestinationFingerprint: docs.destFP, ExpectedPlanHash: sourcePlan}
	m.outcome["state"] = "exporting"
	if err := m.checkpoint(ctx); err != nil {
		m.outcome["state"] = "preflight_failed"
		return m.finish(errors.Join(err, m.withdraw(ctx, docs.nonce)), false)
	}
	docs.receipt, err = m.dispatch(ctx, "export", req.Source, export, nil)
	if err != nil {
		// An export may have held the source before it failed. The source
		// keeps its data; its existing journal answers a later readback.
		m.outcome["state"], m.outcome["source_retained"] = "source_verification_required", true
		return m.finish(errors.Join(fmt.Errorf("the source export did not complete; the source keeps its data: %w", err), m.withdraw(ctx, docs.nonce)), false)
	}
	if err := docs.readReceipt(req.Source.OwnerRef); err != nil {
		m.outcome["state"], m.outcome["source_retained"] = "source_verification_required", true
		return m.finish(err, false)
	}
	// The destination must receive exactly the applications the source
	// exported; otherwise the move stops before any data leaves the source.
	if !sameWorkloads(added, docs.workloads) {
		return m.finish(m.abort(ctx, docs, fmt.Errorf("the destination adds other applications than the source exported")), false)
	}
	m.outcome["transfer_id"], m.outcome["archive_sha256"], m.outcome["state"] = docs.transferID, docs.archive, "transferring"
	m.outcome["source_receipt"] = string(docs.receipt)
	m.outcome["source_retained"], m.outcome["source_state"] = true, "held"
	if err := m.checkpoint(ctx); err != nil {
		return m.finish(m.abort(ctx, docs, err), false)
	}

	// 4. Node to node: the source serves the ciphertext once to the
	// destination address, which keeps it only when its digest matches.
	job.setStep("stackkit_transfer_copy")
	q.UpdateProgress(job.ID, 45, "Copying the encrypted data to the destination server")
	var serveErr, fetchErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, serveErr = m.dispatch(ctx, "serve", req.Source, stackkitcommand.ApplicationTransferRequest{Step: stackkitcommand.TransferStepServe, OperationID: docs.operation, Peer: req.DestinationAddress}, nil)
	}()
	go func() {
		defer wg.Done()
		_, fetchErr = m.dispatch(ctx, "fetch", req.Destination, stackkitcommand.ApplicationTransferRequest{Step: stackkitcommand.TransferStepFetch, Peer: req.SourceAddress, ArchiveSHA256: docs.archive}, nil)
	}()
	wg.Wait()
	if fetchErr != nil {
		return m.finish(m.abort(ctx, docs, errors.Join(fetchErr, serveErr)), false)
	}

	// 5. The destination adopts the transferred credentials before its first
	// Apply, applies its admitted plan, then imports and reads back the data.
	job.setStep("stackkit_transfer_prepare")
	q.UpdateProgress(job.ID, 60, "Preparing the applications on the destination server")
	received := m.receivedRequest(docs)
	if capability, err = m.issueReceive(ctx, "prepare", candidate.SpecHash); err == nil {
		received.Step = stackkitcommand.TransferStepPrepare
		_, err = m.dispatch(ctx, "prepare", req.Destination, received, capability)
	}
	if err != nil {
		return m.finish(m.abort(ctx, docs, err), false)
	}
	job.setStep("stackkit_transfer_apply")
	q.UpdateProgress(job.ID, 70, "Starting the applications on the destination server")
	apply := req.step(req.Destination, StackKitLifecycleApply)
	applied, err := planStackKitLifecycleApply(ctx, cfg.Sender, job.ID+"-destination", apply, release)
	if err == nil && applied != docs.plan {
		err = fmt.Errorf("the destination plan changed after its admission")
	}
	if err == nil {
		_, _, err = dispatchTypedStackKitOperation(ctx, cfg.Sender, job.ID+"-destination", apply, release, func(command *agentpb.StackKitCommand) { command.ExpectedPlanHash = docs.plan })
	}
	if err != nil {
		return m.finish(m.abort(ctx, docs, err), false)
	}
	job.setStep("stackkit_transfer_import")
	q.UpdateProgress(job.ID, 80, "Moving the data into the destination applications")
	if capability, err = m.issueReceive(ctx, "import", candidate.SpecHash); err != nil {
		return m.finish(m.abort(ctx, docs, err), false)
	}
	received.Step = stackkitcommand.TransferStepImport
	docs.outcome, err = m.dispatch(ctx, "import", req.Destination, received, capability)
	if err != nil {
		if errors.Is(err, errTransferStepUnanswered) {
			// The import may have committed: never repeat it. Both sides keep
			// their data until its own journal resolves the outcome.
			m.outcome["state"] = "destination_verification_required"
			return m.finish(fmt.Errorf("the destination import is unconfirmed; the source stays stopped with its data: %w", err), false)
		}
		return m.finish(m.abort(ctx, docs, err), false)
	}
	readback, err := docs.verifyOutcome("activated")
	if err != nil {
		m.outcome["state"] = "destination_verification_required"
		return m.finish(err, false)
	}
	m.outcome["destination_readback"] = readback
	m.outcome["destination_outcome"] = string(docs.outcome)
	m.outcome["state"] = "destination_verified"
	if err := m.checkpoint(ctx); err != nil {
		return m.finish(err, false)
	}

	// 6. Only now is the source retired: its selection leaves the desired
	// intent, it stays stopped and its data is retained.
	job.setStep("stackkit_transfer_finish")
	q.UpdateProgress(job.ID, 90, "Retiring the applications on the source server; their data is kept")
	finished, err := m.retire(ctx, req.Source, "finish", "source", stackkitcommand.ApplicationTransferRequest{Step: stackkitcommand.TransferStepFinish, SourceReceipt: docs.receipt, TargetOutcome: docs.outcome, DestinationFingerprint: docs.destFP}, docs)
	if err != nil {
		m.outcome["state"] = "moved_source_retirement_pending"
		return m.finish(fmt.Errorf("the applications run on the destination; the source stays stopped with its data until it is retired: %w", err), false)
	}
	var source struct {
		SourceState string `json:"sourceState"`
	}
	_ = json.Unmarshal(finished, &source)
	m.outcome["state"], m.outcome["source_state"] = "moved", source.SourceState
	q.UpdateProgress(job.ID, 100, "The applications run on the destination server")
	return m.finish(nil, false)
}

// receivedRequest is the common admission of the receiving steps.
func (m *nativeRelocation) receivedRequest(docs transferDocuments) stackkitcommand.ApplicationTransferRequest {
	return stackkitcommand.ApplicationTransferRequest{SourceReceipt: docs.receipt, SourceOwner: m.req.Source.OwnerRef, SourceFingerprint: docs.sourceFP,
		TransferID: docs.transferID, ArchiveSHA256: docs.archive, ExpectedPlanHash: docs.plan}
}

// retire runs a finish (source) or abort (destination) step. A managed node
// first names the exact retirement scope it awaits; Core checks every field
// it can reconstruct, issues the retire capability for exactly that scope and
// runs the same step once more with it.
func (m *nativeRelocation) retire(ctx context.Context, node WorkloadRelocationNode, name, role string, request stackkitcommand.ApplicationTransferRequest, docs transferDocuments) (json.RawMessage, error) {
	document, err := m.dispatch(ctx, name, node, request, nil)
	if err == nil {
		return document, nil
	}
	var denial struct {
		ReasonCode         string                   `json:"reasonCode"`
		Status             string                   `json:"status"`
		TransferRetirement *wire.TransferRetirement `json:"transferRetirement"`
	}
	if json.Unmarshal(document, &denial) != nil || denial.Status != "awaiting-capability" || denial.TransferRetirement == nil {
		return document, err
	}
	scope := *denial.TransferRetirement
	operation := docs.receiptOperation()
	if role == "destination" {
		operation = "transfer-abort." + strings.TrimPrefix(docs.transferID, "sha256:")
	}
	if scope.TransferID != docs.transferID || scope.Role != role || scope.OperationID != operation {
		return nil, fmt.Errorf("the node asked to retire another transfer")
	}
	if role == "source" {
		digest := sha256.Sum256(docs.outcome)
		if scope.OutcomeDigest != "sha256:"+hex.EncodeToString(digest[:]) {
			return nil, fmt.Errorf("the source retirement names another destination outcome")
		}
	}
	capability, err := m.issueRetirement(ctx, node, scope)
	if err != nil {
		return nil, err
	}
	return m.dispatch(ctx, name+"-granted", node, request, capability)
}

// abort resolves a failure after the export: the destination signs that it
// holds the transfer stopped (retiring any receiving runtime under its own
// grant), then the source resumes. Without that signed stop the source stays
// held with its data.
func (m *nativeRelocation) abort(ctx context.Context, docs transferDocuments, cause error) error {
	m.q.UpdateProgress(m.job.ID, 90, "Stopping the move; the source keeps its data")
	request := m.receivedRequest(docs)
	request.Step = stackkitcommand.TransferStepAbort
	stopped, err := m.retire(ctx, m.req.Destination, "abort", "destination", request, docs)
	if err == nil {
		docs.outcome = stopped
		_, err = docs.verifyOutcome("stopped")
	}
	if err != nil {
		m.outcome["state"], m.outcome["source_retained"] = "destination_verification_required", true
		return fmt.Errorf("%w; the destination stop is unconfirmed, the source stays stopped with its data: %v", cause, err)
	}
	resumed, err := m.dispatch(ctx, "resume", m.req.Source, stackkitcommand.ApplicationTransferRequest{Step: stackkitcommand.TransferStepResume, SourceReceipt: docs.receipt, TargetOutcome: stopped, DestinationFingerprint: docs.destFP}, nil)
	if err != nil {
		m.outcome["state"], m.outcome["source_retained"] = "source_resume_required", true
		m.outcome["destination_outcome"] = string(stopped)
		return fmt.Errorf("%w; the destination stopped the transfer but the source did not resume yet; its data is kept: %v", cause, err)
	}
	var source struct {
		SourceState string `json:"sourceState"`
	}
	_ = json.Unmarshal(resumed, &source)
	m.outcome["state"], m.outcome["source_retained"], m.outcome["source_state"] = "aborted", true, source.SourceState
	m.outcome["destination_outcome"] = string(stopped)
	return fmt.Errorf("%w; the move was stopped and the source runs again with its data", cause)
}

// withdraw returns an unused destination admission and the intent it
// replaced. It never touches a consumed admission.
func (m *nativeRelocation) withdraw(ctx context.Context, nonce string) error {
	if nonce == "" {
		return nil
	}
	_, err := m.dispatch(ctx, "withdraw", m.req.Destination, stackkitcommand.ApplicationTransferRequest{Step: stackkitcommand.TransferStepWithdraw, Nonce: nonce}, nil)
	if err != nil {
		return fmt.Errorf("withdraw the destination admission: %w", err)
	}
	return nil
}

// readReceipt admits the source's signed export receipt: its Owner must be
// the source's trust-bound Owner and it names the transfer and ciphertext.
// The source signing fingerprint comes from that receipt, received over the
// source node's authenticated channel.
func (docs *transferDocuments) readReceipt(sourceOwner string) error {
	var receipt struct {
		APIVersion string `json:"apiVersion"`
		Export     struct {
			ArchiveSHA256 string `json:"archiveSHA256"`
			Manifest      struct {
				ApplicationTransfer *struct {
					ID                    string                                                              `json:"id"`
					Mode                  string                                                              `json:"mode"`
					PortableConfiguration map[string]stackkitcommand.ApplicationTransferWorkloadConfiguration `json:"portableConfiguration"`
					Runtimes              []struct {
						WorkloadRef string `json:"workloadRef"`
					} `json:"runtimes"`
				} `json:"applicationTransfer"`
			} `json:"manifest"`
		} `json:"export"`
		SourceOperationID string `json:"sourceOperationId"`
		State             string `json:"state"`
		Signature         struct {
			OwnerRef  string `json:"ownerRef"`
			PublicKey string `json:"publicKey"`
		} `json:"signature"`
	}
	if json.Unmarshal(docs.receipt, &receipt) != nil || receipt.APIVersion != "stackkit.application-transfer-export-receipt/v1" ||
		receipt.Export.Manifest.ApplicationTransfer == nil || receipt.Export.Manifest.ApplicationTransfer.Mode != "migrate" ||
		receipt.Signature.OwnerRef != sourceOwner || receipt.SourceOperationID != docs.operation || len(receipt.Export.ArchiveSHA256) != 64 {
		return fmt.Errorf("the source export receipt differs from the admitted source custody")
	}
	if !reflect.DeepEqual(receipt.Export.Manifest.ApplicationTransfer.PortableConfiguration, docs.portableConfiguration) {
		return fmt.Errorf("the source export carries another portable application configuration than the receiving candidate admitted")
	}
	public, err := base64.RawStdEncoding.DecodeString(receipt.Signature.PublicKey)
	if err != nil || len(public) != 32 {
		return fmt.Errorf("the source export receipt has no signing key")
	}
	fingerprint := sha256.Sum256(public)
	docs.sourceFP = "sha256:" + hex.EncodeToString(fingerprint[:])
	docs.transferID = receipt.Export.Manifest.ApplicationTransfer.ID
	docs.workloads = docs.workloads[:0]
	for _, runtime := range receipt.Export.Manifest.ApplicationTransfer.Runtimes {
		docs.workloads = append(docs.workloads, runtime.WorkloadRef)
	}
	docs.archive = "sha256:" + receipt.Export.ArchiveSHA256
	return nil
}

func (docs transferDocuments) receiptOperation() string { return docs.operation }

// addedTransferWorkloads names the workloads the receiving candidate adds to
// the destination's previous intent.
func addedTransferWorkloads(previous, candidate json.RawMessage) ([]string, error) {
	var before, after struct {
		Workloads map[string]json.RawMessage `json:"workloads"`
	}
	if json.Unmarshal(previous, &before) != nil || json.Unmarshal(candidate, &after) != nil {
		return nil, fmt.Errorf("the receiving candidate lacks its previous and new intent")
	}
	added := []string{}
	for workload := range after.Workloads {
		if _, ok := before.Workloads[workload]; !ok {
			added = append(added, workload)
		}
	}
	if len(added) == 0 || len(after.Workloads) != len(before.Workloads)+len(added) {
		return nil, fmt.Errorf("the receiving candidate must only add applications")
	}
	slices.Sort(added)
	return added, nil
}

func sameWorkloads(a, b []string) bool {
	left, right := slices.Clone(a), slices.Clone(b)
	slices.Sort(left)
	slices.Sort(right)
	return slices.Equal(left, right)
}

// verifyOutcome admits the destination's signed outcome of this transfer.
func (docs transferDocuments) verifyOutcome(state string) (map[string]interface{}, error) {
	var outcome struct {
		APIVersion                  string `json:"apiVersion"`
		TransferID                  string `json:"transferId"`
		State                       string `json:"state"`
		ActivationID                string `json:"activationId"`
		ReadbackDigest              string `json:"readbackDigest"`
		PortableConfigurationDigest string `json:"portableConfigurationDigest"`
		Destination                 struct {
			SigningFingerprint string `json:"signingFingerprint"`
			PlanHash           string `json:"planHash"`
		} `json:"destination"`
	}
	if json.Unmarshal(docs.outcome, &outcome) != nil || outcome.APIVersion != "stackkit.application-transfer-outcome/v1" || outcome.TransferID != docs.transferID ||
		outcome.State != state || outcome.Destination.SigningFingerprint != docs.destFP || outcome.Destination.PlanHash != docs.plan {
		return nil, fmt.Errorf("the destination outcome differs from this transfer")
	}
	if state == "activated" && (outcome.ActivationID == "" || !strings.HasPrefix(outcome.ReadbackDigest, "sha256:")) {
		return nil, fmt.Errorf("the destination activation lacks its data readback")
	}
	if state == "activated" && outcome.PortableConfigurationDigest != docs.portableConfigurationDigest {
		return nil, fmt.Errorf("the destination activation did not verify the source portable application configuration")
	}
	return map[string]interface{}{"activation_id": outcome.ActivationID, "readback_digest": outcome.ReadbackDigest, "state": outcome.State,
		"portable_configuration_digest": outcome.PortableConfigurationDigest}, nil
}
