package guardpower

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Default thresholds. They are deliberately conservative: the Guard waits for
// a sustained battery feed before it acts, and it keeps enough runtime for the
// ordered sequence to finish.
const (
	DefaultOnBatteryGrace     = time.Minute
	DefaultLowChargePercent   = 40
	DefaultCriticalChargePct  = 20
	DefaultLowRuntime         = 15 * time.Minute
	DefaultCriticalRuntime    = 5 * time.Minute
	DefaultApplicationTimeout = 2 * time.Minute
	DefaultStorageTimeout     = time.Minute
	DefaultHostDelay          = 30 * time.Second
	DefaultHostTimeout        = 30 * time.Second
)

var (
	serviceKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	mountUnitPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.-]{0,127}\.(mount|swap)$`)
)

// reservedMountUnits are mount points that host the OS, the Guard's own
// custody or the runtime. Stopping them would make the journal and the
// sequence itself unreliable, so the policy refuses them.
var reservedMountUnits = map[string]struct{}{
	"-.mount": {}, "var.mount": {}, "usr.mount": {}, "etc.mount": {}, "boot.mount": {},
	"opt.mount": {}, "home.mount": {}, "root.mount": {}, "srv.mount": {}, "tmp.mount": {}, "run.mount": {},
}

// Policy is the owner's UPS protection policy on one Guard.
type Policy struct {
	// Enabled is the owner's explicit opt-in. A Guard never shuts a host down
	// without it.
	Enabled bool
	// OnBatteryGrace is how long the host must stay on battery before a low
	// charge or a short runtime starts the sequence. A critical reading
	// bypasses it.
	OnBatteryGrace     time.Duration
	LowChargePercent   float64
	CriticalChargePct  float64
	LowRuntime         time.Duration
	CriticalRuntime    time.Duration
	ApplicationTimeout time.Duration
	StorageTimeout     time.Duration
	HostDelay          time.Duration
	HostTimeout        time.Duration
	// ApplicationServices are the admitted StackKits service keys stopped
	// first. Critical services (base, id, auth) are refused by StackKits.
	ApplicationServices []string
	// StorageUnits are the systemd mount units flushed and unmounted after the
	// applications. A unit stop flushes the filesystem and unmounts it.
	StorageUnits []string
}

// DefaultPolicy returns the thresholds an unconfigured Guard would use. Enabled
// stays false: the owner turns protection on.
func DefaultPolicy() Policy {
	return Policy{
		OnBatteryGrace:     DefaultOnBatteryGrace,
		LowChargePercent:   DefaultLowChargePercent,
		CriticalChargePct:  DefaultCriticalChargePct,
		LowRuntime:         DefaultLowRuntime,
		CriticalRuntime:    DefaultCriticalRuntime,
		ApplicationTimeout: DefaultApplicationTimeout,
		StorageTimeout:     DefaultStorageTimeout,
		HostDelay:          DefaultHostDelay,
		HostTimeout:        DefaultHostTimeout,
	}
}

// Validate refuses a policy the Guard cannot apply safely.
func (p Policy) Validate() error {
	if err := p.validateThresholds(); err != nil {
		return err
	}
	return p.validateNames()
}

func (p Policy) validateThresholds() error {
	switch {
	case p.OnBatteryGrace < 0:
		return errors.New("guardpower: the on-battery grace is negative")
	case p.ApplicationTimeout <= 0 || p.StorageTimeout <= 0 || p.HostDelay < 0 || p.HostTimeout <= 0:
		return errors.New("guardpower: the step timeouts are not positive")
	case p.LowChargePercent < 0 || p.LowChargePercent > 100:
		return errors.New("guardpower: the low charge threshold is outside 0..100")
	case p.CriticalChargePct < 0 || p.CriticalChargePct > 100:
		return errors.New("guardpower: the critical charge threshold is outside 0..100")
	case p.CriticalChargePct > p.LowChargePercent:
		return errors.New("guardpower: the critical charge threshold is above the low threshold")
	case p.LowRuntime < 0 || p.CriticalRuntime < 0:
		return errors.New("guardpower: a runtime threshold is negative")
	case p.CriticalRuntime > p.LowRuntime:
		return errors.New("guardpower: the critical runtime is above the low runtime")
	}
	return nil
}

func (p Policy) validateNames() error {
	if len(p.ApplicationServices) > maxItems || len(p.StorageUnits) > maxItems {
		return fmt.Errorf("guardpower: at most %d application services and storage units are supported", maxItems)
	}
	for _, key := range p.ApplicationServices {
		if !serviceKeyPattern.MatchString(key) {
			return fmt.Errorf("guardpower: application service %q is not a StackKits service key", key)
		}
	}
	for _, unit := range p.StorageUnits {
		if !mountUnitPattern.MatchString(unit) {
			return fmt.Errorf("guardpower: storage unit %q is not a systemd mount or swap unit", unit)
		}
		if _, reserved := reservedMountUnits[unit]; reserved {
			return fmt.Errorf("guardpower: storage unit %q hosts the OS or the Guard's own custody", unit)
		}
	}
	if slices.Contains(p.StorageUnits, "var-lib-docker.mount") {
		// Docker's data root is stopped by the application step; stopping its
		// mount unit while containers still run would corrupt the daemon state.
		return errors.New("guardpower: the Docker data root is stopped with the applications, not as storage")
	}
	return nil
}

// View returns the effective policy as the control plane shows it.
func (p Policy) View() PolicyView {
	return PolicyView{
		OnBatteryGraceSeconds:  int64(p.OnBatteryGrace / time.Second),
		LowChargePercent:       p.LowChargePercent,
		CriticalChargePercent:  p.CriticalChargePct,
		LowRuntimeSeconds:      int64(p.LowRuntime / time.Second),
		CriticalRuntimeSeconds: int64(p.CriticalRuntime / time.Second),
		ApplicationTimeoutSec:  int64(p.ApplicationTimeout / time.Second),
		StorageTimeoutSec:      int64(p.StorageTimeout / time.Second),
		HostDelaySeconds:       int64(p.HostDelay / time.Second),
		HostTimeoutSeconds:     int64(p.HostTimeout / time.Second),
		ApplicationServices:    append([]string{}, p.ApplicationServices...),
		StorageUnits:           append([]string{}, p.StorageUnits...),
	}
}

// Trigger reports whether a reading starts the ordered shutdown. The grace
// applies to a sustained battery feed; a critical charge or runtime is acted on
// at once because the remaining runtime may not cover another interval.
func (p Policy) Trigger(reading Reading, onBatterySince, now time.Time) bool {
	if !p.Enabled || !reading.OnBattery {
		return false
	}
	if p.Critical(reading) {
		return true
	}
	if onBatterySince.IsZero() || now.Sub(onBatterySince) < p.OnBatteryGrace {
		return false
	}
	if reading.ChargePercent != nil && *reading.ChargePercent <= p.LowChargePercent {
		return true
	}
	if reading.RuntimeSeconds != nil && time.Duration(*reading.RuntimeSeconds)*time.Second <= p.LowRuntime {
		return true
	}
	// A UPS that reports no charge and no runtime is still on battery; after
	// the grace the Guard cannot prove the host will outlast the sequence.
	return reading.ChargePercent == nil && reading.RuntimeSeconds == nil
}

// Critical reports a reading that leaves no room for the grace.
func (p Policy) Critical(reading Reading) bool {
	if !reading.OnBattery {
		return false
	}
	if reading.LowBattery {
		return true
	}
	if reading.ChargePercent != nil && *reading.ChargePercent <= p.CriticalChargePct {
		return true
	}
	if reading.RuntimeSeconds != nil && time.Duration(*reading.RuntimeSeconds)*time.Second <= p.CriticalRuntime {
		return true
	}
	return false
}

// normalise trims and deduplicates the configured names without reordering
// them: the owner's order is the order the steps run in.
func normalise(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || slices.Contains(out, value) {
			continue
		}
		out = append(out, value)
	}
	return out
}
