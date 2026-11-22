package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"runtime"

	"github.com/kombifyio/homelab/pkg/guardexec"
	"github.com/kombifyio/homelab/pkg/ril/nodeproof"
)

// isGuardExecutorMode reports whether the binary was invoked as
// `homelab guard-executor`: the privileged half of kombify Guard.
func isGuardExecutorMode(args []string) bool { return len(args) > 1 && args[1] == "guard-executor" }

// runGuardExecutor runs one of the privileged executor's subcommands:
//
//	serve    handle one connection from the socket unit (stdin is the socket)
//	trial    supervise the first run of an updated Guard (--resume: take over a
//	         trial whose supervisor is gone)
//	trial-recover  the Guard service's recovery hook while an update trial is open
//	layout-trial  supervise the first run of the split layout
//	recover  clear the recovery flag after an operator restored a verified Guard
//	status   print the executor's status as JSON
//	admission-check  --manifest F --bundle F: print, as JSON, whether this host would
//	         admit the release a signed manifest describes (root, read-only)
func runGuardExecutor(ctx context.Context, args []string) error {
	if runtime.GOOS != "linux" {
		return errors.New("the privileged executor is only available on Linux")
	}
	if len(args) == 0 {
		return errors.New("usage: homelab guard-executor serve|trial|trial-recover|recover|status|admission-check")
	}
	flags := flag.NewFlagSet("homelab guard-executor "+args[0], flag.ContinueOnError)
	opts := guardexec.Options{Version: version}
	flags.StringVar(&opts.StateDir, "state-dir", envDefault("HOMELAB_GUARD_EXECUTOR_STATE_DIR", ""), "root-only executor state directory")
	flags.StringVar(&opts.HandoffDir, "handoff-dir", envDefault("HOMELAB_GUARD_HANDOFF_DIR", ""), "directory shared with the unprivileged daemon")
	flags.StringVar(&opts.AgentPath, "agent-path", envDefault("HOMELAB_AGENT_EXECUTABLE_PATH", ""), "installed Guard executable")
	flags.StringVar(&opts.OperationsPath, "operations-path", envDefault("HOMELAB_OPERATIONS_EXECUTABLE_PATH", ""), "installed StackKits operations executable")
	flags.BoolVar(&opts.GuardOnly, "guard-only", false, "trial: this host updates only the Guard executable (a Proxmox substrate Guard)")
	flags.StringVar(&opts.GuardUser, "guard-user", envDefault("HOMELAB_GUARD_USER", ""), "the daemon's system user")
	trialID := flags.String("id", "", "update trial ID")
	resume := flags.Bool("resume", false, "trial: take over a trial whose supervisor is gone, without restarting the Guard")
	manifestPath := flags.String("manifest", "", "admission-check: signed release manifest (release-manifest.json)")
	bundlePath := flags.String("bundle", "", "admission-check: its Sigstore bundle (release-manifest.sigstore.json)")
	socket := flags.String("socket", envDefault("HOMELAB_GUARD_EXECUTOR_SOCKET", guardexec.DefaultSocket), "executor socket for status")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	opts.Log = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})).With("service", "homelab-guard-executor", "version", version)
	switch args[0] {
	case "ril-keys-refresh":
		// A host that never applied the hosted profile has nothing to refresh:
		// it dials nothing and the unit ends clean. Every other admission
		// failure still fails the unit.
		if err := nodeproof.RequireHostedProfile(); errors.Is(err, nodeproof.ErrHostedProfileNotApplied) {
			opts.Log.Info("ril_keys_refresh_not_applicable", "reason", "no hosted profile is applied on this host")
			return nil
		} else if err != nil {
			return err
		}
		if _, err := os.Stat("/etc/homelab/ril-hosted-enrollment"); errors.Is(err, os.ErrNotExist) {
			if err := bootstrapExistingHostedRIL(ctx); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		return nodeproof.RefreshPurposeKeys(ctx, "/etc/homelab")
	case "serve":
		conn, err := guardexec.ConnFromStdin()
		if err != nil {
			return err
		}
		return guardexec.ServeConnection(ctx, conn, opts)
	case "trial":
		if *trialID == "" {
			return errors.New("--id is required")
		}
		return guardexec.RunTrial(ctx, opts, *trialID, *resume)
	case "trial-recover":
		return guardexec.RecoverTrial(ctx, opts)
	case "layout-trial":
		if *trialID == "" {
			return errors.New("--id is required")
		}
		return guardexec.RunLayoutTrial(ctx, opts, layoutTrialOptions(opts), *trialID, describeLayoutBudget(opts))
	case "recover":
		return guardexec.Recover(opts)
	case "admission-check":
		decision, err := guardexec.CheckManifestAdmission(opts, *manifestPath, *bundlePath)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(decision)
	case "status":
		var status guardexec.Status
		if err := (&guardexec.Client{Socket: *socket}).CallInto(ctx, guardexec.OpStatus, nil, &status); err != nil {
			return fmt.Errorf("the privileged executor is not reachable: %w", err)
		}
		return json.NewEncoder(os.Stdout).Encode(status)
	default:
		return fmt.Errorf("unknown guard-executor command %q", args[0])
	}
}
