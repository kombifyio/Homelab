// Package recoveryproof is the Homelab-side consumer of two independent
// proofs about one PostgreSQL-backed application (kombify-StackKits-q9p6,
// Guard A): the dependency-complete application recovery receipt emitted by
// `stackkit advanced restore-drill` (stackkit.application-recovery-receipt/v1,
// standalone or nested inside stackkit.restore-drill-report/v1), and the
// Guard G backup survival receipt (stackkit.guard-backup-survival-receipt/v1).
//
// The two proofs are projected separately. An absent, stale or unqualified
// survival proof never blocks or downgrades an ordinary application restore
// drill; only the combined survival-plus-recovery claim needs both. Every
// refusal is fail-closed and names its reason; nothing here reads a clock,
// a database or the network, so the caller passes the evaluation time.
//
// The package defines self-contained wire structs and imports no other Homelab
// or StackKits package: receipts arrive as bytes and leave as a report.
package recoveryproof

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// ApplicationRecoveryReceiptSchema is the Guard A producer receipt schema.
	ApplicationRecoveryReceiptSchema = "stackkit.application-recovery-receipt/v1"
	// RestoreDrillReportSchema is the outer restore-drill report schema that may embed applicationRecovery.
	RestoreDrillReportSchema = "stackkit.restore-drill-report/v1"
	// BackupSurvivalReceiptSchema is the Guard G producer receipt schema.
	BackupSurvivalReceiptSchema = "stackkit.guard-backup-survival-receipt/v1"
	// BoundSurvivalEnvelopeSchema is the optional Homelab-bound Guard G envelope schema.
	BoundSurvivalEnvelopeSchema = "kombify.guard-backup-survival-bound/v1"
	// JoinedSchema is the schema tag of the joined qualification receipt.
	JoinedSchema = "kombify.application-recovery-survival.v1"

	maxReceiptBytes = 1 << 20
	maxListEntries  = 64
	readbackMethod  = "volume-file-tree-sha256/v1"
)

var drillIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{7,99}$`)

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
	// RecoveryMaxAge is how old a recovery drill completion may be at evaluation time.
	RecoveryMaxAge time.Duration
	// SurvivalMaxAge is how old a Guard G survival receipt may be at evaluation time.
	SurvivalMaxAge time.Duration
	// MaxFutureSkew is how far an observation may lie ahead of the evaluation
	// clock before it counts as a future receipt.
	MaxFutureSkew time.Duration
	// MaxRecoverySeconds is the longest measured drill duration the receipt may
	// report; a slower drill is unqualified evidence of a different system.
	MaxRecoverySeconds int64
	// SurvivalPublicKey, when set, pins the trusted Ed25519 public key of
	// the node that produced the Guard G backup survival receipt.
	SurvivalPublicKey ed25519.PublicKey
	// RequireSurvivalSignature requires SurvivalPublicKey to be set and fails
	// closed if a non-empty Guard G survival receipt is unsigned or invalid.
	RequireSurvivalSignature bool
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

// Validate refuses a policy with an unset or non-positive bound.
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
	case len(p.SurvivalPublicKey) != 0 && len(p.SurvivalPublicKey) != ed25519.PublicKeySize:
		return errors.New("recoveryproof: policy survival public key must be an Ed25519 public key")
	case p.RequireSurvivalSignature && len(p.SurvivalPublicKey) != ed25519.PublicKeySize:
		return errors.New("recoveryproof: policy requires a trusted Ed25519 survival public key")
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

// Versions are the exact pinned versions and artifact digests the evidence was produced at.
type Versions struct {
	StackKit             string            `json:"stackkit"`
	Application          string            `json:"application"`
	PostgreSQL           string            `json:"postgresql"`
	ModuleRef            string            `json:"module_ref,omitempty"`
	PlanHash             string            `json:"plan_hash,omitempty"`
	ManifestHash         string            `json:"manifest_hash,omitempty"`
	PolicyArtifactDigest string            `json:"policy_artifact_digest,omitempty"`
	KopiaHelperImage     string            `json:"kopia_helper_image,omitempty"`
	Dependencies         map[string]string `json:"dependencies,omitempty"`
}

// ScopeDetail records the exact scope verified by the application recovery drill.
type ScopeDetail struct {
	SiteRef              string   `json:"site_ref"`
	NodeRef              string   `json:"node_ref"`
	LinuxProfile         string   `json:"linux_profile"`
	LiveProject          string   `json:"live_project"`
	CloneProject         string   `json:"clone_project"`
	BackedUpVolumes      []string `json:"backed_up_volumes"`
	StagedVolumes        []string `json:"staged_volumes"`
	RestoredVolumes      []string `json:"restored_volumes"`
	VerifiedDependencies []string `json:"verified_dependencies"`
	ConfigFilesVerified  []string `json:"config_files_verified"`
	SecretSlotsVerified  []string `json:"secret_slots_verified"`
}

// EvidenceAge records the timestamps and measured durations of the recovery drill.
type EvidenceAge struct {
	SnapshotAnchorID   string    `json:"snapshot_anchor_id"`
	SnapshotCreatedAt  time.Time `json:"snapshot_created_at"`
	SnapshotAgeSeconds int64     `json:"snapshot_age_seconds"`
	RestoreResultID    string    `json:"restore_result_id"`
	RestoreCompletedAt time.Time `json:"restore_completed_at"`
	DrillStartedAt     time.Time `json:"drill_started_at"`
	DrillCompletedAt   time.Time `json:"drill_completed_at"`
	DrillDurationMS    int64     `json:"drill_duration_ms"`
}

// Claim is the verdict on one proof with the facts a reader needs: the exact
// versions and scope, the evidence age, explicit unknowns and not-claimed bounds.
type Claim struct {
	Status           Status       `json:"status"`
	Reason           string       `json:"reason,omitempty"`
	DrillID          string       `json:"drill_id,omitempty"`
	Subject          *Subject     `json:"subject,omitempty"`
	Versions         *Versions    `json:"versions,omitempty"`
	Scope            string       `json:"scope,omitempty"`
	ScopeDetail      *ScopeDetail `json:"scope_detail,omitempty"`
	EvidenceAge      *EvidenceAge `json:"evidence_age,omitempty"`
	ObservedAt       *time.Time   `json:"observed_at,omitempty"`
	AgeSeconds       int64        `json:"age_seconds,omitempty"`
	Digest           string       `json:"digest,omitempty"`
	BindingDigest    string       `json:"binding_digest,omitempty"`
	Canary           *CanaryRef   `json:"canary,omitempty"`
	Unknowns         []string     `json:"unknowns,omitempty"`
	NotClaimed       []string     `json:"not_claimed,omitempty"`
	RecoverySeconds  int64        `json:"recovery_seconds,omitempty"`
	SnapshotAnchorID string       `json:"snapshot_anchor_id,omitempty"`
	DatabaseName     string       `json:"database_name,omitempty"`
	PublicTableCount int64        `json:"public_table_count,omitempty"`
	EntryHTTPStatus  int          `json:"entry_http_status,omitempty"`
}

// CanaryRef identifies the disposable canary object verified by Guard G.
type CanaryRef struct {
	Key       string `json:"key"`
	VersionID string `json:"version_id"`
}

// Wire structs matching schemas/stackkit-restore-drill-report-v1.schema.json
// and schemas/stackkit-application-recovery-receipt-v1.schema.json without
// importing root StackKits packages.

type restoreDrillReportWire struct {
	BackupRenewal       json.RawMessage                 `json:"backupRenewal,omitempty"`
	SchemaVersion       string                          `json:"schemaVersion"`
	Mode                string                          `json:"mode"`
	Operation           string                          `json:"operation"`
	DrillID             string                          `json:"drillId"`
	Status              string                          `json:"status"`
	CapabilityID        string                          `json:"capabilityId"`
	AnchorID            string                          `json:"anchorId"`
	AnchorSource        string                          `json:"anchorSource"`
	KopiaSnapshotIDs    []string                        `json:"kopiaSnapshotIds"`
	RestoreResultID     string                          `json:"restoreResultId"`
	StagingPath         string                          `json:"stagingPath"`
	StagingRemoved      bool                            `json:"stagingRemoved"`
	Activated           bool                            `json:"activated"`
	Phases              []restoreDrillPhaseWire         `json:"phases"`
	Verification        []restoreDrillCheckWire         `json:"verification"`
	ApplicationRecovery *applicationRecoveryReceiptWire `json:"applicationRecovery,omitempty"`
	BackupSurvival      json.RawMessage                 `json:"backupSurvival,omitempty"`
	StartedAt           time.Time                       `json:"startedAt"`
	CompletedAt         time.Time                       `json:"completedAt"`
}

type restoreDrillPhaseWire struct {
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	StartedAt  time.Time `json:"startedAt"`
	DurationMS int64     `json:"durationMs"`
	Error      string    `json:"error,omitempty"`
}

type restoreDrillCheckWire struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type applicationRecoveryReceiptWire struct {
	SchemaVersion      string                          `json:"schemaVersion"`
	DrillID            string                          `json:"drillId"`
	Status             string                          `json:"status"`
	FailureCode        string                          `json:"failureCode,omitempty"`
	Error              string                          `json:"error,omitempty"`
	ReceiptPath        string                          `json:"receiptPath"`
	WorkloadRef        string                          `json:"workloadRef"`
	ModuleRef          string                          `json:"moduleRef"`
	AppRelease         string                          `json:"appRelease"`
	PostgresImage      string                          `json:"postgresImage"`
	DependencyVersions map[string]string               `json:"dependencyVersions"`
	Scope              applicationRecoveryScopeWire    `json:"scope"`
	Versions           applicationRecoveryVersionsWire `json:"versions"`
	EvidenceAge        applicationRecoveryAgeWire      `json:"evidenceAge"`
	Isolation          applicationRecoveryIsoWire      `json:"isolation"`
	Readiness          applicationRecoveryReadyWire    `json:"readiness"`
	DataReadback       applicationRecoveryReadbackWire `json:"dataReadback"`
	Unknowns           []string                        `json:"unknowns"`
}

type applicationRecoveryScopeWire struct {
	SiteRef              string   `json:"siteRef"`
	NodeRef              string   `json:"nodeRef"`
	LinuxProfile         string   `json:"linuxProfile"`
	LiveProject          string   `json:"liveProject"`
	CloneProject         string   `json:"cloneProject"`
	BackedUpVolumes      []string `json:"backedUpVolumes"`
	StagedVolumes        []string `json:"stagedVolumes"`
	RestoredVolumes      []string `json:"restoredVolumes"`
	VerifiedDependencies []string `json:"verifiedDependencies"`
	ConfigFilesVerified  []string `json:"configFilesVerified"`
	SecretSlotsVerified  []string `json:"secretSlotsVerified"`
}

type applicationRecoveryVersionsWire struct {
	StackKitVersion      string `json:"stackkitVersion"`
	PlanHash             string `json:"planHash"`
	ManifestHash         string `json:"manifestHash"`
	PolicyArtifactDigest string `json:"policyArtifactDigest"`
	KopiaHelperImage     string `json:"kopiaHelperImage"`
}

type applicationRecoveryAgeWire struct {
	SnapshotAnchorID   string    `json:"snapshotAnchorId"`
	SnapshotCreatedAt  time.Time `json:"snapshotCreatedAt"`
	SnapshotAgeSeconds int64     `json:"snapshotAgeSeconds"`
	RestoreResultID    string    `json:"restoreResultId"`
	RestoreCompletedAt time.Time `json:"restoreCompletedAt"`
	DrillStartedAt     time.Time `json:"drillStartedAt"`
	DrillCompletedAt   time.Time `json:"drillCompletedAt"`
	DrillDurationMS    int64     `json:"drillDurationMs"`
}

type applicationRecoveryIsoWire struct {
	CloneProject                    string `json:"cloneProject"`
	LiveProjectUntouched            bool   `json:"liveProjectUntouched"`
	LiveVolumesUnmounted            bool   `json:"liveVolumesUnmounted"`
	InternalNetworksOnly            bool   `json:"internalNetworksOnly"`
	MasqueradeDisabled              bool   `json:"masqueradeDisabled"`
	RoutingLabelsStripped           bool   `json:"routingLabelsStripped"`
	PublishedHostPortsOnlyLoopback  bool   `json:"publishedHostPortsOnlyLoopback"`
	OutboundNotificationsProhibited bool   `json:"outboundNotificationsProhibited"`
	OutboundNotificationsDetected   bool   `json:"outboundNotificationsDetected"`
	CloneResourcesCleanedUp         bool   `json:"cloneResourcesCleanedUp"`
}

type applicationRecoveryReadyWire struct {
	DatabaseReady    bool                  `json:"databaseReady"`
	DatabaseName     string                `json:"databaseName"`
	PublicTableCount int64                 `json:"publicTableCount"`
	EntryHTTPStatus  int                   `json:"entryHttpStatus"`
	EntryHTTPReady   bool                  `json:"entryHttpReady"`
	DependencyChecks []dependencyCheckWire `json:"dependencyChecks"`
}

type dependencyCheckWire struct {
	ComponentRef string `json:"componentRef"`
	Role         string `json:"role"`
	Lifecycle    string `json:"lifecycle"`
	State        string `json:"state"`
	Health       string `json:"health,omitempty"`
	ExitCode     int    `json:"exitCode"`
	Ready        bool   `json:"ready"`
}

type applicationRecoveryReadbackWire struct {
	Method  string                   `json:"method"`
	Volumes []volumeVerificationWire `json:"volumes"`
}

type volumeVerificationWire struct {
	LiveName      string `json:"liveName"`
	CloneName     string `json:"cloneName"`
	Files         int64  `json:"files"`
	Bytes         int64  `json:"bytes"`
	Digest        string `json:"digest"`
	MatchesStaged bool   `json:"matchesStaged"`
}

// ProjectRecovery evaluates a Guard A application-recovery receipt on its own.
// It accepts either a standalone `stackkit.application-recovery-receipt/v1`
// document or a `stackkit.restore-drill-report/v1` report carrying
// `applicationRecovery`. It never looks at survival evidence.
func ProjectRecovery(raw []byte, now time.Time, policy Policy) (Claim, error) {
	if err := policy.Validate(); err != nil {
		return Claim{}, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return Claim{Status: StatusAbsent, Reason: "no application recovery receipt"}, nil
	}
	var peek struct {
		SchemaVersion string `json:"schemaVersion"`
	}
	if len(raw) > maxReceiptBytes {
		return Claim{Status: StatusInvalid, Reason: "application recovery receipt is oversized"}, nil
	}
	if err := json.Unmarshal(raw, &peek); err != nil {
		return Claim{Status: StatusInvalid, Reason: fmt.Sprintf("application recovery receipt: not a valid receipt: %v", err)}, nil
	}

	var receipt applicationRecoveryReceiptWire
	switch peek.SchemaVersion {
	case ApplicationRecoveryReceiptSchema:
		if status, err := decodeStrict(raw, &receipt); err != nil {
			return Claim{Status: status, Reason: "application recovery receipt: " + err.Error()}, nil
		}
	case RestoreDrillReportSchema:
		var outer restoreDrillReportWire
		if status, err := decodeStrict(raw, &outer); err != nil {
			return Claim{Status: status, Reason: "restore drill report: " + err.Error()}, nil
		}
		if reason := validateOuterRestoreDrillReport(outer); reason != "" {
			status := StatusIncomplete
			if outer.Status == "failed" {
				status = StatusUnqualified
			}
			return Claim{Status: status, DrillID: outer.DrillID, Reason: "restore drill report: " + reason}, nil
		}
		if outer.ApplicationRecovery == nil {
			return Claim{Status: StatusAbsent, DrillID: outer.DrillID, Reason: "restore drill report has no application recovery receipt"}, nil
		}
		receipt = *outer.ApplicationRecovery
		if receipt.DrillID != outer.DrillID {
			return Claim{Status: StatusInvalid, DrillID: outer.DrillID, Reason: "application recovery drillId differs from outer restore drill report"}, nil
		}
		if outer.AnchorID != "" && receipt.EvidenceAge.SnapshotAnchorID != outer.AnchorID {
			return Claim{Status: StatusInvalid, DrillID: outer.DrillID, Reason: "application recovery snapshotAnchorId differs from outer restore drill report"}, nil
		}
		if outer.RestoreResultID != "" && receipt.EvidenceAge.RestoreResultID != outer.RestoreResultID {
			return Claim{Status: StatusInvalid, DrillID: outer.DrillID, Reason: "application recovery restoreResultId differs from outer restore drill report"}, nil
		}
	default:
		return Claim{Status: StatusInvalid, Reason: fmt.Sprintf("application recovery receipt: unsupported schema %q", peek.SchemaVersion)}, nil
	}

	return evaluateApplicationRecovery(raw, receipt, now, policy), nil
}

func validateOuterRestoreDrillReport(outer restoreDrillReportWire) string {
	switch {
	case outer.Mode != "advanced":
		return fmt.Sprintf("unsupported mode %q", outer.Mode)
	case outer.Operation != "restore.drill":
		return fmt.Sprintf("unsupported operation %q", outer.Operation)
	case !drillIDPattern.MatchString(outer.DrillID):
		return "drillId is missing or malformed"
	case outer.Status != "succeeded":
		return fmt.Sprintf("restore drill status is %q", outer.Status)
	case outer.Activated:
		return "restore drill activated live state"
	case !outer.StagingRemoved:
		return "restore drill did not remove staging"
	case outer.StartedAt.IsZero() || outer.CompletedAt.IsZero():
		return "restore drill timestamps are missing"
	}
	return ""
}

func evaluateApplicationRecovery(raw []byte, r applicationRecoveryReceiptWire, now time.Time, policy Policy) Claim {
	claim := buildRecoveryClaimMetadata(r)
	if r.Status == "failed" {
		reason := "application recovery drill failed"
		if text(r.FailureCode) {
			reason += " (" + r.FailureCode + ")"
		}
		if text(r.Error) {
			reason += ": " + r.Error
		}
		claim.Status = StatusUnqualified
		claim.Reason = reason
		return claim
	}
	if r.Status != "succeeded" {
		claim.Status = StatusIncomplete
		claim.Reason = fmt.Sprintf("application recovery receipt: unsupported status %q", r.Status)
		return claim
	}
	if reason := r.incomplete(); reason != "" {
		claim.Status = StatusIncomplete
		claim.Reason = "application recovery receipt: " + reason
		return claim
	}
	claim.Digest = digestOf(raw)
	completedUTC := r.EvidenceAge.DrillCompletedAt.UTC()
	claim.ObservedAt = &completedUTC
	claim.RecoverySeconds = (r.EvidenceAge.DrillDurationMS + 999) / 1000

	for _, ts := range []struct {
		name string
		at   time.Time
	}{
		{"snapshotCreatedAt", r.EvidenceAge.SnapshotCreatedAt},
		{"restoreCompletedAt", r.EvidenceAge.RestoreCompletedAt},
		{"drillStartedAt", r.EvidenceAge.DrillStartedAt},
		{"drillCompletedAt", r.EvidenceAge.DrillCompletedAt},
	} {
		if ts.at.After(now.Add(policy.MaxFutureSkew)) {
			claim.Status = StatusFuture
			claim.Reason = fmt.Sprintf("application recovery receipt: %s lies in the future", ts.name)
			return claim
		}
	}
	if status, reason, age := freshness(completedUTC, now, policy.RecoveryMaxAge, policy.MaxFutureSkew); status != StatusVerified {
		claim.Status = status
		claim.Reason = "application recovery receipt: " + reason
		claim.AgeSeconds = age
		return claim
	}
	claim.AgeSeconds = max(int64(now.Sub(completedUTC).Seconds()), 0)
	if r.EvidenceAge.DrillDurationMS > policy.MaxRecoverySeconds*1000 {
		claim.Status = StatusUnqualified
		claim.Reason = fmt.Sprintf("measured recovery of %dms exceeds the %ds bound", r.EvidenceAge.DrillDurationMS, policy.MaxRecoverySeconds)
		return claim
	}
	claim.Status = StatusVerified
	return claim
}

func buildRecoveryClaimMetadata(r applicationRecoveryReceiptWire) Claim {
	depsCopy := make(map[string]string, len(r.DependencyVersions))
	for k, v := range r.DependencyVersions {
		depsCopy[k] = v
	}
	scopeLabel := ""
	if r.Scope.LinuxProfile != "" && r.WorkloadRef != "" {
		scopeLabel = r.Scope.LinuxProfile + ":" + r.WorkloadRef
	} else if r.Scope.LinuxProfile != "" {
		scopeLabel = r.Scope.LinuxProfile
	}
	return Claim{
		DrillID: r.DrillID,
		Subject: &Subject{
			NodeID:     r.Scope.NodeRef,
			WorkloadID: r.WorkloadRef,
		},
		Versions: &Versions{
			StackKit:             r.Versions.StackKitVersion,
			Application:          r.AppRelease,
			PostgreSQL:           r.PostgresImage,
			ModuleRef:            r.ModuleRef,
			PlanHash:             r.Versions.PlanHash,
			ManifestHash:         r.Versions.ManifestHash,
			PolicyArtifactDigest: r.Versions.PolicyArtifactDigest,
			KopiaHelperImage:     r.Versions.KopiaHelperImage,
			Dependencies:         depsCopy,
		},
		Scope: scopeLabel,
		ScopeDetail: &ScopeDetail{
			SiteRef:              r.Scope.SiteRef,
			NodeRef:              r.Scope.NodeRef,
			LinuxProfile:         r.Scope.LinuxProfile,
			LiveProject:          r.Scope.LiveProject,
			CloneProject:         r.Scope.CloneProject,
			BackedUpVolumes:      append([]string(nil), r.Scope.BackedUpVolumes...),
			StagedVolumes:        append([]string(nil), r.Scope.StagedVolumes...),
			RestoredVolumes:      append([]string(nil), r.Scope.RestoredVolumes...),
			VerifiedDependencies: append([]string(nil), r.Scope.VerifiedDependencies...),
			ConfigFilesVerified:  append([]string(nil), r.Scope.ConfigFilesVerified...),
			SecretSlotsVerified:  append([]string(nil), r.Scope.SecretSlotsVerified...),
		},
		EvidenceAge: &EvidenceAge{
			SnapshotAnchorID:   r.EvidenceAge.SnapshotAnchorID,
			SnapshotCreatedAt:  r.EvidenceAge.SnapshotCreatedAt.UTC(),
			SnapshotAgeSeconds: r.EvidenceAge.SnapshotAgeSeconds,
			RestoreResultID:    r.EvidenceAge.RestoreResultID,
			RestoreCompletedAt: r.EvidenceAge.RestoreCompletedAt.UTC(),
			DrillStartedAt:     r.EvidenceAge.DrillStartedAt.UTC(),
			DrillCompletedAt:   r.EvidenceAge.DrillCompletedAt.UTC(),
			DrillDurationMS:    r.EvidenceAge.DrillDurationMS,
		},
		Unknowns:         append([]string(nil), r.Unknowns...),
		SnapshotAnchorID: r.EvidenceAge.SnapshotAnchorID,
		DatabaseName:     r.Readiness.DatabaseName,
		PublicTableCount: r.Readiness.PublicTableCount,
		EntryHTTPStatus:  r.Readiness.EntryHTTPStatus,
	}
}

func (r applicationRecoveryReceiptWire) incomplete() string {
	if r.SchemaVersion != ApplicationRecoveryReceiptSchema {
		return "unsupported schema"
	}
	switch {
	case !drillIDPattern.MatchString(r.DrillID):
		return "drillId is missing or malformed"
	case !text(r.ReceiptPath):
		return "receiptPath is missing"
	case !identifier(r.WorkloadRef):
		return "workloadRef is missing or malformed"
	case !identifier(r.ModuleRef):
		return "moduleRef is missing or malformed"
	case !version(r.AppRelease):
		return "appRelease is missing, zero or unpinned"
	case !pinnedImage(r.PostgresImage):
		return "postgresImage is missing, zero or unpinned"
	}
	if reason := r.validateScopeAndVersions(); reason != "" {
		return reason
	}
	if reason := r.validateEvidenceTimestamps(); reason != "" {
		return reason
	}
	if reason := r.validateIsolation(); reason != "" {
		return reason
	}
	if reason := r.validateReadiness(); reason != "" {
		return reason
	}
	if reason := r.validateDataReadback(); reason != "" {
		return reason
	}
	if r.Unknowns == nil {
		return "unknowns array is missing"
	}
	if len(r.Unknowns) > maxListEntries {
		return "unknowns are oversized"
	}
	return ""
}

func (r applicationRecoveryReceiptWire) validateScopeAndVersions() string {
	s := r.Scope
	switch {
	case !identifier(s.SiteRef):
		return "scope.siteRef is missing or malformed"
	case !identifier(s.NodeRef):
		return "scope.nodeRef is missing or malformed"
	case !identifier(s.LinuxProfile):
		return "scope.linuxProfile is missing or malformed"
	case !identifier(s.LiveProject):
		return "scope.liveProject is missing or malformed"
	case !identifier(s.CloneProject):
		return "scope.cloneProject is missing or malformed"
	case s.CloneProject == s.LiveProject:
		return "scope.cloneProject collides with liveProject"
	case len(s.BackedUpVolumes) == 0 || len(s.BackedUpVolumes) > maxListEntries:
		return "scope.backedUpVolumes is empty or oversized"
	case len(s.StagedVolumes) == 0 || len(s.StagedVolumes) > maxListEntries:
		return "scope.stagedVolumes is empty or oversized"
	case len(s.RestoredVolumes) == 0 || len(s.RestoredVolumes) > maxListEntries:
		return "scope.restoredVolumes is empty or oversized"
	case len(s.VerifiedDependencies) == 0 || len(s.VerifiedDependencies) > maxListEntries:
		return "scope.verifiedDependencies is empty or oversized"
	case len(s.ConfigFilesVerified) == 0 || len(s.ConfigFilesVerified) > maxListEntries:
		return "scope.configFilesVerified is empty or oversized"
	case len(s.SecretSlotsVerified) == 0 || len(s.SecretSlotsVerified) > maxListEntries:
		return "scope.secretSlotsVerified is empty or oversized"
	}
	for _, list := range [][]string{s.BackedUpVolumes, s.StagedVolumes, s.RestoredVolumes, s.VerifiedDependencies, s.ConfigFilesVerified, s.SecretSlotsVerified} {
		for _, item := range list {
			if !identifier(item) {
				return fmt.Sprintf("scope entry %q is malformed", item)
			}
		}
	}
	stagedSet := stringSet(s.StagedVolumes)
	for _, bv := range s.BackedUpVolumes {
		if !stagedSet[bv] {
			return fmt.Sprintf("backed-up volume %q is missing from stagedVolumes", bv)
		}
	}
	v := r.Versions
	switch {
	case !version(v.StackKitVersion):
		return "versions.stackkitVersion is missing, zero or unpinned"
	case !digest(v.PlanHash):
		return "versions.planHash is missing or malformed"
	case !digest(v.ManifestHash):
		return "versions.manifestHash is missing or malformed"
	case !digest(v.PolicyArtifactDigest):
		return "versions.policyArtifactDigest is missing or malformed"
	case !pinnedImage(v.KopiaHelperImage):
		return "versions.kopiaHelperImage is missing, zero or unpinned"
	case len(r.DependencyVersions) == 0 || len(r.DependencyVersions) > maxListEntries:
		return "dependencyVersions is empty or oversized"
	}
	for dep, ver := range r.DependencyVersions {
		if !identifier(dep) || !pinnedImage(ver) {
			return fmt.Sprintf("dependencyVersions[%q] is missing or unpinned", dep)
		}
	}
	for _, dep := range s.VerifiedDependencies {
		if _, ok := r.DependencyVersions[dep]; !ok {
			return fmt.Sprintf("verified dependency %q has no pinned version in dependencyVersions", dep)
		}
	}
	return ""
}

func (r applicationRecoveryReceiptWire) validateEvidenceTimestamps() string {
	ea := r.EvidenceAge
	switch {
	case !digest(ea.SnapshotAnchorID):
		return "evidenceAge.snapshotAnchorId is missing or malformed"
	case !digest(ea.RestoreResultID):
		return "evidenceAge.restoreResultId is missing or malformed"
	case ea.SnapshotCreatedAt.IsZero():
		return "evidenceAge.snapshotCreatedAt is missing"
	case ea.SnapshotAgeSeconds < 0:
		return "evidenceAge.snapshotAgeSeconds is negative"
	case ea.RestoreCompletedAt.IsZero():
		return "evidenceAge.restoreCompletedAt is missing"
	case ea.DrillStartedAt.IsZero():
		return "evidenceAge.drillStartedAt is missing"
	case ea.DrillCompletedAt.IsZero():
		return "evidenceAge.drillCompletedAt is missing"
	case ea.DrillDurationMS <= 0:
		return "evidenceAge.drillDurationMs was not measured"
	case ea.DrillCompletedAt.Before(ea.DrillStartedAt):
		return "evidenceAge.drillCompletedAt precedes drillStartedAt"
	case ea.RestoreCompletedAt.Before(ea.SnapshotCreatedAt):
		return "evidenceAge.restoreCompletedAt precedes snapshotCreatedAt"
	case ea.DrillCompletedAt.Before(ea.SnapshotCreatedAt):
		return "evidenceAge.drillCompletedAt precedes snapshotCreatedAt"
	}
	return ""
}

func (r applicationRecoveryReceiptWire) validateIsolation() string {
	iso := r.Isolation
	switch {
	case iso.CloneProject != r.Scope.CloneProject:
		return "isolation.cloneProject differs from scope.cloneProject"
	case !iso.LiveProjectUntouched:
		return "isolation.liveProjectUntouched is false"
	case !iso.LiveVolumesUnmounted:
		return "isolation.liveVolumesUnmounted is false"
	case !iso.InternalNetworksOnly:
		return "isolation.internalNetworksOnly is false"
	case !iso.MasqueradeDisabled:
		return "isolation.masqueradeDisabled is false"
	case !iso.RoutingLabelsStripped:
		return "isolation.routingLabelsStripped is false"
	case !iso.PublishedHostPortsOnlyLoopback:
		return "isolation.publishedHostPortsOnlyLoopback is false"
	case !iso.OutboundNotificationsProhibited:
		return "isolation.outboundNotificationsProhibited is false"
	case iso.OutboundNotificationsDetected:
		return "outbound notifications were detected during the isolated recovery drill"
	case !iso.CloneResourcesCleanedUp:
		return "isolation.cloneResourcesCleanedUp is false"
	}
	return ""
}

func (r applicationRecoveryReceiptWire) validateReadiness() string {
	rd := r.Readiness
	switch {
	case !rd.DatabaseReady:
		return "readiness.databaseReady is false"
	case !identifier(rd.DatabaseName):
		return "readiness.databaseName is missing or malformed"
	case rd.PublicTableCount <= 0:
		return "readiness.publicTableCount is zero"
	case !rd.EntryHTTPReady || rd.EntryHTTPStatus < 200 || rd.EntryHTTPStatus >= 400:
		return "readiness.entryHttpReady failed"
	case len(rd.DependencyChecks) == 0 || len(rd.DependencyChecks) > maxListEntries:
		return "readiness.dependencyChecks is empty or oversized"
	}
	checked := make(map[string]dependencyCheckWire, len(rd.DependencyChecks))
	for _, dc := range rd.DependencyChecks {
		if !identifier(dc.ComponentRef) || !identifier(dc.Role) || !identifier(dc.Lifecycle) || !identifier(dc.State) {
			return fmt.Sprintf("dependency check %q has missing metadata", dc.ComponentRef)
		}
		if !dc.Ready || dc.ExitCode != 0 {
			return fmt.Sprintf("dependency %q is not ready (state=%q exit=%d)", dc.ComponentRef, dc.State, dc.ExitCode)
		}
		checked[dc.ComponentRef] = dc
	}
	for _, dep := range r.Scope.VerifiedDependencies {
		if _, ok := checked[dep]; !ok {
			return fmt.Sprintf("verified dependency %q lacks a readiness check", dep)
		}
	}
	return ""
}

func (r applicationRecoveryReceiptWire) validateDataReadback() string {
	dr := r.DataReadback
	if dr.Method != readbackMethod {
		return fmt.Sprintf("dataReadback.method %q is not %s", dr.Method, readbackMethod)
	}
	if len(dr.Volumes) == 0 || len(dr.Volumes) > maxListEntries {
		return "dataReadback.volumes is empty or oversized"
	}
	restoredSet := stringSet(r.Scope.RestoredVolumes)
	byLive := make(map[string]volumeVerificationWire, len(dr.Volumes))
	for _, vol := range dr.Volumes {
		switch {
		case !identifier(vol.LiveName) || !identifier(vol.CloneName):
			return "dataReadback volume name is missing or malformed"
		case !restoredSet[vol.CloneName]:
			return fmt.Sprintf("dataReadback clone volume %q is missing from scope.restoredVolumes", vol.CloneName)
		case vol.Files <= 0 || vol.Bytes <= 0:
			return fmt.Sprintf("dataReadback volume %q has empty readback (files=%d bytes=%d)", vol.LiveName, vol.Files, vol.Bytes)
		case !digest(vol.Digest):
			return fmt.Sprintf("dataReadback volume %q lacks a sha256 digest", vol.LiveName)
		case !vol.MatchesStaged:
			return fmt.Sprintf("dataReadback volume %q did not match staged data", vol.LiveName)
		}
		if _, dup := byLive[vol.LiveName]; dup {
			return fmt.Sprintf("dataReadback volume %q is listed twice", vol.LiveName)
		}
		byLive[vol.LiveName] = vol
	}
	for _, bv := range r.Scope.BackedUpVolumes {
		if _, ok := byLive[bv]; !ok {
			return fmt.Sprintf("backed-up volume %q lacks dataReadback verification", bv)
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
	if value == "" || len(value) > 200 || value != strings.TrimSpace(value) || !utf8.ValidString(value) {
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

func pinnedImage(value string) bool {
	if !version(value) {
		return false
	}
	lower := strings.ToLower(value)
	if strings.HasSuffix(lower, ":latest") {
		return false
	}
	return true
}

func stringSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}
