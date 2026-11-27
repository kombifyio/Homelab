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

// NotClaimed is the bound of every joined receipt. It is part of the signed
// body, so a reader cannot drop it without breaking the digest.
var NotClaimed = []string{
	"No claim beyond the named node, workload, target and scope.",
	"No claim for another operating system profile, application or PostgreSQL major version.",
	"No claim that the backup stays restorable after the evidence age; a later loss is not excluded.",
	"No claim of fenced multi-site recovery or failback (Guard F).",
	"No claim of cold start without WAN or control-plane access (Guard H).",
	"No claim about production notification delivery; the clone was only proven unable to send it.",
	"No claim about data written after the newest recovered transaction.",
}

// Report is the outcome of one evaluation. Recovery and Survival are separate
// verdicts: Survival never changes Recovery. Joined is set only when both
// proofs are verified and agree; JoinGaps explains why it is not otherwise.
type Report struct {
	Recovery Claim          `json:"recovery"`
	Survival Claim          `json:"survival"`
	Joined   *JoinedReceipt `json:"joined,omitempty"`
	JoinGaps []string       `json:"join_gaps,omitempty"`
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
// both are verified for the same node, workload and target. An error means the
// policy is unusable; a bad receipt is a status, not an error.
func Evaluate(recoveryRaw, survivalRaw []byte, now time.Time, policy Policy) (Report, error) {
	recovery, err := ProjectRecovery(recoveryRaw, now, policy)
	if err != nil {
		return Report{}, err
	}
	survival, err := ProjectSurvival(survivalRaw, now, policy)
	if err != nil {
		return Report{}, err
	}
	report := Report{Recovery: recovery, Survival: survival}
	if recovery.Status != StatusVerified {
		report.JoinGaps = append(report.JoinGaps, fmt.Sprintf("recovery proof is %s", recovery.Status))
	}
	if survival.Status != StatusVerified {
		report.JoinGaps = append(report.JoinGaps, fmt.Sprintf("survival proof is %s", survival.Status))
	}
	if len(report.JoinGaps) > 0 {
		return report, nil
	}
	report.JoinGaps = identityGaps(recovery, survival)
	if len(report.JoinGaps) > 0 {
		return report, nil
	}
	body := JoinedBody{
		Schema:      JoinedSchema,
		EvaluatedAt: now.UTC(),
		Subject:     *recovery.Subject,
		Recovery:    recovery,
		Survival:    survival,
		Unknowns:    joinUnknowns(recovery, survival),
		NotClaimed:  append([]string(nil), NotClaimed...),
	}
	sum, err := chainDigest(recovery.Digest, survival.Digest, body)
	if err != nil {
		return Report{}, err
	}
	report.Joined = &JoinedReceipt{Body: body, RecoveryDigest: recovery.Digest, SurvivalDigest: survival.Digest, Digest: sum}
	return report, nil
}

func identityGaps(recovery, survival Claim) []string {
	var gaps []string
	r, s := recovery.Subject, survival.Subject
	if r.NodeID != s.NodeID {
		gaps = append(gaps, "node identity differs between the proofs")
	}
	if r.WorkloadID != s.WorkloadID {
		gaps = append(gaps, "workload identity differs between the proofs")
	}
	if r.TargetID != s.TargetID {
		gaps = append(gaps, "backup target identity differs between the proofs")
	}
	if recovery.SnapshotAnchorID != "" && survival.SnapshotAnchorID != "" && recovery.SnapshotAnchorID != survival.SnapshotAnchorID {
		gaps = append(gaps, "the proofs name different snapshot anchors")
	}
	return gaps
}

func joinUnknowns(recovery, survival Claim) []string {
	unknowns := []string{
		fmt.Sprintf("Recovery evidence is %ds old and survival evidence %ds old; neither shows the state after its observation.", recovery.AgeSeconds, survival.AgeSeconds),
	}
	if survival.SnapshotAnchorID == "" {
		unknowns = append(unknowns, "The survival proof names no snapshot anchor, so it is not shown to cover the restored snapshot itself.")
	}
	seen := map[string]bool{}
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
	rederived, err := Evaluate(recoveryRaw, survivalRaw, joined.Body.EvaluatedAt, policy)
	if err != nil {
		return err
	}
	if rederived.Joined == nil || rederived.Joined.Digest != joined.Digest {
		return errors.New("recoveryproof: joined receipt is not what the supplied receipts qualify")
	}
	current, err := Evaluate(recoveryRaw, survivalRaw, now, policy)
	if err != nil {
		return err
	}
	if current.Joined == nil {
		return fmt.Errorf("recoveryproof: joined qualification no longer holds: %s", strings.Join(current.JoinGaps, "; "))
	}
	return nil
}
