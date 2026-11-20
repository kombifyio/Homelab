package guardexec

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// RunTrial supervises the first run of a new Guard. It restarts the Guard
// service and waits for the Guard to report an authenticated round trip. If that
// does not happen within the budget, or the service fails repeatedly, the
// previous executable is restored when it is still admissible; otherwise the
// host is marked as needing recovery and mutations are denied. A resumed
// supervisor takes over a trial whose supervisor is gone (AdoptTrial): it
// restarts the Guard only when the running one is not the candidate.
func (u *Updater) RunTrial(ctx context.Context, id string, resume bool) error {
	trial, ok := u.openTrial()
	if !ok || trial.ID != id {
		return errors.New("the trial record does not name this trial")
	}
	if !resume || u.guardRunsReplacedExecutable(ctx, trial) {
		if err := u.cfg.Systemd.Restart(ctx, u.cfg.AgentUnit); err != nil {
			u.cfg.Log.Error("guard_trial_restart_failed", "error", err.Error())
		}
	}
	ticker := time.NewTicker(u.cfg.TrialPoll)
	defer ticker.Stop()
	for {
		if u.trialConfirmed(trial) {
			return u.finishTrial(ctx, trial, false)
		}
		if reason := u.trialFailure(ctx, trial); reason != "" {
			return u.failTrial(ctx, trial, reason, false)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// guardRunsReplacedExecutable reports whether the running Guard is not the
// candidate: a supervisor lost before its restart left the previous Guard
// running. A Guard that is starting has no main process yet and is the candidate.
func (u *Updater) guardRunsReplacedExecutable(ctx context.Context, trial trialRecord) bool {
	props, err := u.cfg.Systemd.Show(ctx, u.cfg.AgentUnit, "MainPID")
	pid := props["MainPID"]
	if err != nil || pid == "" || pid == "0" {
		return false
	}
	running, err := fileDigest("/proc/" + pid + "/exe")
	return err == nil && running != trial.SHA256
}

// openTrial reads the trial in progress, if there is one.
func (u *Updater) openTrial() (trialRecord, bool) {
	raw, err := os.ReadFile(u.path(trialFile))
	if err != nil {
		return trialRecord{}, false
	}
	var trial trialRecord
	if json.Unmarshal(raw, &trial) != nil || trial.ID == "" {
		return trialRecord{}, false
	}
	return trial, true
}

// trialStillOpen reports whether trial is still the trial in progress: another
// process (the supervisor, a start of the Guard, an executor connection) may
// have ended it meanwhile.
func (u *Updater) trialStillOpen(trial trialRecord) bool {
	current, ok := u.openTrial()
	return ok && current.ID == trial.ID
}

func (u *Updater) trialConfirmed(trial trialRecord) bool {
	raw, err := os.ReadFile(filepath.Join(u.cfg.HandoffDir, HandoffTrialReadyFile))
	if err != nil {
		return false
	}
	var ready TrialReady
	return json.Unmarshal(raw, &ready) == nil && ready.TrialID == trial.ID && ready.Version == trial.Version && !ready.At.Before(trial.StartedAt)
}

func (u *Updater) trialFailure(ctx context.Context, trial trialRecord) string {
	if !u.cfg.Now().Before(trial.Deadline) {
		return "trial_not_confirmed_within_budget"
	}
	props, err := u.cfg.Systemd.Show(ctx, u.cfg.AgentUnit, "NRestarts", "ActiveState")
	if err == nil && props["ActiveState"] == "failed" {
		return "service_failed"
	}
	if restarts, parseErr := strconv.Atoi(props["NRestarts"]); err == nil && parseErr == nil && restarts >= trialCrashLoopRestarts {
		return "service_crash_loop"
	}
	return ""
}

// releaseCannotRun reports whether a trial failure means the new Guard does not
// stay up at all, rather than running without confirming.
func releaseCannotRun(reason string) bool {
	return reason == "service_failed" || reason == "service_crash_loop"
}

// finishTrial commits a confirmed trial. atGuardStart is set when a start of the
// Guard service itself ends the trial: it must not reload the unit it starts.
func (u *Updater) finishTrial(ctx context.Context, trial trialRecord, atGuardStart bool) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.trialStillOpen(trial) {
		return nil
	}
	err := u.record(UpdateStatus{State: StateCommitted, Version: trial.Version, PreviousVersion: trial.PrevVersion, TrialID: trial.ID})
	_ = os.Remove(u.path(stagedFile))
	u.closeTrial()
	u.disarmTrialRecovery(ctx, !atGuardStart)
	return err
}

// closeTrial removes the trial record. It is the last step of every outcome: a
// process that ends before it leaves the trial open for the next adoption.
func (u *Updater) closeTrial() {
	_ = os.Remove(filepath.Join(u.cfg.HandoffDir, HandoffTrialFile))
	_ = os.Remove(u.path(trialFile))
}

// failTrial restores the previous Guard, or marks the host as needing recovery
// when it is not admissible. The outcome is recorded before the trial record is
// removed and before the Guard service is restarted: the restart may end the
// calling process. A start of the Guard service (atGuardStart) restores the
// executables in place and lets that start run the previous Guard instead of
// restarting the unit it is starting.
func (u *Updater) failTrial(ctx context.Context, trial trialRecord, reason string, atGuardStart bool) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.trialStillOpen(trial) {
		return nil
	}
	u.cfg.Log.Error("guard_trial_failed", "version", trial.Version, "reason", reason)
	u.markFailed(trial.SHA256)
	problem := u.fallbackProblem(trial)
	if problem == "" {
		if err := u.switchBackForRollback(); err != nil {
			problem = "rollback_failed"
		}
	}
	if problem != "" {
		return u.needRecovery(ctx, trial, problem, releaseCannotRun(reason), atGuardStart)
	}
	err := u.record(UpdateStatus{State: StateRolledBack, Version: trial.PrevVersion, PreviousVersion: trial.Version, Reason: reason, TrialID: trial.ID})
	u.closeTrial()
	u.disarmTrialRecovery(ctx, !atGuardStart)
	if atGuardStart {
		return err
	}
	if restartErr := u.cfg.Systemd.Restart(ctx, u.cfg.AgentUnit); restartErr != nil {
		u.cfg.Log.Error("guard_rollback_restart_failed", "error", restartErr.Error())
	}
	return err
}

// needRecovery leaves the failed release in place because nothing admissible can
// replace it, and denies mutations until an operator recovers the host. A Guard
// that runs without confirming stays up for diagnosis; one that does not stay up
// is halted: stopped instead of restarting forever. The recovery drop-in that
// replaces the trial hook keeps a halted release from starting, after a reboot
// too, and ends a later crash loop through the service's start limit, without
// running the previous Guard again.
func (u *Updater) needRecovery(ctx context.Context, trial trialRecord, problem string, halt, atGuardStart bool) error {
	if err := os.WriteFile(u.path(denyMutationsFile), []byte(problem+"\n"), 0o600); err != nil {
		return err
	}
	if halt {
		if err := os.WriteFile(u.path(haltedFile), []byte(trial.Version+"\n"), 0o600); err != nil {
			return err
		}
		problem += "_release_halted"
	}
	err := u.record(UpdateStatus{State: StateRecoveryNeeded, Version: trial.Version, PreviousVersion: trial.PrevVersion, Reason: problem, TrialID: trial.ID})
	if armErr := u.armRecoveryGuard(ctx, !atGuardStart); armErr != nil {
		u.cfg.Log.Error("guard_recovery_guard_not_armed", "error", armErr.Error())
	}
	u.closeTrial()
	if halt {
		u.stopGuard(ctx)
	}
	return err
}

func (u *Updater) stopGuard(ctx context.Context) {
	if err := u.cfg.Systemd.StopNoBlock(ctx, u.cfg.AgentUnit); err != nil {
		u.cfg.Log.Error("guard_halt_stop_failed", "error", err.Error())
	}
}

func (u *Updater) switchBackForRollback() error { return u.switchBack() }

// fallbackProblem returns why the previous executable may not run again, or ""
// when it is intact and still admissible. A revoked or below-minimum binary is
// never re-admitted, even to recover from a failed update.
func (u *Updater) fallbackProblem(trial trialRecord) string {
	digest, err := fileDigest(u.cfg.AgentPath + ".prev")
	if err != nil || digest != trial.PrevSHA256 {
		return "previous_executable_missing_or_changed"
	}
	if !u.cfg.GuardOnly {
		if opsDigest, err := fileDigest(u.cfg.OperationsPath + ".prev"); err != nil || opsDigest != trial.PrevSHA256 {
			return "previous_operations_executable_missing_or_changed"
		}
	}
	floor, err := u.loadFloor()
	if err != nil {
		return "manifest_floor_unreadable"
	}
	if denial := floor.Admit(trial.PrevVersion, trial.PrevSHA256); denial != nil {
		return "previous_executable_not_admissible_" + denial.Code
	}
	return ""
}

// trialUnit names the transient unit of a trial's supervisor.
func trialUnit(id string) string { return "homelab-guard-trial-" + id }

// supervisorArgv runs the supervisor from the previous Guard, the one that
// started the trial. Every path is explicit: the supervisor also runs from the
// Guard service's recovery hook, which has none of the executor's environment.
func (u *Updater) supervisorArgv(id string, resume bool) []string {
	argv := append([]string{u.cfg.AgentPath + ".prev", "guard-executor", "trial", "--id", id}, u.layoutArgs()...)
	if resume {
		argv = append(argv, "--resume")
	}
	return argv
}

func (u *Updater) layoutArgs() []string {
	args := []string{"--state-dir", u.cfg.StateDir, "--handoff-dir", u.cfg.HandoffDir, "--agent-path", u.cfg.AgentPath}
	if u.cfg.GuardOnly {
		return append(args, "--guard-only")
	}
	return append(args, "--operations-path", u.cfg.OperationsPath)
}

func (u *Updater) failedCandidate(digest string) (time.Time, bool) {
	var failed map[string]time.Time
	raw, err := os.ReadFile(u.path(failedFile))
	if err != nil || json.Unmarshal(raw, &failed) != nil {
		return time.Time{}, false
	}
	at, ok := failed[digest]
	return at, ok
}

// deniedRecord is a candidate digest Stage refused on policy after it arrived.
type deniedRecord struct {
	At      time.Time `json:"at"`
	Code    string    `json:"code"`
	Message string    `json:"message"`
}

func (u *Updater) deniedCandidate(digest string) (deniedRecord, bool) {
	var denied map[string]deniedRecord
	raw, err := os.ReadFile(u.path(deniedFile))
	if err != nil || json.Unmarshal(raw, &denied) != nil {
		return deniedRecord{}, false
	}
	record, ok := denied[digest]
	return record, ok
}

// rememberDenial records a policy refusal of digest, so the next cycles refuse
// it before downloading it again, and returns err unchanged. A failure of this
// host (an unreadable floor, a busy executor) is not a decision about the
// release and is not recorded.
func (u *Updater) rememberDenial(digest string, err error) error {
	var denial *Denial
	if digest == "" || !errors.As(err, &denial) {
		return err
	}
	switch denial.Code {
	case CodeFailed, CodeUnavailable, CodeRecoveryNeeded:
		return err
	}
	denied := map[string]deniedRecord{}
	if raw, readErr := os.ReadFile(u.path(deniedFile)); readErr == nil {
		_ = json.Unmarshal(raw, &denied)
	}
	now := u.cfg.Now()
	for key, record := range denied {
		if now.Sub(record.At) > deniedCandidateCooldown {
			delete(denied, key)
		}
	}
	message := denial.Message
	if len(message) > 256 {
		message = strings.ToValidUTF8(message[:256], "")
	}
	denied[digest] = deniedRecord{At: now, Code: denial.Code, Message: message}
	_ = writeJSONAtomic(u.path(deniedFile), denied, 0o600)
	return err
}

func (u *Updater) markFailed(digest string) {
	failed := map[string]time.Time{}
	if raw, err := os.ReadFile(u.path(failedFile)); err == nil {
		_ = json.Unmarshal(raw, &failed)
	}
	failed[digest] = u.cfg.Now()
	for key, at := range failed {
		if u.cfg.Now().Sub(at) > 30*24*time.Hour {
			delete(failed, key)
		}
	}
	_ = writeJSONAtomic(u.path(failedFile), failed, 0o600)
}
