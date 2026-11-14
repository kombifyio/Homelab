package guardexec

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// An update trial is supervised by a transient unit, which neither a reboot nor
// its own failure leaves behind. While a trial is open, a drop-in on the Guard
// service therefore lets the previous Guard check every start of the service
// first (AdoptTrial): a trial whose supervisor is gone is committed when the new
// Guard confirmed it, rolled back when it expired or crash-loops, and supervised
// again otherwise. The executor adopts it on each connection too, so a healthy
// Guard whose supervisor was lost does not stay in update_trial.

const (
	// trialRecoveryDropIn sorts first: drop-ins add their ExecStartPre lines in
	// name order, and the hook must run before any other step that can fail.
	trialRecoveryDropIn = "00-kombify-guard-trial.conf"
	// supervisorStartWait bounds how long Commit waits for the supervisor unit
	// it queued to run.
	supervisorStartWait = 15 * time.Second
	// trialAdoptionGrace keeps an executor connection from adopting a trial that
	// Commit is still starting in another executor instance (daemon-reload and
	// the supervisor start wait both lie inside it).
	trialAdoptionGrace = 2 * time.Minute
	// Once a failed trial left a release with no admissible fallback, the start
	// limit ends a later crash loop of it without any hook.
	recoveryStartLimit = "StartLimitIntervalSec=10min\nStartLimitBurst=3\n"
)

// TrialAdopter says who adopts a trial whose supervisor is gone.
type TrialAdopter int

const (
	// AdoptAtGuardStart runs before every start of the Guard service, as its
	// recovery hook: at boot, at the trial's own restart and at every restart of
	// a failing Guard. It decides before the new Guard runs again.
	AdoptAtGuardStart TrialAdopter = iota
	// AdoptAtExecutorConnection runs when the Guard connects to the executor. It
	// commits a confirmed trial and leaves every other decision to a supervisor.
	AdoptAtExecutorConnection
)

// AdoptTrial ends or supervises again an open trial that no supervisor watches.
func (u *Updater) AdoptTrial(ctx context.Context, by TrialAdopter) error {
	atGuardStart := by == AdoptAtGuardStart
	trial, open := u.openTrial()
	if !open {
		if !atGuardStart {
			return nil
		}
		if u.DenyMutations() {
			// The recovery drop-in replaced this hook on disk; until the next
			// reload the hook still runs and ends a crash loop at once.
			u.haltCrashLoop(ctx)
			return nil
		}
		// A hook left by a trial that already ended.
		u.disarmTrialRecovery(ctx, false)
		return nil
	}
	if !atGuardStart && u.cfg.Now().Before(trial.StartedAt.Add(trialAdoptionGrace)) {
		return nil
	}
	if u.supervisorRunning(ctx, trial.ID) {
		return nil
	}
	if installed, err := fileDigest(u.cfg.AgentPath); err == nil && installed == trial.PrevSHA256 && installed != trial.SHA256 {
		if !atGuardStart {
			// Commit may still be switching in another executor instance; only a
			// start of the Guard, which ended that executor, closes the trial.
			return nil
		}
		// Commit was interrupted before the switch: the previous Guard is installed.
		u.cfg.Log.Warn("guard_trial_adopted", "trial_id", trial.ID, "outcome", "not_started")
		u.mu.Lock()
		defer u.mu.Unlock()
		if !u.trialStillOpen(trial) {
			return nil
		}
		err := u.record(UpdateStatus{State: StateRolledBack, Version: trial.PrevVersion, PreviousVersion: trial.Version, Reason: "trial_interrupted_before_switch", TrialID: trial.ID})
		u.endUnstartedTrial(ctx, trial, false)
		return err
	}
	if u.trialConfirmed(trial) {
		u.cfg.Log.Info("guard_trial_adopted", "trial_id", trial.ID, "outcome", "confirmed")
		return u.finishTrial(ctx, trial, atGuardStart)
	}
	if atGuardStart {
		if reason := u.trialFailure(ctx, trial); reason != "" {
			u.cfg.Log.Warn("guard_trial_adopted", "trial_id", trial.ID, "outcome", "failed", "reason", reason)
			return u.failTrial(ctx, trial, reason, true)
		}
	}
	u.cfg.Log.Info("guard_trial_adopted", "trial_id", trial.ID, "outcome", "supervised_again")
	if err := u.cfg.Systemd.RunTransient(ctx, trialUnit(trial.ID), u.supervisorArgv(trial.ID, true)); err != nil {
		if u.supervisorRunning(ctx, trial.ID) {
			// Another adopter started it first.
			return nil
		}
		if atGuardStart {
			// The new Guard is about to run without anyone to restore the
			// previous one, so it does not run.
			return u.failTrial(ctx, trial, "trial_supervisor_unavailable", true)
		}
		return err
	}
	return nil
}

// haltCrashLoop stops a release a failed trial had to leave in place once it
// crash-loops.
func (u *Updater) haltCrashLoop(ctx context.Context) {
	if u.halted() {
		return
	}
	props, err := u.cfg.Systemd.Show(ctx, u.cfg.AgentUnit, "NRestarts")
	restarts, parseErr := strconv.Atoi(props["NRestarts"])
	if err != nil || parseErr != nil || restarts < trialCrashLoopRestarts {
		return
	}
	status := u.Status()
	if err := os.WriteFile(u.path(haltedFile), []byte(status.Version+"\n"), 0o600); err != nil {
		u.cfg.Log.Error("guard_halt_not_recorded", "error", err.Error())
		return
	}
	status.Reason, status.At = strings.TrimSuffix(status.Reason, "_release_halted")+"_release_halted", time.Time{}
	_ = u.record(status)
	u.stopGuard(ctx)
}

// supervisorRunning reports whether the trial's supervisor unit runs.
func (u *Updater) supervisorRunning(ctx context.Context, id string) bool {
	props, err := u.cfg.Systemd.Show(ctx, trialUnit(id), "ActiveState")
	return err == nil && unitRunning(props)
}

func unitRunning(props map[string]string) bool {
	switch props["ActiveState"] {
	case "active", "activating", "deactivating", "reloading":
		return true
	}
	return false
}

// supervisorStarted waits until the supervisor Commit queued runs. Its first act
// restarts the Guard service, which may end this executor before it returns.
func (u *Updater) supervisorStarted(ctx context.Context, id string) bool {
	unit := trialUnit(id)
	deadline := time.Now().Add(supervisorStartWait)
	for {
		props, err := u.cfg.Systemd.Show(ctx, unit, "LoadState", "ActiveState")
		switch {
		case err == nil && unitRunning(props):
			return true
		case !u.trialFileNames(id):
			// The supervisor already ran and ended the trial; anything else that
			// removed the record leaves nobody supervising the switch.
			status := u.Status()
			return status.TrialID == id && status.State != StateTrial
		case err == nil && (props["ActiveState"] == "failed" || props["LoadState"] == "not-found"):
			// Failed, or collected after it ended without ending the trial.
			return false
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (u *Updater) trialFileNames(id string) bool {
	trial, open := u.openTrial()
	return open && trial.ID == id
}

// endUnstartedTrial removes a trial nothing supervises and puts back every
// executable its switch already replaced; one that still has the previous digest
// is left alone, so a stale .prev from an earlier update is never installed. The
// previous Guard was never stopped and keeps running. The caller holds u.mu.
func (u *Updater) endUnstartedTrial(ctx context.Context, trial trialRecord, reload bool) {
	for _, target := range u.executables() {
		if digest, err := fileDigest(target); err == nil && digest == trial.PrevSHA256 {
			continue
		}
		if digest, err := fileDigest(target + ".prev"); err != nil || digest != trial.PrevSHA256 {
			u.cfg.Log.Error("guard_trial_restore_skipped", "path", target)
			continue
		}
		restore := target + ".restore"
		_ = os.Remove(restore)
		if err := os.Link(target+".prev", restore); err == nil {
			_ = os.Rename(restore, target)
		}
	}
	_ = syncDir(filepath.Dir(u.cfg.AgentPath))
	u.closeTrial()
	u.disarmTrialRecovery(ctx, reload)
}

func (u *Updater) halted() bool {
	_, err := os.Stat(u.path(haltedFile))
	return err == nil
}

func (u *Updater) trialDropInPath() string {
	return filepath.Join(u.cfg.SystemdDir, u.cfg.AgentUnit+".d", trialRecoveryDropIn)
}

// armTrialRecovery writes the Guard service's recovery hook. The hook runs as
// root ("+") from the previous Guard, the one that starts the trial, and never
// keeps the service from starting ("-"). The condition keeps a halted release
// (needRecovery) from starting at all.
func (u *Updater) armTrialRecovery(ctx context.Context) error {
	hook := append([]string{u.cfg.AgentPath + ".prev", "guard-executor", "trial-recover"}, u.layoutArgs()...)
	return u.writeGuardDropIn(ctx, "# Written by the kombify Guard executor while an update trial is open; removed when it ends.\n"+
		"[Unit]\n"+u.haltCondition()+"\n[Service]\nExecStartPre=+-"+systemdCommandLine(hook)+"\n", true)
}

// armRecoveryGuard replaces the trial hook once a failed trial had to leave its
// release in place: the previous Guard is not admissible, so nothing runs it
// again. What stays is the halt condition and a start limit.
func (u *Updater) armRecoveryGuard(ctx context.Context, reload bool) error {
	return u.writeGuardDropIn(ctx, "# Written by the kombify Guard executor after a failed update left no admissible Guard; removed by guard-executor recover.\n"+
		"[Unit]\n"+u.haltCondition()+recoveryStartLimit, reload)
}

func (u *Updater) haltCondition() string {
	return "ConditionPathExists=!" + strings.ReplaceAll(u.path(haltedFile), "%", "%%") + "\n"
}

func (u *Updater) writeGuardDropIn(ctx context.Context, text string, reload bool) error {
	path := u.trialDropInPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := writeFileAtomic(path, []byte(text), 0o644); err != nil {
		return err
	}
	if !reload {
		return nil
	}
	return u.cfg.Systemd.DaemonReload(ctx)
}

// disarmTrialRecovery removes the recovery hook. A start of the Guard service
// does not reload the unit it starts; the hook it still holds in memory finds no
// trial and does nothing.
func (u *Updater) disarmTrialRecovery(ctx context.Context, reload bool) {
	path := u.trialDropInPath()
	if err := os.Remove(path); err != nil {
		return
	}
	_ = os.Remove(filepath.Dir(path))
	if !reload {
		return
	}
	if err := u.cfg.Systemd.DaemonReload(ctx); err != nil {
		u.cfg.Log.Warn("guard_trial_hook_reload_failed", "error", err.Error())
	}
}

// systemdCommandLine renders argv for an Exec line: specifiers and variables are
// escaped, and an argument with other than path characters is quoted.
func systemdCommandLine(argv []string) string {
	words := make([]string, len(argv))
	for i, arg := range argv {
		words[i] = systemdArg(arg)
	}
	return strings.Join(words, " ")
}

func systemdArg(arg string) string {
	arg = strings.NewReplacer("%", "%%", "$", "$$").Replace(arg)
	if arg != "" && strings.Trim(arg, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-") == "" {
		return arg
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(arg) + `"`
}
