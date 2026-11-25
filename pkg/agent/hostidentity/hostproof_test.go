package hostidentity

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kombifyio/homelab/pkg/guardidentity"
)

// The Guard signs a host proof only while its identity is usable, and only with
// its own key: a revoked or recovery-needed Guard mints nothing, so stale
// state cannot keep reach alive.
func TestMintHostProofSignsWithOwnKeyOnlyWhileIdentityIsUsable(t *testing.T) {
	manager, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	input := guardidentity.HostProofInput{Issuer: "worker-1", Subject: "server-1", TenantID: "t1", Audience: guardidentity.AudienceRelay, Generation: 2}
	if _, err := manager.MintHostProof(input); !errors.Is(err, ErrNoIdentityKey) {
		t.Fatalf("before the key exists = %v", err)
	}
	if err := manager.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	proof, err := manager.MintHostProof(input)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := guardidentity.VerifyHostProof(proof, guardidentity.HostProofExpectation{
		Issuer: "worker-1", Subject: "server-1", TenantID: "t1", Audience: guardidentity.AudienceRelay, Generation: 2,
	})
	if err != nil || verified.Thumbprint != manager.Record().KeyThumbprint {
		t.Fatalf("proof = %v, %v", verified, err)
	}
	for state, want := range map[State]error{StateRevoked: ErrRevoked, StateRecoveryNeeded: ErrRecoveryNeeded} {
		if err := manager.SetState(state, "test"); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.MintHostProof(input); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", state, err, want)
		}
	}
}

func TestPaperworkPullProofRequiresBoundUsableIdentity(t *testing.T) {
	manager, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	in := guardidentity.HostProofInput{Issuer: "worker-1", Subject: "server-1", TenantID: "tenant-1", Generation: 1}
	digest := strings.Repeat("a", 64)
	if _, err := manager.MintPaperworkPullProof(in, digest); !errors.Is(err, ErrNoIdentityKey) {
		t.Fatalf("missing identity: %v", err)
	}
	if err := manager.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.MintPaperworkPullProof(in, digest); err == nil {
		t.Fatal("unbound identity signed pull")
	}
	if err := manager.SetState(StateBound, "test"); err != nil {
		t.Fatal(err)
	}
	proof, err := manager.MintPaperworkPullProof(in, digest)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		t.Fatal("invalid proof")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(manager.key.Public().(ed25519.PublicKey), []byte(parts[0]+"."+parts[1]), signature) {
		t.Fatal("pull proof not signed by node key")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Audience string `json:"aud"`
		JTI      string `json:"jti"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Audience != "urn:kombify:paperwork-selfhost-pull:sha256:"+digest || claims.JTI == "" {
		t.Fatal("pull proof not request-bound")
	}
	if _, err := manager.MintPaperworkPullProof(in, strings.ToUpper(digest)); err == nil {
		t.Fatal("noncanonical digest accepted")
	}
	for state, want := range map[State]error{StateRevoked: ErrRevoked, StateRecoveryNeeded: ErrRecoveryNeeded} {
		if err := manager.SetState(state, "test"); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.MintPaperworkPullProof(in, digest); !errors.Is(err, want) {
			t.Fatalf("%s: %v", state, err)
		}
	}
}
