package guardidentity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Same-subject tenant rebind (platform-ilxzn): a person's tenant is only
// usr:<auth0 sub>. An enrolled Guard still bound to that person's legacy
// tenant (the bare subject, or an operator-mapped label such as a personal
// workspace name) is moved to the canonical tenant without anyone at the host.
//
// The operator stages one directive on the worker under the legacy tenant.
// The control plane offers it on the Guard's authenticated heartbeat together
// with a credential derived for the canonical tenant; the Guard stores the
// offer and acknowledges it. After the operator moved the tenant's rows and
// committed the credential under the canonical tenant, the legacy credential
// is refused: only then does the Guard rewrite its enrollment and journal to
// the canonical tenant and restart. Nothing reads a legacy tenant afterwards.
const (
	// TenantRebindContractVersion versions the directive and the offer.
	TenantRebindContractVersion = "guard.tenant-rebind/v1"
	// TenantRebindResourceKey is the worker resource holding the directive.
	TenantRebindResourceKey = "guard_tenant_rebind"
	// TenantRebindOfferField is the heartbeat response field of the offer.
	TenantRebindOfferField = "guard_tenant_rebind"
	// CanonicalPersonTenantPrefix prefixes a person's only tenant.
	CanonicalPersonTenantPrefix = "usr:"
	// TenantRebindMaxLifetime bounds how long a directive may be offered.
	TenantRebindMaxLifetime = 7 * 24 * time.Hour
	// TenantRebindDefaultLifetime is the lifetime the operator command uses.
	TenantRebindDefaultLifetime = 72 * time.Hour
)

// ErrTenantRebindRefused names a directive or offer that fails the
// same-subject rules. It never carries a credential.
var ErrTenantRebindRefused = errors.New("guard tenant rebind refused")

// ErrTenantRebindReady reports that the control plane refused the Guard's
// legacy credential and accepted the offered canonical one: the Guard stops and
// its restart applies the stored offer instead of entering recovery.
var ErrTenantRebindReady = errors.New("the control plane moved this server to its canonical tenant; restarting to apply the stored tenant rebind")

// TenantRebindPinnedKey is a command-binding public key the Guard pins for the
// canonical tenant (the JSON shape of guardbinding.PinnedKey). Binding keys are
// derived per tenant and worker, so the legacy pins cannot verify the canonical
// tenant's bindings.
type TenantRebindPinnedKey struct {
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
}

// TenantRebindDirective is the non-secret, operator-staged move of one worker
// from a legacy tenant to the canonical tenant of the same subject.
type TenantRebindDirective struct {
	ContractVersion      string    `json:"contract_version"`
	DirectiveID          string    `json:"directive_id"`
	FromTenantID         string    `json:"from_tenant_id"`
	ToTenantID           string    `json:"to_tenant_id"`
	SubjectID            string    `json:"subject_id"`
	RuntimeAgentID       string    `json:"runtime_agent_id"`
	ServerID             string    `json:"server_id"`
	StackID              string    `json:"stack_id,omitempty"`
	CredentialGeneration int64     `json:"credential_generation"`
	IssuedAt             time.Time `json:"issued_at"`
	NotAfter             time.Time `json:"not_after"`
	// StagedAt is recorded by the control plane when the Guard acknowledged
	// that it stored the offer. It is not part of the digest.
	StagedAt *time.Time `json:"staged_at,omitempty"`
}

// TenantRebindOffer is what the Guard receives: the directive plus the
// credential for the canonical tenant. It is held only in the Guard's private
// state directory and is never logged.
type TenantRebindOffer struct {
	Directive  TenantRebindDirective `json:"directive"`
	AgentToken string                `json:"agent_token"`
	// BindingSigningKeys replace the enrollment's pinned command-binding keys.
	BindingSigningKeys []TenantRebindPinnedKey `json:"binding_signing_keys,omitempty"`
	// BindingPinRotation is the guardbinding pin rotation statement for
	// BindingSigningKeys, signed with the worker's legacy-tenant binding key, for
	// the executor that holds its own pins (privilege-split layout).
	BindingPinRotation json.RawMessage `json:"binding_pin_rotation,omitempty"`
}

// CanonicalPersonTenant returns usr:<subject>, or "" for an unusable subject.
func CanonicalPersonTenant(subject string) string {
	subject = strings.TrimSpace(subject)
	if !validRebindSubject(subject) {
		return ""
	}
	return CanonicalPersonTenantPrefix + subject
}

func validRebindSubject(subject string) bool {
	if subject == "" || len(subject) > 255 || strings.HasPrefix(subject, CanonicalPersonTenantPrefix) ||
		subject != strings.TrimSpace(subject) {
		return false
	}
	for _, r := range subject {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// ValidateTenantRebindPair applies the same-subject rule: to must be
// usr:<subject>; from must be the bare subject (the legacy form), or exactly
// the legacy label the operator mapped to this subject. A usr: tenant is never
// a source, and nothing moves between two subjects.
func ValidateTenantRebindPair(from, to, subject, mappedLabel string) error {
	from, to, subject, mappedLabel = strings.TrimSpace(from), strings.TrimSpace(to),
		strings.TrimSpace(subject), strings.TrimSpace(mappedLabel)
	canonical := CanonicalPersonTenant(subject)
	switch {
	case canonical == "":
		return fmt.Errorf("%w: the subject is not a usable auth subject", ErrTenantRebindRefused)
	case to != canonical:
		return fmt.Errorf("%w: the target tenant must be %s", ErrTenantRebindRefused, canonical)
	case from == "" || from == to || strings.HasPrefix(from, CanonicalPersonTenantPrefix):
		return fmt.Errorf("%w: the source must be a legacy tenant of the subject", ErrTenantRebindRefused)
	case from == subject:
		return nil
	case mappedLabel != "" && from == mappedLabel && validRebindSubject(mappedLabel):
		return nil
	default:
		return fmt.Errorf("%w: the source is neither the bare subject nor the label mapped to it", ErrTenantRebindRefused)
	}
}

// NewTenantRebindDirectiveID returns a random directive id.
func NewTenantRebindDirectiveID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "trb_" + hex.EncodeToString(raw[:]), nil
}

// Validate checks a directive's shape and that it is still offered at now. It
// does not decide whether from is mapped to the subject: the operator command
// does that when it stages the directive; the Guard follows its own tenant.
func (d TenantRebindDirective) Validate(now time.Time) error {
	if err := d.ValidateShape(); err != nil {
		return err
	}
	if !now.Before(d.NotAfter) {
		return fmt.Errorf("%w: directive expired", ErrTenantRebindRefused)
	}
	return nil
}

// ValidateShape checks everything but the offer lifetime: a Guard keeps an
// offer it stored past NotAfter, because the operator may have committed it
// before then and only a proof against the control plane tells.
func (d TenantRebindDirective) ValidateShape() error {
	switch {
	case d.ContractVersion != TenantRebindContractVersion:
		return fmt.Errorf("%w: unsupported directive contract", ErrTenantRebindRefused)
	case !strings.HasPrefix(d.DirectiveID, "trb_") || len(d.DirectiveID) != 36:
		return fmt.Errorf("%w: directive id is malformed", ErrTenantRebindRefused)
	case d.RuntimeAgentID == "" || d.ServerID == "":
		return fmt.Errorf("%w: directive names no worker and server", ErrTenantRebindRefused)
	case d.CredentialGeneration < 0:
		return fmt.Errorf("%w: credential generation is negative", ErrTenantRebindRefused)
	case d.IssuedAt.IsZero() || !d.NotAfter.After(d.IssuedAt) || d.NotAfter.Sub(d.IssuedAt) > TenantRebindMaxLifetime:
		return fmt.Errorf("%w: directive lifetime is invalid", ErrTenantRebindRefused)
	}
	if d.ToTenantID != CanonicalPersonTenant(d.SubjectID) {
		return fmt.Errorf("%w: target tenant is not the subject's canonical tenant", ErrTenantRebindRefused)
	}
	if d.FromTenantID == "" || d.FromTenantID == d.ToTenantID || strings.HasPrefix(d.FromTenantID, CanonicalPersonTenantPrefix) {
		return fmt.Errorf("%w: source tenant is not a legacy tenant", ErrTenantRebindRefused)
	}
	return nil
}

// Digest is the lowercase hex SHA-256 of the directive's canonical identity
// (without StagedAt). The credential for the canonical tenant is derived from
// it, so a changed directive yields a different credential.
func (d TenantRebindDirective) Digest() string {
	payload, _ := json.Marshal(struct {
		ContractVersion      string `json:"contract_version"`
		DirectiveID          string `json:"directive_id"`
		FromTenantID         string `json:"from_tenant_id"`
		ToTenantID           string `json:"to_tenant_id"`
		SubjectID            string `json:"subject_id"`
		RuntimeAgentID       string `json:"runtime_agent_id"`
		ServerID             string `json:"server_id"`
		StackID              string `json:"stack_id"`
		CredentialGeneration int64  `json:"credential_generation"`
		IssuedAt             string `json:"issued_at"`
		NotAfter             string `json:"not_after"`
	}{
		d.ContractVersion, d.DirectiveID, d.FromTenantID, d.ToTenantID, d.SubjectID,
		d.RuntimeAgentID, d.ServerID, d.StackID, d.CredentialGeneration,
		d.IssuedAt.UTC().Format(time.RFC3339Nano), d.NotAfter.UTC().Format(time.RFC3339Nano),
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// IdempotencyKey is the credential idempotency key of the directive.
func (d TenantRebindDirective) IdempotencyKey() string {
	return "guard-tenant-rebind:" + d.DirectiveID
}

// ValidateForGuard checks an offer against the Guard's own binding: the
// offer must move exactly this worker away from exactly its current tenant.
func (o TenantRebindOffer) ValidateForGuard(currentTenantID, runtimeAgentID string, now time.Time) error {
	if err := o.Directive.Validate(now); err != nil {
		return err
	}
	if strings.TrimSpace(currentTenantID) == "" || o.Directive.FromTenantID != strings.TrimSpace(currentTenantID) {
		return fmt.Errorf("%w: the offer does not move this Guard's current tenant", ErrTenantRebindRefused)
	}
	if o.Directive.RuntimeAgentID != strings.TrimSpace(runtimeAgentID) {
		return fmt.Errorf("%w: the offer names another worker", ErrTenantRebindRefused)
	}
	return o.ValidateMaterial()
}

// ValidateMaterial checks the offered credential and binding keys.
func (o TenantRebindOffer) ValidateMaterial() error {
	token := strings.TrimSpace(o.AgentToken)
	if !strings.HasPrefix(token, "tsra.opaque.") || len(token) > 256 || token != o.AgentToken {
		return fmt.Errorf("%w: the offer carries no opaque runtime-agent credential", ErrTenantRebindRefused)
	}
	if len(o.BindingPinRotation) > 8<<10 || (len(o.BindingPinRotation) > 0 && len(o.BindingSigningKeys) == 0) {
		return fmt.Errorf("%w: the offer carries a malformed binding key rotation", ErrTenantRebindRefused)
	}
	if len(o.BindingSigningKeys) > 4 {
		return fmt.Errorf("%w: the offer pins too many binding keys", ErrTenantRebindRefused)
	}
	for _, key := range o.BindingSigningKeys {
		if len(key.KeyID) != 16 || key.PublicKey == "" || len(key.PublicKey) > 128 {
			return fmt.Errorf("%w: the offer pins a malformed binding key", ErrTenantRebindRefused)
		}
	}
	return nil
}

// DirectiveFromResource decodes the directive stored on a worker resource.
func DirectiveFromResource(value any) (TenantRebindDirective, bool) {
	if value == nil {
		return TenantRebindDirective{}, false
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return TenantRebindDirective{}, false
	}
	var directive TenantRebindDirective
	if json.Unmarshal(raw, &directive) != nil || directive.ContractVersion != TenantRebindContractVersion {
		return TenantRebindDirective{}, false
	}
	return directive, true
}

// ResourceValue encodes a directive for the worker resource.
func (d TenantRebindDirective) ResourceValue() (map[string]any, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}
