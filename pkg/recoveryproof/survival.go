package recoveryproof

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// RequiredBackupSurvivalNotClaimed is the closed set of bounds every Guard G
// `stackkit.guard-backup-survival-receipt/v1` receipt must state, matching
// StackKits `internal/guardproof/backupsurvival.RequiredNotClaimed`.
var RequiredBackupSurvivalNotClaimed = []string{
	"backup-admin-compromise: whoever holds the maintenance or bypass authority can delete retained points; this proof does not protect against that identity",
	"provider-account-compromise: the cloud account owner and provider staff are outside the proof",
	"real-recovery-points: no deletion was attempted against any real retained recovery point",
	"lock-renewal: retention was verified at proof time only; renewal before expiry is not proven",
	"application-recovery: restored bytes are verified, application behaviour on them is not",
	"live-target: the proof ran against the bound backend only, no other target or provider is covered",
}

const (
	survivalVerdictSurvives = "survives"
	survivalVerdictFailed   = "failed"

	survivalOutcomeDenied   = "denied"
	survivalOutcomeAbsorbed = "absorbed"
	survivalOutcomeBreached = "breached"
	survivalOutcomeFailed   = "failed"
	survivalOutcomeSkipped  = "skipped"

	canaryPrefixMarker = "survival-canary/"

	survivalGenesisDomain     = "backupsurvival/genesis/v1\x00"
	survivalEntryDomain       = "backupsurvival/entry/v1\x00"
	survivalReceiptDomain     = "backupsurvival/receipt/v1\x00"
	survivalSignatureDomain   = "stackkit.guard-backup-survival-receipt/v1\x00"
	boundEnvelopeDigestDomain = "kombify.guard-backup-survival-bound/v1\x00"
	boundEnvelopeSigDomain    = "kombify.guard-backup-survival-bound-sig/v1\x00"
)

var requiredSurvivalCoreSteps = []string{
	"binding",
	"capacity",
	"commit-canary",
	"verify-retention",
	"verify-survival",
	"clean-room-restore",
	"maintenance-prune",
}

var requiredSurvivalAttacks = []struct {
	step   string
	absorb bool
}{
	{"attack:delete-object", true},
	{"attack:overwrite-object", true},
	{"attack:delete-version", false},
	{"attack:delete-version-with-bypass", false},
	{"attack:shorten-retention", false},
	{"attack:disable-versioning", false},
	{"attack:disable-object-lock", false},
	{"attack:prune", false},
}

// Self-contained wire structs matching StackKits `internal/guardproof/backupsurvival/receipt.go`.

type survivalCanaryWire struct {
	Key       string `json:"key"`
	VersionID string `json:"versionId"`
}

type survivalEntryWire struct {
	Seq      int       `json:"seq"`
	Step     string    `json:"step"`
	Outcome  string    `json:"outcome"`
	Detail   string    `json:"detail"`
	At       time.Time `json:"at"`
	PrevHash string    `json:"prevHash"`
	Hash     string    `json:"hash"`
}

type survivalSignatureWire struct {
	KeyID string `json:"keyId"`
	Value string `json:"value"`
}

type survivalReceiptWire struct {
	APIVersion    string                 `json:"apiVersion"`
	BindingDigest string                 `json:"bindingDigest"`
	BackendRef    string                 `json:"backendRef"`
	NodeID        string                 `json:"nodeId,omitempty"`
	WorkloadID    string                 `json:"workloadId,omitempty"`
	Canary        survivalCanaryWire     `json:"canary"`
	Verdict       string                 `json:"verdict"`
	Genesis       string                 `json:"genesis"`
	Entries       []survivalEntryWire    `json:"entries"`
	HeadHash      string                 `json:"headHash"`
	NotClaimed    []string               `json:"notClaimed"`
	CompletedAt   time.Time              `json:"completedAt"`
	ReceiptDigest string                 `json:"receiptDigest"`
	Signature     *survivalSignatureWire `json:"signature,omitempty"`
}

// boundSurvivalEnvelopeWire wraps a Guard G receipt with the Homelab node,
// workload and optional snapshot anchor identity it was produced for.
type boundSurvivalEnvelopeWire struct {
	SchemaVersion    string                 `json:"schemaVersion"`
	NodeID           string                 `json:"nodeId"`
	WorkloadID       string                 `json:"workloadId"`
	TargetID         string                 `json:"targetId"`
	SnapshotAnchorID string                 `json:"snapshotAnchorId,omitempty"`
	Scope            string                 `json:"scope,omitempty"`
	Unknowns         []string               `json:"unknowns,omitempty"`
	Receipt          survivalReceiptWire    `json:"receipt"`
	EnvelopeDigest   string                 `json:"envelopeDigest"`
	Signature        *survivalSignatureWire `json:"signature,omitempty"`
}

// ProjectSurvivalWithKey evaluates a Guard G receipt or bound envelope and
// requires a valid Ed25519 signature made by trustedKey.
func ProjectSurvivalWithKey(raw []byte, trustedKey ed25519.PublicKey, now time.Time, policy Policy) (Claim, error) {
	policy.SurvivalPublicKey = trustedKey
	policy.RequireSurvivalSignature = true
	return ProjectSurvival(raw, now, policy)
}

// ProjectSurvival evaluates a Guard G `stackkit.guard-backup-survival-receipt/v1`
// receipt (or a `kombify.guard-backup-survival-bound/v1` envelope wrapping one)
// on its own.
func ProjectSurvival(raw []byte, now time.Time, policy Policy) (Claim, error) {
	if err := policy.Validate(); err != nil {
		return Claim{}, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return Claim{Status: StatusAbsent, Reason: "no Guard G backup survival receipt"}, nil
	}
	if len(raw) > maxReceiptBytes {
		return Claim{Status: StatusInvalid, Reason: "backup survival receipt is oversized"}, nil
	}
	var peek struct {
		APIVersion    string `json:"apiVersion"`
		SchemaVersion string `json:"schemaVersion"`
	}
	if err := json.Unmarshal(raw, &peek); err != nil {
		return Claim{Status: StatusInvalid, Reason: fmt.Sprintf("backup survival receipt: not a valid receipt: %v", err)}, nil
	}

	var receipt survivalReceiptWire
	var subject Subject
	var snapshotAnchorID string
	var scope string
	var unknowns []string

	switch {
	case peek.SchemaVersion == BoundSurvivalEnvelopeSchema:
		var env boundSurvivalEnvelopeWire
		if status, err := decodeStrict(raw, &env); err != nil {
			return Claim{Status: status, Reason: "backup survival envelope: " + err.Error()}, nil
		}
		if !identifier(env.NodeID) || !identifier(env.WorkloadID) || !identifier(env.TargetID) {
			return Claim{Status: StatusIncomplete, Reason: "backup survival envelope lacks valid nodeId, workloadId or targetId"}, nil
		}
		if env.SnapshotAnchorID != "" && !digest(env.SnapshotAnchorID) {
			return Claim{Status: StatusInvalid, Reason: "backup survival envelope snapshotAnchorId is malformed"}, nil
		}
		if env.Receipt.BackendRef != env.TargetID {
			return Claim{Status: StatusInvalid, Reason: "backup survival envelope targetId differs from receipt backendRef"}, nil
		}
		if env.Receipt.NodeID != "" && env.Receipt.NodeID != env.NodeID {
			return Claim{Status: StatusInvalid, Reason: "backup survival envelope nodeId differs from receipt nodeId"}, nil
		}
		if env.Receipt.WorkloadID != "" && env.Receipt.WorkloadID != env.WorkloadID {
			return Claim{Status: StatusInvalid, Reason: "backup survival envelope workloadId differs from receipt workloadId"}, nil
		}
		if !digest(env.EnvelopeDigest) {
			return Claim{Status: StatusIncomplete, Reason: "backup survival envelope digest is missing or malformed"}, nil
		}
		wantEnvDigest, err := boundEnvelopeDigest(env)
		if err != nil || env.EnvelopeDigest != wantEnvDigest {
			return Claim{Status: StatusInvalid, Reason: "backup survival envelope digest does not match its canonical body"}, nil
		}
		if env.Signature != nil {
			if _, err := decodeSurvivalSignature(*env.Signature); err != nil {
				return Claim{Status: StatusInvalid, Reason: "backup survival envelope: " + err.Error()}, nil
			}
		}
		if len(policy.SurvivalPublicKey) > 0 || policy.RequireSurvivalSignature {
			if err := verifyBoundEnvelopeSignature(env, policy.SurvivalPublicKey); err != nil {
				return Claim{Status: StatusInvalid, Reason: "backup survival envelope: " + err.Error()}, nil
			}
		}
		receipt = env.Receipt
		subject = Subject{NodeID: env.NodeID, WorkloadID: env.WorkloadID, TargetID: env.TargetID}
		snapshotAnchorID = env.SnapshotAnchorID
		scope = env.Scope
		unknowns = env.Unknowns
	case peek.APIVersion == BackupSurvivalReceiptSchema:
		if status, err := decodeStrict(raw, &receipt); err != nil {
			return Claim{Status: status, Reason: "backup survival receipt: " + err.Error()}, nil
		}
		subject = Subject{NodeID: receipt.NodeID, WorkloadID: receipt.WorkloadID, TargetID: receipt.BackendRef}
	default:
		return Claim{Status: StatusInvalid, Reason: "backup survival receipt: unsupported schema or apiVersion"}, nil
	}

	if scope == "" && receipt.BackendRef != "" {
		scope = "backup-target:" + receipt.BackendRef
	}
	claim := Claim{
		Subject:          &subject,
		Scope:            scope,
		BindingDigest:    receipt.BindingDigest,
		Canary:           &CanaryRef{Key: receipt.Canary.Key, VersionID: receipt.Canary.VersionID},
		SnapshotAnchorID: snapshotAnchorID,
		Unknowns:         append([]string(nil), unknowns...),
		NotClaimed:       append([]string(nil), receipt.NotClaimed...),
	}

	status, reason := verifySurvivalReceipt(receipt, now, policy)
	if status != StatusVerified {
		claim.Status = status
		claim.Reason = "backup survival receipt: " + reason
		if !receipt.CompletedAt.IsZero() && status == StatusStale {
			utc := receipt.CompletedAt.UTC()
			claim.ObservedAt = &utc
			claim.AgeSeconds = max(int64(now.Sub(utc).Seconds()), 0)
		}
		return claim, nil
	}

	utc := receipt.CompletedAt.UTC()
	claim.Digest = digestOf(raw)
	claim.ObservedAt = &utc
	claim.AgeSeconds = max(int64(now.Sub(utc).Seconds()), 0)
	claim.Status = StatusVerified
	return claim, nil
}

func verifySurvivalReceipt(r survivalReceiptWire, now time.Time, policy Policy) (Status, string) {
	switch {
	case r.APIVersion != BackupSurvivalReceiptSchema:
		return StatusInvalid, fmt.Sprintf("unknown apiVersion %q", r.APIVersion)
	case !digest(r.BindingDigest):
		return StatusIncomplete, "bindingDigest is missing or malformed"
	case !identifier(r.BackendRef):
		return StatusIncomplete, "backendRef is missing or malformed"
	case (r.NodeID != "" || r.WorkloadID != "") && (!identifier(r.NodeID) || !identifier(r.WorkloadID)):
		return StatusIncomplete, "nodeId and workloadId must both be valid identifiers when bound"
	case !strings.HasPrefix(r.Canary.Key, canaryPrefixMarker) || len(r.Canary.Key) <= len(canaryPrefixMarker) || !identifier(r.Canary.VersionID):
		return StatusIncomplete, "canary ref is missing or outside survival-canary/"
	case !digest(r.Genesis):
		return StatusIncomplete, "genesis is missing or malformed"
	case len(r.Entries) == 0 || len(r.Entries) > maxListEntries:
		return StatusIncomplete, "entries are missing or oversized"
	case !digest(r.HeadHash):
		return StatusIncomplete, "headHash is missing or malformed"
	case r.CompletedAt.IsZero() || r.CompletedAt.Location() != time.UTC:
		return StatusIncomplete, "completedAt must be a non-zero UTC timestamp"
	case !digest(r.ReceiptDigest):
		return StatusIncomplete, "receiptDigest is missing or malformed"
	}

	prev, err := survivalGenesis(r)
	if err != nil || r.Genesis != prev {
		return StatusInvalid, "genesis does not match receipt header binding"
	}
	var lastAt time.Time
	hasFailure := false
	seenSteps := make(map[string]string, len(r.Entries))
	for i, entry := range r.Entries {
		if entry.Seq != i+1 || entry.PrevHash != prev {
			return StatusInvalid, fmt.Sprintf("hash chain broken at entry %d", i+1)
		}
		if entry.At.IsZero() || entry.At.Location() != time.UTC {
			return StatusIncomplete, fmt.Sprintf("entry %d timestamp must be non-zero UTC", i+1)
		}
		if i > 0 && entry.At.Before(lastAt) {
			return StatusInvalid, fmt.Sprintf("entry %d timestamp moved backwards", i+1)
		}
		if entry.At.After(now.Add(policy.MaxFutureSkew)) {
			return StatusFuture, fmt.Sprintf("entry %d timestamp lies in the future", i+1)
		}
		want, err := survivalEntryHash(entry)
		if err != nil || want != entry.Hash {
			return StatusInvalid, fmt.Sprintf("entry %d hash does not verify", i+1)
		}
		if entry.Outcome == survivalOutcomeFailed || entry.Outcome == survivalOutcomeBreached {
			hasFailure = true
		}
		seenSteps[entry.Step] = entry.Outcome
		prev = entry.Hash
		lastAt = entry.At
	}
	if r.CompletedAt.Before(lastAt) {
		return StatusInvalid, "completedAt precedes the last recorded entry"
	}
	if r.HeadHash != prev {
		return StatusInvalid, "headHash does not match the chain"
	}
	switch r.Verdict {
	case survivalVerdictSurvives:
		if hasFailure {
			return StatusInvalid, "survives verdict contradicts a failed or breached entry"
		}
	case survivalVerdictFailed:
		if !hasFailure {
			return StatusInvalid, "failed verdict contradicts recorded entries"
		}
	default:
		return StatusInvalid, fmt.Sprintf("unknown verdict %q", r.Verdict)
	}

	haveBounds := make(map[string]bool, len(r.NotClaimed))
	for _, bound := range r.NotClaimed {
		if strings.TrimSpace(bound) == "" {
			return StatusInvalid, "empty notClaimed bound"
		}
		if haveBounds[bound] {
			return StatusInvalid, "duplicate notClaimed bound"
		}
		haveBounds[bound] = true
	}
	for _, bound := range RequiredBackupSurvivalNotClaimed {
		if !haveBounds[bound] {
			return StatusIncomplete, "required notClaimed bound is missing"
		}
	}

	wantDigest, err := survivalReceiptDigest(r)
	if err != nil || r.ReceiptDigest != wantDigest {
		return StatusInvalid, "receiptDigest does not match canonical receipt"
	}
	if r.Signature != nil {
		if _, err := decodeSurvivalSignature(*r.Signature); err != nil {
			return StatusInvalid, err.Error()
		}
	}
	if len(policy.SurvivalPublicKey) > 0 || policy.RequireSurvivalSignature {
		if err := verifySurvivalSignature(r, policy.SurvivalPublicKey); err != nil {
			return StatusInvalid, err.Error()
		}
	}

	if status, reason, _ := freshness(r.CompletedAt.UTC(), now, policy.SurvivalMaxAge, policy.MaxFutureSkew); status != StatusVerified {
		return status, reason
	}

	if r.Verdict != survivalVerdictSurvives {
		return StatusUnqualified, "backup survival verdict is failed"
	}

	for _, step := range requiredSurvivalCoreSteps {
		outcome, ok := seenSteps[step]
		if !ok || outcome == survivalOutcomeSkipped {
			return StatusIncomplete, fmt.Sprintf("survives verdict lacks step %q", step)
		}
	}
	for _, atk := range requiredSurvivalAttacks {
		outcome, ok := seenSteps[atk.step]
		if !ok {
			return StatusIncomplete, fmt.Sprintf("survives verdict lacks attack %q", atk.step)
		}
		if atk.absorb {
			if outcome != survivalOutcomeDenied && outcome != survivalOutcomeAbsorbed {
				return StatusUnqualified, fmt.Sprintf("attack %q had outcome %q", atk.step, outcome)
			}
		} else if outcome != survivalOutcomeDenied {
			return StatusUnqualified, fmt.Sprintf("attack %q was not denied (%q)", atk.step, outcome)
		}
	}
	return StatusVerified, ""
}

func survivalHashOf(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func survivalGenesis(r survivalReceiptWire) (string, error) {
	raw, err := json.Marshal(struct {
		APIVersion    string `json:"apiVersion"`
		BindingDigest string `json:"bindingDigest"`
		BackendRef    string `json:"backendRef"`
		NodeID        string `json:"nodeId,omitempty"`
		WorkloadID    string `json:"workloadId,omitempty"`
	}{
		APIVersion:    r.APIVersion,
		BindingDigest: r.BindingDigest,
		BackendRef:    r.BackendRef,
		NodeID:        r.NodeID,
		WorkloadID:    r.WorkloadID,
	})
	if err != nil {
		return "", err
	}
	return survivalHashOf([]byte(survivalGenesisDomain), raw), nil
}

func survivalEntryHash(e survivalEntryWire) (string, error) {
	e.Hash = ""
	raw, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	return survivalHashOf([]byte(survivalEntryDomain), []byte(e.PrevHash), []byte{0}, raw), nil
}

func survivalSealBytes(r survivalReceiptWire) ([]byte, error) {
	return json.Marshal(struct {
		APIVersion    string              `json:"apiVersion"`
		BindingDigest string              `json:"bindingDigest"`
		BackendRef    string              `json:"backendRef"`
		NodeID        string              `json:"nodeId,omitempty"`
		WorkloadID    string              `json:"workloadId,omitempty"`
		Canary        survivalCanaryWire  `json:"canary"`
		Verdict       string              `json:"verdict"`
		Genesis       string              `json:"genesis"`
		Entries       []survivalEntryWire `json:"entries"`
		HeadHash      string              `json:"headHash"`
		NotClaimed    []string            `json:"notClaimed"`
		CompletedAt   time.Time           `json:"completedAt"`
	}{
		APIVersion:    r.APIVersion,
		BindingDigest: r.BindingDigest,
		BackendRef:    r.BackendRef,
		NodeID:        r.NodeID,
		WorkloadID:    r.WorkloadID,
		Canary:        r.Canary,
		Verdict:       r.Verdict,
		Genesis:       r.Genesis,
		Entries:       r.Entries,
		HeadHash:      r.HeadHash,
		NotClaimed:    r.NotClaimed,
		CompletedAt:   r.CompletedAt,
	})
}

func survivalReceiptDigest(r survivalReceiptWire) (string, error) {
	raw, err := survivalSealBytes(r)
	if err != nil {
		return "", err
	}
	return survivalHashOf([]byte(survivalReceiptDomain), raw), nil
}

func survivalSigningDigest(r survivalReceiptWire) ([]byte, error) {
	raw, err := survivalSealBytes(r)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write([]byte(survivalSignatureDomain))
	h.Write(raw)
	return h.Sum(nil), nil
}

func boundEnvelopeSealBytes(env boundSurvivalEnvelopeWire) ([]byte, error) {
	return json.Marshal(struct {
		SchemaVersion    string   `json:"schemaVersion"`
		NodeID           string   `json:"nodeId"`
		WorkloadID       string   `json:"workloadId"`
		TargetID         string   `json:"targetId"`
		SnapshotAnchorID string   `json:"snapshotAnchorId,omitempty"`
		Scope            string   `json:"scope,omitempty"`
		Unknowns         []string `json:"unknowns,omitempty"`
		ReceiptDigest    string   `json:"receiptDigest"`
	}{
		SchemaVersion:    env.SchemaVersion,
		NodeID:           env.NodeID,
		WorkloadID:       env.WorkloadID,
		TargetID:         env.TargetID,
		SnapshotAnchorID: env.SnapshotAnchorID,
		Scope:            env.Scope,
		Unknowns:         env.Unknowns,
		ReceiptDigest:    env.Receipt.ReceiptDigest,
	})
}

func boundEnvelopeDigest(env boundSurvivalEnvelopeWire) (string, error) {
	raw, err := boundEnvelopeSealBytes(env)
	if err != nil {
		return "", err
	}
	return survivalHashOf([]byte(boundEnvelopeDigestDomain), raw), nil
}

func boundEnvelopeSigningDigest(env boundSurvivalEnvelopeWire) ([]byte, error) {
	raw, err := boundEnvelopeSealBytes(env)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write([]byte(boundEnvelopeSigDomain))
	h.Write(raw)
	return h.Sum(nil), nil
}

func survivalKeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "ed25519://sha256/" + hex.EncodeToString(sum[:])
}

func decodeSurvivalSignature(sig survivalSignatureWire) ([]byte, error) {
	hexPart, ok := strings.CutPrefix(sig.KeyID, "ed25519://sha256/")
	if !ok || len(hexPart) != 64 {
		return nil, fmt.Errorf("signature keyId is malformed")
	}
	for _, c := range hexPart {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return nil, fmt.Errorf("signature keyId is malformed")
		}
	}
	raw, err := base64.RawStdEncoding.Strict().DecodeString(sig.Value)
	if err != nil || len(raw) != ed25519.SignatureSize {
		return nil, fmt.Errorf("signature value is malformed")
	}
	return raw, nil
}

func verifySurvivalSignature(r survivalReceiptWire, publicKey ed25519.PublicKey) error {
	if len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("trusted public key is missing or invalid")
	}
	if r.Signature == nil {
		return fmt.Errorf("signature is missing")
	}
	if r.Signature.KeyID != survivalKeyID(publicKey) {
		return fmt.Errorf("signature keyId does not match the trusted public key")
	}
	sigBytes, err := decodeSurvivalSignature(*r.Signature)
	if err != nil {
		return err
	}
	digest, err := survivalSigningDigest(r)
	if err != nil {
		return err
	}
	if !ed25519.Verify(publicKey, digest, sigBytes) {
		return fmt.Errorf("signature does not verify")
	}
	return nil
}

func verifyBoundEnvelopeSignature(env boundSurvivalEnvelopeWire, publicKey ed25519.PublicKey) error {
	if len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("trusted public key is missing or invalid")
	}
	if env.Signature == nil {
		return fmt.Errorf("envelope signature is missing")
	}
	if env.Signature.KeyID != survivalKeyID(publicKey) {
		return fmt.Errorf("envelope signature keyId does not match the trusted public key")
	}
	sigBytes, err := decodeSurvivalSignature(*env.Signature)
	if err != nil {
		return err
	}
	digest, err := boundEnvelopeSigningDigest(env)
	if err != nil {
		return err
	}
	if !ed25519.Verify(publicKey, digest, sigBytes) {
		return fmt.Errorf("envelope signature does not verify")
	}
	return nil
}
