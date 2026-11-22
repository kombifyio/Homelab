package guardexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kombifyio/homelab/pkg/guardrelease"
)

// Update states. They are reported to the control plane and shown to the owner.
const (
	StateStaged = "update_staged"
	StateTrial  = "update_trial"
	// StateDenied records that a candidate release was refused; the running
	// Guard is unchanged and keeps working.
	StateDenied         = "update_denied"
	StateCommitted      = "update_committed"
	StateRolledBack     = "update_rolled_back"
	StateRecoveryNeeded = "recovery-needed"
)

const (
	maxManifestFile = guardrelease.MaxManifestBytes
	maxBundleFile   = guardrelease.MaxBundleBytes
	maxArtifactFile = 512 << 20

	// DefaultTrialBudget is how long a new Guard has to complete an
	// authenticated round trip before the previous one is restored.
	DefaultTrialBudget = 5 * time.Minute
	// failedCandidateCooldown keeps a candidate that failed its trial from being
	// staged again immediately.
	failedCandidateCooldown = 24 * time.Hour
	// deniedCandidateCooldown keeps a candidate Stage refused after it was
	// downloaded (its signature, a revoked digest) from being downloaded again
	// every convergence cycle. A new release has a new digest and is not held.
	deniedCandidateCooldown = 6 * time.Hour
	// trialCrashLoopRestarts is how many restarts during a trial count as a
	// crash loop; waiting out the budget would only prolong the outage.
	trialCrashLoopRestarts = 3

	statusFile        = "status.json"
	trialFile         = "trial.json"
	stagedFile        = "staged.json"
	manifestFile      = "manifest.json"
	manifestBundle    = "manifest.sigstore.json"
	floorFile         = "manifest-floor.json"
	failedFile        = "failed-candidates.json"
	deniedFile        = "denied-candidates.json"
	denyMutationsFile = "deny-mutations"
	// haltedFile marks a release that cannot start with no admissible previous
	// Guard: the Guard service stays stopped until an operator recovers it.
	haltedFile = "halted-release"

	// Files shared with the unprivileged daemon through the handoff directory.
	HandoffStatusFile     = "update-status.json"
	HandoffTrialFile      = "update-trial.json"
	HandoffTrialReadyFile = "update-trial-ready.json"
)

// UpdateStatus is the recorded state of the latest update.
type UpdateStatus struct {
	State           string    `json:"state"`
	Version         string    `json:"version,omitempty"`
	PreviousVersion string    `json:"previous_version,omitempty"`
	Reason          string    `json:"reason,omitempty"`
	TrialID         string    `json:"trial_id,omitempty"`
	At              time.Time `json:"at"`
}

// TrialReady is written by the unprivileged daemon once the new Guard completed
// an authenticated round trip with the control plane.
type TrialReady struct {
	TrialID string    `json:"trial_id"`
	Version string    `json:"version"`
	At      time.Time `json:"at"`
}

type trialRecord struct {
	ID          string    `json:"id"`
	Version     string    `json:"version"`
	SHA256      string    `json:"sha256"`
	PrevVersion string    `json:"prev_version"`
	PrevSHA256  string    `json:"prev_sha256"`
	StartedAt   time.Time `json:"started_at"`
	Deadline    time.Time `json:"deadline"`
}

type stagedRecord struct {
	Version  string    `json:"version"`
	SHA256   string    `json:"sha256"`
	StagedAt time.Time `json:"staged_at"`
}

// ReleaseVerifier authenticates release manifests and artifacts. The production
// implementation is *guardrelease.Verifier; the interface lets tests run the
// updater's manifest and floor handling without a Sigstore deployment.
type ReleaseVerifier interface {
	VerifyManifest(raw, bundleJSON []byte) (*guardrelease.Manifest, error)
	VerifyArtifact(manifest *guardrelease.Manifest, goos, goarch, path string, bundleJSON []byte, installedVersion string) (guardrelease.Evidence, error)
}

// Systemd is the service manager the updater drives.
type Systemd interface {
	Restart(ctx context.Context, unit string) error
	// Show returns the requested properties of a unit.
	Show(ctx context.Context, unit string, properties ...string) (map[string]string, error)
	// RunTransient starts argv in its own transient unit, outside the caller's
	// control group, so it survives the restart it supervises.
	RunTransient(ctx context.Context, unit string, argv []string) error
	// StopNoBlock queues a stop of a unit without waiting for it, so a process
	// of that unit may ask for its own stop.
	StopNoBlock(ctx context.Context, unit string) error
	// DaemonReload makes the service manager read changed unit files.
	DaemonReload(ctx context.Context) error
}

// UpdaterConfig wires an Updater.
type UpdaterConfig struct {
	// StateDir is root-only state: verified manifest, trial, staged release,
	// failed candidates and the recovery flag.
	StateDir string
	// HandoffDir is written by the daemon and read by the executor as untrusted
	// input; the executor also publishes non-secret update status there.
	HandoffDir     string
	AgentPath      string
	OperationsPath string
	// GuardOnly converges the Guard executable alone: a Proxmox substrate Guard
	// has no StackKits operations executable and must not gain one.
	GuardOnly bool
	// AgentUnit is the service restarted to run the new Guard.
	AgentUnit string
	// SystemdDir holds the drop-in that arms trial recovery on AgentUnit.
	SystemdDir string
	// CurrentVersion is the version of the Guard that is running now.
	CurrentVersion string
	GOOS, GOARCH   string
	Verifier       ReleaseVerifier
	// AllowUnsigned admits an artifact without release evidence. It is read from
	// the executor's own environment, never from a request, and exists only for
	// unsigned local and self-hosted builds.
	AllowUnsigned bool
	Systemd       Systemd
	TrialBudget   time.Duration
	Now           func() time.Time
	Log           *slog.Logger
	// TrialPoll is how often the supervisor looks for readiness.
	TrialPoll time.Duration
}

// Updater stages, switches to and supervises signed Guard releases.
type Updater struct {
	cfg UpdaterConfig
	mu  sync.Mutex
	// floorMu serializes the read-modify-write of the manifest floor: manifests
	// are authenticated for updates (under mu) and for runtime installs (not).
	floorMu sync.Mutex
}

// NewUpdater validates cfg and returns an Updater.
func NewUpdater(cfg UpdaterConfig) (*Updater, error) {
	if cfg.GuardOnly {
		cfg.OperationsPath = ""
	}
	if cfg.StateDir == "" || cfg.HandoffDir == "" || cfg.AgentPath == "" || (cfg.OperationsPath == "" && !cfg.GuardOnly) {
		return nil, errors.New("updater needs a state directory, a handoff directory and both executable paths")
	}
	if cfg.AgentUnit == "" {
		cfg.AgentUnit = AgentUnit
	}
	if cfg.SystemdDir == "" {
		cfg.SystemdDir = "/etc/systemd/system"
	}
	if cfg.GOOS == "" {
		cfg.GOOS = runtime.GOOS
	}
	if cfg.GOARCH == "" {
		cfg.GOARCH = runtime.GOARCH
	}
	if cfg.TrialBudget <= 0 {
		cfg.TrialBudget = DefaultTrialBudget
	}
	if cfg.TrialPoll <= 0 {
		cfg.TrialPoll = 2 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(cfg.StateDir, 0o700); err != nil {
		return nil, err
	}
	return &Updater{cfg: cfg}, nil
}

func (u *Updater) path(name string) string { return filepath.Join(u.cfg.StateDir, name) }

// executables lists what an update replaces, the Guard executable first.
func (u *Updater) executables() []string {
	if u.cfg.GuardOnly {
		return []string{u.cfg.AgentPath}
	}
	return []string{u.cfg.AgentPath, u.cfg.OperationsPath}
}

// DenyMutations reports whether a failed update left no admissible fallback.
func (u *Updater) DenyMutations() bool {
	_, err := os.Stat(u.path(denyMutationsFile))
	return err == nil
}

// Recover clears the recovery flag after an operator restored a verified Guard.
func (u *Updater) Recover() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := os.Remove(u.path(denyMutationsFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// The halt of a release that could not start ends with the operator's recovery.
	if err := os.Remove(u.path(haltedFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	u.disarmTrialRecovery(context.Background(), true)
	return u.record(UpdateStatus{State: StateCommitted, Version: u.cfg.CurrentVersion, Reason: "operator_recovery", At: u.cfg.Now()})
}

// Status is the recorded update state.
func (u *Updater) Status() UpdateStatus {
	raw, err := os.ReadFile(u.path(statusFile))
	if err != nil {
		return UpdateStatus{}
	}
	var status UpdateStatus
	if json.Unmarshal(raw, &status) != nil {
		return UpdateStatus{}
	}
	return status
}

func (u *Updater) record(status UpdateStatus) error {
	if status.At.IsZero() {
		status.At = u.cfg.Now()
	}
	if err := writeJSONAtomic(u.path(statusFile), status, 0o600); err != nil {
		return err
	}
	// The daemon reports this status; it holds nothing secret.
	if err := writeJSONAtomic(filepath.Join(u.cfg.HandoffDir, HandoffStatusFile), status, 0o644); err != nil {
		u.cfg.Log.Warn("guard_update_status_not_published", "error", err.Error())
	}
	return nil
}

// Stage verifies a release from the handoff directory and stages it beside the
// installed executables. Nothing runs from the staged files yet.
func (u *Updater) Stage(_ context.Context, params StageParams) (StageResult, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	workDir := u.path("stage")
	if err := os.RemoveAll(workDir); err != nil {
		return StageResult{}, err
	}
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return StageResult{}, err
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	privateArtifact := filepath.Join(workDir, "artifact")
	digest, _, err := copyHandoff(u.cfg.HandoffDir, params.ArtifactFile, privateArtifact, maxArtifactFile)
	if err != nil {
		return StageResult{}, err
	}
	version, err := u.authenticate(params, privateArtifact, digest)
	if err != nil {
		return StageResult{}, u.recordDenial(u.rememberDenial(digest, err))
	}
	if failedAt, failed := u.failedCandidate(digest); failed && u.cfg.Now().Sub(failedAt) < failedCandidateCooldown {
		return StageResult{}, u.recordDenial(deny(CodeNotAllowed, "this release failed its trial at %s and is not staged again yet", failedAt.UTC().Format(time.RFC3339)))
	}
	if current, err := fileDigest(u.cfg.AgentPath); err == nil && current == digest {
		return StageResult{Version: version, SHA256: digest, AlreadyCurrent: true}, nil
	}
	for _, target := range u.executables() {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return StageResult{}, err
		}
		if err := copyExecutable(privateArtifact, target+".next"); err != nil {
			return StageResult{}, fmt.Errorf("stage %s: %w", target, err)
		}
	}
	if err := writeJSONAtomic(u.path(stagedFile), stagedRecord{Version: version, SHA256: digest, StagedAt: u.cfg.Now()}, 0o600); err != nil {
		return StageResult{}, err
	}
	_ = u.record(UpdateStatus{State: StateStaged, Version: version, PreviousVersion: u.cfg.CurrentVersion})
	return StageResult{Version: version, SHA256: digest}, nil
}

// authenticate applies the release policy to a private copy of the artifact and
// returns its version. The verified manifest is remembered: it is the evidence
// that later decides whether the previous executable is still admissible.
func (u *Updater) authenticate(params StageParams, artifact, digest string) (string, error) {
	if params.ManifestFile == "" && params.ManifestBundleFile == "" && params.ArtifactBundleFile == "" {
		if !u.cfg.AllowUnsigned {
			return "", deny(guardrelease.CodeEvidenceMissing, "the release has no signature evidence and this host admits only signed Guard releases")
		}
		if _, ok := guardrelease.CompareVersions(params.Version, params.Version); !ok {
			return "", deny(CodeInvalidRequest, "an unsigned release needs an explicit X.Y.Z version")
		}
		// Unsigned local builds still obey the downgrade guard and the floor: an
		// unsigned artifact must not roll the host back below what it runs or
		// what it has ever revoked.
		if order, ok := guardrelease.CompareVersions(params.Version, u.cfg.CurrentVersion); ok && order < 0 {
			return "", deny(CodeNotAllowed, "an unsigned release cannot be older than the running Guard %s", u.cfg.CurrentVersion)
		}
		floor, err := u.loadFloor()
		if err != nil {
			return "", err
		}
		if denial := floor.Admit(params.Version, digest); denial != nil {
			return "", &Denial{Code: denial.Code, Message: denial.Message}
		}
		u.cfg.Log.Warn("guard_unsigned_update_admitted", "version", params.Version, "sha256", digest)
		return params.Version, nil
	}
	manifest, err := u.AuthenticateManifest(params.ManifestFile, params.ManifestBundleFile)
	if err != nil {
		return "", err
	}
	artifactSig, err := readHandoff(u.cfg.HandoffDir, params.ArtifactBundleFile, maxBundleFile)
	if err != nil {
		return "", err
	}
	evidence, err := u.cfg.Verifier.VerifyArtifact(manifest, u.cfg.GOOS, u.cfg.GOARCH, artifact, artifactSig, u.cfg.CurrentVersion)
	if err != nil {
		return "", asDenial(err)
	}
	if evidence.SHA256 != digest {
		return "", deny(CodeFailed, "the verified artifact is not the copied artifact")
	}
	// The manifest alone does not decide: every revocation and the highest
	// minimum version this host has ever accepted apply too.
	floor, err := u.loadFloor()
	if err != nil {
		return "", err
	}
	if denial := floor.Admit(evidence.Version, evidence.SHA256); denial != nil {
		return "", &Denial{Code: denial.Code, Message: denial.Message}
	}
	return evidence.Version, nil
}

// Commit switches the installed executables to the staged release and starts a
// supervised trial. The previous executables are kept as .prev (hard links) so
// the switch is an atomic rename per file.
func (u *Updater) Commit(ctx context.Context, params CommitParams) (CommitResult, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	raw, err := os.ReadFile(u.path(stagedFile))
	if err != nil {
		return CommitResult{}, deny(CodeNotAllowed, "no release is staged")
	}
	var staged stagedRecord
	if err := json.Unmarshal(raw, &staged); err != nil || staged.Version != params.Version || staged.SHA256 != params.SHA256 {
		return CommitResult{}, deny(CodeNotAllowed, "the staged release is not the one named")
	}
	floor, err := u.loadFloor()
	if err != nil {
		return CommitResult{}, err
	}
	if denial := floor.Admit(staged.Version, staged.SHA256); denial != nil {
		return CommitResult{}, &Denial{Code: denial.Code, Message: denial.Message}
	}
	for _, target := range u.executables() {
		if digest, err := fileDigest(target + ".next"); err != nil || digest != staged.SHA256 {
			return CommitResult{}, deny(CodeNotAllowed, "the staged file for %s changed after verification", target)
		}
	}
	prevDigest, err := fileDigest(u.cfg.AgentPath)
	if err != nil {
		return CommitResult{}, fmt.Errorf("read the installed executable: %w", err)
	}
	now := u.cfg.Now()
	trial := trialRecord{
		ID: fmt.Sprintf("%d", now.UnixNano()), Version: staged.Version, SHA256: staged.SHA256,
		PrevVersion: u.cfg.CurrentVersion, PrevSHA256: prevDigest, StartedAt: now, Deadline: now.Add(u.cfg.TrialBudget),
	}
	if err := writeJSONAtomic(u.path(trialFile), trial, 0o600); err != nil {
		return CommitResult{}, err
	}
	if err := writeJSONAtomic(filepath.Join(u.cfg.HandoffDir, HandoffTrialFile), trial, 0o644); err != nil {
		return CommitResult{}, err
	}
	_ = os.Remove(filepath.Join(u.cfg.HandoffDir, HandoffTrialReadyFile))
	// The recovery hook is armed before anything is switched: from here on a
	// reboot or a lost supervisor is adopted by the next start of the Guard.
	if err := u.armTrialRecovery(ctx); err != nil {
		u.endUnstartedTrial(ctx, trial, true)
		return CommitResult{}, fmt.Errorf("arm the update trial recovery: %w", err)
	}
	if err := u.switchTo(".next"); err != nil {
		u.endUnstartedTrial(ctx, trial, true)
		return CommitResult{}, err
	}
	_ = u.record(UpdateStatus{State: StateTrial, Version: staged.Version, PreviousVersion: u.cfg.CurrentVersion, TrialID: trial.ID})
	err = u.cfg.Systemd.RunTransient(ctx, trialUnit(trial.ID), u.supervisorArgv(trial.ID, false))
	if err != nil && u.supervisorRunning(ctx, trial.ID) {
		// An adoption started the supervisor first.
		err = nil
	}
	if err == nil && !u.supervisorStarted(ctx, trial.ID) {
		err = errors.New("the supervisor unit did not start")
	}
	if err != nil {
		// Nothing supervises the switch, so it is not kept: the previous Guard was
		// never stopped and keeps running.
		_ = u.record(UpdateStatus{State: StateRolledBack, Version: u.cfg.CurrentVersion, PreviousVersion: staged.Version, Reason: "trial_supervisor_unavailable", TrialID: trial.ID})
		u.endUnstartedTrial(ctx, trial, true)
		return CommitResult{}, fmt.Errorf("start the update trial supervisor: %w", err)
	}
	return CommitResult{TrialID: trial.ID}, nil
}

// switchTo makes suffix the installed executable, keeping the running one as
// .prev. Hard links and renames keep every path populated at all times.
func (u *Updater) switchTo(suffix string) error {
	// The Guard executable moves last, after everything else did.
	for _, target := range slices.Backward(u.executables()) {
		prev := target + ".prev"
		_ = os.Remove(prev)
		if err := os.Link(target, prev); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("keep the previous %s: %w", filepath.Base(target), err)
		}
		if err := os.Rename(target+suffix, target); err != nil {
			return fmt.Errorf("activate %s: %w", filepath.Base(target), err)
		}
	}
	return syncDir(filepath.Dir(u.cfg.AgentPath))
}

// switchBack restores the .prev executables.
func (u *Updater) switchBack() error {
	var first error
	for _, target := range u.executables() {
		prev := target + ".prev"
		if _, err := os.Stat(prev); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		restore := target + ".restore"
		_ = os.Remove(restore)
		if err := os.Link(prev, restore); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		if err := os.Rename(restore, target); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Describe summarizes the update state for status.
func (u *Updater) Describe() Status {
	status := u.Status()
	return Status{
		Ready: true, Privileged: os.Geteuid() == 0,
		UpdateState: status.State, UpdateVersion: status.Version, UpdateReason: strings.TrimSpace(status.Reason),
		DenyMutations: u.DenyMutations(), AdmitsUnsigned: u.cfg.AllowUnsigned,
	}
}

// SyncOperations makes the operations executable byte-identical to the Guard
// executable. They are one binary; a difference means a half-finished update.
func (u *Updater) SyncOperations() error {
	if u.cfg.GuardOnly {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	agent, err := fileDigest(u.cfg.AgentPath)
	if err != nil {
		return err
	}
	if ops, err := fileDigest(u.cfg.OperationsPath); err == nil && ops == agent {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(u.cfg.OperationsPath), 0o755); err != nil {
		return err
	}
	return copyExecutable(u.cfg.AgentPath, u.cfg.OperationsPath)
}
