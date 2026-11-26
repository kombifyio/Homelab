package hostidentity

import (
	"errors"

	"github.com/kombifyio/homelab/pkg/guardidentity"
)

// ErrNoIdentityKey means no identity key is loaded, so nothing can be signed.
var ErrNoIdentityKey = errors.New("kombify Guard identity key is not available")

// MintHostProof signs a host proof for one verifier audience with the Guard
// identity key. The key stays inside the Manager: callers receive only the
// compact JWS. A Guard whose identity is revoked or needs recovery signs
// nothing, so a stale or cloned state directory cannot keep reach alive.
func (m *Manager) MintHostProof(in guardidentity.HostProofInput) (string, error) {
	m.mu.Lock()
	key, state := m.key, m.rec.State
	m.mu.Unlock()
	if key == nil {
		return "", ErrNoIdentityKey
	}
	switch state {
	case StateRevoked:
		return "", ErrRevoked
	case StateRecoveryNeeded:
		return "", ErrRecoveryNeeded
	}
	return guardidentity.SignHostProof(key, in)
}

func (m *Manager) MintPaperworkMoveProof(in guardidentity.HostProofInput, grant string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.key == nil {
		return "", ErrNoIdentityKey
	}
	switch m.rec.State {
	case StateRevoked:
		return "", ErrRevoked
	case StateRecoveryNeeded:
		return "", ErrRecoveryNeeded
	case StateBound:
	default:
		return "", errors.New("paperwork move proof requires a bound Guard")
	}
	return guardidentity.SignPaperworkMoveProof(m.key, in, grant)
}

// MintPaperworkPullProof retains node-key custody and refuses an identity that
// was revoked, needs recovery or has not yet been bound. Each call mints a new
// JTI; a retained pull session never turns into a bearer credential.
func (m *Manager) MintPaperworkPullProof(in guardidentity.HostProofInput, requestSHA256 string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.key == nil {
		return "", ErrNoIdentityKey
	}
	switch m.rec.State {
	case StateRevoked:
		return "", ErrRevoked
	case StateRecoveryNeeded:
		return "", ErrRecoveryNeeded
	case StateBound:
	default:
		return "", errors.New("paperwork pull proof requires a bound Guard")
	}
	return guardidentity.SignPaperworkPullProof(m.key, in, requestSHA256)
}

func (m *Manager) MintPaperworkSignInProof(in guardidentity.HostProofInput, body []byte) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.key == nil || m.rec.State != StateBound {
		return "", errors.New("paperwork sign-in proof requires a bound Guard")
	}
	return guardidentity.SignPaperworkSignInProof(m.key, in, body)
}
