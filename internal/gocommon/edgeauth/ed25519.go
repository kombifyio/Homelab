package edgeauth

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"strings"
)

// parseEdgePublicKeys parses "kid=base64url,kid2=base64url". A malformed
// entry fails the whole set so a typo never silently disables verification.
func parseEdgePublicKeys(raw string) (map[string]ed25519.PublicKey, error) {
	keys := map[string]ed25519.PublicKey{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		kid, encoded, ok := strings.Cut(entry, "=")
		kid, encoded = strings.TrimSpace(kid), strings.TrimRight(strings.TrimSpace(encoded), "=")
		key, err := base64.RawURLEncoding.DecodeString(encoded)
		if !ok || kid == "" || err != nil || len(key) != ed25519.PublicKeySize {
			return nil, ErrEdgePublicKeyInvalid
		}
		keys[kid] = ed25519.PublicKey(key)
	}
	return keys, nil
}

// verifyEd25519Envelope checks a "<kid>.<base64url signature>" value.
// Returns nil when valid, ErrEdgeUnknownKeyID when the signed key id is absent
// from the origin's configured key set (key-split / rotation fault), and
// edge_signature_invalid when the key is known but the signature does not match.
func verifyEd25519Envelope(keys map[string]ed25519.PublicKey, presented, payload string) error {
	kid, encoded, ok := strings.Cut(presented, ".")
	if !ok {
		return fmt.Errorf("edge_signature_invalid")
	}
	key, known := keys[strings.TrimSpace(kid)]
	if !known {
		return ErrEdgeUnknownKeyID
	}
	sig, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("edge_signature_invalid")
	}
	if !ed25519.Verify(key, []byte(payload), sig) {
		return fmt.Errorf("edge_signature_invalid")
	}
	return nil
}
