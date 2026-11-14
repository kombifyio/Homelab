package guardbinding

import (
	"testing"
	"time"
)

func signedPinRotation(t *testing.T, legacy *Signer, keys []PinnedKey) Binding {
	t.Helper()
	digest, err := PinSetDigest(keys)
	if err != nil {
		t.Fatal(err)
	}
	rotation := Binding{
		CommandID: "trb_1", TenantID: "marcel-personal", WorkerID: "worker_1", ServerID: "server_1",
		Operation: PinRotationOperation, PayloadDigest: digest,
		AuthorityExpiresAt: time.Now().Add(time.Hour).UTC(), ContractVersion: ContractVersion,
	}
	if err := legacy.Sign(&rotation); err != nil {
		t.Fatal(err)
	}
	return rotation
}

func TestPinRotationIsAdmittedOnlyWhenSignedByAPinnedKeyForExactlyItsKeys(t *testing.T) {
	base := []byte("0123456789abcdef0123456789abcdef")
	legacy, err := DeriveWorkerSigner(base, "marcel-personal", "worker_1")
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := DeriveWorkerSigner(base, "usr:google-oauth2|1", "worker_1")
	if err != nil {
		t.Fatal(err)
	}
	keys := canonical.PublicKeys()
	rotation := signedPinRotation(t, legacy, keys)
	if denial := VerifyPinRotation(legacy.PublicKeys(), rotation, keys); denial != nil {
		t.Fatalf("a rotation signed by the pinned key was refused: %v", denial)
	}
	if VerifyPinRotation(nil, rotation, keys) == nil {
		t.Fatal("a rotation was admitted although nothing is pinned")
	}
	if VerifyPinRotation(keys, rotation, keys) == nil {
		t.Fatal("a rotation was admitted again after its signing key was rotated out")
	}
	other, err := DeriveWorkerSigner(base, "usr:auth0|other", "worker_1")
	if err != nil {
		t.Fatal(err)
	}
	if VerifyPinRotation(legacy.PublicKeys(), rotation, other.PublicKeys()) == nil {
		t.Fatal("a rotation was admitted for keys it does not name")
	}
	forged := signedPinRotation(t, other, keys)
	if VerifyPinRotation(legacy.PublicKeys(), forged, keys) == nil {
		t.Fatal("a rotation signed by an unpinned key was admitted")
	}
	command := rotation
	command.Operation = "stackkit.execute"
	if VerifyPinRotation(legacy.PublicKeys(), command, keys) == nil {
		t.Fatal("a statement of another operation was admitted as a rotation")
	}
}
