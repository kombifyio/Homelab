package guardbinding

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"
)

// retiredWorkerKeyLabel is the worker binding key label of Techstack 0.49.19
// (pkg/guardbinding/signature.go at 6b36975b9). A Guard paired by that server
// pinned the public key derived under it, and keeps that pin across a binary
// swap: the pin file lives in the Guard state directory, not in the binary.
const retiredWorkerKeyLabel = "techstack.guard.command-binding-key/v1"

// retiredPin derives the pin a 0.49.19 pairing handed the Guard, with the same
// HKDF construction the server used then.
func retiredPin(t *testing.T, base []byte, tenantID, workerID string) []PinnedKey {
	t.Helper()
	info := retiredWorkerKeyLabel
	for _, field := range []string{tenantID, workerID} {
		info += fmt.Sprintf("|%d:%s", len(field), field)
	}
	seed, err := hkdf.Key(sha256.New, base, nil, info, ed25519.SeedSize)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := NewSignerFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKeys()
}

// A Guard enrolled under the Techstack naming cannot be carried into the
// Homelab server by swapping its binary in place: the server signs with the
// worker key derived under the Homelab label, the swapped Guard still pins the
// key of its 0.49.19 pairing, and the only in-band repair, a pin rotation, has
// to be signed by the key the Guard already pins, which the forward-only
// server no longer derives. Only a fresh pairing, which replaces the pin set
// (the managed Guard re-enrollment, executionchannel.ReenrollGuard), makes the
// node's bindings admissible again.
func TestGuardPairedUnderTheRetiredLabelNeedsAFreshPairing(t *testing.T) {
	base := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	tenantID, workerID := "usr:auth0|subject", "worker_lease_1"
	oldPins := retiredPin(t, base, tenantID, workerID)
	server, err := DeriveWorkerSigner(base, tenantID, workerID)
	if err != nil {
		t.Fatal(err)
	}
	newPins := server.PublicKeys()
	if oldPins[0].KeyID == newPins[0].KeyID {
		t.Fatal("the Homelab label derives the same worker key as the retired label")
	}

	binding := Binding{
		CommandID: "cmd-reenroll-1", TenantID: tenantID, WorkerID: workerID, ServerID: "server_lease_1",
		Operation: "STACKKIT_OPERATION_APPLY", PayloadDigest: "digest", WorkerGeneration: 1, CurrentGeneration: 1,
		AuthorityExpiresAt: time.Now().Add(time.Hour).UTC(), ContractVersion: ContractVersion,
	}
	if err := server.Sign(&binding); err != nil {
		t.Fatal(err)
	}
	denial := binding.VerifySignature(oldPins)
	if denial == nil {
		t.Fatal("a swapped Guard that keeps its 0.49.19 pin admitted a binding signed by the Homelab server")
	}
	if denial.Code != CodeBindingMismatch {
		t.Fatalf("refusal code = %q, want %q", denial.Code, CodeBindingMismatch)
	}

	rotation := signedPinRotation(t, server, newPins)
	if VerifyPinRotation(oldPins, rotation, newPins) == nil {
		t.Fatal("a pin rotation signed only by the new worker key was admitted by a Guard pinning the retired key")
	}

	// A fresh pairing hands the Guard the Homelab pin; from then on the same
	// binding is admitted.
	if denial := binding.VerifySignature(newPins); denial != nil {
		t.Fatalf("a freshly paired Guard refused the server's binding: %v", denial)
	}
}
