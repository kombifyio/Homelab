package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kombifyio/techstack/pkg/guardexec"
	"github.com/kombifyio/techstack/pkg/guardposture"
	"github.com/kombifyio/techstack/pkg/guardpower"
)

// Environment of the UPS-aware power protection. Protection is off unless the
// owner turns it on: a Guard never shuts a host down by default.
const (
	guardPowerAgentEnv       = "TECHSTACK_GUARD_POWER_ENABLED"
	guardPowerUPSEnv         = "TECHSTACK_GUARD_POWER_UPS"
	guardPowerGraceEnv       = "TECHSTACK_GUARD_POWER_GRACE"
	guardPowerLowChargeEnv   = "TECHSTACK_GUARD_POWER_LOW_CHARGE_PERCENT"
	guardPowerCritChargeEnv  = "TECHSTACK_GUARD_POWER_CRITICAL_CHARGE_PERCENT"
	guardPowerLowRuntimeEnv  = "TECHSTACK_GUARD_POWER_LOW_RUNTIME"
	guardPowerCritRuntimeEnv = "TECHSTACK_GUARD_POWER_CRITICAL_RUNTIME"
	guardPowerAppTimeoutEnv  = "TECHSTACK_GUARD_POWER_APPLICATION_TIMEOUT"
	guardPowerStorageTmoEnv  = "TECHSTACK_GUARD_POWER_STORAGE_TIMEOUT"
	guardPowerHostDelayEnv   = "TECHSTACK_GUARD_POWER_HOST_DELAY"
	guardPowerHostTimeoutEnv = "TECHSTACK_GUARD_POWER_HOST_TIMEOUT"
	guardPowerServicesEnv    = "TECHSTACK_GUARD_POWER_APPLICATION_SERVICES"
	guardPowerUnitsEnv       = "TECHSTACK_GUARD_POWER_STORAGE_UNITS"
)

// startGuardPower runs the UPS-aware ordered shutdown. It needs admitted
// custody for the durable journal and a governed executor for every host
// effect; without either it does not start rather than acting unrecorded.
func startGuardPower(ctx context.Context, custodyRoot *os.Root, enrollment agentEnrollment, identity guardIdentityRuntime, execution guardExecution, log *slog.Logger) {
	policy := guardPowerPolicy()
	if !policy.Enabled {
		return
	}
	if err := policy.Validate(); err != nil {
		log.Warn("guard_power_disabled", "reason", err.Error())
		return
	}
	url := derivedAgentControlURL(enrollment.HeartbeatURL, "posture")
	if url == "" || custodyRoot == nil {
		log.Warn("guard_power_disabled", "reason", "the posture channel or admitted custody is unavailable")
		return
	}
	if !execution.powerAvailable() {
		log.Warn("guard_power_disabled", "reason", "no governed executor for host effects")
		return
	}
	probe, err := guardpower.NewNUTProbe(guardpower.NUTConfig{UPS: envDefault(guardPowerUPSEnv, "ups")})
	if err != nil {
		log.Warn("guard_power_disabled", "reason", err.Error())
		return
	}
	journal, err := guardpower.OpenJournalRoot(custodyRoot, "power-journal")
	if err != nil {
		log.Warn("guard_power_disabled", "reason", err.Error())
		return
	}
	reporter := powerReporter{url: url, client: identity.httpClient(30 * time.Second), token: enrollment.AgentToken, tenant: enrollment.TenantID, signed: execution.signedUpdates}
	controller, err := guardpower.NewController(guardpower.ControllerConfig{
		Policy: policy, Probe: probe, Steps: execution.powerSteps(),
		Journal: journal, Source: guardpower.Source{Driver: "nut", Name: strings.TrimSpace(envDefault(guardPowerUPSEnv, "ups"))},
		Report: reporter.Report,
		Logger: log,
	})
	if err != nil {
		_ = journal.Close()
		log.Warn("guard_power_disabled", "reason", err.Error())
		return
	}
	go func() {
		defer func() { _ = journal.Close() }()
		controller.Run(ctx)
	}()
	log.Info("guard_power_started", "ups", strings.TrimSpace(envDefault(guardPowerUPSEnv, "ups")), "services", len(policy.ApplicationServices), "storage_units", len(policy.StorageUnits))
}

func guardPowerPolicy() guardpower.Policy {
	policy := guardpower.DefaultPolicy()
	policy.Enabled = envBoolDefault(guardPowerAgentEnv, false)
	policy.OnBatteryGrace = durationEnv(guardPowerGraceEnv, policy.OnBatteryGrace)
	policy.LowChargePercent = floatEnv(guardPowerLowChargeEnv, policy.LowChargePercent)
	policy.CriticalChargePct = floatEnv(guardPowerCritChargeEnv, policy.CriticalChargePct)
	policy.LowRuntime = durationEnv(guardPowerLowRuntimeEnv, policy.LowRuntime)
	policy.CriticalRuntime = durationEnv(guardPowerCritRuntimeEnv, policy.CriticalRuntime)
	policy.ApplicationTimeout = durationEnv(guardPowerAppTimeoutEnv, policy.ApplicationTimeout)
	policy.StorageTimeout = durationEnv(guardPowerStorageTmoEnv, policy.StorageTimeout)
	policy.HostDelay = durationEnv(guardPowerHostDelayEnv, policy.HostDelay)
	policy.HostTimeout = durationEnv(guardPowerHostTimeoutEnv, policy.HostTimeout)
	policy.ApplicationServices = listEnv(guardPowerServicesEnv)
	policy.StorageUnits = listEnv(guardPowerUnitsEnv)
	return policy
}

func floatEnv(name string, fallback float64) float64 {
	value, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(name)), 64)
	if err != nil {
		return fallback
	}
	return value
}

func listEnv(name string) []string {
	var out []string
	for _, item := range strings.Split(os.Getenv(name), ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// powerReporter sends one snapshot through the Guard's authenticated posture
// channel. The control plane stores it beside the other posture observations;
// a failed send leaves the snapshot in the local journal.
type powerReporter struct {
	url    string
	client *http.Client
	token  string
	tenant string
	signed func() bool
}

func (r powerReporter) Report(ctx context.Context, power guardpower.PowerReport) error {
	report := guardposture.Report{
		SchemaVersion: guardposture.ReportSchema,
		SourceEpoch:   power.SourceEpoch,
		Sequence:      power.Sequence,
		ObservedAt:    power.ObservedAt,
		GuardVersion:  version,
		GuardSigned:   r.signed != nil && r.signed(),
		Power:         &power,
	}
	body, err := json.Marshal(report)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if token := strings.TrimSpace(r.token); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if tenant := strings.TrimSpace(r.tenant); tenant != "" {
		request.Header.Set("X-Kombify-Tenant-ID", tenant)
	}
	response, err := r.client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 16<<10))
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("the control plane refused the power report (HTTP %d)", response.StatusCode)
	}
	return nil
}

// powerSteps implements the governed shutdown over the Guard's execution
// boundary: the privileged executor in the split layout, the in-process
// executor on a legacy root Guard.
type powerSteps struct {
	execution guardExecution
}

func (e guardExecution) powerAvailable() bool {
	if e.remote != nil {
		return e.remote.PowerAvailable()
	}
	if legacy, ok := e.runtime.(*guardexec.LegacyRoot); ok && legacy != nil {
		return legacy.PowerAvailable()
	}
	return false
}

func (e guardExecution) powerSteps() guardpower.Steps { return powerSteps{execution: e} }

func (p powerSteps) backend() (guardexec.PowerBackend, error) {
	if p.execution.remote != nil {
		return p.execution.remote, nil
	}
	if legacy, ok := p.execution.runtime.(*guardexec.LegacyRoot); ok && legacy != nil {
		return legacy, nil
	}
	return nil, errors.New("the Guard has no governed executor for host effects")
}

func (p powerSteps) StopApplications(ctx context.Context, services []string) (guardpower.StepResult, error) {
	backend, err := p.backend()
	if err != nil {
		return guardpower.StepResult{}, err
	}
	result, err := backend.PowerApplicationsStop(ctx, services)
	return guardpower.StepResult{Detail: result.Detail, Items: result.Items, Outcome: guardpower.Outcome(result.Outcome)}, err
}

func (p powerSteps) QuiesceStorage(ctx context.Context, units []string) (guardpower.StepResult, error) {
	backend, err := p.backend()
	if err != nil {
		return guardpower.StepResult{}, err
	}
	result, err := backend.PowerStorageQuiesce(ctx, units)
	return guardpower.StepResult{Detail: result.Detail, Items: result.Items, Outcome: guardpower.Outcome(result.Outcome)}, err
}

func (p powerSteps) PowerOff(ctx context.Context) error {
	backend, err := p.backend()
	if err != nil {
		return err
	}
	return backend.PowerOff(ctx)
}
