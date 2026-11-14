// Package guardpower is kombify Guard's UPS-aware power protection.
//
// The Guard reads one supported UPS through NUT, applies the owner's policy and,
// when the battery cannot carry the host to line power again, runs the ordered
// shutdown: admitted applications first (a graceful, bounded stop), then
// storage (flush and unmount through the governed systemd mount units), then
// the host. Every step is written to a durable journal before its effect and
// again with its outcome, so a restart or a power loss leaves a record instead
// of a guess. While the host still has line power a returning feed cancels the
// sequence before the host step; after the host step is journaled nothing is
// cancelled, because the host is already going down.
//
// This package holds the parts both sides share: the report the Guard sends,
// the policy it applied and the read model the control plane projects. The
// node-side probe, journal and controller live in the same package but are
// only started by the Guard agent.
package guardpower

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// ReportSchema is the wire version of one Guard power report.
	ReportSchema = "kombify.guard-power/v1"
	// JournalSchema is the durable evidence the Guard keeps on the node.
	JournalSchema = "kombify.guard-power-journal/v1"
	// EvidenceRef names the node-side evidence without carrying a host path to
	// the control plane.
	EvidenceRef = "guard-power-journal/v1"

	// MaxReportBytes bounds one report; the policy and the last sequence are
	// small and no host path or secret is carried.
	MaxReportBytes = 32 << 10
	// StaleAfter is how old a report may be before the read model says unknown.
	StaleAfter = 3 * time.Minute
	// DefaultInterval is the probe cadence.
	DefaultInterval = 10 * time.Second
	// DefaultReportEvery is how often an unchanged snapshot is resent.
	DefaultReportEvery = time.Minute

	maxSteps       = 8
	maxDetailBytes = 512
	maxItems       = 32
)

// Phase is the ordered shutdown phase of one Guard.
type Phase string

const (
	PhaseIdle         Phase = "idle"
	PhaseArmed        Phase = "armed"
	PhaseApplications Phase = "applications"
	PhaseStorage      Phase = "storage"
	PhaseHost         Phase = "host"
	PhaseCanceled     Phase = "canceled"
	PhaseComplete     Phase = "complete"
)

// StepName names one step of the ordered shutdown.
type StepName string

const (
	StepApplications StepName = "applications"
	StepStorage      StepName = "storage"
	StepHost         StepName = "host"
)

// Outcome is the measured result of one step.
type Outcome string

const (
	OutcomePending   Outcome = "pending"
	OutcomeRunning   Outcome = "running"
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeFailed    Outcome = "failed"
	OutcomeSkipped   Outcome = "skipped"
	OutcomeCanceled  Outcome = "canceled"
)

// Power states of the projected read model.
const (
	StateLine      = "line"
	StateOnBattery = "on_battery"
	StateShutdown  = "shutdown"
	StateCanceled  = "canceled"
	StateUnknown   = "unknown"
)

// Reason codes of a projected state that is unknown.
const (
	ReasonNoReport         = "no_report"
	ReasonObservationStale = "observation_stale"
)

// Step is one measured step of the ordered shutdown.
type Step struct {
	Name       StepName   `json:"name"`
	Outcome    Outcome    `json:"outcome"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// Detail is a bounded, secret-free explanation of the outcome.
	Detail string `json:"detail,omitempty"`
	// Items names the services or units the step acted on, bounded and never a
	// host path.
	Items []string `json:"items,omitempty"`
}

// Source names the UPS the Guard read. It is configuration, not a host path.
type Source struct {
	Driver string `json:"driver"`
	Name   string `json:"name"`
	Model  string `json:"model,omitempty"`
}

// PolicyView is the effective policy the Guard applied, with durations in
// seconds so the control plane never has to guess a Go duration.
type PolicyView struct {
	OnBatteryGraceSeconds  int64    `json:"on_battery_grace_seconds"`
	LowChargePercent       float64  `json:"low_charge_percent"`
	CriticalChargePercent  float64  `json:"critical_charge_percent"`
	LowRuntimeSeconds      int64    `json:"low_runtime_seconds"`
	CriticalRuntimeSeconds int64    `json:"critical_runtime_seconds"`
	ApplicationTimeoutSec  int64    `json:"application_timeout_seconds"`
	StorageTimeoutSec      int64    `json:"storage_timeout_seconds"`
	HostDelaySeconds       int64    `json:"host_delay_seconds"`
	HostTimeoutSeconds     int64    `json:"host_timeout_seconds"`
	ApplicationServices    []string `json:"application_services,omitempty"`
	StorageUnits           []string `json:"storage_units,omitempty"`
}

// PowerReport is the snapshot a Guard sends. It is a state, not a command: the
// control plane never asks a Guard to shut down from this channel.
type PowerReport struct {
	SchemaVersion string    `json:"schema_version"`
	SourceEpoch   string    `json:"source_epoch"`
	Sequence      int64     `json:"sequence"`
	ObservedAt    time.Time `json:"observed_at"`

	State           string     `json:"state"`
	Phase           Phase      `json:"phase"`
	OnBattery       bool       `json:"on_battery"`
	ChargePercent   *float64   `json:"charge_percent,omitempty"`
	RuntimeSeconds  *int64     `json:"runtime_seconds,omitempty"`
	Source          Source     `json:"source"`
	Steps           []Step     `json:"steps,omitempty"`
	Policy          PolicyView `json:"policy"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	CanceledAt      *time.Time `json:"canceled_at,omitempty"`
	CancelReason    string     `json:"cancel_reason,omitempty"`
	HostRequestedAt *time.Time `json:"host_requested_at,omitempty"`
	Evidence        string     `json:"evidence"`
}

// View is the projected read model of one Guard's power protection.
type View struct {
	State           string     `json:"state"`
	Phase           Phase      `json:"phase"`
	ReasonCode      string     `json:"reason_code,omitempty"`
	ObservedAt      *time.Time `json:"observed_at,omitempty"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	OnBattery       bool       `json:"on_battery"`
	ChargePercent   *float64   `json:"charge_percent,omitempty"`
	RuntimeSeconds  *int64     `json:"runtime_seconds,omitempty"`
	Source          Source     `json:"source"`
	Steps           []Step     `json:"steps"`
	Policy          PolicyView `json:"policy"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	CanceledAt      *time.Time `json:"canceled_at,omitempty"`
	CancelReason    string     `json:"cancel_reason,omitempty"`
	HostRequestedAt *time.Time `json:"host_requested_at,omitempty"`
	Evidence        string     `json:"evidence"`
}

// ErrInvalidReport marks a power report the control plane refuses.
var ErrInvalidReport = errors.New("guardpower: the power report is not acceptable")

func invalidReport(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidReport, fmt.Sprintf(format, args...))
}

// Validate checks one report. It refuses anything the read model could not show
// honestly: an unknown state or phase, an unbounded detail, a path in an item.
func (r *PowerReport) Validate(now time.Time) error {
	if err := r.validateEnvelope(now); err != nil {
		return err
	}
	return r.validateReading()
}

func (r *PowerReport) validateEnvelope(now time.Time) error {
	if r == nil {
		return invalidReport("the report is missing")
	}
	if r.SchemaVersion != ReportSchema {
		return invalidReport("schema %q is not %s", r.SchemaVersion, ReportSchema)
	}
	r.SourceEpoch = strings.TrimSpace(r.SourceEpoch)
	if r.SourceEpoch == "" || len(r.SourceEpoch) > 64 || r.Sequence < 0 {
		return invalidReport("the source epoch or sequence is missing")
	}
	if r.ObservedAt.IsZero() || r.ObservedAt.After(now.Add(5*time.Minute)) {
		return invalidReport("the observation time is missing or in the future")
	}
	switch r.State {
	case StateLine, StateOnBattery, StateShutdown, StateCanceled:
	default:
		return invalidReport("state %q is unknown", r.State)
	}
	switch r.Phase {
	case PhaseIdle, PhaseArmed, PhaseApplications, PhaseStorage, PhaseHost, PhaseCanceled, PhaseComplete:
	default:
		return invalidReport("phase %q is unknown", r.Phase)
	}
	if r.Evidence != EvidenceRef {
		return invalidReport("the evidence reference is unknown")
	}
	return nil
}

func (r *PowerReport) validateReading() error {
	if r.ChargePercent != nil && (*r.ChargePercent < 0 || *r.ChargePercent > 100) {
		return invalidReport("the charge percentage is outside 0..100")
	}
	if r.RuntimeSeconds != nil && *r.RuntimeSeconds < 0 {
		return invalidReport("the runtime is negative")
	}
	if r.Source.Driver == "" || len(r.Source.Driver) > 32 || len(r.Source.Name) > 64 || len(r.Source.Model) > 64 {
		return invalidReport("the power source identity is invalid")
	}
	if len(r.Steps) > maxSteps {
		return invalidReport("too many steps")
	}
	for _, step := range r.Steps {
		if err := step.validate(); err != nil {
			return err
		}
	}
	if len(r.CancelReason) > maxDetailBytes {
		return invalidReport("the cancel reason is too long")
	}
	return nil
}

func (s Step) validate() error {
	switch s.Name {
	case StepApplications, StepStorage, StepHost:
	default:
		return invalidReport("step %q is unknown", s.Name)
	}
	switch s.Outcome {
	case OutcomePending, OutcomeRunning, OutcomeSucceeded, OutcomeFailed, OutcomeSkipped, OutcomeCanceled:
	default:
		return invalidReport("step outcome %q is unknown", s.Outcome)
	}
	if len(s.Detail) > maxDetailBytes {
		return invalidReport("a step detail is too long")
	}
	if len(s.Items) > maxItems {
		return invalidReport("a step names too many items")
	}
	for _, item := range s.Items {
		if item == "" || len(item) > 64 || strings.ContainsAny(item, "/\\\x00\n\r") {
			return invalidReport("a step item is not a bounded name")
		}
	}
	return nil
}

// StateOf derives the wire state from the phase and the host request.
func StateOf(onBattery bool, phase Phase, hostRequested bool, canceled bool) string {
	switch {
	case phase == PhaseComplete || phase == PhaseHost:
		return StateShutdown
	case canceled || phase == PhaseCanceled:
		return StateCanceled
	case hostRequested:
		return StateShutdown
	case onBattery:
		return StateOnBattery
	default:
		return StateLine
	}
}

// Project turns a stored report into the read model. A report that is absent
// or too old is unknown, never line and never protected.
func Project(report *PowerReport, reportedAt, now time.Time) View {
	if report == nil {
		return View{State: StateUnknown, Phase: PhaseIdle, ReasonCode: ReasonNoReport, Steps: []Step{}, Evidence: EvidenceRef}
	}
	observed := report.ObservedAt.UTC()
	// Freshness comes from when the control plane received the report, never
	// from the Guard's own clock, which a host that lost power may have drifted.
	received := observed
	if !reportedAt.IsZero() {
		received = reportedAt.UTC()
	}
	expires := received.Add(StaleAfter)
	view := View{
		State: report.State, Phase: report.Phase, ObservedAt: &observed, ExpiresAt: &expires,
		OnBattery: report.OnBattery, ChargePercent: report.ChargePercent, RuntimeSeconds: report.RuntimeSeconds,
		Source: report.Source, Steps: append([]Step{}, report.Steps...), Policy: report.Policy,
		StartedAt: report.StartedAt, CanceledAt: report.CanceledAt, CancelReason: report.CancelReason,
		HostRequestedAt: report.HostRequestedAt, Evidence: EvidenceRef,
	}
	if view.Steps == nil {
		view.Steps = []Step{}
	}
	if now.After(expires) {
		view.State, view.ReasonCode = StateUnknown, ReasonObservationStale
	}
	return view
}
