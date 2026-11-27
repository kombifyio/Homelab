package guardcanary

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"
)

// run carries the state of one qualification.
type run struct {
	cfg   Config
	req   Request
	c     chain
	start time.Time

	oldestAdmission time.Time
	baselineAt      time.Time
	canaryNewest    time.Time
	recoveryNewest  time.Time
	unknowns        []string
}

func (cfg Config) now() time.Time {
	if cfg.Now != nil {
		return cfg.Now()
	}
	return time.Now()
}

func (cfg Config) sleep(ctx context.Context, d time.Duration) error {
	if cfg.Sleep != nil {
		return cfg.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Qualify runs one stateless OCI update and returns its receipt. Admission
// refusals, an unhealthy or inconclusive canary and a failed rollback are all
// recorded outcomes, not errors; an error means the request or wiring is
// invalid and nothing was observed or changed.
//
// A candidate that is not proven healthy never stays: the retained previous
// release is applied again on a context detached from a cancelled caller, and
// the receipt claims a rollback only after the previous digest is observed
// running and healthy and the candidate digest is observed gone.
func Qualify(ctx context.Context, cfg Config, req Request) (Receipt, error) {
	if err := cfg.Policy.Validate(); err != nil {
		return Receipt{}, err
	}
	if cfg.Verifier == nil || cfg.Runtime == nil {
		return Receipt{}, errors.New("guardcanary: verifier and runtime are required")
	}
	if req.App == "" {
		return Receipt{}, errors.New("guardcanary: application name is required")
	}
	r := &run{cfg: cfg, req: req, start: cfg.now()}
	r.unknowns = []string{
		"signature_verification_internals_are_observed_only_as_reported_by_the_injected_verifier",
		"transparency_log_inclusion_is_not_evaluated_here",
		"probe_source_authenticity_is_not_evaluated_here",
	}
	if req.EvidenceSource == "" {
		r.unknowns = append(r.unknowns, "evidence_source_not_declared")
	}
	return r.execute(ctx), nil
}

func (r *run) execute(ctx context.Context) Receipt {
	pol := r.cfg.Policy
	prev, cand := r.req.Previous, r.req.Candidate

	if !r.req.Stateless {
		return r.reject("", ReasonStateful, "a stateful application needs its own compatibility contract")
	}
	if err := validateRelease(prev); err != nil {
		return r.reject(KindAdmissionPrevious, reasonOf(err), err.Error())
	}
	if err := validateRelease(cand); err != nil {
		return r.reject(KindAdmissionCandidate, reasonOf(err), err.Error())
	}
	if prev.Digest == cand.Digest {
		return r.reject(KindAdmissionCandidate, ReasonSameRelease, "candidate digest is the running digest")
	}

	// Admission: the rollback target must itself be admissible, so a rollback
	// never lands on an image that would be refused today.
	for _, step := range []struct {
		kind    string
		release Release
	}{{KindAdmissionPrevious, prev}, {KindAdmissionCandidate, cand}} {
		ev, err := r.cfg.Verifier.Verify(ctx, step.release.Reference)
		if err != nil {
			return r.reject(step.kind, ReasonVerifyFailed, err.Error())
		}
		if err := checkSignature(ev, step.release, r.cfg.now(), pol); err != nil {
			return r.reject(step.kind, reasonOf(err), err.Error())
		}
		if r.oldestAdmission.IsZero() || ev.ObservedAt.Before(r.oldestAdmission) {
			r.oldestAdmission = ev.ObservedAt
		}
		r.c.add(r.cfg.now(), step.kind, step.release.Digest, ResultOK, "", map[string]string{
			"issuer": ev.Issuer, "subject": ev.Subject, "signatureObservedAt": stamp(ev.ObservedAt),
		})
	}

	live, err := r.cfg.Runtime.Running(ctx)
	if err != nil {
		return r.reject(KindBaseline, ReasonRuntimeUnread, err.Error())
	}
	if err := checkBaseline(live, prev, r.cfg.now(), pol); err != nil {
		return r.reject(KindBaseline, reasonOf(err), err.Error())
	}
	r.baselineAt = live.ObservedAt
	r.c.add(r.cfg.now(), KindBaseline, prev.Digest, ResultOK, "", map[string]string{"configSha256": prev.ConfigSHA256})

	// From here the host may change: every failure ends in rollback.
	if err := r.cfg.Runtime.Apply(ctx, cand); err != nil {
		r.c.add(r.cfg.now(), KindApplyCandidate, cand.Digest, ResultError, "apply_failed", map[string]string{"error": err.Error()})
		return r.rollback(ctx, "candidate apply failed: "+err.Error())
	}
	r.c.add(r.cfg.now(), KindApplyCandidate, cand.Digest, ResultOK, "", nil)

	if reason := r.runningMatches(ctx, cand, KindCandidateRunning); reason != "" {
		return r.rollback(ctx, reason)
	}

	a, detail := r.canary(ctx, cand.Digest)
	if a.Fresh > 0 {
		r.canaryNewest = r.cfg.now().Add(-a.NewestAge)
	}
	r.c.add(r.cfg.now(), KindCanary, cand.Digest, string(a.Verdict), "", detail)
	if a.Verdict != VerdictHealthy {
		return r.rollback(ctx, "canary "+string(a.Verdict)+": "+joinReasons(a.Reasons))
	}
	return r.finish(OutcomePromoted, "candidate digest admitted and canary healthy")
}

// runningMatches records and checks that exactly the release is running.
// It returns a failure reason, or "" when the observation matches.
func (r *run) runningMatches(ctx context.Context, want Release, kind string) string {
	got, err := r.cfg.Runtime.Running(ctx)
	if err != nil {
		r.c.add(r.cfg.now(), kind, want.Digest, ResultError, ReasonRuntimeUnread, map[string]string{"error": err.Error()})
		return "runtime state unreadable after apply: " + err.Error()
	}
	detail := map[string]string{"running": fmt.Sprint(got.Digests), "configSha256": got.ConfigSHA256}
	if err := checkFresh("runtime observation", got.ObservedAt, r.cfg.now(), r.cfg.Policy); err != nil {
		r.c.add(r.cfg.now(), kind, want.Digest, ResultRefused, reasonOf(err), detail)
		return err.Error()
	}
	if len(got.Digests) != 1 || got.Digests[0] != want.Digest || got.ConfigSHA256 != want.ConfigSHA256 {
		r.c.add(r.cfg.now(), kind, want.Digest, ResultRefused, ReasonReferenceMismatch, detail)
		return fmt.Sprintf("running %v/%s is not the applied %s/%s", got.Digests, got.ConfigSHA256, want.Digest, want.ConfigSHA256)
	}
	r.c.add(r.cfg.now(), kind, want.Digest, ResultOK, "", detail)
	return ""
}

// canary samples the release until it is healthy, unhealthy or the window ends.
func (r *run) canary(ctx context.Context, digest string) (Assessment, map[string]string) {
	pol := r.cfg.Policy
	var samples []Sample
	probeErrors := 0
	began := r.cfg.now()
	var a Assessment
	for {
		s, err := r.cfg.Runtime.Probe(ctx)
		if err != nil {
			probeErrors++
		} else {
			samples = append(samples, s)
		}
		now := r.cfg.now()
		a = Assess(pol, digest, samples, now)
		if probeErrors > 0 {
			a.Reasons = append(a.Reasons, strconv.Itoa(probeErrors)+" probe(s) failed")
		}
		if a.Verdict != VerdictInconclusive {
			break
		}
		if now.Sub(began)+pol.SampleInterval > pol.Window {
			a.Reasons = append(a.Reasons, "canary window elapsed without proof")
			break
		}
		if err := r.cfg.sleep(ctx, pol.SampleInterval); err != nil {
			a.Reasons = append(a.Reasons, "canary interrupted: "+err.Error())
			break
		}
	}
	return a, map[string]string{
		"samples": strconv.Itoa(len(samples)), "fresh": strconv.Itoa(a.Fresh), "probeErrors": strconv.Itoa(probeErrors),
		"newestSampleAgeSeconds": strconv.FormatInt(int64(a.NewestAge/time.Second), 10), "reasons": joinReasons(a.Reasons),
	}
}

// rollback applies the retained previous release again and proves it. It runs
// on a detached, bounded context so a cancelled caller cannot leave the
// rejected candidate running.
func (r *run) rollback(ctx context.Context, why string) Receipt {
	prev, cand := r.req.Previous, r.req.Candidate
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.cfg.Policy.RollbackTimeout)
	defer cancel()

	if err := r.cfg.Runtime.Apply(rctx, prev); err != nil {
		r.c.add(r.cfg.now(), KindRollbackApply, prev.Digest, ResultError, "rollback_apply_failed", map[string]string{"error": err.Error()})
		return r.finish(OutcomeRollbackFailed, why+"; rollback apply failed: "+err.Error())
	}
	r.c.add(r.cfg.now(), KindRollbackApply, prev.Digest, ResultOK, "", map[string]string{"configSha256": prev.ConfigSHA256})

	got, err := r.cfg.Runtime.Running(rctx)
	if err != nil {
		r.c.add(r.cfg.now(), KindRollbackVerify, prev.Digest, ResultError, ReasonRuntimeUnread, map[string]string{"error": err.Error()})
		return r.finish(OutcomeRollbackFailed, why+"; runtime unreadable after rollback: "+err.Error())
	}
	candRunning := slices.Contains(got.Digests, cand.Digest)
	detail := map[string]string{
		"running": fmt.Sprint(got.Digests), "configSha256": got.ConfigSHA256,
		"rejectedDigestRunning": strconv.FormatBool(candRunning),
	}
	if err := checkFresh("runtime observation", got.ObservedAt, r.cfg.now(), r.cfg.Policy); err != nil ||
		len(got.Digests) != 1 || got.Digests[0] != prev.Digest || got.ConfigSHA256 != prev.ConfigSHA256 {
		r.c.add(r.cfg.now(), KindRollbackVerify, prev.Digest, ResultRefused, "previous_release_not_restored", detail)
		return r.finish(OutcomeRollbackFailed, why+"; previous release is not what runs after rollback")
	}
	r.c.add(r.cfg.now(), KindRollbackVerify, prev.Digest, ResultOK, "", detail)

	a, detail := r.canary(rctx, prev.Digest)
	if a.Fresh > 0 {
		r.recoveryNewest = r.cfg.now().Add(-a.NewestAge)
	}
	r.c.add(r.cfg.now(), KindRecovery, prev.Digest, string(a.Verdict), "", detail)
	if a.Verdict != VerdictHealthy {
		return r.finish(OutcomeRollbackFailed, why+"; previous release did not recover: "+joinReasons(a.Reasons))
	}
	return r.finish(OutcomeRolledBack, why)
}

// reject ends a run that never touched the host.
func (r *run) reject(kind, reason, detail string) Receipt {
	if kind == "" {
		kind = KindAdmissionCandidate
	}
	r.c.add(r.cfg.now(), kind, "", ResultRefused, reason, map[string]string{"detail": detail})
	return r.finish(OutcomeRejected, reason+": "+detail)
}

func (r *run) finish(outcome, reason string) Receipt {
	end := r.cfg.now()
	r.c.add(end, KindOutcome, "", outcome, "", map[string]string{"reason": reason})
	ages := map[string]int64{}
	age := func(name string, t time.Time) {
		if !t.IsZero() {
			ages[name] = int64(end.Sub(t) / time.Second)
		}
	}
	age("admission", r.oldestAdmission)
	age("baseline", r.baselineAt)
	age("canary", r.canaryNewest)
	age("recovery", r.recoveryNewest)
	unknowns := slices.Clone(r.unknowns)
	if _, ok := ages["canary"]; !ok {
		unknowns = append(unknowns, "candidate_health_never_observed")
	}
	if outcome == OutcomeRollbackFailed {
		unknowns = append(unknowns, "host_state_after_failed_rollback")
	}
	rec := Receipt{
		Schema: ReceiptSchema, PackageVersion: PackageVersion,
		Scope:    Scope{App: r.req.App, Slice: SliceStatelessOCI, OS: "linux", EvidenceSource: r.req.EvidenceSource},
		Versions: Versions{Previous: r.req.Previous, Candidate: r.req.Candidate, TrustedSigners: slices.Clone(r.cfg.Policy.TrustedSigners)},
		Outcome:  outcome, OutcomeReason: reason, StartedAt: stamp(r.start), FinishedAt: stamp(end),
		EvidenceAgeSeconds: ages, Unknowns: unknowns, NotClaimed: slices.Clone(NotClaimed),
		Chain: r.c.entries, HeadHash: r.c.entries[len(r.c.entries)-1].Hash,
	}
	rec.ReceiptSHA256 = receiptHash(rec)
	return rec
}
