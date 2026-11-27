package guardcanary

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Outcomes of a qualification run.
const (
	// OutcomePromoted: admitted, canary healthy, candidate stays.
	OutcomePromoted = "promoted"
	// OutcomeRolledBack: admitted but not proven healthy; the retained previous
	// release is running and healthy again and the candidate digest is gone.
	OutcomeRolledBack = "rolled_back"
	// OutcomeRejected: refused before anything was applied.
	OutcomeRejected = "rejected"
	// OutcomeRollbackFailed: the rollback itself is not proven. The host state
	// is unknown and the receipt claims nothing about it.
	OutcomeRollbackFailed = "rollback_failed"
)

// Entry kinds, in the order a run records them.
const (
	KindAdmissionPrevious  = "admission_previous"
	KindAdmissionCandidate = "admission_candidate"
	KindBaseline           = "baseline_running"
	KindApplyCandidate     = "apply_candidate"
	KindCandidateRunning   = "candidate_running"
	KindCanary             = "canary_assessment"
	KindRollbackApply      = "rollback_apply"
	KindRollbackVerify     = "rollback_verify"
	KindRecovery           = "recovery_assessment"
	KindOutcome            = "outcome"
)

// Entry results.
const (
	ResultOK           = "ok"
	ResultRefused      = "refused"
	ResultError        = "error"
	ResultHealthy      = string(VerdictHealthy)
	ResultUnhealthy    = string(VerdictUnhealthy)
	ResultInconclusive = string(VerdictInconclusive)
)

// NotClaimed lists the bounds every receipt carries. A verifier rejects a
// receipt that drops one.
var NotClaimed = []string{
	"supply_chain_safety_beyond_the_admitted_digest_and_signer",
	"image_content_is_free_of_vulnerabilities_or_malicious_behaviour",
	"application_correctness_beyond_the_listed_probes",
	"stateful_data_migration_or_database_rollback",
	"multi_replica_or_multi_host_rollout",
	"real_user_journey_passage",
	"health_after_the_receipt_time",
}

// Entry is one hash-chained observation. Hash covers every other field,
// including the previous entry's hash.
type Entry struct {
	Seq      int               `json:"seq"`
	Kind     string            `json:"kind"`
	At       string            `json:"at"`
	Subject  string            `json:"subject,omitempty"`
	Result   string            `json:"result"`
	Reason   string            `json:"reason,omitempty"`
	Detail   map[string]string `json:"detail,omitempty"`
	PrevHash string            `json:"prevHash"`
	Hash     string            `json:"hash"`
}

// Scope states exactly what the receipt is about.
type Scope struct {
	App            string `json:"app"`
	Slice          string `json:"slice"`
	OS             string `json:"os"`
	EvidenceSource string `json:"evidenceSource"`
}

// Versions are the exact releases and trust the run used.
type Versions struct {
	Previous       Release  `json:"previous"`
	Candidate      Release  `json:"candidate"`
	TrustedSigners []Signer `json:"trustedSigners"`
}

// Receipt is the hash-chained qualification record.
type Receipt struct {
	Schema         string   `json:"schema"`
	PackageVersion string   `json:"packageVersion"`
	Scope          Scope    `json:"scope"`
	Versions       Versions `json:"versions"`
	Outcome        string   `json:"outcome"`
	OutcomeReason  string   `json:"outcomeReason"`
	StartedAt      string   `json:"startedAt"`
	FinishedAt     string   `json:"finishedAt"`
	// EvidenceAgeSeconds is the age of each evidence class at FinishedAt.
	EvidenceAgeSeconds map[string]int64 `json:"evidenceAgeSeconds"`
	Unknowns           []string         `json:"unknowns"`
	NotClaimed         []string         `json:"notClaimed"`
	Chain              []Entry          `json:"chain"`
	HeadHash           string           `json:"headHash"`
	ReceiptSHA256      string           `json:"receiptSha256"`
}

const genesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func entryHash(e Entry) string {
	e.Hash = ""
	raw, _ := json.Marshal(e) // fixed struct field order and sorted map keys: deterministic
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func receiptHash(r Receipt) string {
	r.ReceiptSHA256 = ""
	raw, _ := json.Marshal(r)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

type chain struct{ entries []Entry }

func (c *chain) add(at time.Time, kind, subject, result, reason string, detail map[string]string) {
	prev := genesisHash
	if n := len(c.entries); n > 0 {
		prev = c.entries[n-1].Hash
	}
	e := Entry{Seq: len(c.entries) + 1, Kind: kind, At: stamp(at), Subject: subject, Result: result, Reason: reason, Detail: detail, PrevHash: prev}
	e.Hash = entryHash(e)
	c.entries = append(c.entries, e)
}

func (c *chain) last(kind string) *Entry {
	for i := len(c.entries) - 1; i >= 0; i-- {
		if c.entries[i].Kind == kind {
			return &c.entries[i]
		}
	}
	return nil
}

// VerifyReceipt checks a receipt without trusting its producer: the hash chain
// and the receipt hash are recomputed, the bounds and scope are required, and
// the outcome must be supported by the recorded entries. It re-derives nothing
// from the host, so it proves the record is intact and coherent, not that the
// host did what it says.
func VerifyReceipt(r Receipt) error {
	if r.Schema != ReceiptSchema {
		return errors.New("guardcanary: unsupported receipt schema")
	}
	if r.Scope.Slice != SliceStatelessOCI || r.Scope.OS != "linux" || r.Scope.App == "" {
		return errors.New("guardcanary: receipt scope is not the supported slice")
	}
	have := map[string]bool{}
	for _, n := range r.NotClaimed {
		have[n] = true
	}
	for _, n := range NotClaimed {
		if !have[n] {
			return fmt.Errorf("guardcanary: receipt drops the not-claimed bound %q", n)
		}
	}
	if len(r.Unknowns) == 0 {
		return errors.New("guardcanary: receipt lists no unknowns")
	}
	prev := genesisHash
	for i, e := range r.Chain {
		if e.Seq != i+1 || e.PrevHash != prev || e.Hash != entryHash(e) {
			return fmt.Errorf("guardcanary: hash chain breaks at entry %d", i+1)
		}
		prev = e.Hash
	}
	if len(r.Chain) == 0 || r.HeadHash != prev {
		return errors.New("guardcanary: head hash does not match the chain")
	}
	if r.ReceiptSHA256 != receiptHash(r) {
		return errors.New("guardcanary: receipt hash does not match its content")
	}
	return verifyOutcome(r)
}

func verifyOutcome(r Receipt) error {
	c := chain{entries: r.Chain}
	end := r.Chain[len(r.Chain)-1]
	if end.Kind != KindOutcome || end.Result != r.Outcome {
		return errors.New("guardcanary: outcome entry missing or not final")
	}
	ok := func(kind, result string) bool {
		e := c.last(kind)
		return e != nil && e.Result == result
	}
	admitted := ok(KindAdmissionPrevious, ResultOK) && ok(KindAdmissionCandidate, ResultOK) && ok(KindBaseline, ResultOK)
	applied := c.last(KindApplyCandidate) != nil
	switch r.Outcome {
	case OutcomeRejected:
		if applied || admitted {
			return errors.New("guardcanary: rejected receipt records an applied or fully admitted release")
		}
	case OutcomePromoted:
		if !admitted || !ok(KindApplyCandidate, ResultOK) || !ok(KindCandidateRunning, ResultOK) || !ok(KindCanary, ResultHealthy) ||
			c.last(KindRollbackApply) != nil {
			return errors.New("guardcanary: promoted receipt lacks admission, a running candidate or a healthy canary")
		}
	case OutcomeRolledBack:
		verify := c.last(KindRollbackVerify)
		if !admitted || !applied || !ok(KindRollbackApply, ResultOK) || verify == nil || verify.Result != ResultOK ||
			verify.Detail["rejectedDigestRunning"] != "false" || !ok(KindRecovery, ResultHealthy) {
			return errors.New("guardcanary: rolled-back receipt lacks an executed rollback, a gone candidate or recovered health")
		}
		if ok(KindCanary, ResultHealthy) && ok(KindApplyCandidate, ResultOK) && ok(KindCandidateRunning, ResultOK) {
			return errors.New("guardcanary: rolled-back receipt records a healthy canary")
		}
	case OutcomeRollbackFailed:
		if !applied || (ok(KindRollbackApply, ResultOK) && ok(KindRollbackVerify, ResultOK) && ok(KindRecovery, ResultHealthy)) {
			return errors.New("guardcanary: rollback-failed receipt records a proven rollback or no applied release")
		}
	default:
		return fmt.Errorf("guardcanary: unknown outcome %q", r.Outcome)
	}
	return nil
}
