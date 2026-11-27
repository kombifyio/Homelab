package guardcoldstart

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

const (
	// ReceiptVersion names the rehearsal receipt.
	ReceiptVersion = "guard.cold-start-rehearsal/v1"

	entryDomain = "homelab.guard.cold-start-entry/v1\x00"
	genesisHash = "genesis"
)

// Outcomes of a rehearsal. Only OutcomeRecovered claims recovery; a denial is
// recoverable by an online exchange and never an authentication bypass.
const (
	OutcomeRecovered = "recovered"
	OutcomeDenied    = "denied_recoverable"
	OutcomeFailed    = "failed"
	OutcomeUnknown   = "unknown_effect"
)

// Entry kinds in the order a rehearsal records them.
const (
	KindStarted         = "rehearsal_started"
	KindAuthority       = "authority_checked"
	KindArtifact        = "artifact_verified"
	KindCut             = "cut_observed"
	KindStep            = "step_result"
	KindLocalAccess     = "local_access_checked"
	KindEgress          = "egress_summary"
	KindReconcileQueued = "reconciliation_queued"
	KindFinished        = "rehearsal_finished"
)

// Entry is one link of the receipt's hash chain.
type Entry struct {
	Seq      int               `json:"seq"`
	Kind     string            `json:"kind"`
	At       time.Time         `json:"at"`
	Fields   map[string]string `json:"fields,omitempty"`
	PrevHash string            `json:"prev_hash"`
	Hash     string            `json:"hash"`
}

// Denial is the structured refusal recorded in a receipt.
type Denial struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Timing is the recovery timing as far as it is known. A zero time or
// negative duration is unknown and listed in Receipt.Unknowns.
type Timing struct {
	CutAt          time.Time `json:"cut_at,omitempty"`
	BootAt         time.Time `json:"boot_at,omitempty"`
	RehearsalStart time.Time `json:"rehearsal_start"`
	RecoveredAt    time.Time `json:"recovered_at,omitempty"`
	// RecoverySeconds is RecoveredAt minus BootAt, or -1 when unknown.
	RecoverySeconds float64 `json:"recovery_seconds"`
	// RehearsalSeconds is the engine's own run time.
	RehearsalSeconds float64 `json:"rehearsal_seconds"`
}

// Effect is one step effect pending reconciliation with the control plane.
type Effect struct {
	EffectID string `json:"effect_id"`
	StepID   string `json:"step_id"`
	Result   string `json:"result"`
	// Executed is false for a step the journal already held as done: the
	// rehearsal did not repeat it.
	Executed bool `json:"executed"`
}

// Receipt is the hash-chained, bounded record of one rehearsal.
type Receipt struct {
	Version    string          `json:"version"`
	Outcome    string          `json:"outcome"`
	Denial     *Denial         `json:"denial,omitempty"`
	PlanID     string          `json:"plan_id"`
	PlanDigest string          `json:"plan_digest,omitempty"`
	Release    string          `json:"release,omitempty"`
	Timing     Timing          `json:"timing"`
	Egress     []EgressAttempt `json:"egress"`
	Effects    []Effect        `json:"effects"`
	Entries    []Entry         `json:"entries"`
	HeadHash   string          `json:"head_hash"`
	// NotClaimed lists what this receipt does not prove, always present.
	NotClaimed []string `json:"not_claimed"`
	// Unknowns lists the facts this run could not establish.
	Unknowns []string `json:"unknowns"`
}

// NotClaimed is the fixed set of bounds every receipt carries.
func NotClaimed() []string {
	return []string{
		"wan_cut_not_performed_by_engine: unreachability of the declared dependencies is host-observed, the engine only refuses to proceed when it was not",
		"reboot_not_performed_by_engine: boot time is host-reported",
		"no_remote_revocation_observed: a revocation issued while offline is unseen until reconnection, bounded by the offline authority lifetime",
		"no_data_restore_or_backup_survival_proof: restored state integrity belongs to Guard A and backup survival",
		"no_fleet_or_multi_server_recovery: one node, one supported Linux profile",
		"no_live_test_window_receipt: this receipt alone is not a disposable-host test window",
		"no_effect_reconciliation_with_control_plane: effects stay queued until the node reconnects",
	}
}

type chain struct {
	entries []Entry
}

func (c *chain) add(kind string, at time.Time, fields map[string]string) {
	prev := genesisHash
	if n := len(c.entries); n > 0 {
		prev = c.entries[n-1].Hash
	}
	e := Entry{Seq: len(c.entries) + 1, Kind: kind, At: at.UTC(), Fields: fields, PrevHash: prev}
	e.Hash = entryHash(e)
	c.entries = append(c.entries, e)
}

func entryHash(e Entry) string {
	e.Hash = ""
	raw, _ := json.Marshal(e) // map keys marshal sorted: deterministic
	sum := sha256.Sum256(append([]byte(entryDomain), raw...))
	return hex.EncodeToString(sum[:])
}

// VerifyChain recomputes the receipt's hash chain. A receipt with no entries,
// a broken link, a reordered or altered entry, or a head that is not the last
// hash does not verify.
func VerifyChain(r Receipt) error {
	if r.Version != ReceiptVersion || len(r.Entries) == 0 {
		return fmt.Errorf("guardcoldstart: receipt has no chain")
	}
	prev := genesisHash
	for i, e := range r.Entries {
		if e.Seq != i+1 || e.PrevHash != prev || e.Hash != entryHash(e) {
			return fmt.Errorf("guardcoldstart: chain breaks at entry %d", i+1)
		}
		prev = e.Hash
	}
	if r.HeadHash != prev {
		return fmt.Errorf("guardcoldstart: head hash does not match the last entry")
	}
	return nil
}

// ReconcileOnReconnect returns the effects the control plane has not yet
// acknowledged. It only ever reports: nothing in a receipt is executed again,
// so reconnecting cannot repeat an effect.
func ReconcileOnReconnect(r Receipt, acknowledged map[string]bool) []Effect {
	var pending []Effect
	for _, e := range r.Effects {
		if !acknowledged[e.EffectID] {
			pending = append(pending, e)
		}
	}
	return pending
}
