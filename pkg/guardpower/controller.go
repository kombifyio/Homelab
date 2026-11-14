package guardpower

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"
)

// StepResult is what one governed step reports: a bounded detail and the
// names it acted on. Names are service keys or unit names, never host paths.
type StepResult struct {
	Detail  string
	Items   []string
	Outcome Outcome
}

// Steps is the governed execution surface of the ordered shutdown. The node
// wiring implements it over the privileged executor; a Guard without one
// refuses instead of pretending.
type Steps interface {
	// StopApplications gracefully stops the named admitted services, bounded by
	// the caller's context.
	StopApplications(ctx context.Context, services []string) (StepResult, error)
	// QuiesceStorage flushes and unmounts the named systemd mount units.
	QuiesceStorage(ctx context.Context, units []string) (StepResult, error)
	// PowerOff asks the host to power off. It returns once the request is
	// accepted; the host may stop before the call returns.
	PowerOff(ctx context.Context) error
}

// Reporter sends one snapshot to the control plane. A failure leaves the
// snapshot in the local journal; the next tick retries.
type Reporter func(context.Context, PowerReport) error

// ControllerConfig wires the power controller.
type ControllerConfig struct {
	Policy  Policy
	Probe   Probe
	Steps   Steps
	Journal *Journal
	Source  Source
	Report  Reporter
	// Interval is the probe cadence and ReportEvery the unchanged-snapshot
	// cadence. Zero selects the defaults.
	Interval, ReportEvery time.Duration
	Now                   func() time.Time
	Logger                *slog.Logger
}

// Controller applies one policy to one UPS and runs the ordered shutdown.
type Controller struct {
	cfg ControllerConfig
	log *slog.Logger
	now func() time.Time

	mu              sync.Mutex
	reading         Reading
	onBatterySince  time.Time
	phase           Phase
	plan            []Step
	startedAt       *time.Time
	canceledAt      *time.Time
	hostRequestedAt *time.Time
	cancelReason    string
	running         bool
	stepCancel      context.CancelFunc
	epoch           string
	bootID          string
	seq             int64
	lastReport      time.Time

	reportMu sync.Mutex
}

// NewController validates the wiring and recovers what the journal proves. A
// host power-off that was already journaled is never repeated.
func NewController(cfg ControllerConfig) (*Controller, error) {
	if cfg.Probe == nil {
		return nil, errors.New("guardpower: a UPS probe is required")
	}
	if cfg.Steps == nil {
		return nil, errors.New("guardpower: governed shutdown steps are required")
	}
	if cfg.Journal == nil {
		return nil, errors.New("guardpower: a durable journal is required")
	}
	if err := cfg.Policy.Validate(); err != nil {
		return nil, err
	}
	cfg.Policy.ApplicationServices = normalise(cfg.Policy.ApplicationServices)
	cfg.Policy.StorageUnits = normalise(cfg.Policy.StorageUnits)
	if cfg.Source.Driver == "" {
		cfg.Source.Driver = "nut"
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.ReportEvery <= 0 {
		cfg.ReportEvery = DefaultReportEvery
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return nil, err
	}
	c := &Controller{cfg: cfg, log: logger.With("component", "guard-power"), now: cfg.Now, epoch: hex.EncodeToString(random), bootID: currentBootID(), phase: PhaseIdle}
	if cfg.Journal != nil {
		c.recover(cfg.Journal.Records())
	}
	return c, nil
}

// recover restores what the journal proves after a restart. A host power-off
// that was already journaled on this boot is never repeated; a power-off from
// an earlier boot is history and protection starts again. A step that was
// running when the process stopped is recorded as interrupted instead of
// staying "running" forever.
func (c *Controller) recover(records []Event) {
	start := -1
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].Kind == "trigger" {
			start = i
			break
		}
	}
	currentBoot := func(event Event) bool { return event.BootID == "" || event.BootID == c.bootID }
	open := map[StepName]bool{}
	if start >= 0 {
		at := records[start].At.UTC()
		c.startedAt = &at
		c.phase = PhaseApplications
		c.plan = []Step{
			{Name: StepApplications, Outcome: OutcomePending},
			{Name: StepStorage, Outcome: OutcomePending},
			{Name: StepHost, Outcome: OutcomePending},
		}
		for _, event := range records[start:] {
			switch event.Kind {
			case "step":
				if event.Outcome == OutcomeRunning {
					open[event.Step] = true
				} else {
					delete(open, event.Step)
				}
				c.setStep(event.Step, func(step *Step) {
					step.Outcome = event.Outcome
					if event.Detail != "" {
						step.Detail = event.Detail
					}
					at := event.At.UTC()
					if event.Outcome == OutcomeRunning {
						step.StartedAt = &at
					} else {
						step.FinishedAt = &at
					}
				})
			case "cancel":
				at := event.At.UTC()
				c.canceledAt = &at
				c.cancelReason = event.CancelReason
				c.phase = PhaseCanceled
			case "host_request":
				if currentBoot(event) {
					at := event.At.UTC()
					c.hostRequestedAt = &at
					c.phase = PhaseHost
				}
			case "host_failed":
				c.hostRequestedAt = nil
				at := event.At.UTC()
				c.canceledAt = &at
				c.cancelReason = "host_step_failed"
				c.phase = PhaseCanceled
			}
		}
		// A step whose journal line says "running" with no outcome after it was
		// cut off by the restart.
		for name := range open {
			c.setStep(name, func(step *Step) {
				step.Outcome = OutcomeCanceled
				step.Detail = "interrupted by restart"
			})
		}
	}
	interrupted := len(open) > 0
	if c.hostRequestedAt != nil {
		c.phase = PhaseComplete
		return
	}
	if interrupted && c.phase != PhaseCanceled {
		at := records[len(records)-1].At.UTC()
		c.canceledAt = &at
		c.cancelReason = "interrupted_by_restart"
		c.phase = PhaseCanceled
	}
	if start < 0 {
		// A journal that rotated past its trigger still proves a current-boot
		// host request; it must not be repeated.
		for i := len(records) - 1; i >= 0; i-- {
			if records[i].Kind == "host_request" && currentBoot(records[i]) {
				at := records[i].At.UTC()
				c.hostRequestedAt = &at
				c.phase = PhaseComplete
				break
			}
		}
	}
}

// Run probes until the context ends.
func (c *Controller) Run(ctx context.Context) {
	ticker := time.NewTicker(c.cfg.Interval)
	defer ticker.Stop()
	c.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			c.stopStep()
			return
		case <-ticker.C:
			c.tick(ctx)
		}
	}
}

const (
	// reportTimeout bounds one snapshot send; a dead uplink delays nothing else.
	reportTimeout = 5 * time.Second
	// rearmAfter is how long a host may keep running after an accepted
	// power-off before protection is armed again.
	rearmAfter = 5 * time.Minute
)

func (c *Controller) tick(ctx context.Context) {
	probeCtx, cancel := context.WithTimeout(ctx, c.cfg.Interval)
	reading, err := c.cfg.Probe.Read(probeCtx)
	cancel()
	if err != nil {
		c.log.Warn("guard_power_probe_failed", "error", err.Error())
		return
	}
	if reading.ObservedAt.IsZero() {
		reading.ObservedAt = c.now()
	}
	c.observe(ctx, reading)
}

func (c *Controller) observe(ctx context.Context, reading Reading) {
	now := c.now()
	c.mu.Lock()
	start, changed := c.applyReadingLocked(now, reading)
	c.mu.Unlock()
	if start {
		go c.runSequence(ctx)
	}
	if changed || c.reportDue(now) {
		c.send(ctx)
	}
}

// applyReadingLocked applies one reading and reports whether a sequence starts
// and whether the observable state changed.
func (c *Controller) applyReadingLocked(now time.Time, reading Reading) (start, changed bool) {
	previousBattery := c.reading.OnBattery
	c.reading = reading
	if c.phase == PhaseComplete && c.hostRequestedAt != nil && now.Sub(*c.hostRequestedAt) > rearmAfter {
		// The host is still running well after the power-off was accepted
		// (inhibitor, cancelled shutdown): protection starts again.
		c.log.Warn("guard_power_poweroff_did_not_take_effect")
		c.phase = PhaseIdle
		c.hostRequestedAt = nil
		changed = true
	}
	if reading.OnBattery && !previousBattery {
		changed = c.enterBatteryLocked(now, reading)
	}
	if !reading.OnBattery {
		if c.enterLineLocked(now, previousBattery) {
			changed = true
		}
	}
	if !c.running && c.phase != PhaseComplete && c.cfg.Policy.Trigger(reading, c.onBatterySince, now) {
		c.startSequenceLocked(now)
		start, changed = true, true
	}
	return start, changed
}

func (c *Controller) enterBatteryLocked(now time.Time, reading Reading) bool {
	c.onBatterySince = now
	c.record(Event{Kind: "battery", Phase: c.phase, OnBattery: boolPtr(true), ChargePercent: reading.ChargePercent, RuntimeSeconds: reading.RuntimeSeconds})
	return true
}

func (c *Controller) enterLineLocked(now time.Time, previousBattery bool) bool {
	if !previousBattery {
		// A host that stays on line power is idle; keep the last canceled
		// evidence for a short while so the owner can read why it stopped.
		if c.phase == PhaseCanceled && c.canceledAt != nil && now.Sub(*c.canceledAt) > time.Minute {
			c.phase = PhaseIdle
			return true
		}
		return false
	}
	c.onBatterySince = time.Time{}
	c.record(Event{Kind: "line", Phase: c.phase, OnBattery: boolPtr(false)})
	if c.running && c.hostRequestedAt == nil {
		// The feed returned before the host step: cancel the sequence. The
		// running step sees its context end and records itself canceled.
		c.cancelReason = "line_restored"
		c.stopStepLocked()
	}
	return true
}

func (c *Controller) startSequenceLocked(now time.Time) {
	c.running = true
	c.phase = PhaseApplications
	c.plan = []Step{
		{Name: StepApplications, Outcome: OutcomePending},
		{Name: StepStorage, Outcome: OutcomePending},
		{Name: StepHost, Outcome: OutcomePending},
	}
	at := now.UTC()
	c.startedAt = &at
	c.canceledAt = nil
	c.cancelReason = ""
	c.hostRequestedAt = nil
	c.record(Event{Kind: "trigger", Phase: c.phase})
}

func (c *Controller) runSequence(ctx context.Context) {
	stepCtx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.stepCancel = cancel
	c.mu.Unlock()
	defer cancel()

	if !c.runStep(stepCtx, StepApplications, c.cfg.Policy.ApplicationTimeout, func(stepCtx context.Context) (StepResult, error) {
		return c.cfg.Steps.StopApplications(stepCtx, c.cfg.Policy.ApplicationServices)
	}) {
		c.finish(ctx, false)
		return
	}
	if !c.runStep(stepCtx, StepStorage, c.cfg.Policy.StorageTimeout, func(stepCtx context.Context) (StepResult, error) {
		return c.cfg.Steps.QuiesceStorage(stepCtx, c.cfg.Policy.StorageUnits)
	}) {
		c.finish(ctx, false)
		return
	}
	// The last moment a returning feed can still save the host.
	if !c.waitHostDelay(stepCtx) {
		c.finish(ctx, false)
		return
	}
	c.mu.Lock()
	if c.cancelReason != "" {
		c.mu.Unlock()
		c.finish(ctx, false)
		return
	}
	now := c.now().UTC()
	c.phase = PhaseHost
	c.hostRequestedAt = &now
	if err := c.recordErr(Event{Kind: "host_request", Phase: PhaseHost, Step: StepHost}); err != nil {
		// The journal is evidence, not a precondition: a depleting UPS must still
		// get its clean power-off. Only a repeat after a restart on the same boot
		// is then not suppressed, which a still-depleted battery wants anyway.
		c.log.Error("guard_power_host_request_not_durable", "error", err.Error())
	}
	c.mu.Unlock()
	// Tell the control plane in the background; the poweroff never waits for
	// the network, and the local journal is the durable proof either way.
	go c.send(ctx)
	hostCtx, hostCancel := context.WithTimeout(stepCtx, c.cfg.Policy.HostTimeout)
	err := c.cfg.Steps.PowerOff(hostCtx)
	hostCancel()
	c.recordStepOutcome(StepHost, err)
	c.finish(ctx, err == nil)
}

func (c *Controller) runStep(ctx context.Context, name StepName, timeout time.Duration, run func(context.Context) (StepResult, error)) bool {
	c.mu.Lock()
	if c.cancelReason != "" {
		c.mu.Unlock()
		return false
	}
	now := c.now().UTC()
	at := now
	c.setStep(name, func(step *Step) {
		step.Outcome, step.StartedAt = OutcomeRunning, &at
	})
	c.phase = phaseOfStep(name)
	c.record(Event{Kind: "step", Phase: c.phase, Step: name, Outcome: OutcomeRunning})
	c.mu.Unlock()

	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	result, err := run(stepCtx)
	cancel()

	c.mu.Lock()
	cancelled := c.cancelReason != ""
	finished := c.now().UTC()
	c.setStep(name, func(step *Step) {
		step.FinishedAt = &finished
		step.Detail = boundText(result.Detail, maxDetailBytes)
		step.Items = boundItems(result.Items)
		switch {
		case cancelled:
			step.Outcome = OutcomeCanceled
		case err != nil:
			step.Outcome = OutcomeFailed
			if step.Detail == "" {
				step.Detail = boundText(err.Error(), maxDetailBytes)
			}
		case result.Outcome == OutcomeSkipped:
			step.Outcome = OutcomeSkipped
		case result.Outcome == OutcomeFailed:
			step.Outcome = OutcomeFailed
		default:
			step.Outcome = OutcomeSucceeded
		}
	})
	outcome := c.stepOutcome(name)
	c.record(Event{Kind: "step", Phase: c.phase, Step: name, Outcome: outcome, Detail: c.stepDetail(name)})
	c.mu.Unlock()
	if err != nil {
		c.log.Warn("guard_power_step_failed", "step", string(name), "error", err.Error())
	}
	// A failed step is recorded and the sequence continues: a host that cannot
	// stop an application still has to flush storage and reach the host step.
	return !cancelled
}

func (c *Controller) waitHostDelay(ctx context.Context) bool {
	timer := time.NewTimer(c.cfg.Policy.HostDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (c *Controller) finish(ctx context.Context, powered bool) {
	c.mu.Lock()
	c.running = false
	c.stepCancel = nil
	now := c.now().UTC()
	switch {
	case powered:
		c.phase = PhaseComplete
	case c.hostRequestedAt != nil:
		c.phase = PhaseCanceled
		c.canceledAt = &now
		c.cancelReason = "host_step_failed"
		c.record(Event{Kind: "cancel", Phase: PhaseCanceled, CancelReason: c.cancelReason})
	case c.cancelReason != "":
		c.phase = PhaseCanceled
		c.canceledAt = &now
		c.record(Event{Kind: "cancel", Phase: PhaseCanceled, CancelReason: c.cancelReason})
	default:
		c.phase = PhaseCanceled
		c.canceledAt = &now
		c.cancelReason = "sequence_interrupted"
		c.record(Event{Kind: "cancel", Phase: PhaseCanceled, CancelReason: c.cancelReason})
	}
	c.mu.Unlock()
	c.send(ctx)
}

func (c *Controller) recordStepOutcome(name StepName, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now().UTC()
	c.setStep(name, func(step *Step) {
		step.FinishedAt = &now
		if err != nil {
			step.Outcome = OutcomeFailed
			step.Detail = boundText(err.Error(), maxDetailBytes)
		} else {
			step.Outcome = OutcomeSucceeded
		}
	})
	c.record(Event{Kind: "step", Phase: c.phase, Step: name, Outcome: c.stepOutcome(name), Detail: c.stepDetail(name)})
	if name == StepHost && err != nil {
		c.record(Event{Kind: "host_failed", Phase: PhaseHost, Step: name, Detail: c.stepDetail(name)})
	}
}

func (c *Controller) stopStep() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopStepLocked()
}

func (c *Controller) stopStepLocked() {
	if c.stepCancel != nil {
		c.stepCancel()
	}
}

func (c *Controller) reportDue(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return now.Sub(c.lastReport) >= c.cfg.ReportEvery
}

func (c *Controller) snapshotLocked(sequence int64) PowerReport {
	hostRequested := c.hostRequestedAt != nil
	state := StateOf(c.reading.OnBattery, c.phase, hostRequested, c.phase == PhaseCanceled)
	source := c.cfg.Source
	if source.Model == "" {
		source.Model = c.reading.Model
	}
	report := PowerReport{
		SchemaVersion: ReportSchema, SourceEpoch: c.epoch, Sequence: sequence, ObservedAt: c.now(),
		State: state, Phase: c.phase, OnBattery: c.reading.OnBattery,
		ChargePercent: c.reading.ChargePercent, RuntimeSeconds: c.reading.RuntimeSeconds,
		Source: source, Steps: append([]Step{}, c.plan...), Policy: c.cfg.Policy.View(),
		StartedAt: c.startedAt, CanceledAt: c.canceledAt, CancelReason: c.cancelReason,
		HostRequestedAt: c.hostRequestedAt, Evidence: EvidenceRef,
	}
	if report.Steps == nil {
		report.Steps = []Step{}
	}
	return report
}

func (c *Controller) send(ctx context.Context) {
	if c.cfg.Report == nil {
		return
	}
	c.reportMu.Lock()
	defer c.reportMu.Unlock()
	c.mu.Lock()
	// The sequence advances per attempt: a request whose answer was lost may
	// have been stored, and the control plane drops a snapshot at or below the
	// stored one as a replay.
	c.seq++
	report := c.snapshotLocked(c.seq)
	c.mu.Unlock()
	sendCtx, cancel := context.WithTimeout(ctx, reportTimeout)
	err := c.cfg.Report(sendCtx, report)
	cancel()
	if err != nil {
		c.log.Warn("guard_power_report_failed", "error", err.Error())
		return
	}
	c.mu.Lock()
	c.lastReport = c.now()
	c.mu.Unlock()
}

func (c *Controller) record(event Event) {
	if err := c.recordErr(event); err != nil {
		c.log.Warn("guard_power_journal_failed", "kind", event.Kind, "error", err.Error())
	}
}

// recordErr writes one durable event and reports whether it reached the disk.
// The host step must not proceed when its intent cannot be journaled.
func (c *Controller) recordErr(event Event) error {
	if c.cfg.Journal == nil {
		return nil
	}
	event.BootID = c.bootID
	_, err := c.cfg.Journal.Append(event)
	return err
}

func (c *Controller) setStep(name StepName, update func(*Step)) {
	for i := range c.plan {
		if c.plan[i].Name == name {
			update(&c.plan[i])
			return
		}
	}
	c.plan = append(c.plan, Step{Name: name, Outcome: OutcomePending})
	update(&c.plan[len(c.plan)-1])
}

func (c *Controller) stepOutcome(name StepName) Outcome {
	for _, step := range c.plan {
		if step.Name == name {
			return step.Outcome
		}
	}
	return OutcomePending
}

func (c *Controller) stepDetail(name StepName) string {
	for _, step := range c.plan {
		if step.Name == name {
			return step.Detail
		}
	}
	return ""
}

func phaseOfStep(name StepName) Phase {
	switch name {
	case StepApplications:
		return PhaseApplications
	case StepStorage:
		return PhaseStorage
	case StepHost:
		return PhaseHost
	}
	return PhaseIdle
}

func boundItems(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	out := make([]string, 0, min(len(items), maxItems))
	for _, item := range items {
		item = boundText(item, 64)
		if item == "" {
			continue
		}
		out = append(out, item)
		if len(out) == maxItems {
			break
		}
	}
	return out
}

func boolPtr(value bool) *bool { return &value }
