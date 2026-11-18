package edgeauth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Signed feature-flag / token-budget delivery headers.
//
// ENTITLEMENTS-ARCHITECTURE §8.1 removes the feature_flags claim array from the
// session JWT. Instead the Cloudflare Edge Router evaluates Flagship per request
// (sub-ms) and forwards the result to the origin as signed per-request headers.
// This file is the origin-side reference verifier and the test/edge-side signer.
//
// The flag headers carry their OWN signature (HeaderFlagsSignature), separate
// from the identity-header signature in edgeauth.go, so the two evolve and
// verify independently. Legacy verifiers reuse EDGE_AUTH_SECRET[_NEXT]; the
// explicit-key APIs keep decision authority separate from process environment.
const (
	// HeaderFlags is a JSON object {"<dotted.key>": bool, ...}.
	HeaderFlags = "X-Kombify-Flags"
	// HeaderBudgets is a JSON object {"<dotted.key>": {<budget object>}, ...}.
	HeaderBudgets = "X-Kombify-Budgets"
	// HeaderFlagsSignature is "v1=<base64url-hmac>" over the flags payload.
	HeaderFlagsSignature = "X-Kombify-Flags-Signature"
	// HeaderFlagsTimestamp is the unix-seconds timestamp bound into the signature.
	HeaderFlagsTimestamp = "X-Kombify-Flags-Timestamp"
	// HeaderFlagsKeyID identifies which EDGE_AUTH secret signed the payload.
	HeaderFlagsKeyID = "X-Kombify-Flags-Key-ID"

	decisionSignatureVersionV2 = "v2"

	// FlagEvaluationStatePrefix and FlagUnavailablePrefix name the reserved
	// keys of the signed flags map that carry the evaluation state
	// (kombify-Gateway docs/API_REFERENCE.md "Signed flag envelope"): exactly
	// one "$flag_evaluation:<state>" and one "$flag_unavailable:<key>" per key
	// the Gateway could not evaluate, each true. Verification moves every "$"
	// key out of Flags.
	FlagEvaluationStatePrefix = "$flag_evaluation:"
	FlagUnavailablePrefix     = "$flag_unavailable:"
)

// FlagEvaluationState says how complete the Gateway's flag evaluation behind
// a verified FlagSet was.
type FlagEvaluationState string

const (
	// FlagEvaluationComplete: every evaluated flag has a provider answer, so a
	// false value is a denial.
	FlagEvaluationComplete FlagEvaluationState = "complete"
	// FlagEvaluationFailed: the evaluation is incomplete; a false value and
	// every key in UnavailableFlags is not an answer.
	FlagEvaluationFailed FlagEvaluationState = "failed"
	// FlagEvaluationNotEvaluated: the Gateway evaluated no flag for this hop.
	FlagEvaluationNotEvaluated FlagEvaluationState = "not_evaluated"
	// FlagEvaluationUnstated: no readable state, as from a producer that
	// predates it, an application-built set or an expired one.
	FlagEvaluationUnstated FlagEvaluationState = ""
)

// FlagDecision is one flag as an origin may act on it. Only FlagDenied means
// "turned off"; FlagFailed means the Gateway could not evaluate it.
type FlagDecision int

const (
	// FlagUnanswered: no current signed answer (absent key, nothing
	// evaluated, stateless, expired or application-built set).
	FlagUnanswered FlagDecision = iota
	// FlagGranted: the signed value is true.
	FlagGranted
	// FlagDenied: a complete evaluation answered false.
	FlagDenied
	// FlagFailed: the evaluation of this flag, or the whole evaluation, failed.
	FlagFailed
)

func (d FlagDecision) String() string {
	switch d {
	case FlagGranted:
		return "granted"
	case FlagDenied:
		return "denied"
	case FlagFailed:
		return "failed"
	default:
		return "unanswered"
	}
}

// FlagSet is a snapshot of per-request flags and budgets from the edge.
// Verified sets retain their original signature window across copies. Read
// request authority through Bool, FlagsFromContext, or VerifiedBudget; the
// exported maps alone are snapshots, not proof of current authority.
type FlagSet struct {
	Flags   map[string]bool
	Budgets map[string]json.RawMessage

	// decisionBinding is deliberately unexported. Callers may still construct a
	// FlagSet for non-authoritative release flags, but only this package can mark
	// a commercial decision as verified against the request-bound v2 envelope.
	decisionBinding *DecisionBinding
	decisionDigest  [sha256.Size]byte
	validFrom       time.Time
	validUntil      time.Time
	// decision is what verification saw, never changed afterwards and shared
	// by copies. Decision reads only this, so an application-built set or a
	// later change of Flags cannot grant.
	decision *flagDecisionSnapshot
}

// flagDecisionSnapshot is immutable after verification.
type flagDecisionSnapshot struct {
	flags       map[string]bool
	state       FlagEvaluationState
	unavailable []string
}

// DecisionBinding is the immutable provenance of a request-bound v2 flag and
// budget decision. Origins must compare SubjectID and TenantID with their
// authenticated resource operation before treating a budget as authority.
type DecisionBinding struct {
	Version      string
	KeyID        string
	SubjectID    string
	TenantID     string
	Audience     string
	PublicPrefix string
	Method       string
	SignedPath   string
	RequestID    string
	EdgeKeyID    string
	Nonce        string
	IssuedAt     time.Time
}

// VerifiedDecisionBinding returns request-bound commercial-decision
// provenance only when FlagSet came from VerifyDecisionHeaders. A FlagSet
// assembled by application code or restored from a job payload has no such
// provenance and must fail closed for cost-bearing authorization.
func (f FlagSet) VerifiedDecisionBinding() (DecisionBinding, bool) {
	if !f.validAt(time.Now()) || f.decisionBinding == nil || f.decisionDigest == ([sha256.Size]byte{}) || f.decisionDigest != flagSetDigest(f) {
		return DecisionBinding{}, false
	}
	return *f.decisionBinding, true
}

// VerifiedBudget returns a detached copy of one budget together with its
// request binding. It fails closed if application code changed any flag or
// budget after verification.
func (f FlagSet) VerifiedBudget(key string) (json.RawMessage, DecisionBinding, bool) {
	binding, ok := f.VerifiedDecisionBinding()
	if !ok {
		return nil, DecisionBinding{}, false
	}
	raw, ok := f.Budgets[key]
	if !ok {
		return nil, DecisionBinding{}, false
	}
	return append(json.RawMessage(nil), raw...), binding, true
}

// Bool returns the flag value, or def if absent. A verified set outside its
// signature window returns false even when def is true. Application-created
// release flags have no signature window; they confer no commercial provenance.
func (f FlagSet) Bool(key string, def bool) bool {
	if !f.validAt(time.Now()) {
		return false
	}
	if f.Flags == nil {
		return def
	}
	if v, ok := f.Flags[key]; ok {
		return v
	}
	return def
}

// EvaluationState returns the signed evaluation state of a verified set
// within its signature window, otherwise FlagEvaluationUnstated.
func (f FlagSet) EvaluationState() FlagEvaluationState {
	if f.decision == nil || !f.validAt(time.Now()) {
		return FlagEvaluationUnstated
	}
	return f.decision.state
}

// UnavailableFlags returns a copy of the keys the Gateway could not evaluate.
func (f FlagSet) UnavailableFlags() []string {
	if f.decision == nil || !f.validAt(time.Now()) {
		return nil
	}
	return append([]string(nil), f.decision.unavailable...)
}

// Decision tells a denial from a failed evaluation for one flag. It answers
// from the snapshot verification took, so only a set this package verified
// grants or denies: an application-built set, and a key added to or changed
// in Flags afterwards, answer what was signed or FlagUnanswered. It keeps the
// signature window: an expired set answers nothing.
func (f FlagSet) Decision(key string) FlagDecision {
	snapshot := f.decision
	if snapshot == nil || !f.validAt(time.Now()) {
		return FlagUnanswered
	}
	value, ok := snapshot.flags[key]
	if !ok {
		return FlagUnanswered
	}
	for _, unavailable := range snapshot.unavailable {
		if unavailable == key {
			return FlagFailed
		}
	}
	switch {
	case value:
		return FlagGranted
	case snapshot.state == FlagEvaluationComplete:
		return FlagDenied
	case snapshot.state == FlagEvaluationFailed:
		return FlagFailed
	default:
		return FlagUnanswered
	}
}

// WithFlagEvaluation returns flags for a signer with the evaluation state in
// the reserved keys. Caller keys starting with "$" are dropped; unavailable
// keys are de-duplicated.
func WithFlagEvaluation(flags map[string]bool, state FlagEvaluationState, unavailable ...string) map[string]bool {
	out := make(map[string]bool, len(flags)+1+len(unavailable))
	for key, value := range flags {
		if !strings.HasPrefix(key, "$") {
			out[key] = value
		}
	}
	out[FlagEvaluationStatePrefix+string(state)] = true
	for _, key := range sortedUnique(unavailable) {
		out[FlagUnavailablePrefix+key] = true
	}
	return out
}

func sortedUnique(keys []string) []string {
	seen := make(map[string]struct{}, len(keys))
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// bindFlagEvaluation moves every reserved "$" key out of Flags and takes the
// decision snapshot. A state is readable only as exactly one known
// "$flag_evaluation:<state>" set to true; a missing, unknown or contradictory
// reserved entry is unstated, never an error: it cannot deny anything.
func (f *FlagSet) bindFlagEvaluation() {
	var states, unavailable []string
	malformed := false
	for key, value := range f.Flags {
		if !strings.HasPrefix(key, "$") {
			continue
		}
		delete(f.Flags, key)
		switch {
		case strings.HasPrefix(key, FlagEvaluationStatePrefix) && value:
			states = append(states, strings.TrimPrefix(key, FlagEvaluationStatePrefix))
		case strings.HasPrefix(key, FlagUnavailablePrefix) && value && len(key) > len(FlagUnavailablePrefix):
			unavailable = append(unavailable, strings.TrimPrefix(key, FlagUnavailablePrefix))
		default:
			malformed = true
		}
	}
	snapshot := &flagDecisionSnapshot{flags: make(map[string]bool, len(f.Flags)), state: FlagEvaluationUnstated}
	for key, value := range f.Flags {
		snapshot.flags[key] = value
	}
	if !malformed && len(states) == 1 {
		switch state := FlagEvaluationState(states[0]); state {
		case FlagEvaluationComplete, FlagEvaluationFailed, FlagEvaluationNotEvaluated:
			snapshot.state, snapshot.unavailable = state, sortedUnique(unavailable)
		}
	}
	f.decision = snapshot
}

// validAt uses the same inclusive window as ingress timestamp validation.
// The zero window belongs only to application-created, non-authoritative flags.
func (f FlagSet) validAt(now time.Time) bool {
	return f.validUntil.IsZero() || (!now.Before(f.validFrom) && !now.After(f.validUntil))
}

func (f *FlagSet) setValidity(issuedAt time.Time, window time.Duration) {
	if window <= 0 {
		window = defaultSignatureWindow
	}
	f.validFrom = issuedAt.Add(-window)
	f.validUntil = issuedAt.Add(window)
}

type flagsCtxKey struct{}

// FlagsToContext stores a verified FlagSet for downstream handlers.
func FlagsToContext(ctx context.Context, fs FlagSet) context.Context {
	return context.WithValue(ctx, flagsCtxKey{}, cloneFlagSet(fs))
}

// FlagsFromContext retrieves a detached snapshot only while its original
// signature window is valid. Copying or reattaching a set never renews it.
func FlagsFromContext(ctx context.Context) (FlagSet, bool) {
	fs, ok := ctx.Value(flagsCtxKey{}).(FlagSet)
	if !ok || !fs.validAt(time.Now()) {
		return FlagSet{}, false
	}
	return cloneFlagSet(fs), true
}

func cloneFlagSet(in FlagSet) FlagSet {
	out := FlagSet{
		Flags:          make(map[string]bool, len(in.Flags)),
		Budgets:        make(map[string]json.RawMessage, len(in.Budgets)),
		decisionDigest: in.decisionDigest,
		validFrom:      in.validFrom,
		validUntil:     in.validUntil,
		decision:       in.decision,
	}
	for key, value := range in.Flags {
		out.Flags[key] = value
	}
	for key, value := range in.Budgets {
		out.Budgets[key] = append(json.RawMessage(nil), value...)
	}
	if in.decisionBinding != nil {
		binding := *in.decisionBinding
		out.decisionBinding = &binding
	}
	return out
}

func flagSetDigest(set FlagSet) [sha256.Size]byte {
	flagsJSON, flagsErr := json.Marshal(set.Flags)
	budgetsJSON, budgetsErr := json.Marshal(set.Budgets)
	if flagsErr != nil || budgetsErr != nil {
		return [sha256.Size]byte{}
	}
	return sha256.Sum256(append(append(flagsJSON, '\n'), budgetsJSON...))
}

func flagsPayload(keyID, timestamp, flagsJSON, budgetsJSON string) string {
	return strings.Join([]string{edgeSignatureVersion, keyID, timestamp, flagsJSON, budgetsJSON}, "\n")
}

// DecisionSignInput binds one flags/budgets decision to the already minted
// Edge identity envelope and exact origin request. EdgeTimestamp, EdgeNonce,
// EdgeKeyID, and EdgeSignature must be copied from that same identity envelope.
type DecisionSignInput struct {
	Secret        string
	KeyID         string
	Method        string
	SignedPath    string
	Audience      string
	PublicPrefix  string
	SubjectID     string
	TenantID      string
	RequestID     string
	EdgeKeyID     string
	EdgeTimestamp string
	EdgeNonce     string
	EdgeSignature string
	Flags         map[string]bool
	Budgets       map[string]any
}

// DecisionVerifyConfig fixes the trusted signing keys and intended origin
// audience. ExpectedAudience is mandatory; ExpectedPublicPrefix is optional
// and should be set when one origin serves multiple public route prefixes.
type DecisionVerifyConfig struct {
	PrimarySecret        string
	NextSecret           string
	PrimaryKeyID         string
	NextKeyID            string
	ExpectedAudience     string
	ExpectedPublicPrefix string
	SignatureWindow      time.Duration
	// Now selects the ingress validation clock only. Retained FlagSet authority
	// always expires against the wall clock, including after request detachment.
	Now func() time.Time
	// IdentityConfig verifies the exact Edge identity envelope whose
	// signature/timestamp/nonce are bound into the decision signature. It must
	// use the Edge identity keys, which may differ from the decision keys.
	IdentityConfig Config
}

// BuildDecisionSignaturePayload is the canonical v2 flags/budgets payload.
// Its field order is a cross-language wire contract shared with kombify-Gateway.
func BuildDecisionSignaturePayload(
	keyID, method, signedPath, audience, publicPrefix, subjectID, tenantID,
	requestID, edgeKeyID, timestamp, nonce, edgeSignature, flagsJSON, budgetsJSON string,
) string {
	return strings.Join([]string{
		decisionSignatureVersionV2,
		keyID,
		strings.ToUpper(method),
		signedPath,
		audience,
		publicPrefix,
		subjectID,
		tenantID,
		requestID,
		edgeKeyID,
		timestamp,
		nonce,
		edgeSignature,
		flagsJSON,
		budgetsJSON,
	}, "\n")
}

// SignDecisionHeaders signs a request-bound v2 decision. It does not mint the
// Edge identity envelope; callers must pass fields from the exact envelope
// already created for the same upstream request.
func SignDecisionHeaders(input DecisionSignInput) (http.Header, error) {
	secret := strings.TrimSpace(input.Secret)
	if secret == "" {
		return nil, fmt.Errorf("edge_decision_secret_missing")
	}
	keyID := strings.TrimSpace(input.KeyID)
	if keyID == "" {
		keyID = defaultEdgeKeyID
	}
	for name, value := range map[string]string{
		"method":         input.Method,
		"signed_path":    input.SignedPath,
		"audience":       input.Audience,
		"subject":        input.SubjectID,
		"tenant":         input.TenantID,
		"request_id":     input.RequestID,
		"edge_key_id":    input.EdgeKeyID,
		"edge_timestamp": input.EdgeTimestamp,
		"edge_nonce":     input.EdgeNonce,
		"edge_signature": input.EdgeSignature,
	} {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("edge_decision_binding_missing: %s", name)
		}
	}
	if !supportsEntitlementIdentitySignature(input.EdgeSignature) {
		return nil, fmt.Errorf("edge_decision_identity_version_invalid")
	}
	if err := validateTimestampAt(input.EdgeTimestamp, defaultSignatureWindow, time.Now()); err != nil {
		return nil, err
	}

	flags := input.Flags
	if flags == nil {
		flags = map[string]bool{}
	}
	budgets := input.Budgets
	if budgets == nil {
		budgets = map[string]any{}
	}
	flagsJSON, err := json.Marshal(flags)
	if err != nil {
		return nil, err
	}
	budgetsJSON, err := json.Marshal(budgets)
	if err != nil {
		return nil, err
	}
	payload := BuildDecisionSignaturePayload(
		keyID,
		input.Method,
		input.SignedPath,
		strings.TrimSpace(input.Audience),
		strings.TrimSpace(input.PublicPrefix),
		strings.TrimSpace(input.SubjectID),
		strings.TrimSpace(input.TenantID),
		strings.TrimSpace(input.RequestID),
		strings.TrimSpace(input.EdgeKeyID),
		strings.TrimSpace(input.EdgeTimestamp),
		strings.TrimSpace(input.EdgeNonce),
		strings.TrimSpace(input.EdgeSignature),
		string(flagsJSON),
		string(budgetsJSON),
	)

	h := http.Header{}
	h.Set(HeaderFlags, string(flagsJSON))
	h.Set(HeaderBudgets, string(budgetsJSON))
	h.Set(HeaderFlagsTimestamp, strings.TrimSpace(input.EdgeTimestamp))
	h.Set(HeaderFlagsKeyID, keyID)
	h.Set(HeaderFlagsSignature, decisionSignatureVersionV2+"="+signPayload(secret, payload))
	return h, nil
}

// VerifyDecisionHeaders verifies a request-bound v2 flags/budgets decision.
// It rejects detached v1 headers and binds the decision to subject, tenant,
// intended audience, method/path, request id, and the exact Edge identity
// timestamp/nonce/signature. Exact HTTP retries remain the origin's durable
// idempotency responsibility; changing any bound field invalidates the HMAC.
func VerifyDecisionHeaders(r *http.Request, cfg DecisionVerifyConfig) (FlagSet, error) {
	cfg.PrimarySecret = firstNonEmpty(cfg.PrimarySecret, os.Getenv("EDGE_FLAGS_SECRET"), os.Getenv("EDGE_AUTH_SECRET"))
	cfg.NextSecret = firstNonEmpty(cfg.NextSecret, os.Getenv("EDGE_FLAGS_SECRET_NEXT"), os.Getenv("EDGE_AUTH_SECRET_NEXT"))
	cfg.PrimaryKeyID = firstNonEmpty(cfg.PrimaryKeyID, os.Getenv("EDGE_FLAGS_KEY_ID"), defaultEdgeKeyID)
	cfg.NextKeyID = firstNonEmpty(cfg.NextKeyID, os.Getenv("EDGE_FLAGS_KEY_ID_NEXT"), defaultEdgeNextKeyID)
	return VerifyDecisionHeadersWithKeys(r, cfg)
}

// VerifyDecisionHeadersWithKeys verifies a request-bound v2 decision using only
// the decision keys supplied in cfg. Empty key IDs default to primary/next;
// empty secrets stay disabled, regardless of environment. IdentityConfig still
// follows the separate edge identity verifier contract and never uses these
// decision keys as identity authority.
func VerifyDecisionHeadersWithKeys(r *http.Request, cfg DecisionVerifyConfig) (FlagSet, error) {
	if r == nil {
		return FlagSet{}, fmt.Errorf("edge_decision_request_missing")
	}
	if strings.TrimSpace(cfg.ExpectedAudience) == "" {
		return FlagSet{}, fmt.Errorf("edge_decision_audience_missing")
	}
	keys, err := resolveDecisionVerificationKeys(cfg)
	if err != nil {
		return FlagSet{}, err
	}
	envelope, err := readDecisionEnvelope(r, cfg)
	if err != nil {
		return FlagSet{}, err
	}
	if !verifyDecisionEnvelopeSignature(r.Method, envelope, keys) {
		return FlagSet{}, fmt.Errorf("edge_decision_signature_invalid")
	}
	window := cfg.SignatureWindow
	if window <= 0 {
		window = defaultSignatureWindow
	}
	// The identity is independently verified; a decision cannot outlive it.
	if identityWindow := cfg.IdentityConfig.SignatureWindow; identityWindow > 0 && identityWindow < window {
		window = identityWindow
	}
	return decodeVerifiedDecision(r.Method, envelope, window)
}

type decisionVerificationKeys struct {
	primarySecret string
	nextSecret    string
	primaryKeyID  string
	nextKeyID     string
}

func resolveDecisionVerificationKeys(cfg DecisionVerifyConfig) (decisionVerificationKeys, error) {
	keys := decisionVerificationKeys{
		primarySecret: strings.TrimSpace(cfg.PrimarySecret),
		nextSecret:    strings.TrimSpace(cfg.NextSecret),
		primaryKeyID:  strings.TrimSpace(firstNonEmpty(cfg.PrimaryKeyID, defaultEdgeKeyID)),
		nextKeyID:     strings.TrimSpace(firstNonEmpty(cfg.NextKeyID, defaultEdgeNextKeyID)),
	}
	if keys.primarySecret == "" && keys.nextSecret == "" {
		return decisionVerificationKeys{}, fmt.Errorf("edge_decision_secret_missing")
	}
	if keys.primarySecret != "" && keys.nextSecret != "" && keys.primaryKeyID == keys.nextKeyID {
		return decisionVerificationKeys{}, fmt.Errorf("edge_decision_rotation_key_id_conflict")
	}
	return keys, nil
}

type decisionEnvelope struct {
	flagsJSON     string
	budgetsJSON   string
	timestamp     string
	keyID         string
	signature     string
	audience      string
	publicPrefix  string
	subjectID     string
	tenantID      string
	requestID     string
	edgeKeyID     string
	nonce         string
	edgeSignature string
	signedPath    string
}

func readDecisionEnvelope(r *http.Request, cfg DecisionVerifyConfig) (decisionEnvelope, error) {
	envelope := decisionEnvelope{
		flagsJSON: r.Header.Get(HeaderFlags), budgetsJSON: r.Header.Get(HeaderBudgets),
		timestamp:    strings.TrimSpace(r.Header.Get(HeaderFlagsTimestamp)),
		keyID:        strings.TrimSpace(r.Header.Get(HeaderFlagsKeyID)),
		signature:    strings.TrimSpace(r.Header.Get(HeaderFlagsSignature)),
		audience:     strings.TrimSpace(r.Header.Get(HeaderEdgeService)),
		publicPrefix: strings.TrimSpace(r.Header.Get(HeaderPublicPrefix)),
		subjectID:    strings.TrimSpace(r.Header.Get(HeaderUserID)), tenantID: strings.TrimSpace(r.Header.Get(HeaderOrgID)),
		requestID: strings.TrimSpace(r.Header.Get(HeaderRequestID)), edgeKeyID: strings.TrimSpace(r.Header.Get(HeaderEdgeKeyID)),
		nonce: strings.TrimSpace(r.Header.Get(HeaderEdgeNonce)), edgeSignature: strings.TrimSpace(r.Header.Get(HeaderEdgeSignature)),
		signedPath: strings.TrimSpace(r.Header.Get(HeaderEdgeSignedPath)),
	}
	if decisionEnvelopeMissing(envelope) {
		return decisionEnvelope{}, fmt.Errorf("edge_decision_binding_missing")
	}
	if !strings.HasPrefix(envelope.signature, decisionSignatureVersionV2+"=") {
		return decisionEnvelope{}, fmt.Errorf("edge_decision_signature_version_invalid")
	}
	if envelope.audience != strings.TrimSpace(cfg.ExpectedAudience) {
		return decisionEnvelope{}, fmt.Errorf("edge_decision_audience_mismatch")
	}
	if expectedPrefix := strings.TrimSpace(cfg.ExpectedPublicPrefix); expectedPrefix != "" && envelope.publicPrefix != expectedPrefix {
		return decisionEnvelope{}, fmt.Errorf("edge_decision_public_prefix_mismatch")
	}
	if envelope.signedPath != requestSignedPath(r) {
		return decisionEnvelope{}, fmt.Errorf("edge_decision_path_mismatch")
	}
	if envelope.timestamp != strings.TrimSpace(r.Header.Get(HeaderEdgeTimestamp)) {
		return decisionEnvelope{}, fmt.Errorf("edge_decision_timestamp_mismatch")
	}
	if !supportsEntitlementIdentitySignature(envelope.edgeSignature) {
		return decisionEnvelope{}, fmt.Errorf("edge_decision_identity_version_invalid")
	}
	identityConfig := cfg.IdentityConfig
	if identityConfig.SignatureWindow <= 0 {
		identityConfig.SignatureWindow = cfg.SignatureWindow
	}
	if err := verifyEdgeSignature(r, identityConfig); err != nil {
		return decisionEnvelope{}, fmt.Errorf("edge_decision_identity_invalid: %w", err)
	}
	now := time.Now()
	if cfg.Now != nil {
		now = cfg.Now()
	}
	if err := validateTimestampAt(envelope.timestamp, cfg.SignatureWindow, now); err != nil {
		return decisionEnvelope{}, err
	}
	return envelope, nil
}

func decisionEnvelopeMissing(envelope decisionEnvelope) bool {
	for _, value := range []string{
		envelope.flagsJSON, envelope.budgetsJSON, envelope.timestamp, envelope.keyID,
		envelope.signature, envelope.audience, envelope.subjectID, envelope.tenantID,
		envelope.requestID, envelope.edgeKeyID, envelope.nonce, envelope.edgeSignature,
		envelope.signedPath,
	} {
		if value == "" {
			return true
		}
	}
	return false
}

func verifyDecisionEnvelopeSignature(method string, envelope decisionEnvelope, keys decisionVerificationKeys) bool {
	payload := BuildDecisionSignaturePayload(
		envelope.keyID, method, envelope.signedPath, envelope.audience, envelope.publicPrefix,
		envelope.subjectID, envelope.tenantID, envelope.requestID, envelope.edgeKeyID,
		envelope.timestamp, envelope.nonce, envelope.edgeSignature, envelope.flagsJSON, envelope.budgetsJSON,
	)
	presented := strings.TrimPrefix(envelope.signature, decisionSignatureVersionV2+"=")
	for _, candidate := range []struct{ keyID, secret string }{
		{keys.primaryKeyID, keys.primarySecret},
		{keys.nextKeyID, keys.nextSecret},
	} {
		if candidate.secret != "" && envelope.keyID == candidate.keyID &&
			hmac.Equal([]byte(presented), []byte(signPayload(candidate.secret, payload))) {
			return true
		}
	}
	return false
}

func decodeVerifiedDecision(method string, envelope decisionEnvelope, window time.Duration) (FlagSet, error) {
	fs := FlagSet{Flags: map[string]bool{}, Budgets: map[string]json.RawMessage{}}
	if err := json.Unmarshal([]byte(envelope.flagsJSON), &fs.Flags); err != nil {
		return FlagSet{}, fmt.Errorf("edge_flags_payload_invalid")
	}
	if err := json.Unmarshal([]byte(envelope.budgetsJSON), &fs.Budgets); err != nil {
		return FlagSet{}, fmt.Errorf("edge_budgets_payload_invalid")
	}
	fs.bindFlagEvaluation()
	issuedSeconds, _ := strconv.ParseInt(envelope.timestamp, 10, 64)
	fs.decisionBinding = &DecisionBinding{
		Version: decisionSignatureVersionV2, KeyID: envelope.keyID,
		SubjectID: envelope.subjectID, TenantID: envelope.tenantID, Audience: envelope.audience,
		PublicPrefix: envelope.publicPrefix, Method: strings.ToUpper(method), SignedPath: envelope.signedPath,
		RequestID: envelope.requestID, EdgeKeyID: envelope.edgeKeyID, Nonce: envelope.nonce,
		IssuedAt: time.Unix(issuedSeconds, 0).UTC(),
	}
	fs.setValidity(fs.decisionBinding.IssuedAt, window)
	fs.decisionDigest = flagSetDigest(fs)
	return fs, nil
}

// supportsEntitlementIdentitySignature admits the identity versions a
// request-bound decision may pair with: v2 and its supersets v7 and v9.
func supportsEntitlementIdentitySignature(signature string) bool {
	signature = strings.TrimSpace(signature)
	return strings.HasPrefix(signature, edgeSignatureVersionV2+"=") ||
		strings.HasPrefix(signature, EdgeSignatureVersionV7+"=") ||
		strings.HasPrefix(signature, EdgeSignatureVersionV9+"=")
}

// SignFlagHeaders produces the signed flag-delivery headers. Edge/runtime and
// tests use this; secret is an EDGE_AUTH secret, keyID its identifier. Map keys
// are marshaled in sorted order by encoding/json, so signing is deterministic.
func SignFlagHeaders(secret, keyID string, flags map[string]bool, budgets map[string]any, ts time.Time) (http.Header, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, fmt.Errorf("edge_flags_secret_missing")
	}
	if keyID == "" {
		keyID = defaultEdgeKeyID
	}
	if flags == nil {
		flags = map[string]bool{}
	}
	if budgets == nil {
		budgets = map[string]any{}
	}
	flagsJSON, err := json.Marshal(flags)
	if err != nil {
		return nil, err
	}
	budgetsJSON, err := json.Marshal(budgets)
	if err != nil {
		return nil, err
	}
	timestamp := fmt.Sprintf("%d", ts.Unix())
	sig := signPayload(secret, flagsPayload(keyID, timestamp, string(flagsJSON), string(budgetsJSON)))

	h := http.Header{}
	h.Set(HeaderFlags, string(flagsJSON))
	h.Set(HeaderBudgets, string(budgetsJSON))
	h.Set(HeaderFlagsTimestamp, timestamp)
	h.Set(HeaderFlagsKeyID, keyID)
	h.Set(HeaderFlagsSignature, edgeSignatureVersion+"="+sig)
	return h, nil
}

// VerifyFlagHeaders verifies and parses the signed flag headers on r. It is
// fail-closed: any missing header, bad signature, or stale timestamp yields an
// error and an empty FlagSet. Reuses the EDGE_AUTH secrets/window from Config.
func VerifyFlagHeaders(r *http.Request, cfg Config) (FlagSet, error) {
	return verifyFlagHeaders(r, FlagVerifyConfig{
		PrimarySecret:   firstNonEmpty(cfg.EdgeAuthSecret, os.Getenv("EDGE_AUTH_SECRET")),
		NextSecret:      firstNonEmpty(cfg.EdgeAuthNextSecret, os.Getenv("EDGE_AUTH_SECRET_NEXT")),
		PrimaryKeyID:    firstNonEmpty(cfg.EdgeAuthKeyID, os.Getenv("EDGE_AUTH_KEY_ID"), defaultEdgeKeyID),
		NextKeyID:       firstNonEmpty(cfg.EdgeAuthNextKeyID, os.Getenv("EDGE_AUTH_KEY_ID_NEXT"), defaultEdgeNextKeyID),
		SignatureWindow: cfg.SignatureWindow,
	}, false)
}

// FlagVerifyConfig explicitly selects the signed v1 flag/budget verifier keys.
// Empty secrets disable their slot; no field is resolved from environment.
// Empty key IDs use primary and next respectively.
type FlagVerifyConfig struct {
	PrimarySecret   string
	NextSecret      string
	PrimaryKeyID    string
	NextKeyID       string
	SignatureWindow time.Duration
}

// VerifyFlagHeadersWithKeys verifies signed v1 flags with only cfg's key set.
// It fails closed for nil input, absent keys, duplicate active key IDs and any
// invalid signature, timestamp or payload. It does not confer v2 provenance.
func VerifyFlagHeadersWithKeys(r *http.Request, cfg FlagVerifyConfig) (FlagSet, error) {
	return verifyFlagHeaders(r, cfg, true)
}

func verifyFlagHeaders(r *http.Request, cfg FlagVerifyConfig, rejectDuplicateIDs bool) (FlagSet, error) {
	if r == nil {
		return FlagSet{}, fmt.Errorf("edge_flags_request_missing")
	}
	keys, keyErr := flagVerificationKeys(cfg, rejectDuplicateIDs)
	if keyErr != nil {
		return FlagSet{}, keyErr
	}

	flagsJSON := r.Header.Get(HeaderFlags)
	budgetsJSON := r.Header.Get(HeaderBudgets)
	timestamp := strings.TrimSpace(r.Header.Get(HeaderFlagsTimestamp))
	keyID := strings.TrimSpace(r.Header.Get(HeaderFlagsKeyID))
	signature := strings.TrimSpace(r.Header.Get(HeaderFlagsSignature))
	if flagsJSON == "" || timestamp == "" || keyID == "" || signature == "" {
		return FlagSet{}, fmt.Errorf("edge_flags_missing")
	}
	if budgetsJSON == "" {
		budgetsJSON = "{}"
	}
	if !strings.HasPrefix(signature, edgeSignatureVersion+"=") {
		return FlagSet{}, fmt.Errorf("edge_flags_signature_version_invalid")
	}
	if err := validateTimestamp(timestamp, cfg.SignatureWindow); err != nil {
		return FlagSet{}, err
	}

	presented := strings.TrimPrefix(signature, edgeSignatureVersion+"=")
	payload := flagsPayload(keyID, timestamp, flagsJSON, budgetsJSON)
	verified := verifyFlagSignature(presented, payload, keyID, keys)
	if !verified {
		return FlagSet{}, fmt.Errorf("edge_flags_signature_invalid")
	}

	fs := FlagSet{Flags: map[string]bool{}, Budgets: map[string]json.RawMessage{}}
	if err := json.Unmarshal([]byte(flagsJSON), &fs.Flags); err != nil {
		return FlagSet{}, fmt.Errorf("edge_flags_payload_invalid")
	}
	if err := json.Unmarshal([]byte(budgetsJSON), &fs.Budgets); err != nil {
		return FlagSet{}, fmt.Errorf("edge_budgets_payload_invalid")
	}
	fs.bindFlagEvaluation()
	issuedSeconds, _ := strconv.ParseInt(timestamp, 10, 64)
	fs.setValidity(time.Unix(issuedSeconds, 0), cfg.SignatureWindow)
	return fs, nil
}

type flagSigningKey struct{ keyID, secret string }

func flagVerificationKeys(cfg FlagVerifyConfig, rejectDuplicateIDs bool) ([]flagSigningKey, error) {
	primary := strings.TrimSpace(cfg.PrimarySecret)
	next := strings.TrimSpace(cfg.NextSecret)
	primaryKeyID := strings.TrimSpace(firstNonEmpty(cfg.PrimaryKeyID, defaultEdgeKeyID))
	nextKeyID := strings.TrimSpace(firstNonEmpty(cfg.NextKeyID, defaultEdgeNextKeyID))
	if rejectDuplicateIDs && primary != "" && next != "" && primaryKeyID == nextKeyID {
		return nil, fmt.Errorf("edge_flags_rotation_key_id_conflict")
	}
	if primary == "" && next == "" {
		return nil, fmt.Errorf("edge_flags_secret_missing")
	}
	return []flagSigningKey{{primaryKeyID, primary}, {nextKeyID, next}}, nil
}

func verifyFlagSignature(presented, payload, keyID string, keys []flagSigningKey) bool {
	verified := false
	for _, candidate := range keys {
		if candidate.secret == "" || keyID != candidate.keyID {
			continue
		}
		if hmac.Equal([]byte(presented), []byte(signPayload(candidate.secret, payload))) {
			verified = true
			break
		}
	}
	return verified
}
