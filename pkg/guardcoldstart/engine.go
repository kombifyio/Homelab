package guardcoldstart

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/kombifyio/homelab/pkg/guardbinding"
)

// Denial codes of a rehearsal, besides the guardbinding codes it reuses
// (authority_expired, authority_revoked, online_revalidation_required,
// local_authority_missing, binding_mismatch, stale_generation).
const (
	CodePlanInvalid       = "cold_start_plan_invalid"
	CodeArtifactIntegrity = "cached_artifact_integrity"
	CodeCutNotProven      = "cut_not_proven"
	CodeEgressViolation   = "egress_violation"
	CodeLocalAccessOpen   = "local_access_not_enforced"
	CodeStepFailed        = "recovery_step_failed"
	CodeClockUntrusted    = guardbinding.CodeRevalidationRequired
)

// ArtifactCache is the node's local artifact store.
type ArtifactCache interface {
	Open(name string) (io.ReadCloser, error)
}

// DependencyState is what the host observed of one declared dependency after
// the cut.
type DependencyState struct {
	Name      string
	Reachable bool
}

// CutObserver reports whether the declared remote dependencies are unreachable
// from the node, as the host's cut harness observed them.
type CutObserver interface {
	Observe(ctx context.Context, deps []Dependency) ([]DependencyState, error)
}

// StepResult values.
const (
	ResultDone    = "done"
	ResultFailed  = "failed"
	ResultUnknown = "unknown"
)

// StepInput is what a step runner receives: the step, the pinned release it
// must run through, and the only network it may use.
type StepInput struct {
	Plan     Plan
	Step     Step
	EffectID string
	Dial     DialFunc
}

// StepRunner runs one node-local recovery step through the pinned StackKits
// release behind the Guard executor. It is no executor of its own.
type StepRunner interface {
	RunStep(ctx context.Context, in StepInput) (string, error)
}

// LocalAccess probes the recovered application's own authentication.
type LocalAccess interface {
	// UnauthenticatedDenied reports whether an unauthenticated local request is
	// refused. Expired or missing authority must never make this false.
	UnauthenticatedDenied(ctx context.Context) (bool, error)
}

// Journal is the node's durable record of completed effects. A step whose
// effect it holds is not run again.
type Journal interface {
	Completed(effectID string) bool
	Record(effectID string) error
}

// MemoryJournal is a Journal for a single process.
type MemoryJournal struct {
	mu   sync.Mutex
	done map[string]bool
}

// Completed implements Journal.
func (j *MemoryJournal) Completed(id string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.done[id]
}

// Record implements Journal.
func (j *MemoryJournal) Record(id string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.done == nil {
		j.done = map[string]bool{}
	}
	j.done[id] = true
	return nil
}

// ClockEvidence says whether the wall clock can be trusted after the reboot.
// Without a trusted source the authority window cannot be measured and the
// rehearsal refuses.
type ClockEvidence struct {
	Trusted bool
	Source  string
}

// Input is everything one rehearsal needs. Every dependency is required.
type Input struct {
	Plan    Plan
	Pins    []guardbinding.PinnedKey
	Expect  guardbinding.Expectation
	Clock   ClockEvidence
	Revoked bool

	Cache   ArtifactCache
	Cut     CutObserver
	Runner  StepRunner
	Access  LocalAccess
	Journal Journal
	// LocalDial carries the attempts the egress allows; may be nil.
	LocalDial DialFunc

	// CutAt and BootAt are host-reported; zero means unknown.
	CutAt  time.Time
	BootAt time.Time
}

// Engine runs rehearsals. Now defaults to time.Now.
type Engine struct {
	Now func() time.Time
}

func (e Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

type run struct {
	engine  Engine
	in      Input
	chain   chain
	receipt Receipt
	started time.Time
}

// Run executes one rehearsal and always returns a receipt that verifies with
// VerifyChain. The error is non-nil only for a missing dependency, which is a
// caller defect, never a rehearsal result.
func (e Engine) Run(ctx context.Context, in Input) (*Receipt, error) {
	if in.Cache == nil || in.Cut == nil || in.Runner == nil || in.Access == nil || in.Journal == nil {
		return nil, errors.New("guardcoldstart: cache, cut observer, runner, local access and journal are required")
	}
	r := &run{engine: e, in: in, started: e.now()}
	r.receipt = Receipt{Version: ReceiptVersion, PlanID: in.Plan.PlanID, Release: in.Plan.Release.Version,
		Egress: []EgressAttempt{}, Effects: []Effect{}, NotClaimed: NotClaimed(),
		Timing: Timing{RehearsalStart: r.started.UTC(), CutAt: in.CutAt.UTC(), BootAt: in.BootAt.UTC(), RecoverySeconds: -1}}
	r.chain.add(KindStarted, r.started, map[string]string{"plan_id": in.Plan.PlanID, "clock_source": in.Clock.Source})
	r.execute(ctx)
	return &r.receipt, nil
}

func (r *run) execute(ctx context.Context) {
	in := r.in
	if d := r.admit(); d != nil {
		r.finish(OutcomeDenied, d)
		return
	}
	if d := r.verifyArtifacts(); d != nil {
		r.finish(OutcomeFailed, d)
		return
	}
	if d := r.observeCut(ctx); d != nil {
		r.finish(OutcomeDenied, d)
		return
	}
	egress, err := NewEgress(in.Plan, in.LocalDial)
	if err != nil {
		r.finish(OutcomeFailed, &Denial{Code: CodePlanInvalid, Message: err.Error()})
		return
	}
	outcome, denial := r.runSteps(ctx, egress)
	r.receipt.Egress = egress.Attempts()
	r.chain.add(KindEgress, r.engine.now(), map[string]string{
		"attempts": strconv.Itoa(len(r.receipt.Egress)), "violations": strconv.Itoa(egress.Violations()),
	})
	if outcome == "" {
		outcome, denial = r.checkLocalAccess(ctx, egress)
	}
	r.finish(outcome, denial)
}

// admit verifies the plan and the offline authority. It runs before any
// artifact is read or step started, so a denial changes nothing on the host.
func (r *run) admit() *Denial {
	in := r.in
	plan := in.Plan
	digest, err := plan.Digest()
	if err == nil {
		r.receipt.PlanDigest = digest
	}
	fail := func(d *guardbinding.Denial) *Denial {
		r.chain.add(KindAuthority, r.engine.now(), map[string]string{"result": "denied", "code": d.Code})
		return &Denial{Code: d.Code, Message: d.Message}
	}
	if d := plan.VerifySignature(in.Pins); d != nil {
		return fail(d)
	}
	if d := plan.Validate(); d != nil {
		return fail(d)
	}
	if d := r.checkBinding(); d != nil {
		return fail(d)
	}
	if !in.Clock.Trusted {
		return fail(guardbinding.Denyf(CodeClockUntrusted, "the clock after reboot has no trusted source, so the authority window cannot be measured"))
	}
	if d := r.checkAuthority(); d != nil {
		return fail(d)
	}
	r.chain.add(KindAuthority, r.engine.now(), map[string]string{
		"result": "admitted", "authority_until": plan.AuthorityUntil().UTC().Format(time.RFC3339),
	})
	return nil
}

func (r *run) checkBinding() *guardbinding.Denial {
	plan, expect := r.in.Plan, r.in.Expect
	switch {
	case expect.TenantID != "" && plan.TenantID != expect.TenantID,
		expect.WorkerID != "" && plan.WorkerID != expect.WorkerID,
		expect.ServerID != "" && plan.ServerID != "" && plan.ServerID != expect.ServerID:
		return guardbinding.Denyf(guardbinding.CodeBindingMismatch, "the cold-start plan is bound to another node")
	case plan.Generation < expect.HighestGeneration:
		return guardbinding.Denyf(guardbinding.CodeStaleGeneration, "the cold-start plan names an older credential generation")
	}
	return nil
}

// checkAuthority is also the per-step check: expired authority never lapses
// into a local bypass, it stops the recovery.
func (r *run) checkAuthority() *guardbinding.Denial {
	plan, now := r.in.Plan, r.engine.now()
	if r.in.Revoked {
		return guardbinding.Denyf(guardbinding.CodeAuthorityRevoked, "this node observed that its identity was revoked")
	}
	if now.Before(plan.IssuedAt.Add(-guardbinding.ResultClockMargin)) {
		return guardbinding.Denyf(guardbinding.CodeRevalidationRequired, "the clock is %s before the plan was issued", plan.IssuedAt.Sub(now).Round(time.Second))
	}
	if !now.Before(plan.AuthorityUntil()) {
		return guardbinding.Denyf(guardbinding.CodeAuthorityExpired, "the offline authority ended at %s", plan.AuthorityUntil().UTC().Format(time.RFC3339))
	}
	return nil
}

func (r *run) verifyArtifacts() *Denial {
	for _, a := range r.in.Plan.Artifacts {
		sum, err := r.digest(a)
		if err != nil || sum != a.SHA256 {
			msg := fmt.Sprintf("cached artifact %q does not match its pinned digest", a.Name)
			if err != nil {
				msg = fmt.Sprintf("cached artifact %q cannot be read: %v", a.Name, err)
			}
			r.chain.add(KindArtifact, r.engine.now(), map[string]string{"name": a.Name, "result": "refused"})
			return &Denial{Code: CodeArtifactIntegrity, Message: msg}
		}
		r.chain.add(KindArtifact, r.engine.now(), map[string]string{"name": a.Name, "role": a.Role, "sha256": a.SHA256, "result": "verified"})
	}
	return nil
}

func (r *run) digest(a Artifact) (string, error) {
	f, err := r.in.Cache.Open(a.Name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, a.Size+1))
	if err != nil {
		return "", err
	}
	if n != a.Size {
		return "", fmt.Errorf("size %d, pinned %d", n, a.Size)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (r *run) observeCut(ctx context.Context) *Denial {
	deps := r.in.Plan.Dependencies
	states, err := r.in.Cut.Observe(ctx, deps)
	reachable := map[string]bool{}
	seen := map[string]bool{}
	for _, s := range states {
		seen[s.Name] = true
		reachable[s.Name] = s.Reachable
	}
	for _, d := range deps {
		switch {
		case err != nil || !seen[d.Name]:
			r.chain.add(KindCut, r.engine.now(), map[string]string{"dependency": d.Name, "result": "unobserved"})
			return &Denial{Code: CodeCutNotProven, Message: fmt.Sprintf("dependency %q was not observed unreachable", d.Name)}
		case reachable[d.Name]:
			r.chain.add(KindCut, r.engine.now(), map[string]string{"dependency": d.Name, "result": "reachable"})
			return &Denial{Code: CodeCutNotProven, Message: fmt.Sprintf("dependency %q is still reachable", d.Name)}
		}
		r.chain.add(KindCut, r.engine.now(), map[string]string{"dependency": d.Name, "kind": d.Kind, "result": "unreachable"})
	}
	return nil
}

// runSteps returns an empty outcome when every step completed.
func (r *run) runSteps(ctx context.Context, egress *Egress) (string, *Denial) {
	plan := r.in.Plan
	for _, step := range plan.Steps {
		id := plan.EffectID(step.ID)
		if r.in.Journal.Completed(id) {
			r.receipt.Effects = append(r.receipt.Effects, Effect{EffectID: id, StepID: step.ID, Result: ResultDone})
			r.chain.add(KindStep, r.engine.now(), map[string]string{"step": step.ID, "effect": id, "result": "already_done"})
			continue
		}
		if d := r.checkAuthority(); d != nil {
			r.chain.add(KindStep, r.engine.now(), map[string]string{"step": step.ID, "result": "authority_ended", "code": d.Code})
			return OutcomeDenied, &Denial{Code: d.Code, Message: d.Message}
		}
		before := egress.Violations()
		result, err := r.in.Runner.RunStep(ctx, StepInput{Plan: plan, Step: step, EffectID: id, Dial: egress.Dial})
		if err != nil && result == "" {
			result = ResultUnknown
		}
		r.receipt.Effects = append(r.receipt.Effects, Effect{EffectID: id, StepID: step.ID, Result: result, Executed: true})
		r.chain.add(KindStep, r.engine.now(), map[string]string{"step": step.ID, "effect": id, "result": result})
		if egress.Violations() > before {
			return OutcomeFailed, &Denial{Code: CodeEgressViolation, Message: fmt.Sprintf("step %q reached for the network while it is cut", step.ID)}
		}
		switch result {
		case ResultDone:
			if err := r.in.Journal.Record(id); err != nil {
				return OutcomeUnknown, &Denial{Code: CodeStepFailed, Message: fmt.Sprintf("effect of step %q done but not journaled: %v", step.ID, err)}
			}
		case ResultFailed:
			return OutcomeFailed, &Denial{Code: CodeStepFailed, Message: fmt.Sprintf("step %q failed", step.ID)}
		default:
			return OutcomeUnknown, &Denial{Code: CodeStepFailed, Message: fmt.Sprintf("the outcome of step %q is unknown", step.ID)}
		}
	}
	return "", nil
}

func (r *run) checkLocalAccess(ctx context.Context, egress *Egress) (string, *Denial) {
	denied, err := r.in.Access.UnauthenticatedDenied(ctx)
	result := "enforced"
	switch {
	case err != nil:
		result = "unobserved"
	case !denied:
		result = "open"
	}
	r.chain.add(KindLocalAccess, r.engine.now(), map[string]string{"result": result})
	if result != "enforced" {
		return OutcomeFailed, &Denial{Code: CodeLocalAccessOpen, Message: "the recovered application's local authentication was not observed enforced"}
	}
	if egress.Violations() > 0 {
		return OutcomeFailed, &Denial{Code: CodeEgressViolation, Message: "outbound access was attempted while it is cut"}
	}
	return OutcomeRecovered, nil
}

func (r *run) finish(outcome string, denial *Denial) {
	now := r.engine.now()
	r.receipt.Outcome, r.receipt.Denial = outcome, denial
	t := &r.receipt.Timing
	t.RehearsalSeconds = now.Sub(r.started).Seconds()
	if outcome == OutcomeRecovered {
		t.RecoveredAt = now.UTC()
		if !r.in.BootAt.IsZero() {
			t.RecoverySeconds = now.Sub(r.in.BootAt).Seconds()
		}
	}
	r.receipt.Unknowns = r.unknowns(outcome)
	if len(r.receipt.Effects) > 0 {
		r.chain.add(KindReconcileQueued, now, map[string]string{"effects": strconv.Itoa(len(r.receipt.Effects))})
	}
	fields := map[string]string{"outcome": outcome}
	if denial != nil {
		fields["code"] = denial.Code
	}
	r.chain.add(KindFinished, now, fields)
	r.receipt.Entries = r.chain.entries
	r.receipt.HeadHash = r.chain.entries[len(r.chain.entries)-1].Hash
}

func (r *run) unknowns(outcome string) []string {
	u := []string{}
	if r.in.CutAt.IsZero() {
		u = append(u, "cut_time_unknown")
	}
	if r.in.BootAt.IsZero() {
		u = append(u, "boot_time_unknown: recovery duration from boot cannot be stated")
	}
	if outcome != OutcomeRecovered {
		u = append(u, "recovery_not_achieved")
	}
	for _, e := range r.receipt.Effects {
		if e.Result == ResultUnknown {
			u = append(u, "effect_outcome_unknown:"+e.StepID)
		}
	}
	return u
}
