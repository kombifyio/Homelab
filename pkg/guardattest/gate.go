package guardattest

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"
)

// Authority is the protected verifier's source of enrollments and approved
// reference values. An error means no admission: the gate never falls back to
// cached, default or caller-supplied values.
type Authority interface {
	Enrollment(subjectID string) (Enrollment, error)
}

// ChallengeStore holds issued challenges until one attestation consumes them.
// Take must be atomic and single use: a second Take of one id reports false,
// which is what makes a replayed quote fail.
type ChallengeStore interface {
	Put(Challenge) error
	Take(id string) (Challenge, bool, error)
}

// Evidence is what the host sends back for one challenge.
type Evidence struct {
	Quote     []byte
	Signature []byte
}

// Request asks the gate to admit one operation on one subject.
type Request struct {
	ChallengeID string
	Operation   string
	SubjectID   string
	Evidence    Evidence
}

// Config assembles a gate for exactly one sensitive operation.
type Config struct {
	// Operation is the one gated operation id.
	Operation  string
	Authority  Authority
	Challenges ChallengeStore
	// Verifier defaults to TPMQuoteVerifier.
	Verifier QuoteVerifier
	// TTL bounds evidence age; defaults to DefaultChallengeTTL, capped at
	// MaxChallengeTTL.
	TTL time.Duration
	// Now and Rand default to the wall clock and crypto/rand.
	Now  func() time.Time
	Rand io.Reader
}

// Gate is the measured-boot admission gate of one operation.
type Gate struct {
	operation  string
	authority  Authority
	challenges ChallengeStore
	verifier   QuoteVerifier
	ttl        time.Duration
	now        func() time.Time
	rand       io.Reader
}

// New validates cfg. A gate without an authority or challenge store cannot be
// built, so misconfiguration fails at construction and never as an admission.
func New(cfg Config) (*Gate, error) {
	if !operationPattern.MatchString(cfg.Operation) {
		return nil, errors.New("guardattest: gate needs one valid operation id")
	}
	if cfg.Authority == nil || cfg.Challenges == nil {
		return nil, errors.New("guardattest: gate needs an authority and a challenge store")
	}
	g := &Gate{operation: cfg.Operation, authority: cfg.Authority, challenges: cfg.Challenges, verifier: cfg.Verifier, ttl: cfg.TTL, now: cfg.Now, rand: cfg.Rand}
	if g.verifier == nil {
		g.verifier = TPMQuoteVerifier{}
	}
	if g.ttl < 0 || (g.ttl > 0 && g.ttl < time.Second) {
		return nil, errors.New("guardattest: gate TTL must be at least one second")
	}
	if g.ttl == 0 {
		g.ttl = DefaultChallengeTTL
	}
	if g.ttl > MaxChallengeTTL {
		g.ttl = MaxChallengeTTL
	}
	if g.now == nil {
		g.now = time.Now
	}
	if g.rand == nil {
		g.rand = rand.Reader
	}
	return g, nil
}

// Challenge issues a single-use challenge for the gated operation on subject.
func (g *Gate) Challenge(subjectID string) (Challenge, error) {
	if !subjectPattern.MatchString(subjectID) {
		return Challenge{}, errors.New("guardattest: challenge needs a valid subject id")
	}
	var idRaw [16]byte
	nonce := make([]byte, NonceSize)
	if _, err := io.ReadFull(g.rand, idRaw[:]); err != nil {
		return Challenge{}, fmt.Errorf("guardattest: create challenge id: %w", err)
	}
	if _, err := io.ReadFull(g.rand, nonce); err != nil {
		return Challenge{}, fmt.Errorf("guardattest: create challenge nonce: %w", err)
	}
	now := g.now().UTC()
	c := Challenge{ID: base64.RawURLEncoding.EncodeToString(idRaw[:]), Nonce: nonce, Operation: g.operation, SubjectID: subjectID, IssuedAt: now, ExpiresAt: now.Add(g.ttl)}
	// The store keeps its own copy; the caller's slice can be mutated freely.
	if err := g.challenges.Put(c); err != nil {
		return Challenge{}, fmt.Errorf("guardattest: store challenge: %w", err)
	}
	c.Nonce = slices.Clone(nonce)
	return c, nil
}

// Admit decides one request. It is total and fail-closed: every path that does
// not reach a full match returns a denial receipt, and a receipt with
// Admitted true exists only when freshness, signature, key binding, trust
// bound, PCR selection and PCR digest all hold. The challenge is consumed
// before any other check, so a denied or replayed attempt cannot be retried.
func (g *Gate) Admit(req Request) Receipt {
	now := g.now().UTC()
	r := Receipt{Schema: ReceiptSchema, Operation: g.operation, DecidedAt: now, MaxEvidenceAgeSeconds: int64(g.ttl / time.Second), NotClaimed: slices.Clone(notClaimed)}
	deny := func(code string) Receipt { return r.seal(false, code) }

	if !idPattern.MatchString(req.ChallengeID) || !subjectPattern.MatchString(req.SubjectID) || !operationPattern.MatchString(req.Operation) {
		return deny(DenyInvalidRequest)
	}
	r.SubjectID, r.ChallengeID = req.SubjectID, req.ChallengeID
	if req.Operation != g.operation {
		return deny(DenyOperationNotGated)
	}
	if len(req.Evidence.Quote) == 0 || len(req.Evidence.Signature) == 0 {
		// Still burn the challenge: an unsigned attempt must not be a free probe.
		_, _, _ = g.challenges.Take(req.ChallengeID)
		return deny(DenyEvidenceMissing)
	}
	ch, ok, err := g.challenges.Take(req.ChallengeID)
	if err != nil {
		return deny(DenyStoreUnavailable)
	}
	if !ok {
		return deny(DenyChallengeUnknown)
	}
	r.IssuedAt = ch.IssuedAt.UTC()
	if age := now.Sub(ch.IssuedAt); age >= 0 {
		r.EvidenceAgeSeconds = int64(age / time.Second)
	}
	if ch.ID != req.ChallengeID || ch.Operation != req.Operation || ch.SubjectID != req.SubjectID || len(ch.Nonce) != NonceSize {
		return deny(DenyChallengeMismatch)
	}
	if ch.IssuedAt.IsZero() || ch.ExpiresAt.IsZero() || !ch.ExpiresAt.After(ch.IssuedAt) {
		return deny(DenyChallengeExpired)
	}
	if ch.IssuedAt.After(now.Add(ClockSkew)) || !now.Before(ch.ExpiresAt) || now.Sub(ch.IssuedAt) > g.ttl || ch.ExpiresAt.Sub(ch.IssuedAt) > g.ttl {
		return deny(DenyChallengeExpired)
	}

	enrollment, err := g.authority.Enrollment(req.SubjectID)
	if err != nil || enrollment.SubjectID != req.SubjectID {
		return deny(DenyReferenceUnavail)
	}
	r.ProfileID, r.TrustBound = enrollment.Profile.ID, enrollment.TrustBound
	// The trust bound is judged first so an unsupported TPM is named as such
	// even when the rest of its enrollment is incomplete.
	if enrollment.TrustBound != TrustBoundPhysicalTPM2 {
		return deny(DenyTrustBound)
	}
	if enrollment.Key == nil || strings.TrimSpace(enrollment.KeyID) == "" || enrollment.Profile.Validate() != nil {
		return deny(DenyReferenceUnavail)
	}
	r.KeyID = enrollment.KeyID
	currentDigest, err := enrollment.Current.Digest(enrollment.Profile)
	if err != nil {
		return deny(DenyReferenceUnavail)
	}

	claims, err := g.verifier.VerifyQuote(enrollment.Key, req.Evidence.Quote, req.Evidence.Signature)
	switch {
	case errors.Is(err, ErrQuoteSignature):
		return deny(DenySignatureInvalid)
	case errors.Is(err, ErrQuoteKey):
		return deny(DenyKeyUnbound)
	case err != nil:
		return deny(DenyEvidenceMalformed)
	}
	r.FirmwareVersion = claims.FirmwareVersion
	clock := claims.Clock
	r.Clock = &clock

	want := ch.ExtraData()
	if subtle.ConstantTimeCompare(claims.ExtraData, want[:]) != 1 {
		return deny(DenyNonceMismatch)
	}
	if !claims.Clock.Safe {
		return deny(DenyClockUnsafe)
	}
	if len(claims.Selections) != 1 {
		return deny(DenySelectionMismatch)
	}
	if claims.Selections[0].Bank != enrollment.Profile.Bank {
		return deny(DenyBankUnsupported)
	}
	if !slices.Equal(claims.Selections[0].PCRs, enrollment.Profile.PCRs) {
		return deny(DenySelectionMismatch)
	}
	r.PCRBank, r.PCRs, r.PCRDigest = enrollment.Profile.Bank, slices.Clone(enrollment.Profile.PCRs), hex.EncodeToString(claims.PCRDigest)

	if subtle.ConstantTimeCompare(claims.PCRDigest, currentDigest) == 1 {
		r.ReferenceID, r.ReferenceVersion = enrollment.Current.ID, enrollment.Current.Version
		return r.seal(true, CodeAdmitted)
	}
	if enrollment.Transition != nil {
		nextDigest, err := enrollment.Transition.Next.Digest(enrollment.Profile)
		if err != nil {
			return deny(DenyTransitionInvalid)
		}
		if subtle.ConstantTimeCompare(claims.PCRDigest, nextDigest) == 1 {
			if enrollment.Transition.validAt(now) != nil {
				return deny(DenyTransitionInvalid)
			}
			r.ReferenceID, r.ReferenceVersion, r.Transition = enrollment.Transition.Next.ID, enrollment.Transition.Next.Version, true
			r.TransitionApprovalID = enrollment.Transition.ApprovalID
			return r.seal(true, CodeAdmitted)
		}
	}
	return deny(DenyMeasurement)
}

// CodeAdmitted is the Receipt.Code of an admission.
const CodeAdmitted = "admitted"

// notClaimed lists what no receipt ever asserts.
var notClaimed = []string{"malware_free", "runtime_integrity_after_quote", "firmware_supply_chain_origin"}

// Receipt is the structured admission decision of one request. It carries no
// nonce, quote, signature or key material.
type Receipt struct {
	Schema      string     `json:"schema"`
	Admitted    bool       `json:"admitted"`
	Code        string     `json:"code"`
	Operation   string     `json:"operation"`
	SubjectID   string     `json:"subject_id"`
	ChallengeID string     `json:"challenge_id"`
	ProfileID   string     `json:"profile_id,omitempty"`
	TrustBound  TrustBound `json:"trust_bound,omitempty"`
	KeyID       string     `json:"attestation_key_id,omitempty"`
	// ReferenceID and ReferenceVersion name the approved reference set that
	// matched; Transition is true when it was the next set of an approved
	// firmware update.
	ReferenceID          string     `json:"reference_id,omitempty"`
	ReferenceVersion     string     `json:"reference_version,omitempty"`
	Transition           bool       `json:"transition,omitempty"`
	TransitionApprovalID string     `json:"transition_approval_id,omitempty"`
	PCRBank              string     `json:"pcr_bank,omitempty"`
	PCRs                 []int      `json:"pcrs,omitempty"`
	PCRDigest            string     `json:"pcr_digest,omitempty"`
	FirmwareVersion      uint64     `json:"tpm_firmware_version,omitempty"`
	Clock                *ClockInfo `json:"tpm_clock,omitempty"`
	IssuedAt             time.Time  `json:"challenge_issued_at,omitzero"`
	DecidedAt            time.Time  `json:"decided_at"`
	// EvidenceAgeSeconds is the time from challenge issue to decision; the
	// nonce binding proves the quote is no older than that.
	EvidenceAgeSeconds    int64    `json:"evidence_age_seconds"`
	MaxEvidenceAgeSeconds int64    `json:"max_evidence_age_seconds"`
	NotClaimed            []string `json:"not_claimed"`
	// Digest is the SHA-256 of the receipt with this field empty.
	Digest string `json:"digest"`
}

func (r Receipt) seal(admitted bool, code string) Receipt {
	r.Admitted, r.Code = admitted, code
	if !admitted {
		r.Transition, r.ReferenceID, r.ReferenceVersion, r.TransitionApprovalID = false, "", "", ""
	}
	digest, err := r.computeDigest()
	if err != nil {
		// A receipt that cannot be encoded can never admit.
		r.Admitted, r.Code = false, DenyInvalidRequest
		return r
	}
	r.Digest = digest
	return r
}

// computeDigest is the SHA-256 over the receipt encoded with Digest empty.
func (r Receipt) computeDigest() (string, error) {
	r.Digest = ""
	encoded, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// Err is nil only for an admission whose digest recomputes. The digest is an
// unkeyed SHA-256: it detects corruption or alteration of a receipt inside one
// trust boundary. A receipt that crosses a trust boundary needs a MAC or
// signature from its transport; this package holds no signing key.
func (r Receipt) Err() error {
	if r.Admitted && r.Code == CodeAdmitted && r.Schema == ReceiptSchema {
		if want, err := r.computeDigest(); err == nil && subtle.ConstantTimeCompare([]byte(want), []byte(r.Digest)) == 1 {
			return nil
		}
		return &Denied{Code: "receipt_digest_invalid"}
	}
	return &Denied{Code: r.Code}
}

// Denied is the error form of a refused admission.
type Denied struct{ Code string }

func (d *Denied) Error() string { return "guardattest: measured-boot attestation denied: " + d.Code }

// MemoryChallengeStore is a bounded in-process ChallengeStore. A restart
// forgets challenges, which can only deny, never admit.
type MemoryChallengeStore struct {
	mu      sync.Mutex
	pending map[string]Challenge
}

const maxPendingChallenges = 1024

// Put records a challenge, dropping ones already expired at its issue time and
// refusing when the bound is reached.
func (s *MemoryChallengeStore) Put(c Challenge) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		s.pending = make(map[string]Challenge)
	}
	for id, old := range s.pending {
		if !old.ExpiresAt.After(c.IssuedAt) {
			delete(s.pending, id)
		}
	}
	if _, dup := s.pending[c.ID]; dup || len(s.pending) >= maxPendingChallenges {
		return errors.New("challenge store is full or the challenge id repeats")
	}
	c.Nonce = slices.Clone(c.Nonce)
	s.pending[c.ID] = c
	return nil
}

// Take removes and returns the challenge: single use.
func (s *MemoryChallengeStore) Take(id string) (Challenge, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.pending[id]
	delete(s.pending, id)
	return c, ok, nil
}
