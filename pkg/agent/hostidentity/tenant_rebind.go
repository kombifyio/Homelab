package hostidentity

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kombifyio/techstack/internal/privatefs"
	"github.com/kombifyio/techstack/pkg/guardidentity"
)

const (
	// tenantRebindFileName holds a stored same-subject tenant rebind offer. It
	// carries the canonical tenant's credential and is a secret like the
	// enrollment file.
	tenantRebindFileName = "tenant-rebind.json"
	// tenantRebindReceiptName records an applied rebind, without credential.
	tenantRebindReceiptName = "tenant-rebind-receipt.json"
	maxTenantRebindBytes    = 16 << 10
	// tenantRebindProbeRetention is how long an unproven offer is kept after
	// its NotAfter: the operator may have committed it before NotAfter while
	// the host was offline, and only a proof against the control plane tells.
	tenantRebindProbeRetention = 30 * 24 * time.Hour
)

// ErrTenantRebindReady is guardidentity.ErrTenantRebindReady.
var ErrTenantRebindReady = guardidentity.ErrTenantRebindReady

type storedTenantRebind struct {
	Offer    guardidentity.TenantRebindOffer `json:"offer"`
	StoredAt time.Time                       `json:"stored_at"`
	// ReadyAt is set once the control plane refused the legacy credential and
	// accepted the offered one. A ready offer never expires.
	ReadyAt *time.Time `json:"ready_at,omitempty"`
}

// TenantRebindReceipt records an applied same-subject tenant rebind.
type TenantRebindReceipt struct {
	ContractVersion string    `json:"contract_version"`
	DirectiveID     string    `json:"directive_id"`
	FromTenantID    string    `json:"from_tenant_id"`
	ToTenantID      string    `json:"to_tenant_id"`
	JournalEntries  int       `json:"journal_entries"`
	AppliedAt       time.Time `json:"applied_at"`
	// BindingPinRotation and BindingSigningKeys are handed to an executor that
	// pins its own binding keys; PinsRotatedAt records that it adopted them.
	BindingPinRotation json.RawMessage                       `json:"binding_pin_rotation,omitempty"`
	BindingSigningKeys []guardidentity.TenantRebindPinnedKey `json:"binding_signing_keys,omitempty"`
	PinsRotatedAt      *time.Time                            `json:"pins_rotated_at,omitempty"`
}

func (m *Manager) loadTenantRebindLocked() (storedTenantRebind, bool) {
	var stored storedTenantRebind
	data, err := privatefs.ReadFile(m.root, tenantRebindFileName)
	if err != nil || len(data) == 0 || len(data) > maxTenantRebindBytes {
		return stored, false
	}
	if json.Unmarshal(data, &stored) != nil || stored.Offer.Directive.ValidateShape() != nil ||
		stored.Offer.ValidateMaterial() != nil {
		return storedTenantRebind{}, false
	}
	return stored, true
}

func (m *Manager) saveTenantRebindLocked(stored storedTenantRebind) error {
	raw, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	return privatefs.Replace(m.root, tenantRebindFileName, raw, 0o600)
}

// liveTenantRebindLocked returns the stored offer. An unproven offer is
// destroyed only long after its NotAfter; a ready one is kept until applied.
func (m *Manager) liveTenantRebindLocked() (storedTenantRebind, bool) {
	stored, ok := m.loadTenantRebindLocked()
	if !ok {
		return stored, false
	}
	if stored.ReadyAt == nil && m.now().After(stored.Offer.Directive.NotAfter.Add(tenantRebindProbeRetention)) {
		_ = wipeRootedSecretFile(m.root, tenantRebindFileName)
		return storedTenantRebind{}, false
	}
	return stored, true
}

// StageTenantRebind validates offer against this Guard's current tenant and
// worker and stores it. Offering the stored directive again changes nothing.
// Another directive replaces the stored one only when that one expired
// unproven; otherwise it is refused.
func (m *Manager) StageTenantRebind(offer guardidentity.TenantRebindOffer, currentTenantID, runtimeAgentID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rec.State == StateRevoked {
		return fmt.Errorf("%w: this Guard is revoked", guardidentity.ErrTenantRebindRefused)
	}
	now := m.now()
	if err := offer.ValidateForGuard(currentTenantID, runtimeAgentID, now); err != nil {
		return err
	}
	offer.Directive.StagedAt = nil
	if stored, ok := m.liveTenantRebindLocked(); ok {
		if stored.Offer.Directive.DirectiveID == offer.Directive.DirectiveID &&
			stored.Offer.Directive.Digest() == offer.Directive.Digest() && stored.Offer.AgentToken == offer.AgentToken {
			if !sameBindingKeys(stored.Offer.BindingSigningKeys, offer.BindingSigningKeys) {
				return fmt.Errorf("%w: the offer changed its binding keys", guardidentity.ErrTenantRebindRefused)
			}
			return nil
		}
		if stored.ReadyAt != nil || now.Before(stored.Offer.Directive.NotAfter) {
			return fmt.Errorf("%w: another tenant rebind is already stored", guardidentity.ErrTenantRebindRefused)
		}
	}
	return m.saveTenantRebindLocked(storedTenantRebind{Offer: offer, StoredAt: now})
}

func sameBindingKeys(a, b []guardidentity.TenantRebindPinnedKey) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// StagedTenantRebindID returns the stored directive id to acknowledge while
// the directive is still offered.
func (m *Manager) StagedTenantRebindID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, ok := m.liveTenantRebindLocked()
	if !ok || stored.ReadyAt != nil || stored.Offer.Directive.Validate(m.now()) != nil {
		return ""
	}
	return stored.Offer.Directive.DirectiveID
}

// PendingTenantRebind returns a stored offer that is not ready yet.
func (m *Manager) PendingTenantRebind() (guardidentity.TenantRebindOffer, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, ok := m.liveTenantRebindLocked()
	if !ok || stored.ReadyAt != nil {
		return guardidentity.TenantRebindOffer{}, false
	}
	return stored.Offer, true
}

// MarkTenantRebindReady records that the control plane accepted the offered
// credential of directiveID after it refused the legacy one, and signals
// TenantRebindReadyNotify.
func (m *Manager) MarkTenantRebindReady(directiveID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, ok := m.liveTenantRebindLocked()
	if !ok || stored.Offer.Directive.DirectiveID != directiveID {
		return fmt.Errorf("%w: no such stored offer", guardidentity.ErrTenantRebindRefused)
	}
	if stored.ReadyAt == nil {
		now := m.now()
		stored.ReadyAt = &now
		if err := m.saveTenantRebindLocked(stored); err != nil {
			return err
		}
	}
	m.rebindReadyChanLocked()
	m.rebindReadyOnce.Do(func() { close(m.rebindReady) })
	return nil
}

func (m *Manager) rebindReadyChanLocked() chan struct{} {
	if m.rebindReady == nil {
		m.rebindReady = make(chan struct{})
	}
	return m.rebindReady
}

// TenantRebindReadyNotify is closed once an offer became ready in this process.
func (m *Manager) TenantRebindReadyNotify() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rebindReadyChanLocked()
}

// TenantRebindReady reports whether a stored offer is ready: the Guard stops
// so that its restart applies the offer.
func (m *Manager) TenantRebindReady() bool {
	select {
	case <-m.TenantRebindReadyNotify():
		return true
	default:
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, ok := m.liveTenantRebindLocked()
	return ok && stored.ReadyAt != nil
}

// ApplyReadyTenantRebind applies a stored offer that became ready: it rewrites
// every enrollment copy to the canonical tenant, its credential and binding
// keys, then relabels the journal through relabel, then destroys the offer and
// records a receipt. It runs before the Guard opens its journal. A crash in
// between repeats the remaining steps on the next start. It returns nil, nil
// when nothing is ready.
func (m *Manager) ApplyReadyTenantRebind(configuredEnrollment string, relabel func(from, to string) (int, error)) (*TenantRebindReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, ok := m.liveTenantRebindLocked()
	if !ok || stored.ReadyAt == nil {
		return nil, nil
	}
	offer := stored.Offer
	directive := offer.Directive
	rewritten := 0
	for _, copy := range m.enrollmentCopies(configuredEnrollment) {
		raw, err := m.readEnrollmentAt(copy)
		if err != nil {
			return nil, err
		}
		view, err := ParseEnrollment(raw)
		if err != nil {
			return nil, err
		}
		if view.RuntimeAgentID != directive.RuntimeAgentID {
			return nil, fmt.Errorf("%w: the enrollment names another worker", guardidentity.ErrTenantRebindRefused)
		}
		switch {
		case view.TenantID == directive.ToTenantID && view.AgentToken == offer.AgentToken:
			// Rewritten before an interruption.
		case view.TenantID == directive.FromTenantID:
			next, rewriteErr := rewriteEnrollmentTenant(raw, offer)
			if rewriteErr != nil {
				return nil, rewriteErr
			}
			if err := m.writeEnrollmentAt(copy, next); err != nil {
				return nil, fmt.Errorf("persist the rebound enrollment: %w", err)
			}
		default:
			return nil, fmt.Errorf("%w: the enrollment is bound to neither tenant of the offer", guardidentity.ErrTenantRebindRefused)
		}
		rewritten++
	}
	if rewritten == 0 {
		return nil, fmt.Errorf("%w: the enrollment file is missing", guardidentity.ErrTenantRebindRefused)
	}
	entries := 0
	if relabel != nil {
		var err error
		if entries, err = relabel(directive.FromTenantID, directive.ToTenantID); err != nil {
			return nil, fmt.Errorf("relabel the Guard journal: %w", err)
		}
	}
	receipt := TenantRebindReceipt{
		ContractVersion: guardidentity.TenantRebindContractVersion, DirectiveID: directive.DirectiveID,
		FromTenantID: directive.FromTenantID, ToTenantID: directive.ToTenantID,
		JournalEntries: entries, AppliedAt: m.now(),
		BindingPinRotation: offer.BindingPinRotation, BindingSigningKeys: offer.BindingSigningKeys,
	}
	if rawReceipt, marshalErr := json.Marshal(receipt); marshalErr == nil {
		_ = privatefs.Replace(m.root, tenantRebindReceiptName, rawReceipt, 0o600)
	}
	if err := wipeRootedSecretFile(m.root, tenantRebindFileName); err != nil {
		return &receipt, fmt.Errorf("destroy the applied tenant rebind offer: %w", err)
	}
	return &receipt, nil
}

// enrollmentCopies lists every existing enrollment file: the configured path
// and the state-directory copy. Both are rewritten, so neither can later be
// picked up with the legacy binding.
func (m *Manager) enrollmentCopies(configured string) []string {
	var out []string
	if configured != "" {
		if leaf, inside := m.custodyLeaf(configured); inside {
			if _, err := m.root.Stat(leaf); err == nil {
				out = append(out, configured)
			}
		} else if _, err := os.Stat(configured); err == nil {
			out = append(out, configured)
		}
	}
	fallback := filepath.Join(m.Dir(), EnrollmentFileName)
	if _, err := m.root.Stat(EnrollmentFileName); err == nil {
		if len(out) == 0 || !sameEnrollmentPath(out[0], fallback) {
			out = append(out, fallback)
		}
	}
	return out
}

func sameEnrollmentPath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	return errA == nil && errB == nil && absA == absB
}

func (m *Manager) readEnrollmentAt(path string) ([]byte, error) {
	var file *os.File
	var err error
	if leaf, inside := m.custodyLeaf(path); inside {
		file, err = privatefs.OpenFile(m.root, leaf, os.O_RDONLY, 0)
	} else {
		var absolute string
		if absolute, err = filepath.Abs(path); err == nil {
			var directory *os.Root
			if directory, err = os.OpenRoot(filepath.Dir(absolute)); err == nil {
				defer directory.Close()
				file, err = directory.Open(filepath.Base(absolute))
			}
		}
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, 1<<20))
}

// writeEnrollmentAt replaces exactly the named copy, with owner-only
// permissions, without falling back to another location.
func (m *Manager) writeEnrollmentAt(path string, raw []byte) error {
	if leaf, inside := m.custodyLeaf(path); inside {
		return privatefs.Replace(m.root, leaf, raw, 0o600)
	}
	return WriteFileAtomic(path, raw, 0o600)
}

// rewriteEnrollmentTenant moves an enrollment document to the offer's tenant:
// every tenant_id equal to the legacy tenant (top level, the data envelope and
// channel_bootstrap) becomes the canonical tenant, every agent_token the
// offered credential, and binding_signing_keys the offered keys. Any other
// tenant-named value still equal to the legacy tenant refuses the rewrite: an
// unknown binding of the legacy tenant must not survive silently. (Owner and
// subject fields keep the person's subject, which a bare-subject legacy tenant
// equals.)
func rewriteEnrollmentTenant(raw []byte, offer guardidentity.TenantRebindOffer) ([]byte, error) {
	from, to := offer.Directive.FromTenantID, offer.Directive.ToTenantID
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, errors.New("enrollment is not a JSON object")
	}
	var keys []any
	for _, key := range offer.BindingSigningKeys {
		keys = append(keys, map[string]any{"key_id": key.KeyID, "public_key": key.PublicKey})
	}
	rewriteBinding := func(object map[string]any, topLevel bool) error {
		if object == nil {
			return nil
		}
		if value, ok := object["tenant_id"].(string); ok && value == from {
			object["tenant_id"] = to
		}
		if _, ok := object["agent_token"].(string); ok {
			object["agent_token"] = offer.AgentToken
		}
		if _, pinned := object["binding_signing_keys"]; pinned || (topLevel && len(keys) > 0) {
			if len(keys) == 0 {
				return fmt.Errorf("%w: the enrollment pins binding keys the offer does not replace", guardidentity.ErrTenantRebindRefused)
			}
			object["binding_signing_keys"] = keys
		}
		if bootstrap, ok := object["channel_bootstrap"].(map[string]any); ok {
			if value, ok := bootstrap["tenant_id"].(string); ok && value == from {
				bootstrap["tenant_id"] = to
			}
		}
		return nil
	}
	if err := rewriteBinding(document, true); err != nil {
		return nil, err
	}
	if data, ok := document["data"].(map[string]any); ok {
		if err := rewriteBinding(data, false); err != nil {
			return nil, err
		}
	}
	if namesTenant(document, "", from) {
		return nil, fmt.Errorf("%w: the enrollment holds another binding of the legacy tenant", guardidentity.ErrTenantRebindRefused)
	}
	out, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	view, err := ParseEnrollment(out)
	if err != nil || view.TenantID != to || view.AgentToken != offer.AgentToken {
		return nil, fmt.Errorf("%w: the rewritten enrollment does not name the canonical tenant", guardidentity.ErrTenantRebindRefused)
	}
	return out, nil
}

// namesTenant reports whether a value under a tenant-named key equals tenant.
func namesTenant(value any, key, tenant string) bool {
	switch typed := value.(type) {
	case string:
		return typed == tenant && strings.Contains(strings.ToLower(key), "tenant")
	case map[string]any:
		for name, item := range typed {
			if namesTenant(item, name, tenant) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if namesTenant(item, key, tenant) {
				return true
			}
		}
	}
	return false
}

// PendingPinRotation returns the receipt of an applied rebind whose binding key
// rotation an executor has not adopted yet.
func (m *Manager) PendingPinRotation() (TenantRebindReceipt, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var receipt TenantRebindReceipt
	data, err := privatefs.ReadFile(m.root, tenantRebindReceiptName)
	if err != nil || len(data) > maxTenantRebindBytes || json.Unmarshal(data, &receipt) != nil {
		return TenantRebindReceipt{}, false
	}
	if receipt.PinsRotatedAt != nil || len(receipt.BindingPinRotation) == 0 || len(receipt.BindingSigningKeys) == 0 {
		return TenantRebindReceipt{}, false
	}
	return receipt, true
}

// MarkPinsRotated records that the executor adopted the rotation of directiveID.
func (m *Manager) MarkPinsRotated(directiveID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var receipt TenantRebindReceipt
	data, err := privatefs.ReadFile(m.root, tenantRebindReceiptName)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &receipt); err != nil || receipt.DirectiveID != directiveID {
		return fmt.Errorf("%w: no such applied rebind", guardidentity.ErrTenantRebindRefused)
	}
	now := m.now()
	receipt.PinsRotatedAt = &now
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	return privatefs.Replace(m.root, tenantRebindReceiptName, raw, 0o600)
}
