package rilaction

import "fmt"

// StepReceiptAPIVersionV1 identifies Techstack's typed step receipt of one
// governed action. It wraps the unchanged stackkit.ril-action-evidence/v1
// record and adds what that wire cannot carry: the AI Platform approval it ran
// under and the count of executions the durable card audit recorded.
const StepReceiptAPIVersionV1 = "techstack.ril-action-step-receipt/v1"

// Step identifiers of a StepReceipt, in their fixed order.
const (
	StepExecution    = "execution"
	StepVerification = "verification"
)

// StepReceipt is the persisted, public-safe proof of one approved action: one
// execution step and one verification-readback step, cross-linked to the
// approval receipt and Gateway decision. It is derived only from persisted
// rows (card, approval, evidence, append-only transition audit).
type StepReceipt struct {
	APIVersion   string              `json:"api_version"`
	ActionCardID string              `json:"action_card_id"`
	ExecutionID  string              `json:"execution_id"`
	TraceID      string              `json:"trace_id"`
	Approval     StepReceiptApproval `json:"approval"`
	Execution    ExecutionStep       `json:"execution"`
	Verification VerificationStep    `json:"verification"`
	Evidence     Evidence            `json:"evidence"`
}

// StepReceiptApproval links the receipt to the AI Platform approval. ReceiptRef
// is "approval:<receipt id>"; DecisionID is the Gateway Decision it answered.
type StepReceiptApproval struct {
	ReceiptRef  string        `json:"receipt_ref"`
	ReceiptHash string        `json:"receipt_hash"`
	DecisionID  string        `json:"decision_id"`
	Class       ApprovalClass `json:"class"`
	ApprovedAt  string        `json:"approved_at"`
}

// ExecutionStep proves exactly-one execution: Executions counts the card's
// durable transitions into "executing" and Completions its terminal
// transitions. A replayed approval returns the persisted evidence without a
// new transition, so both stay at one.
type ExecutionStep struct {
	Step          string `json:"step"`
	Status        string `json:"status"`
	Executions    int64  `json:"executions"`
	Completions   int64  `json:"completions"`
	ExactlyOnce   bool   `json:"exactly_once"`
	RequestDigest string `json:"request_digest"`
	EvidenceID    string `json:"evidence_id"`
	ExecutorRef   string `json:"executor_ref"`
	TargetRef     string `json:"target_ref"`
	StartedAt     string `json:"started_at,omitempty"`
	CompletedAt   string `json:"completed_at,omitempty"`
}

// VerificationStep is the verification readback of the same execution, taken
// from the evidence's closed checks.
type VerificationStep struct {
	Step                 string              `json:"step"`
	Kind                 string              `json:"kind"`
	Status               string              `json:"status"`
	RuntimeStateObserved bool                `json:"runtime_state_observed"`
	Checks               []VerificationCheck `json:"checks"`
	EvaluatedAt          string              `json:"evaluated_at"`
}

// StepReceiptInput is the persisted state one receipt is built from.
type StepReceiptInput struct {
	ActionCardID string
	ExecutionID  string
	TraceID      string
	Approval     StepReceiptApproval
	Evidence     Evidence
	Executions   int64
	Completions  int64
	StartedAt    string
	CompletedAt  string
}

// NewStepReceipt builds the typed receipt and refuses one whose evidence does
// not belong to the card's execution or that lacks the approval cross-link.
func NewStepReceipt(in StepReceiptInput) (StepReceipt, error) {
	evidence := in.Evidence
	if evidence.APIVersion != EvidenceAPIVersionV1 {
		return StepReceipt{}, invalid("step_receipt.evidence.api_version", "is unsupported")
	}
	if in.ActionCardID == "" || evidence.ActionCardID != in.ActionCardID ||
		in.ExecutionID == "" || evidence.ExecutionID != in.ExecutionID || evidence.TraceID != in.TraceID {
		return StepReceipt{}, invalid("step_receipt.evidence", "does not belong to the card execution")
	}
	if in.Approval.ReceiptRef == "" || in.Approval.ReceiptHash == "" || in.Approval.DecisionID == "" {
		return StepReceipt{}, invalid("step_receipt.approval", "requires the approval receipt and decision id")
	}
	if in.Executions < 0 || in.Completions < 0 {
		return StepReceipt{}, fmt.Errorf("rilaction step receipt counts are invalid")
	}
	return StepReceipt{
		APIVersion:   StepReceiptAPIVersionV1,
		ActionCardID: in.ActionCardID,
		ExecutionID:  in.ExecutionID,
		TraceID:      in.TraceID,
		Approval:     in.Approval,
		Execution: ExecutionStep{
			Step: StepExecution, Status: evidence.Status,
			Executions: in.Executions, Completions: in.Completions,
			ExactlyOnce:   in.Executions == 1 && in.Completions == 1,
			RequestDigest: evidence.RequestDigest, EvidenceID: evidence.EvidenceID,
			ExecutorRef: evidence.ExecutorRef, TargetRef: evidence.TargetRef,
			StartedAt: in.StartedAt, CompletedAt: in.CompletedAt,
		},
		Verification: VerificationStep{
			Step: StepVerification, Kind: evidence.Verification.Kind,
			Status:               evidence.Verification.Status,
			RuntimeStateObserved: evidence.Verification.RuntimeStateObserved,
			Checks:               append([]VerificationCheck(nil), evidence.Verification.Checks...),
			EvaluatedAt:          evidence.EvaluatedAt,
		},
		Evidence: evidence,
	}, nil
}
