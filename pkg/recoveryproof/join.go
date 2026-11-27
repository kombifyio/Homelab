package recoveryproof

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// NotClaimed is the closed set of recovery bounds every joined receipt carries
// alongside the Guard G not-claimed bounds. It is part of the hashed body, so a
// reader cannot drop a bound without breaking the digest.
var NotClaimed = []string{
	"No claim beyond the named node, workload, target and scope.",
	"No claim for another operating system profile, application or PostgreSQL major version.",
	"No claim that the backup stays restorable after the evidence age; a later loss is not excluded.",
	"No claim of fenced multi-site recovery or failback (Guard F).",
	"No claim of cold start without WAN or control-plane access (Guard H).",
	"No claim about production notification delivery; the clone was only proven unable to send it.",
	"No claim about data written after the newest recovered transaction.",
}

// JoinBinding supplies the Homelab custody link between a Guard A application
// recovery receipt and a Guard G backup-survival receipt when the Guard G
// receipt is raw (`stackkit.guard-backup-survival-receipt/v1`) and carries
// `backendRef` + `bindingDigest` without node/workload fields.
type JoinBinding struct {
	NodeID           string `json:"node_id"`
	WorkloadID       string `json:"workload_id"`
	TargetID         string `json:"target_id"`
	BindingDigest    string `json:"binding_digest,omitempty"`
	SnapshotAnchorID string `json:"snapshot_anchor_id,omitempty"`
}

// Qualification states for a Report.
const (
	StateJoined     = "joined"
	StateSourceOnly = "source_only"
	StateUnjoined   = "unjoined"
)

// Report is the outcome of one evaluation. Recovery and Survival are separate
// verdicts: Survival never changes Recovery. Joined is set only when both
// proofs are verified and agree on node, workload and target; JoinGaps explains
// why it is not otherwise. State explicitly distinguishes joined vs source_only
// vs unjoined.
type Report struct {
	State    string         `json:"state"`
	Recovery Claim          `json:"recovery"`
	Survival Claim          `json:"survival"`
	Joined   *JoinedReceipt `json:"joined,omitempty"`
	JoinGaps []string       `json:"join_gaps,omitempty"`
}

// ReportState derives the explicit qualification state from recovery, survival,
// and joined evidence.
func ReportState(recovery, survival Claim, joined *JoinedReceipt) string {
	if joined != nil {
		return StateJoined
	}
	if recovery.Status == StatusVerified && survival.Status == StatusAbsent {
		return StateSourceOnly
	}
	return StateUnjoined
}

// JoinedBody is the hash-chained content of a joined qualification.
type JoinedBody struct {
	Schema      string    `json:"schema"`
	EvaluatedAt time.Time `json:"evaluated_at"`
	Subject     Subject   `json:"subject"`
	Recovery    Claim     `json:"recovery"`
	Survival    Claim     `json:"survival"`
	Unknowns    []string  `json:"unknowns"`
	NotClaimed  []string  `json:"not_claimed"`
}

// JoinedReceipt chains the two input digests and the body into one digest.
type JoinedReceipt struct {
	Body           JoinedBody `json:"body"`
	RecoveryDigest string     `json:"recovery_digest"`
	SurvivalDigest string     `json:"survival_digest"`
	Digest         string     `json:"digest"`
}

// Evaluate projects both proofs separately and joins them when, and only when,
// both are verified and bound to the same node, workload and target. An error
// means the policy is unusable; a bad receipt is a status, not an error.
func Evaluate(recoveryRaw, survivalRaw []byte, now time.Time, policy Policy) (Report, error) {
	return EvaluateBound(recoveryRaw, survivalRaw, JoinBinding{}, now, policy)
}

// EvaluateBound projects both proofs separately and joins them using an
// explicit Homelab custody JoinBinding linking (node_id, workload_id, target_id).
func EvaluateBound(recoveryRaw, survivalRaw []byte, binding JoinBinding, now time.Time, policy Policy) (Report, error) {
	recovery, err := ProjectRecovery(recoveryRaw, now, policy)
	if err != nil {
		return Report{}, err
	}
	survival, err := ProjectSurvival(survivalRaw, now, policy)
	if err != nil {
		return Report{}, err
	}
	report := Report{
		State:    StateUnjoined,
		Recovery: recovery,
		Survival: survival,
	}
	if recovery.Status != StatusVerified {
		report.JoinGaps = append(report.JoinGaps, fmt.Sprintf("recovery proof is %s", recovery.Status))
	}
	if survival.Status != StatusVerified {
		report.JoinGaps = append(report.JoinGaps, fmt.Sprintf("survival proof is %s", survival.Status))
	}
	if len(report.JoinGaps) > 0 {
		report.State = ReportState(recovery, survival, nil)
		return report, nil
	}
	gaps, joinedSubject := identityGaps(recovery, survival, binding)
	if len(gaps) > 0 {
		report.JoinGaps = gaps
		report.State = ReportState(recovery, survival, nil)
		return report, nil
	}
	body := JoinedBody{
		Schema:      JoinedSchema,
		EvaluatedAt: now.UTC(),
		Subject:     joinedSubject,
		Recovery:    recovery,
		Survival:    survival,
		Unknowns:    joinUnknowns(recovery, survival),
		NotClaimed:  joinNotClaimed(survival),
	}
	sum, err := chainDigest(recovery.Digest, survival.Digest, body)
	if err != nil {
		return Report{}, err
	}
	report.Joined = &JoinedReceipt{
		Body:           body,
		RecoveryDigest: recovery.Digest,
		SurvivalDigest: survival.Digest,
		Digest:         sum,
	}
	report.State = ReportState(recovery, survival, report.Joined)
	return report, nil
}

func identityGaps(recovery, survival Claim, binding JoinBinding) ([]string, Subject) {
	var gaps []string
	r := recovery.Subject
	s := survival.Subject
	if r == nil || s == nil {
		return []string{"subject metadata is missing from one or both proofs"}, Subject{}
	}

	resolvedNode := r.NodeID
	resolvedWorkload := r.WorkloadID
	resolvedTarget := s.TargetID

	hasBinding := binding.NodeID != "" || binding.WorkloadID != "" || binding.TargetID != "" || binding.BindingDigest != "" || binding.SnapshotAnchorID != ""
	if hasBinding {
		if !identifier(binding.NodeID) || !identifier(binding.WorkloadID) || !identifier(binding.TargetID) {
			gaps = append(gaps, "join binding lacks complete node_id, workload_id and target_id")
		} else {
			if r.NodeID != binding.NodeID {
				gaps = append(gaps, "recovery node identity does not match binding")
			}
			if r.WorkloadID != binding.WorkloadID {
				gaps = append(gaps, "recovery workload identity does not match binding")
			}
			if s.TargetID != binding.TargetID {
				gaps = append(gaps, "survival backup target identity does not match binding")
			}
			if (s.NodeID != "" || s.WorkloadID != "") && s.NodeID != binding.NodeID {
				gaps = append(gaps, "survival node identity does not match binding")
			}
			if (s.NodeID != "" || s.WorkloadID != "") && s.WorkloadID != binding.WorkloadID {
				gaps = append(gaps, "survival workload identity does not match binding")
			}
			if binding.BindingDigest != "" && survival.BindingDigest != binding.BindingDigest {
				gaps = append(gaps, "survival bindingDigest does not match binding")
			}
			if binding.SnapshotAnchorID != "" && recovery.SnapshotAnchorID != binding.SnapshotAnchorID {
				gaps = append(gaps, "recovery snapshotAnchorId does not match binding")
			}
		}
	} else {
		if s.NodeID == "" || s.WorkloadID == "" {
			gaps = append(gaps, "survival proof lacks node and workload identity binding")
		} else {
			if r.NodeID != s.NodeID {
				gaps = append(gaps, "node identity differs between the proofs")
			}
			if r.WorkloadID != s.WorkloadID {
				gaps = append(gaps, "workload identity differs between the proofs")
			}
		}
		if r.TargetID != "" && s.TargetID != "" && r.TargetID != s.TargetID {
			gaps = append(gaps, "backup target identity differs between the proofs")
		}
	}

	if resolvedNode == "" || resolvedWorkload == "" || resolvedTarget == "" {
		gaps = append(gaps, "joined subject requires node, workload and backup target identity")
	}
	if recovery.SnapshotAnchorID != "" && survival.SnapshotAnchorID != "" && recovery.SnapshotAnchorID != survival.SnapshotAnchorID {
		gaps = append(gaps, "the proofs name different snapshot anchors")
	}
	return gaps, Subject{NodeID: resolvedNode, WorkloadID: resolvedWorkload, TargetID: resolvedTarget}
}

func joinUnknowns(recovery, survival Claim) []string {
	unknowns := []string{
		fmt.Sprintf("Recovery evidence is %ds old and survival evidence %ds old; neither shows the state after its observation.", recovery.AgeSeconds, survival.AgeSeconds),
	}
	if survival.SnapshotAnchorID == "" {
		unknowns = append(unknowns, "The survival proof used a disposable canary prefix rather than a named snapshot anchor, so lock state of the specific application snapshot is bound only by target custody.")
	}
	seen := map[string]bool{unknowns[0]: true}
	if len(unknowns) > 1 {
		seen[unknowns[1]] = true
	}
	for _, list := range [][]string{recovery.Unknowns, survival.Unknowns} {
		for _, unknown := range list {
			if text(unknown) && !seen[unknown] {
				seen[unknown] = true
				unknowns = append(unknowns, unknown)
			}
		}
	}
	return unknowns
}

func joinNotClaimed(survival Claim) []string {
	out := make([]string, 0, len(NotClaimed)+len(survival.NotClaimed))
	seen := map[string]bool{}
	for _, list := range [][]string{NotClaimed, survival.NotClaimed} {
		for _, item := range list {
			if text(item) && !seen[item] {
				seen[item] = true
				out = append(out, item)
			}
		}
	}
	return out
}

// chainDigest binds the body to both input digests under a domain tag.
func chainDigest(recoveryDigest, survivalDigest string, body JoinedBody) (string, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, part := range []string{JoinedSchema, recoveryDigest, survivalDigest} {
		h.Write([]byte(part))
		h.Write([]byte{'\n'})
	}
	h.Write(encoded)
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyJoined checks a stored joined receipt against the two input receipts it
// claims to join: the digest chain, a deterministic re-derivation at the
// recorded evaluation time, and that both proofs are still fresh at now.
func VerifyJoined(joined JoinedReceipt, recoveryRaw, survivalRaw []byte, now time.Time, policy Policy) error {
	return VerifyJoinedBound(joined, recoveryRaw, survivalRaw, JoinBinding{}, now, policy)
}

// VerifyJoinedBound performs VerifyJoined using the same JoinBinding used at evaluation time.
func VerifyJoinedBound(joined JoinedReceipt, recoveryRaw, survivalRaw []byte, binding JoinBinding, now time.Time, policy Policy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	sum, err := chainDigest(joined.RecoveryDigest, joined.SurvivalDigest, joined.Body)
	if err != nil {
		return err
	}
	if sum != joined.Digest {
		return errors.New("recoveryproof: joined receipt digest does not match its body")
	}
	if joined.RecoveryDigest != digestOf(recoveryRaw) || joined.SurvivalDigest != digestOf(survivalRaw) {
		return errors.New("recoveryproof: joined receipt does not chain to the supplied receipts")
	}
	rederived, err := EvaluateBound(recoveryRaw, survivalRaw, binding, joined.Body.EvaluatedAt, policy)
	if err != nil {
		return err
	}
	if rederived.Joined == nil || rederived.Joined.Digest != joined.Digest {
		return errors.New("recoveryproof: joined receipt is not what the supplied receipts qualify")
	}
	current, err := EvaluateBound(recoveryRaw, survivalRaw, binding, now, policy)
	if err != nil {
		return err
	}
	if current.Joined == nil {
		return fmt.Errorf("recoveryproof: joined qualification no longer holds: %s", strings.Join(current.JoinGaps, "; "))
	}
	return nil
}
