package guardexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	agentpkg "github.com/kombifyio/homelab/pkg/agent"
	"github.com/kombifyio/homelab/pkg/api/agentpb"
)

// powerEnabledEnv is the executor-side admission of the UPS power sequence. The
// Guard layout writes it only when the owner turned UPS protection on; without
// it every power request is refused, so a compromised daemon cannot power the
// host off through this channel.
const powerEnabledEnv = "HOMELAB_GUARD_EXECUTOR_POWER"

// The executor's own admission of what the daemon may stop. The daemon's
// policy is not an authority the privileged side can trust; the layout writes
// these root-owned lists when the owner enables protection.
const (
	powerServicesEnv = "HOMELAB_GUARD_EXECUTOR_POWER_SERVICES"
	powerUnitsEnv    = "HOMELAB_GUARD_EXECUTOR_POWER_STORAGE_UNITS"
)

const (
	powerUnitTimeout = 45 * time.Second
	powerStepDetail  = 256
)

var (
	powerServiceKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	powerMountUnitPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.-]{0,127}\.(mount|swap)$`)
)

var errPowerDisabled = errors.New("power protection is not enabled on this executor")

func powerEnabled() bool {
	return envTrue(strings.TrimSpace(os.Getenv(powerEnabledEnv)))
}

func admittedPowerList(name string) map[string]struct{} {
	admitted := map[string]struct{}{}
	for _, item := range strings.Split(os.Getenv(name), ",") {
		if item = strings.TrimSpace(item); item != "" {
			admitted[item] = struct{}{}
		}
	}
	return admitted
}

// PowerApplicationsStop stops the admitted StackKits services first. Each
// service is a typed SERVICE_STOP command built here from the pinned release,
// never an argv a caller supplied. A failed service is recorded and the
// sequence continues: the host still has to reach the storage and host steps.
func (h *Host) PowerApplicationsStop(ctx context.Context, services []string) (PowerStepResult, error) {
	result := PowerStepResult{Step: "applications", Outcome: "skipped", Items: append([]string{}, services...)}
	if !powerEnabled() {
		return result, errPowerDisabled
	}
	if len(services) == 0 {
		result.Detail = "no application services are configured"
		return result, nil
	}
	if h == nil || h.cfg.StackKit == nil {
		return result, errors.New("the StackKits executor is unavailable")
	}
	workspace := agentpkg.HostSecurityWorkspace()
	if workspace == "" {
		return result, errors.New("the node StackKits workspace is unavailable")
	}
	pin, err := h.cfg.StackKit.ReleasePin()
	if err != nil {
		return result, err
	}
	instanceID, err := agentpkg.WorkspaceStackKitInstanceID(workspace, "stack-spec.yaml")
	if err != nil {
		return result, err
	}
	var failures []string
	stopped := make([]string, 0, len(services))
	admitted := admittedPowerList(powerServicesEnv)
	for _, service := range services {
		service = strings.TrimSpace(service)
		if !powerServiceKeyPattern.MatchString(service) {
			failures = append(failures, "invalid service key")
			continue
		}
		if _, ok := admitted[service]; !ok {
			failures = append(failures, service+" (not admitted)")
			continue
		}
		command := &agentpb.StackKitCommand{
			CommandId:          fmt.Sprintf("ups-%d-%s", time.Now().UTC().UnixNano(), service),
			Operation:          agentpb.StackKitOperation_STACKKIT_OPERATION_SERVICE_STOP,
			ServiceKey:         service,
			StackkitInstanceId: instanceID,
			OwnerApproved:      true,
			Release:            pin,
			WorkingDirectory:   workspace,
		}
		run := h.cfg.StackKit.Execute(ctx, command)
		if run == nil || !run.Success {
			failures = append(failures, service)
			continue
		}
		stopped = append(stopped, service)
	}
	result.Items = stopped
	result.Outcome = "succeeded"
	result.Detail = fmt.Sprintf("%d of %d application services stopped", len(stopped), len(services))
	if len(failures) > 0 {
		result.Outcome = "failed"
		result.Detail = boundPowerDetail(result.Detail + "; not stopped: " + strings.Join(failures, ", "))
	}
	return result, nil
}

// PowerStorageQuiesce flushes and unmounts the owner's systemd mount units
// after the applications have stopped. `systemctl stop` on a mount unit flushes
// and unmounts it; a final `sync` flushes the remaining filesystems.
func (h *Host) PowerStorageQuiesce(ctx context.Context, units []string) (PowerStepResult, error) {
	result := PowerStepResult{Step: "storage", Outcome: "skipped", Items: append([]string{}, units...)}
	if !powerEnabled() {
		return result, errPowerDisabled
	}
	if len(units) == 0 {
		result.Detail = "no storage units are configured"
		return result, nil
	}
	var failures []string
	stopped := make([]string, 0, len(units))
	admitted := admittedPowerList(powerUnitsEnv)
	for _, unit := range units {
		unit = strings.TrimSpace(unit)
		if !powerMountUnitPattern.MatchString(unit) {
			failures = append(failures, "invalid unit")
			continue
		}
		if _, ok := admitted[unit]; !ok {
			failures = append(failures, unit+" (not admitted)")
			continue
		}
		if err := runFixed(ctx, powerUnitTimeout, "systemctl", "stop", unit); err != nil {
			failures = append(failures, unit)
			continue
		}
		stopped = append(stopped, unit)
	}
	if err := runFixed(ctx, powerUnitTimeout, "sync"); err != nil {
		failures = append(failures, "sync")
	}
	result.Items = stopped
	result.Outcome = "succeeded"
	result.Detail = fmt.Sprintf("%d of %d storage units quiesced", len(stopped), len(units))
	if len(failures) > 0 {
		result.Outcome = "failed"
		result.Detail = boundPowerDetail(result.Detail + "; failed: " + strings.Join(failures, ", "))
	}
	return result, nil
}

// PowerOff asks the host to power off. The request is queued; the daemon has
// already journaled the host step before it calls this.
func (h *Host) PowerOff(ctx context.Context) error {
	if !powerEnabled() {
		return errPowerDisabled
	}
	return runFixed(ctx, powerUnitTimeout, "systemctl", "poweroff", "--no-block")
}

// PowerAvailable reports whether this root Guard admits the UPS power sequence.
func (l *LegacyRoot) PowerAvailable() bool {
	return l != nil && l.host != nil && powerEnabled()
}

// PowerApplicationsStop implements PowerBackend for a root Guard.
func (l *LegacyRoot) PowerApplicationsStop(ctx context.Context, services []string) (PowerStepResult, error) {
	if l == nil || l.host == nil {
		return PowerStepResult{}, errPowerDisabled
	}
	return l.host.PowerApplicationsStop(ctx, services)
}

// PowerStorageQuiesce implements PowerBackend for a root Guard.
func (l *LegacyRoot) PowerStorageQuiesce(ctx context.Context, units []string) (PowerStepResult, error) {
	if l == nil || l.host == nil {
		return PowerStepResult{}, errPowerDisabled
	}
	return l.host.PowerStorageQuiesce(ctx, units)
}

// PowerOff implements PowerBackend for a root Guard.
func (l *LegacyRoot) PowerOff(ctx context.Context) error {
	if l == nil || l.host == nil {
		return errPowerDisabled
	}
	return l.host.PowerOff(ctx)
}

// runFixed runs a fixed binary with the arguments this package built.
func runFixed(ctx context.Context, timeout time.Duration, name string, args ...string) error {
	path, err := exec.LookPath(name)
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	output, err := exec.CommandContext(runCtx, path, args...).CombinedOutput() // #nosec G204 -- fixed binary and arguments built in this package.
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func boundPowerDetail(detail string) string {
	if len(detail) > powerStepDetail {
		return detail[:powerStepDetail]
	}
	return detail
}
