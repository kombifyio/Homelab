package guardbinding

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// PinRotationOperation names a signed statement that replaces the binding keys
// a Guard pins. Binding keys are derived per tenant and worker, so a
// same-subject tenant rebind (guardidentity.TenantRebindDirective) moves a
// worker to keys the Guard does not pin yet. The control plane signs the new
// key set with the key the Guard pins now; whoever holds the pins (the root
// executor on the privilege-split layout) verifies it against its own pins
// before replacing them, so the unprivileged daemon never chooses them.
const PinRotationOperation = "guard.binding-pins.rotate"

// PinSetDigest is the lowercase hex SHA-256 of the canonical pin set.
func PinSetDigest(keys []PinnedKey) (string, error) {
	encoded, err := EncodePins(keys)
	if err != nil {
		return "", err
	}
	canonical, err := Canonicalize(encoded)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// VerifyPinRotation admits rotation as the replacement of current by keys: a
// pin set exists, rotation is a pin rotation signed by one of the current
// pins, and it names exactly keys, which are well-formed Ed25519 keys. A
// rotation replayed after it took effect is refused, because the key that
// signed it is no longer pinned.
func VerifyPinRotation(current []PinnedKey, rotation Binding, keys []PinnedKey) *Denial {
	if len(current) == 0 {
		return Denyf(CodeBindingMismatch, "nothing is pinned; a pairing pins the binding keys")
	}
	if rotation.Operation != PinRotationOperation || rotation.WorkerID == "" || rotation.TenantID == "" {
		return Denyf(CodeBindingMismatch, "the statement is not a binding key rotation")
	}
	if len(keys) == 0 || len(keys) > 4 {
		return Denyf(CodeBindingMismatch, "a rotation names one to four keys")
	}
	for _, key := range keys {
		raw, err := base64.StdEncoding.DecodeString(key.PublicKey)
		if err != nil || len(raw) != ed25519.PublicKeySize || KeyIDOf(raw) != key.KeyID {
			return Denyf(CodeBindingMismatch, "rotated key %q is malformed", key.KeyID)
		}
	}
	digest, err := PinSetDigest(keys)
	if err != nil || rotation.PayloadDigest != digest {
		return Denyf(CodeBindingMismatch, "the rotation does not name this key set")
	}
	return rotation.VerifySignature(current)
}
