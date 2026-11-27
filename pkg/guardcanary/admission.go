package guardcanary

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var (
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	hexPattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// A pinned reference is repository@sha256:<digest>. A tag beside the digest
	// is refused too: the tag is mutable and only invites a wrong reading.
	referencePattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?::[0-9]{1,5})?(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+@(sha256:[0-9a-f]{64})$`)
)

// Admission reason codes.
const (
	ReasonStateful          = "stateful_application_unsupported"
	ReasonMutableTag        = "mutable_tag_or_unpinned_reference"
	ReasonReferenceMismatch = "reference_digest_mismatch"
	ReasonConfigInvalid     = "config_sha256_invalid"
	ReasonSameRelease       = "candidate_equals_previous"
	ReasonVerifyFailed      = "signature_verification_failed"
	ReasonUnsigned          = "image_unsigned_or_unverified"
	ReasonSignedOther       = "signature_covers_other_digest"
	ReasonUntrustedSigner   = "signer_or_issuer_not_trusted"
	ReasonEvidenceStale     = "evidence_stale"
	ReasonEvidenceFuture    = "evidence_ahead_of_clock"
	ReasonPreviousNotLive   = "previous_release_not_running"
	ReasonRuntimeUnread     = "runtime_state_unreadable"
)

// refusal is a denied admission with a stable reason code.
type refusal struct{ reason, detail string }

func (r *refusal) Error() string { return r.reason + ": " + r.detail }

func refuse(reason, format string, args ...any) *refusal {
	return &refusal{reason: reason, detail: fmt.Sprintf(format, args...)}
}

// reasonOf returns the reason code of a refusal.
func reasonOf(err error) string {
	var r *refusal
	if errors.As(err, &r) {
		return r.reason
	}
	return "internal"
}

// Validate refuses a policy with a missing or nonsensical bound.
func (p Policy) Validate() error {
	switch {
	case len(p.TrustedSigners) == 0:
		return errors.New("guardcanary: policy trusts no signer")
	case p.MaxEvidenceAge <= 0, p.MaxClockSkew < 0, p.Window <= 0, p.SampleInterval <= 0,
		p.MaxSampleAge <= 0, p.StartupDeadline <= 0, p.MaxLatencyP95 <= 0, p.RollbackTimeout <= 0:
		return errors.New("guardcanary: policy has a missing duration bound")
	case p.MinSamples < 1, p.MinRequests < 1:
		return errors.New("guardcanary: policy needs at least one sample and one request")
	case !(p.MaxErrorRate >= 0 && p.MaxErrorRate < 1):
		return errors.New("guardcanary: policy error-rate bound must be in [0,1)")
	}
	for _, s := range p.TrustedSigners {
		if strings.TrimSpace(s.Issuer) == "" || strings.TrimSpace(s.Subject) == "" ||
			strings.ContainsAny(s.Issuer+s.Subject, "*?") {
			return errors.New("guardcanary: a trusted signer needs an exact issuer and subject")
		}
	}
	return nil
}

// validateRelease checks the release is pinned to its own digest.
func validateRelease(r Release) error {
	match := referencePattern.FindStringSubmatch(r.Reference)
	if match == nil {
		return refuse(ReasonMutableTag, "reference %q is not repository@sha256:<digest>", r.Reference)
	}
	if !digestPattern.MatchString(r.Digest) || match[1] != r.Digest {
		return refuse(ReasonReferenceMismatch, "reference %q does not carry digest %q", r.Reference, r.Digest)
	}
	if !hexPattern.MatchString(r.ConfigSHA256) {
		return refuse(ReasonConfigInvalid, "configuration digest for %s is not a sha256", r.Digest)
	}
	return nil
}

// checkFresh bounds an observation time against the clock.
func checkFresh(what string, observed, now time.Time, p Policy) error {
	if observed.IsZero() {
		return refuse(ReasonEvidenceStale, "%s has no observation time", what)
	}
	if observed.After(now.Add(p.MaxClockSkew)) {
		return refuse(ReasonEvidenceFuture, "%s observed %s ahead of the clock", what, observed.Sub(now).Round(time.Millisecond))
	}
	if now.Sub(observed) > p.MaxEvidenceAge {
		return refuse(ReasonEvidenceStale, "%s is %s old, the bound is %s", what, now.Sub(observed).Round(time.Second), p.MaxEvidenceAge)
	}
	return nil
}

// checkSignature accepts evidence only when it is a fresh verified signature by
// an allow-listed identity over exactly the release digest.
func checkSignature(ev SignatureEvidence, release Release, now time.Time, p Policy) error {
	if !ev.Verified {
		return refuse(ReasonUnsigned, "no verified signature for %s", release.Digest)
	}
	if ev.Digest != release.Digest {
		return refuse(ReasonSignedOther, "signature covers %q, not %s", ev.Digest, release.Digest)
	}
	trusted := false
	for _, s := range p.TrustedSigners {
		if s.Issuer == ev.Issuer && s.Subject == ev.Subject {
			trusted = true
			break
		}
	}
	if !trusted {
		return refuse(ReasonUntrustedSigner, "issuer %q subject %q is not allow-listed", ev.Issuer, ev.Subject)
	}
	return checkFresh("signature observation", ev.ObservedAt, now, p)
}

// checkBaseline proves the previous release is what is running now, so there is
// a real retained state to return to.
func checkBaseline(run Running, previous Release, now time.Time, p Policy) error {
	if err := checkFresh("runtime observation", run.ObservedAt, now, p); err != nil {
		return err
	}
	if len(run.Digests) != 1 || run.Digests[0] != previous.Digest || run.ConfigSHA256 != previous.ConfigSHA256 {
		return refuse(ReasonPreviousNotLive, "running %v/%s, retained previous is %s/%s",
			run.Digests, run.ConfigSHA256, previous.Digest, previous.ConfigSHA256)
	}
	return nil
}
