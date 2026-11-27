// Package recoveryproof is the Homelab-side consumer of two independent
// proofs about one PostgreSQL-backed application (kombify-StackKits-q9p6,
// Guard A): the dependency-complete application recovery receipt that a
// StackKits restore drill produced on the node, and the Guard G backup
// survival receipt.
//
// The two proofs are projected separately. An absent, stale or unqualified
// survival proof never blocks or downgrades an ordinary application restore
// drill; only the combined survival-plus-recovery claim needs both. Every
// refusal is fail-closed and names its reason; nothing here reads a clock,
// a database or the network, so the caller passes the evaluation time.
//
// The package imports no other Homelab or StackKits package: receipts arrive as
// bytes and leave as a report.
package recoveryproof

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	// RecoverySchema is the schema tag of an application recovery receipt.
	RecoverySchema = "kombify.application-recovery.v1"
	// SurvivalSchema is the schema tag of a Guard G backup survival receipt.
	SurvivalSchema = "kombify.guard.backup-survival.v1"
	// JoinedSchema is the schema tag of the joined qualification receipt.
	JoinedSchema = "kombify.application-recovery-survival.v1"

	maxReceiptBytes = 1 << 20
	maxListEntries  = 64
)

// Status is the verdict on one proof. Only StatusVerified supports a claim.
type Status string

const (
	StatusVerified    Status = "verified"
	StatusAbsent      Status = "absent"
	StatusStale       Status = "stale"
	StatusFuture      Status = "future"
	StatusIncomplete  Status = "incomplete"
	StatusInvalid     Status = "invalid"
	StatusUnqualified Status = "unqualified"
)

// Policy bounds the evidence the consumer accepts. Every field must be set: a
// zero bound is a configuration error, never an unlimited one.
type Policy struct {
	// RecoveryMaxAge is how old a recovery receipt may be at evaluation time.
	RecoveryMaxAge time.Duration
	// SurvivalMaxAge is the same bound for the Guard G receipt.
	SurvivalMaxAge time.Duration
	// MaxFutureSkew is how far an observation may lie ahead of the evaluation
	// clock before it counts as a future receipt.
	MaxFutureSkew time.Duration
	// MaxRecoverySeconds is the longest measured recovery the receipt may
	// report; a slower drill is stale evidence of a different system.
	MaxRecoverySeconds int64
}

// DefaultPolicy is the bound of the first supported slice: one day for both
// proofs, two minutes of clock skew, a one hour recovery.
func DefaultPolicy() Policy {
	return Policy{
		RecoveryMaxAge:     24 * time.Hour,
		SurvivalMaxAge:     24 * time.Hour,
		MaxFutureSkew:      2 * time.Minute,
		MaxRecoverySeconds: 3600,
	}
}

// Validate refuses a policy with an unset or negative bound.
func (p Policy) Validate() error {
	switch {
	case p.RecoveryMaxAge <= 0:
		return errors.New("recoveryproof: policy recovery max age must be positive")
	case p.SurvivalMaxAge <= 0:
		return errors.New("recoveryproof: policy survival max age must be positive")
	case p.MaxFutureSkew <= 0:
		return errors.New("recoveryproof: policy future skew must be positive")
	case p.MaxRecoverySeconds <= 0:
		return errors.New("recoveryproof: policy recovery bound must be positive")
	}
	return nil
}

// Subject names what a receipt is about. A joined claim requires equality of
// all three members.
type Subject struct {
	NodeID     string `json:"node_id"`
	WorkloadID string `json:"workload_id"`
	TargetID   string `json:"target_id"`
}

func (s Subject) validate() error {
	for _, field := range [][2]string{{"node_id", s.NodeID}, {"workload_id", s.WorkloadID}, {"target_id", s.TargetID}} {
		if !identifier(field[1]) {
			return fmt.Errorf("subject %s is missing or malformed", field[0])
		}
	}
	return nil
}

// Versions are the exact versions the evidence was produced at.
type Versions struct {
	StackKit    string `json:"stackkit"`
	Application string `json:"application"`
	PostgreSQL  string `json:"postgresql"`
	// Recipe is the StackKits recovery recipe the drill ran.
	Recipe string `json:"recipe"`
}

func (v Versions) validate() error {
	for _, field := range [][2]string{{"stackkit", v.StackKit}, {"application", v.Application}, {"postgresql", v.PostgreSQL}, {"recipe", v.Recipe}} {
		if !version(field[1]) {
			return fmt.Errorf("version %s is missing, zero or unpinned", field[0])
		}
	}
	return nil
}

// Control is one removal probe: a required item was taken away from the clone
// and the drill had to fail. A drill that stayed green without the item did not
// need it, so its other findings prove nothing.
type Control struct {
	Removed     bool `json:"removed"`
	DrillFailed bool `json:"drill_failed"`
}

// Check is one dependency-complete recovery element.
type Check struct {
	Name           string  `json:"name"`
	Passed         bool    `json:"passed"`
	EvidenceDigest string  `json:"evidence_digest"`
	RemovalControl Control `json:"removal_control"`
}

func (c Check) validate(kind string) error {
	switch {
	case !identifier(c.Name):
		return fmt.Errorf("%s check has no name", kind)
	case !digest(c.EvidenceDigest):
		return fmt.Errorf("%s %q has no sha256 evidence digest", kind, c.Name)
	case !c.Passed:
		return fmt.Errorf("%s %q did not pass", kind, c.Name)
	case !c.RemovalControl.Removed || !c.RemovalControl.DrillFailed:
		return fmt.Errorf("%s %q has no removal control that failed the drill", kind, c.Name)
	}
	return nil
}

// DataReadback is the PostgreSQL proof: a known transaction was read back, a
// new one was written on the clone, and the newest recovered one is dated.
type DataReadback struct {
	KnownTransactionID      string    `json:"known_transaction_id"`
	KnownTransactionRead    bool      `json:"known_transaction_read"`
	CloneWriteVerified      bool      `json:"clone_write_verified"`
	NewestRecoveredAt       time.Time `json:"newest_recovered_at"`
	LoginVerified           bool      `json:"login_verified"`
	NotificationsSuppressed bool      `json:"notifications_suppressed"`
}

// RecoveryReceipt is the dependency-complete application recovery evidence.
type RecoveryReceipt struct {
	Schema   string   `json:"schema"`
	Subject  Subject  `json:"subject"`
	Versions Versions `json:"versions"`
	// Scope is the one supported slice the drill covered, for example the
	// Linux profile and the application.
	Scope string `json:"scope"`
	// RestoreResultID and SnapshotAnchorID name the TS-M4 restore drill result
	// and the backup anchor it restored.
	RestoreResultID  string    `json:"restore_result_id"`
	SnapshotAnchorID string    `json:"snapshot_anchor_id"`
	ObservedAt       time.Time `json:"observed_at"`
	// RecoverySeconds is the measured time from restore start to a verified
	// login and read of the known transaction.
	RecoverySeconds int64        `json:"recovery_seconds"`
	Dependencies    []Check      `json:"dependencies"`
	Config          []Check      `json:"config"`
	Identity        []Check      `json:"identity"`
	Data            DataReadback `json:"data"`
	Unknowns        []string     `json:"unknowns,omitempty"`
}

// Claim is the verdict on one proof with the facts a reader needs: the exact
// versions and scope, the evidence age and the stated unknowns.
type Claim struct {
	Status     Status     `json:"status"`
	Reason     string     `json:"reason,omitempty"`
	Subject    *Subject   `json:"subject,omitempty"`
	Versions   *Versions  `json:"versions,omitempty"`
	Scope      string     `json:"scope,omitempty"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
	AgeSeconds int64      `json:"age_seconds,omitempty"`
	Digest     string     `json:"digest,omitempty"`
	Unknowns   []string   `json:"unknowns,omitempty"`

	// RecoverySeconds and NewestRecoveredAt are filled for a recovery claim.
	RecoverySeconds   int64      `json:"recovery_seconds,omitempty"`
	NewestRecoveredAt *time.Time `json:"newest_recovered_at,omitempty"`
	SnapshotAnchorID  string     `json:"snapshot_anchor_id,omitempty"`
}

// ProjectRecovery evaluates a recovery receipt on its own. It never looks at
// survival evidence.
func ProjectRecovery(raw []byte, now time.Time, policy Policy) (Claim, error) {
	if err := policy.Validate(); err != nil {
		return Claim{}, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return Claim{Status: StatusAbsent, Reason: "no application recovery receipt"}, nil
	}
	var receipt RecoveryReceipt
	if status, err := decodeStrict(raw, &receipt); err != nil {
		return Claim{Status: status, Reason: "application recovery receipt: " + err.Error()}, nil
	}
	claim := Claim{
		Subject: &receipt.Subject, Versions: &receipt.Versions, Scope: receipt.Scope,
		Unknowns: receipt.Unknowns, SnapshotAnchorID: receipt.SnapshotAnchorID,
	}
	if reason := receipt.incomplete(); reason != "" {
		claim.Status, claim.Reason = StatusIncomplete, "application recovery receipt: "+reason
		return claim, nil
	}
	claim.Digest = digestOf(raw)
	claim.ObservedAt = &receipt.ObservedAt
	claim.RecoverySeconds = receipt.RecoverySeconds
	claim.NewestRecoveredAt = &receipt.Data.NewestRecoveredAt
	if status, reason, age := freshness(receipt.ObservedAt, now, policy.RecoveryMaxAge, policy.MaxFutureSkew); status != StatusVerified {
		claim.Status, claim.Reason, claim.AgeSeconds = status, "application recovery receipt: "+reason, age
		return claim, nil
	}
	claim.AgeSeconds = int64(now.Sub(receipt.ObservedAt).Seconds())
	if receipt.RecoverySeconds > policy.MaxRecoverySeconds {
		claim.Status = StatusUnqualified
		claim.Reason = fmt.Sprintf("measured recovery of %ds exceeds the %ds bound", receipt.RecoverySeconds, policy.MaxRecoverySeconds)
		return claim, nil
	}
	claim.Status = StatusVerified
	return claim, nil
}

func (r RecoveryReceipt) incomplete() string {
	if r.Schema != RecoverySchema {
		return "unsupported schema"
	}
	if err := r.Subject.validate(); err != nil {
		return err.Error()
	}
	if err := r.Versions.validate(); err != nil {
		return err.Error()
	}
	switch {
	case !text(r.Scope):
		return "scope is missing"
	case !identifier(r.RestoreResultID):
		return "restore result id is missing"
	case !identifier(r.SnapshotAnchorID):
		return "snapshot anchor id is missing"
	case r.ObservedAt.IsZero():
		return "observation time is missing"
	case r.RecoverySeconds <= 0:
		return "recovery time was not measured"
	}
	for _, group := range []struct {
		kind   string
		checks []Check
	}{{"dependency", r.Dependencies}, {"config", r.Config}, {"identity", r.Identity}} {
		if len(group.checks) == 0 {
			return group.kind + " evidence is missing"
		}
		if len(group.checks) > maxListEntries {
			return group.kind + " evidence is oversized"
		}
		for _, check := range group.checks {
			if err := check.validate(group.kind); err != nil {
				return err.Error()
			}
		}
	}
	d := r.Data
	switch {
	case !identifier(d.KnownTransactionID) || !d.KnownTransactionRead:
		return "known transaction was not read back from PostgreSQL"
	case !d.CloneWriteVerified:
		return "write on the recovered clone was not verified"
	case !d.LoginVerified:
		return "test user login on the clone was not verified"
	case !d.NotificationsSuppressed:
		return "clone was not proven unable to send production notifications"
	case d.NewestRecoveredAt.IsZero():
		return "newest recovered transaction time is missing"
	case d.NewestRecoveredAt.After(r.ObservedAt):
		return "newest recovered transaction is later than the observation"
	}
	if len(r.Unknowns) > maxListEntries {
		return "unknowns are oversized"
	}
	return ""
}

// SurvivalReceipt is the Guard G backup survival evidence for the same
// target. Gaps are every reason the survival claim is not whole; a receipt with
// a gap or without Qualified never joins.
type SurvivalReceipt struct {
	Schema           string    `json:"schema"`
	Subject          Subject   `json:"subject"`
	Versions         Versions  `json:"versions"`
	Scope            string    `json:"scope"`
	SnapshotAnchorID string    `json:"snapshot_anchor_id,omitempty"`
	ObservedAt       time.Time `json:"observed_at"`
	Qualified        bool      `json:"qualified"`
	Gaps             []string  `json:"gaps"`
	Unknowns         []string  `json:"unknowns,omitempty"`
}

// ProjectSurvival evaluates a Guard G receipt on its own.
func ProjectSurvival(raw []byte, now time.Time, policy Policy) (Claim, error) {
	if err := policy.Validate(); err != nil {
		return Claim{}, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return Claim{Status: StatusAbsent, Reason: "no Guard G backup survival receipt"}, nil
	}
	var receipt SurvivalReceipt
	if status, err := decodeStrict(raw, &receipt); err != nil {
		return Claim{Status: status, Reason: "backup survival receipt: " + err.Error()}, nil
	}
	claim := Claim{
		Subject: &receipt.Subject, Versions: &receipt.Versions, Scope: receipt.Scope,
		Unknowns: receipt.Unknowns, SnapshotAnchorID: receipt.SnapshotAnchorID,
	}
	if reason := receipt.incomplete(); reason != "" {
		claim.Status, claim.Reason = StatusIncomplete, "backup survival receipt: "+reason
		return claim, nil
	}
	claim.Digest = digestOf(raw)
	claim.ObservedAt = &receipt.ObservedAt
	if status, reason, age := freshness(receipt.ObservedAt, now, policy.SurvivalMaxAge, policy.MaxFutureSkew); status != StatusVerified {
		claim.Status, claim.Reason, claim.AgeSeconds = status, "backup survival receipt: "+reason, age
		return claim, nil
	}
	claim.AgeSeconds = int64(now.Sub(receipt.ObservedAt).Seconds())
	if !receipt.Qualified || len(receipt.Gaps) > 0 {
		claim.Status = StatusUnqualified
		claim.Reason = "backup survival is not qualified"
		if len(receipt.Gaps) > 0 {
			claim.Reason += ": " + strings.Join(receipt.Gaps, "; ")
		}
		return claim, nil
	}
	claim.Status = StatusVerified
	return claim, nil
}

func (r SurvivalReceipt) incomplete() string {
	if r.Schema != SurvivalSchema {
		return "unsupported schema"
	}
	if err := r.Subject.validate(); err != nil {
		return err.Error()
	}
	if err := r.Versions.validate(); err != nil {
		return err.Error()
	}
	switch {
	case !text(r.Scope):
		return "scope is missing"
	case r.ObservedAt.IsZero():
		return "observation time is missing"
	case r.Gaps == nil:
		return "gaps are not stated"
	case len(r.Gaps) > maxListEntries || len(r.Unknowns) > maxListEntries:
		return "gaps or unknowns are oversized"
	}
	for _, gap := range r.Gaps {
		if !text(gap) {
			return "a gap is blank"
		}
	}
	return ""
}

// freshness orders an observation against the evaluation clock.
func freshness(observed, now time.Time, maxAge, skew time.Duration) (Status, string, int64) {
	if observed.After(now.Add(skew)) {
		return StatusFuture, "observation lies in the future", 0
	}
	if age := now.Sub(observed); age > maxAge {
		return StatusStale, fmt.Sprintf("evidence is %s old, the bound is %s", age.Truncate(time.Second), maxAge), int64(age.Seconds())
	}
	return StatusVerified, "", 0
}

// decodeStrict reads exactly one JSON object, refusing unknown members and
// trailing data. Malformed input is StatusInvalid, not a Go error: a bad
// receipt is evidence about the system, not a fault in the consumer.
func decodeStrict(raw []byte, into any) (Status, error) {
	if len(raw) > maxReceiptBytes {
		return StatusInvalid, errors.New("receipt is oversized")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return StatusInvalid, fmt.Errorf("not a valid receipt: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return StatusInvalid, errors.New("trailing data after the receipt")
	}
	return "", nil
}

func digestOf(raw []byte) string {
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		compact.Reset()
		compact.Write(raw)
	}
	sum := sha256.Sum256(compact.Bytes())
	return "sha256:" + hex.EncodeToString(sum[:])
}

func identifier(value string) bool {
	if value == "" || len(value) > 200 || value != strings.TrimSpace(value) {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r == 0x7f {
			return false
		}
	}
	return true
}

func text(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= 500
}

func digest(value string) bool {
	hexPart, ok := strings.CutPrefix(value, "sha256:")
	if !ok || len(hexPart) != 64 {
		return false
	}
	_, err := hex.DecodeString(hexPart)
	return err == nil && strings.ToLower(hexPart) == hexPart && strings.Trim(hexPart, "0") != ""
}

func version(value string) bool {
	if !identifier(value) {
		return false
	}
	switch strings.ToLower(strings.TrimPrefix(value, "v")) {
	case "0", "0.0", "0.0.0", "latest", "unknown", "dev", "main", "master":
		return false
	}
	return true
}
