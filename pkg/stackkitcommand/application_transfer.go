package stackkitcommand

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strconv"

	"github.com/kombifyio/homelab/pkg/api/agentpb"
)

// Native application transfer (STACKKIT_OPERATION_APPLICATION_TRANSFER) moves
// one installed use case or individual applications between two managed
// nodes of one Owner through the pinned StackKits `backup transfer-*`
// commands. Every step acts only on the node it runs on. Core carries the
// signed public documents between the nodes (destination admission, source
// export receipt, destination outcome) and never a secret: the archive is
// age-encrypted to the receiving node's own identity and travels node to node.
const (
	ApplicationTransferRequestVersion = "homelab.application-transfer-step/v1"
	// MaxApplicationTransferRequestBytes bounds the step request (documents
	// included).
	MaxApplicationTransferRequestBytes = 1 << 20
	// ApplicationTransferPort is the one-shot share port on the source.
	ApplicationTransferPort         = 8124
	maxApplicationTransferWorkloads = 32
)

// Steps of a native application transfer.
const (
	TransferStepConfiguration = "configuration" // source: project the portable public configuration a receiving candidate is authored from
	TransferStepCandidate     = "candidate"     // destination: author the receiving candidate (transfer-candidate)
	TransferStepTarget        = "target"        // destination: persist and generate the candidate, issue the admission (receive admit)
	TransferStepExport        = "export"        // source: export the selection to the admitted destination and hold it
	TransferStepServe         = "serve"         // source: serve the exact ciphertext once to the destination address
	TransferStepFetch         = "fetch"         // destination: download and keep the ciphertext only when its digest matches
	TransferStepPrepare       = "prepare"       // destination: adopt the transferred credentials (receive prepare)
	TransferStepImport        = "import"        // destination: activate the data and sign the readback outcome (receive import)
	TransferStepRecover       = "recover"       // destination: recover an uncertain import through its original journal
	TransferStepFinish        = "finish"        // source: retire the held selection after the verified destination outcome
	TransferStepAbort         = "abort"         // destination: hold the receiving application stopped and sign the stopped outcome
	TransferStepResume        = "resume"        // source: resume the held selection after the verified stopped outcome
	TransferStepWithdraw      = "withdraw"      // destination: withdraw an unused admission and restore the replaced intent
)

var (
	transferWorkloadPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	transferOwnerPattern    = regexp.MustCompile(`^owner/local/[0-9a-f]{32}$`)
	transferOperationID     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{7,99}$`)
	transferNoncePattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ApplicationTransferRequest is one closed step request. Documents are the
// exact signed JSON a node produced in an earlier step.
type ApplicationTransferRequest struct {
	APIVersion                  string          `json:"apiVersion"`
	Step                        string          `json:"step"`
	UseCase                     string          `json:"useCase,omitempty"`
	Workloads                   []string        `json:"workloads,omitempty"`
	OperationID                 string          `json:"operationId,omitempty"`
	SourceOwner                 string          `json:"sourceOwner,omitempty"`
	SourceFingerprint           string          `json:"sourceFingerprint,omitempty"`
	DestinationOwner            string          `json:"destinationOwner,omitempty"`
	DestinationFingerprint      string          `json:"destinationFingerprint,omitempty"`
	TransferID                  string          `json:"transferId,omitempty"`
	ArchiveSHA256               string          `json:"archiveSha256,omitempty"`
	ExpectedPlanHash            string          `json:"expectedPlanHash,omitempty"`
	ExpectedSpecHash            string          `json:"expectedSpecHash,omitempty"`
	Peer                        string          `json:"peer,omitempty"`
	Nonce                       string          `json:"nonce,omitempty"`
	CandidateSpec               json.RawMessage `json:"candidateSpec,omitempty"`
	PortableConfiguration       json.RawMessage `json:"portableConfiguration,omitempty"`
	PortableConfigurationDigest string          `json:"portableConfigurationDigest,omitempty"`
	DestinationAdmission        json.RawMessage `json:"destinationAdmission,omitempty"`
	SourceReceipt               json.RawMessage `json:"sourceReceipt,omitempty"`
	TargetOutcome               json.RawMessage `json:"targetOutcome,omitempty"`
}

// DecodeApplicationTransferRequest strictly decodes and validates one step.
func DecodeApplicationTransferRequest(raw []byte) (ApplicationTransferRequest, error) {
	var request ApplicationTransferRequest
	if len(raw) == 0 || len(raw) > MaxApplicationTransferRequestBytes {
		return request, fmt.Errorf("application transfer request is not bounded")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, fmt.Errorf("application transfer request is invalid: %w", err)
	}
	if decoder.More() {
		return request, fmt.Errorf("application transfer request has trailing data")
	}
	return request, request.Validate()
}

// Encode returns the canonical request bytes.
func (r ApplicationTransferRequest) Encode() ([]byte, error) {
	r.APIVersion = ApplicationTransferRequestVersion
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(r)
}

// ApplicationTransferConfiguration is the source-side portable public
// configuration document a receiving candidate is authored from. It never
// carries secret material, placement, routes or Foundation identities.
type ApplicationTransferConfiguration struct {
	APIVersion                  string                                              `json:"apiVersion"`
	Workloads                   []string                                            `json:"workloads"`
	Configuration               map[string]ApplicationTransferWorkloadConfiguration `json:"configuration"`
	PortableConfigurationDigest string                                              `json:"portableConfigurationDigest"`
}

type ApplicationTransferWorkloadConfiguration struct {
	AlternativeRef string         `json:"alternativeRef"`
	ModuleRef      string         `json:"moduleRef"`
	Settings       map[string]any `json:"settings"`
	Profiles       map[string]any `json:"profiles"`
}

// DecodeApplicationTransferConfiguration admits one source configuration
// document: one alternative per named workload and the canonical digest the
// destination admission and the activated outcome carry. Core carries the
// digest untouched: the destination's transfer-candidate recomputes it from the
// document, the destination admission recomputes it from the generated plan and
// the source export recomputes it from its own projection, each refusing a
// mismatch.
func DecodeApplicationTransferConfiguration(raw json.RawMessage) (ApplicationTransferConfiguration, error) {
	var document ApplicationTransferConfiguration
	if len(raw) == 0 || len(raw) > MaxApplicationTransferRequestBytes {
		return document, fmt.Errorf("portable application configuration is not bounded")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("portable application configuration is invalid: %w", err)
	}
	if decoder.More() {
		return document, fmt.Errorf("portable application configuration has trailing data")
	}
	if document.APIVersion != "stackkit.application-transfer-configuration/v1" || len(document.Configuration) == 0 || !specHashPattern.MatchString(document.PortableConfigurationDigest) {
		return document, fmt.Errorf("portable application configuration is incomplete")
	}
	if len(document.Workloads) != len(document.Configuration) {
		return document, fmt.Errorf("portable application configuration names another selection")
	}
	seen := map[string]bool{}
	for _, workload := range document.Workloads {
		configuration, ok := document.Configuration[workload]
		if !ok || seen[workload] || !transferWorkloadPattern.MatchString(workload) || configuration.AlternativeRef == "" {
			return document, fmt.Errorf("portable application configuration is invalid")
		}
		seen[workload] = true
	}
	return document, nil
}

// CapabilityStep names the StackKits receive step a capability must name,
// or "" when the step takes no receive capability.
func (r ApplicationTransferRequest) CapabilityStep() string {
	switch r.Step {
	case TransferStepTarget:
		return "admit"
	case TransferStepPrepare:
		return "prepare"
	case TransferStepImport, TransferStepRecover:
		return "import"
	default:
		return ""
	}
}

// RetirementStep reports the steps that may carry an application-transfer.retire capability.
func (r ApplicationTransferRequest) RetirementStep() bool {
	return r.Step == TransferStepFinish || r.Step == TransferStepAbort
}

// Mutating reports whether the step needs explicit Owner approval.
func (r ApplicationTransferRequest) Mutating() bool {
	return r.Step != TransferStepConfiguration && r.Step != TransferStepCandidate
}

// Validate checks the step's required and forbidden inputs.
func (r ApplicationTransferRequest) Validate() error {
	if r.APIVersion != ApplicationTransferRequestVersion {
		return fmt.Errorf("application transfer request version is unsupported")
	}
	need := map[string]bool{}
	switch r.Step {
	case TransferStepConfiguration, TransferStepCandidate:
		need["useCase"] = true
		if r.Step == TransferStepCandidate {
			need["portableConfiguration"] = true
		}
	case TransferStepTarget:
		need["candidateSpec"], need["sourceOwner"], need["expectedSpecHash"], need["portableConfigurationDigest"] = true, true, true, true
	case TransferStepExport:
		need["destinationAdmission"], need["destinationOwner"], need["destinationFingerprint"], need["operationId"], need["expectedPlanHash"] = true, true, true, true, true
	case TransferStepServe:
		need["operationId"], need["peer"] = true, true
	case TransferStepFetch:
		need["peer"], need["archiveSha256"] = true, true
	case TransferStepPrepare, TransferStepImport, TransferStepRecover, TransferStepAbort:
		need["sourceReceipt"], need["sourceOwner"], need["sourceFingerprint"], need["transferId"], need["archiveSha256"], need["expectedPlanHash"] = true, true, true, true, true, true
	case TransferStepFinish, TransferStepResume:
		need["sourceReceipt"], need["targetOutcome"], need["destinationFingerprint"] = true, true, true
	case TransferStepWithdraw:
		need["nonce"] = true
	default:
		return fmt.Errorf("application transfer step %q is unsupported", r.Step)
	}
	present := map[string]bool{
		"useCase": r.UseCase != "" || len(r.Workloads) != 0, "candidateSpec": len(r.CandidateSpec) != 0, "sourceOwner": r.SourceOwner != "",
		"sourceFingerprint": r.SourceFingerprint != "", "destinationOwner": r.DestinationOwner != "", "destinationFingerprint": r.DestinationFingerprint != "",
		"transferId": r.TransferID != "", "archiveSha256": r.ArchiveSHA256 != "", "expectedPlanHash": r.ExpectedPlanHash != "", "peer": r.Peer != "",
		"nonce": r.Nonce != "", "destinationAdmission": len(r.DestinationAdmission) != 0, "sourceReceipt": len(r.SourceReceipt) != 0,
		"targetOutcome": len(r.TargetOutcome) != 0, "operationId": r.OperationID != "", "expectedSpecHash": r.ExpectedSpecHash != "",
		"portableConfiguration": len(r.PortableConfiguration) != 0, "portableConfigurationDigest": r.PortableConfigurationDigest != "",
	}
	// The export also names the selection it moves.
	if r.Step == TransferStepExport {
		need["useCase"] = true
	}
	for field, ok := range present {
		if ok && !need[field] {
			return fmt.Errorf("application transfer step %s does not take %s", r.Step, field)
		}
		if !ok && need[field] {
			return fmt.Errorf("application transfer step %s requires %s", r.Step, field)
		}
	}
	if err := ValidateApplicationTransferSelection(r.UseCase, r.Workloads); need["useCase"] && err != nil {
		return err
	}
	if r.Step == TransferStepConfiguration || r.Step == TransferStepCandidate {
		if len(r.PortableConfiguration) != 0 && !json.Valid(r.PortableConfiguration) {
			return fmt.Errorf("portable application configuration is not JSON")
		}
		return nil
	}
	for _, owner := range []string{r.SourceOwner, r.DestinationOwner} {
		if owner != "" && !transferOwnerPattern.MatchString(owner) {
			return fmt.Errorf("application transfer Owner reference is invalid")
		}
	}
	for _, digest := range []string{r.SourceFingerprint, r.DestinationFingerprint, r.TransferID, r.ArchiveSHA256, r.ExpectedPlanHash, r.ExpectedSpecHash, r.PortableConfigurationDigest} {
		if digest != "" && !specHashPattern.MatchString(digest) {
			return fmt.Errorf("application transfer digest is invalid")
		}
	}
	if r.OperationID != "" && !transferOperationID.MatchString(r.OperationID) {
		return fmt.Errorf("application transfer operation id is invalid")
	}
	if r.Nonce != "" && !transferNoncePattern.MatchString(r.Nonce) {
		return fmt.Errorf("application transfer admission nonce is invalid")
	}
	if r.Peer != "" {
		if _, err := ApplicationTransferPeer(r.Peer); err != nil {
			return err
		}
	}
	for _, document := range []json.RawMessage{r.CandidateSpec, r.DestinationAdmission, r.SourceReceipt, r.TargetOutcome} {
		if len(document) != 0 && !json.Valid(document) {
			return fmt.Errorf("application transfer document is not JSON")
		}
	}
	return nil
}

// ValidateApplicationTransferSelection admits either one catalog use case or
// individual installed applications (workload refs), never both.
func ValidateApplicationTransferSelection(useCase string, workloads []string) error {
	if (useCase == "") == (len(workloads) == 0) {
		return fmt.Errorf("select either one use case or individual applications")
	}
	if useCase != "" && !transferWorkloadPattern.MatchString(useCase) {
		return fmt.Errorf("use case name is invalid")
	}
	if len(workloads) > maxApplicationTransferWorkloads {
		return fmt.Errorf("too many applications in one move")
	}
	seen := map[string]bool{}
	for _, workload := range workloads {
		if !transferWorkloadPattern.MatchString(workload) || seen[workload] {
			return fmt.Errorf("application %q is invalid or repeated", workload)
		}
		seen[workload] = true
	}
	return nil
}

// ApplicationTransferPeer admits one exact IP address (the other node), with
// an optional port only for fetch.
func ApplicationTransferPeer(peer string) (string, error) {
	host, port, err := net.SplitHostPort(peer)
	if err != nil {
		host, port = peer, ""
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLoopback() {
		return "", fmt.Errorf("the transfer peer must be one exact IP address")
	}
	if port != "" {
		if value, convErr := strconv.Atoi(port); convErr != nil || value < 1024 || value > 65535 {
			return "", fmt.Errorf("the transfer peer port is invalid")
		}
	}
	return ip.String(), nil
}

// ValidateApplicationTransferFields admits the typed request only on its
// operation and the command fields the operation may carry.
func ValidateApplicationTransferFields(c *agentpb.StackKitCommand) error {
	if c == nil {
		return fmt.Errorf("application transfer command is required")
	}
	if c.Operation != agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_TRANSFER {
		if len(c.ApplicationTransferJson) != 0 {
			return fmt.Errorf("application transfer request belongs only to its typed operation")
		}
		return nil
	}
	if c.StackkitInstanceId == "" || c.SpecPath == "" || len(c.CandidateSpecJson) != 0 || len(c.InventoryJson) != 0 || c.DryRun || c.Offline || c.TargetRelease != "" || c.ExpectedPlanHash != "" {
		return fmt.Errorf("application transfer runs on its exact instance without generic candidate, Inventory or plan inputs")
	}
	request, err := DecodeApplicationTransferRequest(c.ApplicationTransferJson)
	if err != nil {
		return err
	}
	if request.Mutating() != c.OwnerApproved {
		return fmt.Errorf("application transfer step %s requires Owner approval exactly when it changes the node", request.Step)
	}
	capability := len(c.AdvancedCapability) != 0
	if len(c.AdvancedCapability) > MaxAdvancedCapabilityBytes {
		return fmt.Errorf("application transfer capability exceeds %d bytes", MaxAdvancedCapabilityBytes)
	}
	switch {
	case request.CapabilityStep() != "" && !capability:
		return fmt.Errorf("application transfer step %s requires its receive capability", request.Step)
	case request.CapabilityStep() == "" && !request.RetirementStep() && capability:
		return fmt.Errorf("application transfer step %s takes no capability", request.Step)
	}
	return nil
}

// ApplicationTransferEvidence is the normalized result of one step: the JSON
// document the pinned CLI produced (a signed admission, receipt or outcome,
// a transport report, an authored candidate or a denial naming the exact
// retirement scope it awaits).
type ApplicationTransferEvidence struct {
	Step     string          `json:"step"`
	Document json.RawMessage `json:"document,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// ParseApplicationTransferEvidence reads the Guard envelope of one step.
// A failed step still returns its document when the CLI wrote one.
func ParseApplicationTransferEvidence(command *agentpb.StackKitCommand, result *agentpb.StackKitResult) (ApplicationTransferEvidence, error) {
	var evidence ApplicationTransferEvidence
	if command == nil || result == nil || command.Operation != agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_TRANSFER {
		return evidence, fmt.Errorf("application transfer evidence requires its command and result")
	}
	request, err := DecodeApplicationTransferRequest(command.ApplicationTransferJson)
	if err != nil {
		return evidence, err
	}
	var envelope struct {
		Command string                      `json:"command"`
		Data    ApplicationTransferEvidence `json:"data"`
	}
	if err := json.Unmarshal(result.CommandResultJson, &envelope); err != nil || envelope.Command != ResultCommandName(command.Operation) {
		return evidence, fmt.Errorf("application transfer result envelope is invalid")
	}
	evidence = envelope.Data
	if evidence.Step != request.Step {
		return evidence, fmt.Errorf("application transfer result names another step")
	}
	if len(evidence.Document) != 0 && !json.Valid(evidence.Document) {
		return evidence, fmt.Errorf("application transfer document is not JSON")
	}
	if result.Success && len(evidence.Document) == 0 {
		return evidence, fmt.Errorf("application transfer step %s returned no document", request.Step)
	}
	return evidence, nil
}
