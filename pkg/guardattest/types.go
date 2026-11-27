// Package guardattest is the deterministic, fail-closed measured-boot
// attestation gate of kombify Guard (Guard E, kombify-StackKits-p163).
//
// It answers one question for one configured sensitive operation: did this
// enrolled host, right now, present a TPM 2.0 quote signed by its enrolled
// attestation key whose PCR digest equals an approved reference value, bound
// to a single-use challenge for exactly this operation? Everything else is a
// denial, and every decision is a structured receipt. The package performs no
// I/O of its own: the verifier's reference values, enrollment and challenge
// store are interfaces the protected verifier supplies, and the existing
// operation-admission authority (RIL, host-security repair) consumes the
// receipt; this package never executes the operation.
//
// Scope and trust bounds, stated so no receipt over-claims:
//
//   - One profile: a physical TPM 2.0 on Linux, SHA-256 PCR bank. A quote from
//     a cloud-provider vTPM is refused (TrustBoundCloudVTPM), because the
//     provider, not the owner, controls that TPM. Unknown hardware is refused.
//   - Attestation key enrollment (credential activation against the endorsement
//     key) happens before this package; the enrolled key is an input.
//   - Matching reference values prove that the measured boot chain equals the
//     approved one at quote time. They prove nothing about the running system
//     afterwards and are never a malware-free claim (Receipt.NotClaimed).
package guardattest

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	// ReceiptSchema identifies the admission receipt wire shape.
	ReceiptSchema = "kombify.guard-attestation-admission.v1"
	// ProfilePhysicalTPM2Linux is the single supported hardware profile.
	ProfilePhysicalTPM2Linux = "linux-physical-tpm2-sha256"
	// BankSHA256 is the only accepted PCR bank.
	BankSHA256 = "sha256"

	// NonceSize is the size of a challenge nonce.
	NonceSize = 32
	// DefaultChallengeTTL bounds evidence age when the gate does not set one.
	DefaultChallengeTTL = 60 * time.Second
	// MaxChallengeTTL is the longest evidence age any gate may accept.
	MaxChallengeTTL = 5 * time.Minute
	// ClockSkew is the tolerance for a challenge issued slightly in the future.
	ClockSkew = 5 * time.Second
	// MaxTransitionWindow is the longest an approved firmware-update transition
	// admits the next reference set beside the current one.
	MaxTransitionWindow = 72 * time.Hour
	// MaxRecoveryTestAge is how old the last tested recovery-access proof may
	// be when a transition is admitted.
	MaxRecoveryTestAge = 30 * 24 * time.Hour

	maxPCRIndex = 23
	domainTag   = "kombify-guard-attest/v1"
)

// TrustBound names whose TPM produced the quote. Only a physical TPM under the
// owner's control is supported; the other values exist so a refusal names the
// bound instead of a generic failure.
type TrustBound string

const (
	TrustBoundPhysicalTPM2 TrustBound = "physical_tpm2"
	TrustBoundCloudVTPM    TrustBound = "cloud_vtpm"
)

// Stable denial codes. They never echo received values.
const (
	DenyInvalidRequest    = "invalid_request"
	DenyOperationNotGated = "operation_not_gated"
	DenyEvidenceMissing   = "evidence_missing"
	DenyEvidenceMalformed = "evidence_malformed"
	DenySignatureInvalid  = "signature_invalid"
	DenyKeyUnbound        = "key_unbound"
	DenyChallengeUnknown  = "challenge_unknown_or_replayed"
	DenyChallengeExpired  = "challenge_expired"
	DenyChallengeMismatch = "challenge_mismatch"
	DenyNonceMismatch     = "nonce_mismatch"
	DenyClockUnsafe       = "clock_unsafe"
	DenyBankUnsupported   = "bank_unsupported"
	DenySelectionMismatch = "pcr_selection_mismatch"
	DenyMeasurement       = "measurement_mismatch"
	DenyReferenceUnavail  = "reference_unavailable"
	DenyTrustBound        = "trust_bound_unsupported"
	DenyTransitionInvalid = "transition_invalid"
	DenyStoreUnavailable  = "challenge_store_unavailable"
)

var (
	idPattern        = regexp.MustCompile(`^[A-Za-z0-9_-]{22,64}$`)
	operationPattern = regexp.MustCompile(`^[a-z][a-z0-9._:-]{0,127}$`)
	subjectPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	versionPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:+ -]{0,127}$`)
	hexDigest        = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Challenge is one verifier-issued, single-use freshness request for one
// operation on one subject. Its nonce is secret until the quote returns it.
type Challenge struct {
	ID        string
	Nonce     []byte
	Operation string
	SubjectID string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// ExtraData is the qualifying data the host must put into the TPM quote: a
// SHA-256 over the length-prefixed domain, id, operation, subject and nonce.
// Binding the operation and subject means a quote taken for one operation or
// host cannot admit another.
func (c Challenge) ExtraData() [sha256.Size]byte {
	h := sha256.New()
	for _, part := range [][]byte{[]byte(domainTag), []byte(c.ID), []byte(c.Operation), []byte(c.SubjectID), c.Nonce} {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(part)))
		h.Write(n[:])
		h.Write(part)
	}
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

// Profile fixes the PCR bank and the exact PCR selection a quote must cover.
type Profile struct {
	ID   string
	Bank string
	PCRs []int
}

// Validate checks the profile is the supported one-bank, sorted, unique shape.
func (p Profile) Validate() error {
	if p.ID != ProfilePhysicalTPM2Linux {
		return fmt.Errorf("profile %q is not a supported measured-boot profile", p.ID)
	}
	if p.Bank != BankSHA256 {
		return fmt.Errorf("profile bank %q is not %s", p.Bank, BankSHA256)
	}
	if len(p.PCRs) == 0 || !sort.IntsAreSorted(p.PCRs) {
		return fmt.Errorf("profile PCR selection must be non-empty and ascending")
	}
	for i, pcr := range p.PCRs {
		if pcr < 0 || pcr > maxPCRIndex || (i > 0 && p.PCRs[i-1] == pcr) {
			return fmt.Errorf("profile PCR selection holds an invalid or duplicate index")
		}
	}
	return nil
}

// ReferenceSet is one approved boot measurement: the expected SHA-256 value of
// every PCR of the profile, from the owner-approved firmware and boot chain.
type ReferenceSet struct {
	ID string
	// Version names the exact approved firmware and boot chain for receipts.
	Version string
	// PCRs maps PCR index to a lowercase hex SHA-256 value.
	PCRs map[int]string
}

// Digest returns the TPM quote pcrDigest the set implies for profile: the
// SHA-256 over the selected PCR values concatenated in ascending index order.
func (r ReferenceSet) Digest(profile Profile) ([]byte, error) {
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	if !idPattern.MatchString(r.ID) || !versionPattern.MatchString(r.Version) {
		return nil, fmt.Errorf("reference set needs an id and a version")
	}
	if len(r.PCRs) != len(profile.PCRs) {
		return nil, fmt.Errorf("reference set does not cover exactly the profile PCR selection")
	}
	h := sha256.New()
	for _, pcr := range profile.PCRs {
		value, ok := r.PCRs[pcr]
		if !ok || !hexDigest.MatchString(value) {
			return nil, fmt.Errorf("reference set PCR %d is missing or not a SHA-256 value", pcr)
		}
		raw, _ := hex.DecodeString(value)
		if allBytes(raw, 0x00) || allBytes(raw, 0xff) {
			return nil, fmt.Errorf("reference set PCR %d is an unmeasured sentinel", pcr)
		}
		h.Write(raw)
	}
	return h.Sum(nil), nil
}

func allBytes(raw []byte, b byte) bool {
	for _, v := range raw {
		if v != b {
			return false
		}
	}
	return true
}

// RecoveryAccess is the declared, tested way back into a host whose next boot
// no longer matches any reference value (for example a failed firmware
// update). It is evidence the owner tested it, not an execution path.
type RecoveryAccess struct {
	Method      string
	TestedAt    time.Time
	EvidenceRef string
}

// Transition is an owner-approved firmware update: for a bounded window the
// next reference set is admitted beside the current one. It is valid only with
// recently tested recovery access.
type Transition struct {
	Next       ReferenceSet
	ApprovalID string
	ApprovedBy string
	ApprovedAt time.Time
	ExpiresAt  time.Time
	Recovery   RecoveryAccess
}

// validAt reports whether the transition may admit Next at now.
func (t Transition) validAt(now time.Time) error {
	switch {
	case !idPattern.MatchString(t.ApprovalID) || !subjectPattern.MatchString(t.ApprovedBy):
		return fmt.Errorf("transition needs an approval id and approver")
	case t.ApprovedAt.IsZero() || t.ExpiresAt.IsZero() || !t.ExpiresAt.After(t.ApprovedAt) || t.ExpiresAt.Sub(t.ApprovedAt) > MaxTransitionWindow:
		return fmt.Errorf("transition window is missing or longer than %s", MaxTransitionWindow)
	case now.Before(t.ApprovedAt) || !now.Before(t.ExpiresAt):
		return fmt.Errorf("transition is outside its approved window")
	case strings.TrimSpace(t.Recovery.Method) == "" || strings.TrimSpace(t.Recovery.EvidenceRef) == "" || t.Recovery.TestedAt.IsZero():
		return fmt.Errorf("transition has no tested recovery access")
	case t.Recovery.TestedAt.After(now) || now.Sub(t.Recovery.TestedAt) > MaxRecoveryTestAge:
		return fmt.Errorf("transition recovery access test is in the future or older than %s", MaxRecoveryTestAge)
	}
	return nil
}

// Enrollment is what the protected verifier holds about one subject: the
// enrolled attestation key, the trust bound that enrollment established, the
// profile and the approved reference values.
type Enrollment struct {
	SubjectID  string
	KeyID      string
	Key        any // *ecdsa.PublicKey (P-256) or *rsa.PublicKey (>= 2048 bits)
	TrustBound TrustBound
	Profile    Profile
	Current    ReferenceSet
	Transition *Transition
}

// Promoted returns the enrollment after an admitted firmware-update boot: the
// transition's next set becomes current and the transition ends. The receipt
// must be intact, admit exactly this enrollment's approved transition (same
// approval, reference, profile, key, trust bound and PCR digest) and have been
// decided inside the transition window. Any other receipt returns an error and
// no change, so a stale, denied, forged or foreign receipt can never move
// reference values.
func (e Enrollment) Promoted(r Receipt) (Enrollment, error) {
	refuse := errors.New("receipt does not admit this enrollment's approved transition")
	t := e.Transition
	if t == nil || r.Err() != nil || !r.Transition || t.ApprovalID == "" || r.TransitionApprovalID != t.ApprovalID {
		return Enrollment{}, refuse
	}
	if r.SubjectID != e.SubjectID || r.ReferenceID != t.Next.ID || r.ReferenceVersion != t.Next.Version ||
		r.ProfileID != e.Profile.ID || e.KeyID == "" || r.KeyID != e.KeyID || r.TrustBound != TrustBoundPhysicalTPM2 || e.TrustBound != TrustBoundPhysicalTPM2 {
		return Enrollment{}, refuse
	}
	if t.validAt(r.DecidedAt) != nil {
		return Enrollment{}, refuse
	}
	want, err := t.Next.Digest(e.Profile)
	got, decodeErr := hex.DecodeString(r.PCRDigest)
	if err != nil || decodeErr != nil || subtle.ConstantTimeCompare(got, want) != 1 {
		return Enrollment{}, refuse
	}
	e.Current = t.Next
	e.Transition = nil
	return e, nil
}
