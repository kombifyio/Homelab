package guardcanary

import (
	"fmt"
	"strings"
	"time"
)

// Verdict is the outcome of a canary assessment.
type Verdict string

const (
	// VerdictHealthy is the only verdict that lets a release stay.
	VerdictHealthy Verdict = "healthy"
	// VerdictUnhealthy means probes observed a violated bound.
	VerdictUnhealthy Verdict = "unhealthy"
	// VerdictInconclusive means the probes cannot prove health: missing, stale,
	// from another digest or too few requests. It fails closed like unhealthy.
	VerdictInconclusive Verdict = "inconclusive"
)

// Assessment is the verdict and the reasons that produced it.
type Assessment struct {
	Verdict Verdict
	Reasons []string
	// Fresh is the number of samples that counted; NewestAge the age of the
	// newest counted sample at assessment time.
	Fresh     int
	NewestAge time.Duration
}

// Assess evaluates samples of one digest against the policy. Samples that are
// stale or ahead of the clock are dropped; a sample from another digest makes
// the assessment inconclusive because the probe was not observing the release.
// Unhealthy outranks inconclusive: a violated bound is never softened.
func Assess(p Policy, digest string, samples []Sample, now time.Time) Assessment {
	var fresh []Sample
	var unhealthy, inconclusive []string
	dropped := 0
	for _, s := range samples {
		if s.ObservedAt.IsZero() || s.ObservedAt.After(now.Add(p.MaxClockSkew)) || now.Sub(s.ObservedAt) > p.MaxSampleAge {
			dropped++
			continue
		}
		if s.Digest != digest {
			inconclusive = append(inconclusive, fmt.Sprintf("probe answered for %q, not %s", s.Digest, digest))
			continue
		}
		fresh = append(fresh, s)
	}
	if dropped > 0 {
		inconclusive = append(inconclusive, fmt.Sprintf("%d sample(s) stale or ahead of the clock", dropped))
	}
	a := Assessment{Fresh: len(fresh)}
	if len(fresh) > 0 {
		newest := fresh[0].ObservedAt
		for _, s := range fresh {
			if s.ObservedAt.After(newest) {
				newest = s.ObservedAt
			}
		}
		a.NewestAge = now.Sub(newest)
	}

	var requests, errs uint64
	var worstP95 time.Duration
	for _, s := range fresh {
		requests += s.Requests
		errs += s.Errors
		if s.LatencyP95 > worstP95 {
			worstP95 = s.LatencyP95
		}
		if s.Errors > s.Requests {
			unhealthy = append(unhealthy, "sample reports more errors than requests")
		}
		if s.Started && s.StartupDuration > p.StartupDeadline {
			unhealthy = append(unhealthy, fmt.Sprintf("startup took %s, the deadline is %s", s.StartupDuration, p.StartupDeadline))
		}
	}
	if len(fresh) < p.MinSamples {
		inconclusive = append(inconclusive, fmt.Sprintf("%d fresh sample(s), %d required", len(fresh), p.MinSamples))
	} else {
		// The newest MinSamples samples must all be started and ready: health is
		// the steady state, not a single good probe.
		tail := newestN(fresh, p.MinSamples)
		for _, s := range tail {
			if !s.Started || !s.Ready {
				inconclusive = append(inconclusive, "newest probes are not started and ready")
				break
			}
		}
	}
	if requests < p.MinRequests {
		inconclusive = append(inconclusive, fmt.Sprintf("%d request(s) observed, %d required for an error rate", requests, p.MinRequests))
	} else if float64(errs)/float64(requests) > p.MaxErrorRate {
		unhealthy = append(unhealthy, fmt.Sprintf("error rate %d/%d exceeds %.4f", errs, requests, p.MaxErrorRate))
	}
	if worstP95 > p.MaxLatencyP95 {
		unhealthy = append(unhealthy, fmt.Sprintf("p95 latency %s exceeds %s", worstP95, p.MaxLatencyP95))
	}

	switch {
	case len(unhealthy) > 0:
		a.Verdict, a.Reasons = VerdictUnhealthy, append(unhealthy, inconclusive...)
	case len(inconclusive) > 0:
		a.Verdict, a.Reasons = VerdictInconclusive, inconclusive
	default:
		a.Verdict, a.Reasons = VerdictHealthy, []string{"startup, readiness, error rate and latency within bounds"}
	}
	return a
}

// newestN returns the n samples with the latest observation times.
func newestN(samples []Sample, n int) []Sample {
	sorted := append([]Sample(nil), samples...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].ObservedAt.After(sorted[j-1].ObservedAt); j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	if n > len(sorted) {
		n = len(sorted)
	}
	return sorted[:n]
}

func joinReasons(r []string) string { return strings.Join(r, "; ") }
