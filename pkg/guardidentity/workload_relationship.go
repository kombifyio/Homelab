package guardidentity

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Guard D short-lived workload identity relationship primitives.
// Guard does not become a certificate/identity issuer for workloads: it signs
// key-bound assertions for an existing identity authority's workload
// relationship, verifies bounded workload assertions with replay rejection, and
// validates/transports bounded qualification evidence.
const (
	// WorkloadAssertionType is the JOSE typ of a Guard D workload assertion.
	WorkloadAssertionType = "kombify-guard-workload+jwt"

	// WorkloadAssertionMinTTL bounds the shortest accepted assertion lifetime.
	WorkloadAssertionMinTTL = 30 * time.Second
	// WorkloadAssertionDefaultTTL is the default bounded assertion lifetime.
	WorkloadAssertionDefaultTTL = 5 * time.Minute
	// WorkloadAssertionMaxTTL is the longest accepted assertion lifetime.
	WorkloadAssertionMaxTTL = 15 * time.Minute
	// WorkloadSessionMaxLifetime bounds any single established workload session.
	WorkloadSessionMaxLifetime = 60 * time.Second
	// WorkloadClockSkew is the tolerated clock offset for assertion timestamps.
	WorkloadClockSkew = 30 * time.Second

	// WorkloadEvidenceSchema is the canonical Guard D qualification report schema.
	WorkloadEvidenceSchema = "kombify.guard-workload-identity/v1"
	// WorkloadEvidenceMaxBytes bounds one qualification evidence document.
	WorkloadEvidenceMaxBytes = 32 << 10
	// WorkloadEvidenceMinFreshness bounds the shortest evidence freshness window.
	WorkloadEvidenceMinFreshness = time.Minute
	// WorkloadEvidenceMaxFreshness bounds the longest evidence freshness window.
	WorkloadEvidenceMaxFreshness = 24 * time.Hour
	// WorkloadEvidenceMaxAge bounds how old an observation may be before Judge
	// marks it stale regardless of a longer ExpiresAt window.
	WorkloadEvidenceMaxAge = 15 * time.Minute
	// WorkloadEvidenceMaxClockSkew bounds how far ahead of now an observation may be.
	WorkloadEvidenceMaxClockSkew = 5 * time.Minute

	maxWorkloadAssertionBytes = 4096
	maxWorkloadReplayEntries  = 4096
	maxWorkloadUnknowns       = 16
)

// Denial reasons refining CodeProofInvalid / CodeProofReplayed / CodeTokenExpired / CodeIdentityRevoked.
const (
	ReasonWorkloadAssertionMalformed   = "workload_assertion_malformed"
	ReasonWorkloadAssertionHeader      = "workload_assertion_header"
	ReasonWorkloadAssertionSignature   = "workload_assertion_signature"
	ReasonWorkloadAssertionClaims      = "workload_assertion_claims"
	ReasonWorkloadAssertionIdentity    = "workload_assertion_identity"
	ReasonWorkloadAssertionAudience    = "workload_assertion_audience"
	ReasonWorkloadAssertionScope       = "workload_assertion_scope"
	ReasonWorkloadAssertionLifetime    = "workload_assertion_lifetime"
	ReasonWorkloadAssertionExpired     = "workload_assertion_expired"
	ReasonWorkloadAssertionFuture      = "workload_assertion_not_yet_valid"
	ReasonWorkloadAssertionNotEligible = "workload_assertion_not_eligible"
	ReasonWorkloadAssertionReplayed    = "workload_assertion_replayed"
	ReasonWorkloadAssertionRevoked     = "workload_assertion_revoked"
	ReasonWorkloadAssertionKeyBinding  = "workload_assertion_key_binding"
	ReasonWorkloadSessionExpired       = "workload_session_expired"
)

var (
	workloadIDPattern         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
	workloadScopePattern      = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,128}$`)
	workloadVersionPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
	workloadGuardVerPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	workloadServerNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,254}$`)
	workloadTokenPattern      = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	workloadDigestPattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type workloadAssertionHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	JWK JWK    `json:"jwk"`
}

type workloadAssertionClaims struct {
	Iss        string `json:"iss"`
	Sub        string `json:"sub"`
	WorkloadID string `json:"wid"`
	TenantID   string `json:"tid"`
	ServiceRef string `json:"svc"`
	Aud        string `json:"aud"`
	Scope      string `json:"scp"`
	JKT        string `json:"jkt"`
	Gen        int64  `json:"gen"`
	MaxSession int64  `json:"max_sess"`
	Iat        int64  `json:"iat"`
	Nbf        int64  `json:"nbf"`
	Exp        int64  `json:"exp"`
	JTI        string `json:"jti"`
}

// WorkloadAssertionInput describes one short-lived workload identity assertion.
type WorkloadAssertionInput struct {
	Issuer            string
	Subject           string
	WorkloadID        string
	TenantID          string
	ServiceRef        string
	Audience          string
	Scope             string
	Generation        int64
	MaxSessionSeconds int64
	Lifetime          time.Duration
	Now               time.Time
	JTI               string
}

// SignWorkloadAssertion mints a key-bound compact JWS for an eligible workload
// relationship. Private key material never enters the token.
func SignWorkloadAssertion(key ed25519.PrivateKey, in WorkloadAssertionInput) (string, error) {
	if len(key) != ed25519.PrivateKeySize {
		return "", errors.New("guard workload identity key is not available")
	}
	if !hostProofIDPattern.MatchString(in.Issuer) || !hostProofIDPattern.MatchString(in.Subject) ||
		!workloadIDPattern.MatchString(in.WorkloadID) || !hostProofIDPattern.MatchString(in.ServiceRef) {
		return "", errors.New("workload assertion requires canonical issuer, subject, workload and service")
	}
	if strings.TrimSpace(in.TenantID) == "" || len(in.TenantID) > 255 {
		return "", errors.New("workload assertion requires a tenant id")
	}
	if strings.TrimSpace(in.Audience) == "" || len(in.Audience) > 512 || !workloadScopePattern.MatchString(in.Scope) {
		return "", errors.New("workload assertion requires exact audience and scope")
	}
	if in.Generation < 1 {
		return "", errors.New("workload assertion requires generation >= 1")
	}
	maxSess := in.MaxSessionSeconds
	if maxSess <= 0 {
		maxSess = int64(WorkloadSessionMaxLifetime / time.Second)
	}
	if maxSess > int64(WorkloadSessionMaxLifetime/time.Second) {
		maxSess = int64(WorkloadSessionMaxLifetime / time.Second)
	}
	lifetime := in.Lifetime
	if lifetime <= 0 {
		lifetime = WorkloadAssertionDefaultTTL
	}
	if lifetime < WorkloadAssertionMinTTL {
		return "", errors.New("workload assertion lifetime is below minimum bound")
	}
	if lifetime > WorkloadAssertionMaxTTL {
		lifetime = WorkloadAssertionMaxTTL
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	jti := in.JTI
	if jti == "" {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", err
		}
		jti = base64.RawURLEncoding.EncodeToString(raw[:])
	} else if !hostProofJTIPattern.MatchString(jti) {
		return "", errors.New("workload assertion jti is malformed")
	}
	jwk := PublicJWK(key)
	jkt, err := jwk.Thumbprint()
	if err != nil {
		return "", err
	}
	header, err := json.Marshal(workloadAssertionHeader{Alg: ProofAlgorithm, Typ: WorkloadAssertionType, JWK: jwk})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(workloadAssertionClaims{
		Iss: in.Issuer, Sub: in.Subject, WorkloadID: in.WorkloadID, TenantID: in.TenantID,
		ServiceRef: in.ServiceRef, Aud: in.Audience, Scope: in.Scope, JKT: jkt,
		Gen: in.Generation, MaxSession: maxSess,
		Iat: now.Unix(), Nbf: now.Unix(), Exp: now.Add(lifetime).Unix(), JTI: jti,
	})
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	sig := ed25519.Sign(key, []byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// WorkloadAssertionExpectation defines the fail-closed verification rules for
// one workload identity assertion. ExpectedKeyThumbprint is mandatory.
type WorkloadAssertionExpectation struct {
	Issuer                string
	Subject               string
	WorkloadID            string
	TenantID              string
	ServiceRef            string
	Audience              string
	Scope                 string
	ExpectedKeyThumbprint string
	MinGeneration         int64
	MaxSessionSeconds     int64
	Eligible              bool
	RevokedThumbprints    []string
	RetiredJTIs           []string
	ClockSkew             time.Duration
	Now                   time.Time
}

// VerifiedWorkloadAssertion is the authenticated workload identity outcome.
type VerifiedWorkloadAssertion struct {
	Issuer            string
	Subject           string
	WorkloadID        string
	TenantID          string
	ServiceRef        string
	Audience          string
	Scope             string
	Thumbprint        string
	Generation        int64
	MaxSession        time.Duration
	JTI               string
	IssuedAt          time.Time
	NotBefore         time.Time
	ExpiresAt         time.Time
	ReplayRetainUntil time.Time
}

// WorkloadReplayCache rejects replayed workload assertions within their
// validity + skew window while keeping memory bounded.
type WorkloadReplayCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// NewWorkloadReplayCache constructs an empty replay guard.
func NewWorkloadReplayCache() *WorkloadReplayCache {
	return &WorkloadReplayCache{seen: make(map[string]time.Time)}
}

func (c *WorkloadReplayCache) admit(issuer, jti string, retainUntil, now time.Time) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, until := range c.seen {
		if !now.Before(until) {
			delete(c.seen, k)
		}
	}
	key := issuer + "\x00" + jti
	if until, exists := c.seen[key]; exists && now.Before(until) {
		return false
	}
	if len(c.seen) >= maxWorkloadReplayEntries {
		return false
	}
	c.seen[key] = retainUntil
	return true
}

// VerifyWorkloadAssertion validates a short-lived workload assertion fail-closed.
// A non-nil replay cache and a non-empty ExpectedKeyThumbprint are mandatory.
func VerifyWorkloadAssertion(raw string, expect WorkloadAssertionExpectation, replay *WorkloadReplayCache) (*VerifiedWorkloadAssertion, error) {
	deny := func(code, reason string) (*VerifiedWorkloadAssertion, error) {
		return nil, Denied(code, reason)
	}
	if replay == nil {
		return deny(CodeProofInvalid, ReasonWorkloadAssertionReplayed)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxWorkloadAssertionBytes {
		return deny(CodeProofInvalid, ReasonWorkloadAssertionMalformed)
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return deny(CodeProofInvalid, ReasonWorkloadAssertionMalformed)
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return deny(CodeProofInvalid, ReasonWorkloadAssertionMalformed)
	}
	var header workloadAssertionHeader
	headerDec := json.NewDecoder(bytesReader(headerBytes))
	headerDec.DisallowUnknownFields()
	if headerDec.Decode(&header) != nil || headerDec.Decode(&struct{}{}) != io.EOF ||
		header.Alg != ProofAlgorithm || header.Typ != WorkloadAssertionType {
		return deny(CodeProofInvalid, ReasonWorkloadAssertionHeader)
	}
	var rawHeader map[string]json.RawMessage
	if json.Unmarshal(headerBytes, &rawHeader) != nil || len(rawHeader) != 3 {
		return deny(CodeProofInvalid, ReasonWorkloadAssertionHeader)
	}
	pub, err := header.JWK.PublicKey()
	if err != nil {
		return deny(CodeProofInvalid, ReasonWorkloadAssertionHeader)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return deny(CodeProofInvalid, ReasonWorkloadAssertionSignature)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return deny(CodeProofInvalid, ReasonWorkloadAssertionMalformed)
	}
	var claims workloadAssertionClaims
	claimDec := json.NewDecoder(bytesReader(payload))
	claimDec.DisallowUnknownFields()
	if claimDec.Decode(&claims) != nil || claimDec.Decode(&struct{}{}) != io.EOF {
		return deny(CodeProofInvalid, ReasonWorkloadAssertionClaims)
	}
	if !hostProofIDPattern.MatchString(claims.Iss) || !hostProofIDPattern.MatchString(claims.Sub) ||
		!workloadIDPattern.MatchString(claims.WorkloadID) || !hostProofIDPattern.MatchString(claims.ServiceRef) ||
		claims.TenantID == "" || len(claims.TenantID) > 255 || !hostProofJTIPattern.MatchString(claims.JTI) ||
		claims.Gen < 1 || claims.MaxSession <= 0 {
		return deny(CodeProofInvalid, ReasonWorkloadAssertionClaims)
	}
	thumbprint, err := header.JWK.Thumbprint()
	if err != nil {
		return deny(CodeProofInvalid, ReasonWorkloadAssertionHeader)
	}
	if strings.TrimSpace(expect.ExpectedKeyThumbprint) == "" || claims.JKT != thumbprint || claims.JKT != expect.ExpectedKeyThumbprint {
		return deny(CodeProofInvalid, ReasonWorkloadAssertionKeyBinding)
	}
	for _, revoked := range expect.RevokedThumbprints {
		if revoked == thumbprint {
			return deny(CodeIdentityRevoked, ReasonWorkloadAssertionRevoked)
		}
	}
	for _, retired := range expect.RetiredJTIs {
		if retired == claims.JTI {
			return deny(CodeIdentityRevoked, ReasonWorkloadAssertionRevoked)
		}
	}
	if !expect.Eligible {
		return deny(CodeIdentityRevoked, ReasonWorkloadAssertionNotEligible)
	}
	if claims.Aud != expect.Audience {
		return deny(CodeProofInvalid, ReasonWorkloadAssertionAudience)
	}
	if claims.Scope != expect.Scope {
		return deny(CodeProofInvalid, ReasonWorkloadAssertionScope)
	}
	if claims.Iss != expect.Issuer || claims.Sub != expect.Subject || claims.WorkloadID != expect.WorkloadID ||
		claims.TenantID != expect.TenantID || claims.ServiceRef != expect.ServiceRef ||
		(expect.MinGeneration > 0 && claims.Gen < expect.MinGeneration) {
		return deny(CodeProofInvalid, ReasonWorkloadAssertionIdentity)
	}
	maxSess := expect.MaxSessionSeconds
	if maxSess <= 0 || maxSess > int64(WorkloadSessionMaxLifetime/time.Second) {
		maxSess = int64(WorkloadSessionMaxLifetime / time.Second)
	}
	if claims.MaxSession > maxSess {
		return deny(CodeProofInvalid, ReasonWorkloadAssertionLifetime)
	}
	const (
		minTTLSeconds = int64(WorkloadAssertionMinTTL / time.Second)
		maxTTLSeconds = int64(WorkloadAssertionMaxTTL / time.Second)
	)
	if claims.Iat <= 0 || claims.Nbf < claims.Iat || claims.Exp <= claims.Iat {
		return deny(CodeProofInvalid, ReasonWorkloadAssertionLifetime)
	}
	lifetimeSeconds := claims.Exp - claims.Iat
	if lifetimeSeconds < minTTLSeconds || lifetimeSeconds > maxTTLSeconds {
		return deny(CodeProofInvalid, ReasonWorkloadAssertionLifetime)
	}
	skew := expect.ClockSkew
	if skew < 0 {
		return deny(CodeProofInvalid, ReasonWorkloadAssertionClaims)
	}
	if skew == 0 {
		skew = WorkloadClockSkew
	}
	now := expect.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	issuedAt := time.Unix(claims.Iat, 0).UTC()
	notBefore := time.Unix(claims.Nbf, 0).UTC()
	if notBefore.After(now.Add(skew)) || issuedAt.After(now.Add(skew)) {
		return deny(CodeProofInvalid, ReasonWorkloadAssertionFuture)
	}
	expiresAt := time.Unix(claims.Exp, 0).UTC()
	if !now.Before(expiresAt.Add(skew)) {
		return deny(CodeTokenExpired, ReasonWorkloadAssertionExpired)
	}
	retainUntil := expiresAt.Add(skew)
	if !replay.admit(claims.Iss, claims.JTI, retainUntil, now) {
		return deny(CodeProofReplayed, ReasonWorkloadAssertionReplayed)
	}
	return &VerifiedWorkloadAssertion{
		Issuer: claims.Iss, Subject: claims.Sub, WorkloadID: claims.WorkloadID,
		TenantID: claims.TenantID, ServiceRef: claims.ServiceRef, Audience: claims.Aud, Scope: claims.Scope,
		Thumbprint: thumbprint, Generation: claims.Gen,
		MaxSession: time.Duration(claims.MaxSession) * time.Second,
		JTI:        claims.JTI, IssuedAt: issuedAt, NotBefore: notBefore, ExpiresAt: expiresAt,
		ReplayRetainUntil: retainUntil,
	}, nil
}

// BoundWorkloadSessionDeadline computes the strict deadline of an established
// workload session as min(sessionStartedAt + MaxSession, ExpiresAt).
func BoundWorkloadSessionDeadline(sessionStartedAt time.Time, verified *VerifiedWorkloadAssertion) time.Time {
	if verified == nil || sessionStartedAt.IsZero() {
		return time.Time{}
	}
	deadline := sessionStartedAt.UTC().Add(verified.MaxSession)
	if verified.ExpiresAt.Before(deadline) {
		deadline = verified.ExpiresAt
	}
	return deadline
}

// CheckWorkloadSession enforces the bounded established-session lifetime.
func CheckWorkloadSession(sessionStartedAt, now time.Time, verified *VerifiedWorkloadAssertion) error {
	deadline := BoundWorkloadSessionDeadline(sessionStartedAt, verified)
	if deadline.IsZero() || now.IsZero() || !now.UTC().Before(deadline) {
		return Denied(CodeTokenExpired, ReasonWorkloadSessionExpired)
	}
	return nil
}

// Qualification verdicts of Guard D evidence.
const (
	WorkloadVerdictQualified   = "qualified"
	WorkloadVerdictUnqualified = "unqualified"
	WorkloadVerdictDegraded    = "degraded"
	WorkloadVerdictUnknown     = "unknown"
)

// WorkloadEvidenceProfile identifies the supported Linux/systemd/transport slice.
type WorkloadEvidenceProfile struct {
	OS        string `json:"os"`
	Init      string `json:"init"`
	Transport string `json:"transport"`
}

// WorkloadQualificationEvidence matches the canonical
// kombify.guard-workload-identity/v1 wire contract emitted by the identity
// authority and transported by Guard without becoming an issuer.
type WorkloadQualificationEvidence struct {
	SchemaVersion               string                  `json:"schema_version"`
	AuthorityVersion            string                  `json:"authority_version"`
	GuardVersion                string                  `json:"guard_version"`
	Profile                     WorkloadEvidenceProfile `json:"profile"`
	PeerRef                     string                  `json:"peer_ref"`
	WorkloadID                  string                  `json:"workload_id"`
	ServiceRef                  string                  `json:"service_ref"`
	EdgeSiteRef                 string                  `json:"edge_site_ref"`
	Audience                    string                  `json:"audience"`
	Scope                       string                  `json:"scope"`
	ServerName                  string                  `json:"server_name"`
	Eligible                    bool                    `json:"eligible"`
	EligibilityRevision         uint64                  `json:"eligibility_revision"`
	EligibilityReason           string                  `json:"eligibility_reason,omitempty"`
	TTLSeconds                  int                     `json:"ttl_seconds"`
	RenewBeforeSeconds          int                     `json:"renew_before_seconds"`
	MaxSessionSeconds           int                     `json:"max_session_seconds"`
	ActiveCertificateDigest     string                  `json:"active_certificate_digest,omitempty"`
	NotBefore                   *time.Time              `json:"not_before,omitempty"`
	NotAfter                    *time.Time              `json:"not_after,omitempty"`
	RenewalState                string                  `json:"renewal_state"`
	RenewalCount                uint64                  `json:"renewal_count"`
	LastRenewedAt               *time.Time              `json:"last_renewed_at,omitempty"`
	LastRenewalErrorCode        string                  `json:"last_renewal_error_code,omitempty"`
	RetiredCertificateCount     int                     `json:"retired_certificate_count"`
	RevokedKeyCount             int                     `json:"revoked_key_count"`
	IssuerStatus                string                  `json:"issuer_status"`
	ClockStatus                 string                  `json:"clock_status"`
	RecoveryState               string                  `json:"recovery_state"`
	PostExpiryDenied            *bool                   `json:"post_expiry_denied,omitempty"`
	MeasuredSessionBoundSeconds int64                   `json:"measured_session_bound_seconds,omitempty"`
	ObservedAt                  time.Time               `json:"observed_at"`
	ExpiresAt                   time.Time               `json:"expires_at"`
	EvidenceAgeSeconds          int64                   `json:"evidence_age_seconds"`
	Unknowns                    []string                `json:"unknowns,omitempty"`
}

// ParseWorkloadQualificationEvidence strictly decodes and validates untrusted
// Guard D qualification evidence before storage or transport.
func ParseWorkloadQualificationEvidence(raw []byte, now time.Time) (*WorkloadQualificationEvidence, error) {
	if len(raw) == 0 || len(raw) > WorkloadEvidenceMaxBytes {
		return nil, errors.New("guardidentity: workload evidence is empty or exceeds size bound")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var ev WorkloadQualificationEvidence
	dec := json.NewDecoder(bytesReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ev); err != nil {
		return nil, errors.New("guardidentity: malformed workload qualification evidence")
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("guardidentity: trailing data in workload qualification evidence")
	}
	if ev.SchemaVersion != WorkloadEvidenceSchema {
		return nil, errors.New("guardidentity: unexpected workload evidence schema version")
	}
	if !workloadVersionPattern.MatchString(ev.AuthorityVersion) || !workloadGuardVerPattern.MatchString(ev.GuardVersion) {
		return nil, errors.New("guardidentity: workload evidence requires canonical authority and Guard versions")
	}
	if ev.Profile.OS != "linux" || ev.Profile.Init != "systemd" ||
		(ev.Profile.Transport != "mtls-loopback" && ev.Profile.Transport != "https-poll") {
		return nil, errors.New("guardidentity: unsupported workload evidence profile")
	}
	if !hostProofIDPattern.MatchString(ev.PeerRef) || !workloadIDPattern.MatchString(ev.WorkloadID) ||
		!hostProofIDPattern.MatchString(ev.ServiceRef) || !hostProofIDPattern.MatchString(ev.EdgeSiteRef) ||
		!workloadServerNamePattern.MatchString(ev.ServerName) ||
		strings.TrimSpace(ev.Audience) == "" || len(ev.Audience) > 512 || !workloadScopePattern.MatchString(ev.Scope) {
		return nil, errors.New("guardidentity: workload evidence lacks exact identity or scope")
	}
	if containsPEMOrControl(ev.Audience) || containsPEMOrControl(ev.EligibilityReason) || containsPEMOrControl(ev.LastRenewalErrorCode) {
		return nil, errors.New("guardidentity: workload evidence contains disallowed material")
	}
	if ev.TTLSeconds < 30 || ev.TTLSeconds > 86400 ||
		ev.RenewBeforeSeconds <= 0 || ev.RenewBeforeSeconds >= ev.TTLSeconds ||
		ev.MaxSessionSeconds <= 0 || ev.MaxSessionSeconds > int(WorkloadSessionMaxLifetime/time.Second) {
		return nil, errors.New("guardidentity: workload evidence TTL or session bound out of range")
	}
	if ev.ActiveCertificateDigest != "" && !workloadDigestPattern.MatchString(ev.ActiveCertificateDigest) {
		return nil, errors.New("guardidentity: workload evidence certificate digest is malformed")
	}
	if !validWorkloadStatus(ev.IssuerStatus, "available", "outage", "unknown") ||
		!validWorkloadStatus(ev.ClockStatus, "synchronized", "skewed", "unknown") ||
		!validWorkloadStatus(ev.RecoveryState, "none", "recovery_required", "recovered") ||
		!validWorkloadStatus(ev.RenewalState, "healthy", "renewal_stopped", "issuer_outage", "clock_outage", "revoked", "expired", "unknown") {
		return nil, errors.New("guardidentity: workload evidence contains unknown status token")
	}
	if ev.LastRenewalErrorCode != "" && !workloadTokenPattern.MatchString(ev.LastRenewalErrorCode) {
		return nil, errors.New("guardidentity: workload evidence renewal error code is malformed")
	}
	if ev.EligibilityReason != "" && len(ev.EligibilityReason) > 128 {
		return nil, errors.New("guardidentity: workload evidence eligibility reason exceeds bound")
	}
	if len(ev.Unknowns) > maxWorkloadUnknowns {
		return nil, errors.New("guardidentity: workload evidence unknowns exceed bound")
	}
	for _, unknown := range ev.Unknowns {
		if !workloadTokenPattern.MatchString(unknown) {
			return nil, errors.New("guardidentity: workload evidence unknown token is malformed")
		}
	}
	if ev.ObservedAt.IsZero() || ev.ExpiresAt.IsZero() || ev.ObservedAt.After(now.UTC().Add(WorkloadEvidenceMaxClockSkew)) {
		return nil, errors.New("guardidentity: workload evidence observation time is missing or in the future")
	}
	window := ev.ExpiresAt.Sub(ev.ObservedAt)
	if window < WorkloadEvidenceMinFreshness || window > WorkloadEvidenceMaxFreshness {
		return nil, errors.New("guardidentity: workload evidence freshness window is out of bounds")
	}
	return &ev, nil
}

func validWorkloadStatus(value string, allowed ...string) bool {
	for _, item := range allowed {
		if value == item {
			return true
		}
	}
	return false
}

func containsPEMOrControl(value string) bool {
	return strings.Contains(value, "-----BEGIN") || strings.Contains(value, "PRIVATE KEY") || strings.ContainsAny(value, "\r\n\x00")
}

// EvidenceAge returns the non-negative age of the observation at now.
func (e *WorkloadQualificationEvidence) EvidenceAge(now time.Time) time.Duration {
	if e == nil || now.IsZero() || now.UTC().Before(e.ObservedAt) {
		return 0
	}
	return now.UTC().Sub(e.ObservedAt)
}

// Judge evaluates Guard D qualification evidence at now, failing closed to
// unknown/degraded/unqualified when stale, skewed, revoked, expired, degraded or
// incomplete.
func (e *WorkloadQualificationEvidence) Judge(now time.Time) (string, string) {
	if e == nil || now.IsZero() {
		return WorkloadVerdictUnknown, "not_observed"
	}
	now = now.UTC()
	if !now.Before(e.ExpiresAt) || now.Sub(e.ObservedAt) > WorkloadEvidenceMaxAge {
		return WorkloadVerdictUnknown, "evidence_stale"
	}
	switch e.RenewalState {
	case "revoked":
		return WorkloadVerdictUnqualified, "revoked"
	case "expired":
		return WorkloadVerdictUnqualified, "expired"
	case "renewal_stopped":
		return WorkloadVerdictUnqualified, "eligibility_removed"
	case "clock_outage":
		return WorkloadVerdictUnknown, "clock_outage"
	case "issuer_outage":
		return WorkloadVerdictDegraded, "issuer_outage"
	case "unknown":
		return WorkloadVerdictUnknown, "not_observed"
	case "healthy":
	default:
		return WorkloadVerdictUnknown, "renewal_state_unknown"
	}
	if e.ClockStatus != "synchronized" {
		return WorkloadVerdictUnknown, "clock_outage"
	}
	if e.IssuerStatus == "outage" {
		return WorkloadVerdictDegraded, "issuer_outage"
	}
	if e.IssuerStatus != "available" {
		return WorkloadVerdictUnknown, "issuer_status_unknown"
	}
	if !e.Eligible {
		return WorkloadVerdictUnqualified, "eligibility_removed"
	}
	if e.PostExpiryDenied == nil || e.MeasuredSessionBoundSeconds <= 0 || len(e.Unknowns) > 0 {
		return WorkloadVerdictUnknown, "incomplete_evidence"
	}
	if !*e.PostExpiryDenied {
		return WorkloadVerdictUnqualified, "post_expiry_not_denied"
	}
	if e.MeasuredSessionBoundSeconds > int64(e.MaxSessionSeconds) {
		return WorkloadVerdictUnqualified, "session_bound_exceeded"
	}
	return WorkloadVerdictQualified, ""
}
