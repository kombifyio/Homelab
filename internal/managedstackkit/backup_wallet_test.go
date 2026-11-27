package managedstackkit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kombifyio/homelab/pkg/backupstore"
	"github.com/kombifyio/homelab/pkg/stackkitcommand"
	"github.com/kombifyio/stackkits/pkg/hostdelivery"
)

// fakeWallet seals and signs like the kombify Wallet: only to a node whose
// statement pins its delivering key and that the item lists as a host target.
type fakeWallet struct {
	key     ed25519.PrivateKey
	keyID   string
	stored  map[string][]byte
	targets map[string]HostTarget
}

func newFakeWallet(t *testing.T) *fakeWallet {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeWallet{key: key, keyID: "wallet-host-delivery-1", stored: map[string][]byte{}, targets: map[string]HostTarget{}}
}

func (w *fakeWallet) public() ed25519.PublicKey { return w.key.Public().(ed25519.PublicKey) }

func (w *fakeWallet) Authority() (string, string, error) {
	return base64.RawStdEncoding.EncodeToString(w.public()), w.keyID, nil
}

func (w *fakeWallet) StoreBackupTarget(_ context.Context, _, _ string, material []byte) (string, error) {
	itemID := "itm_" + strings.Repeat("a", 26)
	w.stored[itemID] = append([]byte(nil), material...)
	return itemID, nil
}

func (w *fakeWallet) DeliverBackupTarget(_ context.Context, _, itemID, operation string, target HostTarget, raw []byte, challenge string, expiresAt time.Time) ([]byte, error) {
	listed, ok := w.targets[itemID]
	if !ok || listed != target {
		return nil, errors.New("host_target_unknown")
	}
	var statement hostdelivery.Statement
	if err := json.Unmarshal(raw, &statement); err != nil {
		return nil, err
	}
	owner, _ := base64.RawStdEncoding.DecodeString(listed.OwnerPublicKey)
	if err := hostdelivery.VerifyStatement(statement, owner, challenge, time.Now(), 10*time.Minute); err != nil {
		return nil, err
	}
	if err := hostdelivery.VerifyStatementAuthority(statement, w.public(), w.keyID); err != nil {
		return nil, errors.New("host_authority_not_pinned")
	}
	recipient, _ := base64.RawStdEncoding.DecodeString(statement.RecipientKey)
	now := time.Now().UTC()
	unsigned, err := hostdelivery.Seal(recipient, hostdelivery.Context{
		TenantRef: target.TenantRef, DeliveryRef: itemID, ItemVersion: 1, SiteRef: statement.SiteRef, NodeRef: statement.NodeRef,
		BindingGeneration: statement.Generation, Operation: operation, RecipientFingerprint: statement.Fingerprint, OperationID: "op-1",
		IssuedAt: now.Format(time.RFC3339Nano), NotAfter: expiresAt.UTC().Format(time.RFC3339Nano),
	}, w.stored[itemID], now)
	if err != nil {
		return nil, err
	}
	return hostdelivery.SignEnvelope(unsigned, w.key, w.keyID)
}

// pinnedStatement is the node's statement after it pinned authority.
func (n node) pinnedStatement(t *testing.T, challenge string, authority ed25519.PublicKey, keyID string) []byte {
	t.Helper()
	var statement hostdelivery.Statement
	if err := json.Unmarshal(n.statement(t, challenge, time.Now()), &statement); err != nil {
		t.Fatal(err)
	}
	statement.AuthorityKeyID, statement.AuthorityFingerprint, statement.Signature = keyID, hostdelivery.Fingerprint(authority), hostdelivery.Signature{}
	payload, err := statement.SigningBytes()
	if err != nil {
		t.Fatal(err)
	}
	statement.Signature = hostdelivery.Signature{OwnerRef: "owner/local/1", KeyID: "owner-1", Value: base64.RawStdEncoding.EncodeToString(ed25519.Sign(n.owner, hostdelivery.StatementDigest(payload)))}
	encoded, err := json.Marshal(statement)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// In Wallet mode the managed backup target reaches the node only as the
// Wallet's authority-signed delivery: it verifies under the key the node
// pinned and opens there to the S3 target object, and nothing is sealed by
// Homelab itself, not even when the Wallet is missing or the node has not
// pinned its key.
func TestWalletModeDeliversTheBackupTargetOnlyThroughTheWallet(t *testing.T) {
	ctx := context.Background()
	scope := DeliveryScope{TenantID: "tenant-a", StackID: "stack-a", OwnerID: "auth0|synthetic-owner"}
	wallet := newFakeWallet(t)
	custody := &fakeDeliveryCustody{}
	delivery, err := NewBackupDelivery(custody, custody, WithWallet(wallet))
	if err != nil {
		t.Fatal(err)
	}
	delivery.attest = func(_ context.Context, _ backupstore.Credentials, evidence backupstore.CustodyEvidence) (backupstore.CustodyEvidence, error) {
		evidence.ObservedAt = time.Now()
		return evidence, nil
	}
	node := newNode(t)
	nodeOwner := base64.RawStdEncoding.EncodeToString(node.owner.Public().(ed25519.PublicKey))
	wallet.targets["itm_"+strings.Repeat("a", 26)] = HostTarget{SiteRef: cloudOperationsSite, NodeRef: cloudOperationsNode, TenantRef: stackkitcommand.DeliveryTenantRef(scope.TenantID), OwnerPublicKey: nodeOwner}

	// A node that has not pinned the Wallet's key gets nothing, and nothing is stored.
	challenge, _ := delivery.NewChallenge()
	unpinned, err := delivery.AdmitRecipient(ctx, scope, challenge, node.statement(t, challenge, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if sealed, err := delivery.Seal(ctx, scope, unpinned, hostdelivery.OperationBackupTargetImport); err == nil || sealed != nil || len(wallet.stored) != 0 {
		t.Fatalf("a node without the pinned Wallet key received a delivery: %v", err)
	}

	challenge, _ = delivery.NewChallenge()
	recipient, err := delivery.AdmitRecipient(ctx, scope, challenge, node.pinnedStatement(t, challenge, wallet.public(), wallet.keyID))
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{hostdelivery.OperationBackupTargetImport, hostdelivery.OperationBackupTargetRebind} {
		sealed, err := delivery.Seal(ctx, scope, recipient, operation)
		if err != nil {
			t.Fatalf("%s through the Wallet: %v", operation, err)
		}
		envelope, err := hostdelivery.ParseEnvelope(sealed)
		if err != nil || envelope.VerifyAuthority(wallet.public(), wallet.keyID) != nil {
			t.Fatalf("%s is not signed by the pinned Wallet key: %v", operation, err)
		}
		opened, plaintext, err := hostdelivery.Open(node.recipient.Bytes(), sealed, time.Now())
		var material map[string]string
		if err != nil || json.Unmarshal(plaintext, &material) != nil || opened.Operation != operation ||
			material["secretAccessKey"] != "SECRET-ACCESS-KEY" || material["passphrase"] != "REPO-PASSPHRASE" || strings.Contains(string(sealed), "SECRET-ACCESS-KEY") {
			t.Fatalf("%s did not open on the node to the backup target: %v", operation, err)
		}
	}

	// Wallet mode without a configured Wallet fails closed instead of sealing.
	unconfigured, err := NewBackupDelivery(custody, custody, WithWallet(nil))
	if err != nil {
		t.Fatal(err)
	}
	if sealed, err := unconfigured.Seal(ctx, scope, recipient, hostdelivery.OperationBackupTargetImport); !errors.Is(err, ErrBackupWalletUnavailable) || sealed != nil {
		t.Fatalf("Wallet mode without a Wallet sealed itself: %v", err)
	}
}

func TestBackupDeliveryReplacementRecipientHandoffPreservesSourceAndEnforcesStrictNonReplacement(t *testing.T) {
	ctx := context.Background()
	scope := DeliveryScope{TenantID: "tenant-a", StackID: "stack-a", OwnerID: "owner-1"}
	custody := &fakeDeliveryCustody{}
	delivery, err := NewBackupDelivery(custody, custody)
	if err != nil {
		t.Fatal(err)
	}
	delivery.attest = func(_ context.Context, _ backupstore.Credentials, evidence backupstore.CustodyEvidence) (backupstore.CustodyEvidence, error) {
		evidence.ObservedAt = time.Now()
		return evidence, nil
	}

	sourceNode := newNode(t)
	destNode := newNode(t)
	rogueNode := newNode(t)

	// 1. Source Basic Cube S enrolls normally.
	ch, _ := delivery.NewChallenge()
	srcRecipient, err := delivery.AdmitRecipient(ctx, scope, ch, sourceNode.statement(t, ch, time.Now()))
	if err != nil {
		t.Fatalf("source AdmitRecipient: %v", err)
	}
	if _, err := delivery.Seal(ctx, scope, srcRecipient, hostdelivery.OperationBackupTargetImport); err != nil {
		t.Fatalf("source Seal: %v", err)
	}
	srcBinding := backupstore.NodeBinding{
		BackupTargetRef:       "backup-target://sha256/" + strings.Repeat("1", 64),
		CustodyAttestationRef: "backup-custody-attestation://sha256/" + strings.Repeat("2", 64),
	}
	if err := delivery.RecordNodeBinding(ctx, scope, srcBinding); err != nil {
		t.Fatalf("source RecordNodeBinding: %v", err)
	}

	// 2. Non-replacement AdmitRecipient still rejects destination or rogue keys.
	ch, _ = delivery.NewChallenge()
	if _, err := delivery.AdmitRecipient(ctx, scope, ch, destNode.statement(t, ch, time.Now())); err == nil || !strings.Contains(err.Error(), "pinned owner key") {
		t.Fatalf("non-replacement AdmitRecipient with dest owner key = %v, want pinned owner key error", err)
	}
	ch, _ = delivery.NewChallenge()
	swappedOnSource := node{owner: sourceNode.owner, recipient: destNode.recipient}
	if _, err := delivery.AdmitRecipient(ctx, scope, ch, swappedOnSource.statement(t, ch, time.Now())); !errors.Is(err, backupstore.ErrRecipientSubstituted) {
		t.Fatalf("non-replacement AdmitRecipient with substituted recipient key = %v, want ErrRecipientSubstituted", err)
	}

	// 3. Replacement handoff stages destNode without disturbing sourceNode's pin or binding.
	repScope := ReplacementScope{
		TenantID: "tenant-a", StackID: "stack-a", OwnerID: "owner-1",
		ReplacementID: "sr-1", DestinationLeaseID: "lease-dest-1",
		DestinationServerID: "srv-dest-1", DestinationResourceGeneration: "gen-dest-1",
	}
	ch, _ = delivery.NewChallenge()
	dstRecipient, err := delivery.AdmitReplacementRecipient(ctx, repScope, ch, destNode.statement(t, ch, time.Now()))
	if err != nil {
		t.Fatalf("AdmitReplacementRecipient: %v", err)
	}
	// Non-replacement Seal MUST refuse the staged destination recipient key.
	if sealed, err := delivery.Seal(ctx, scope, dstRecipient, hostdelivery.OperationBackupTargetImport); !errors.Is(err, backupstore.ErrRecipientSubstituted) || sealed != nil {
		t.Fatalf("non-replacement Seal with staged dest recipient = %v, want ErrRecipientSubstituted", err)
	}
	if sealed, err := delivery.SealReplacement(ctx, repScope, dstRecipient, hostdelivery.OperationBackupTargetImport); err != nil || len(sealed) == 0 {
		t.Fatalf("SealReplacement for staged replacement recipient: %v", err)
	}
	// SealReplacement MUST refuse an unpinned/substituted key.
	roguePub := rogueNode.recipient.PublicKey().Bytes()
	rogueRecipient := Recipient{
		Public: roguePub, Fingerprint: hostdelivery.Fingerprint(roguePub),
		Generation: dstRecipient.Generation, SiteRef: dstRecipient.SiteRef, NodeRef: dstRecipient.NodeRef,
	}
	if sealed, err := delivery.SealReplacement(ctx, repScope, rogueRecipient, hostdelivery.OperationBackupTargetImport); !errors.Is(err, backupstore.ErrRecipientSubstituted) || sealed != nil {
		t.Fatalf("SealReplacement with rogue recipient = %v, want ErrRecipientSubstituted", err)
	}
	dstBinding := backupstore.NodeBinding{
		BackupTargetRef:       "backup-target://sha256/" + strings.Repeat("3", 64),
		CustodyAttestationRef: "backup-custody-attestation://sha256/" + strings.Repeat("4", 64),
	}
	if err := delivery.RecordReplacementNodeBinding(ctx, repScope, dstBinding); err != nil {
		t.Fatalf("RecordReplacementNodeBinding: %v", err)
	}

	// Source node still owns stack_backup_stores before cutover.
	gotBinding, err := delivery.NodeBinding(ctx, scope)
	if err != nil || gotBinding != srcBinding {
		t.Fatalf("source NodeBinding disturbed before cutover: got=%+v want=%+v err=%v", gotBinding, srcBinding, err)
	}

	// Key substitution during retry of the same replacement_id is refused.
	ch, _ = delivery.NewChallenge()
	if _, err := delivery.AdmitReplacementRecipient(ctx, repScope, ch, rogueNode.statement(t, ch, time.Now())); err == nil || !strings.Contains(err.Error(), "pinned owner key") {
		t.Fatalf("AdmitReplacementRecipient rogue owner key = %v, want pinned owner key error", err)
	}
	ch, _ = delivery.NewChallenge()
	swappedOnDest := node{owner: destNode.owner, recipient: rogueNode.recipient}
	if _, err := delivery.AdmitReplacementRecipient(ctx, repScope, ch, swappedOnDest.statement(t, ch, time.Now())); !errors.Is(err, backupstore.ErrRecipientSubstituted) {
		t.Fatalf("AdmitReplacementRecipient key substitution = %v, want ErrRecipientSubstituted", err)
	}

	// 4. Pre-cutover rollback discards staged handoff while keeping source intact.
	if err := delivery.DiscardReplacementCustody(ctx, "tenant-a", "stack-a", "sr-1"); err != nil {
		t.Fatalf("DiscardReplacementCustody: %v", err)
	}
	ch, _ = delivery.NewChallenge()
	if _, err := delivery.AdmitRecipient(ctx, scope, ch, sourceNode.statement(t, ch, time.Now())); err != nil {
		t.Fatalf("source AdmitRecipient after rollback failed: %v", err)
	}

	// 5. Retry replacement sr-2 commits at cutover, promoting destNode into stack_backup_stores.
	repScope2 := repScope
	repScope2.ReplacementID = "sr-2"
	repScope2.DestinationLeaseID = "lease-dest-2"
	ch, _ = delivery.NewChallenge()
	if _, err := delivery.AdmitReplacementRecipient(ctx, repScope2, ch, destNode.statement(t, ch, time.Now())); err != nil {
		t.Fatalf("AdmitReplacementRecipient sr-2: %v", err)
	}
	if err := delivery.RecordReplacementNodeBinding(ctx, repScope2, dstBinding); err != nil {
		t.Fatalf("RecordReplacementNodeBinding sr-2: %v", err)
	}
	if err := delivery.CommitReplacementCustody(ctx, "tenant-a", "stack-a", "sr-2"); err != nil {
		t.Fatalf("CommitReplacementCustody: %v", err)
	}
	promoted, err := delivery.NodeBinding(ctx, scope)
	if err != nil || promoted != dstBinding {
		t.Fatalf("promoted NodeBinding = %+v, want %+v (err=%v)", promoted, dstBinding, err)
	}
	ch, _ = delivery.NewChallenge()
	if _, err := delivery.AdmitRecipient(ctx, scope, ch, sourceNode.statement(t, ch, time.Now())); err == nil || !strings.Contains(err.Error(), "pinned owner key") {
		t.Fatalf("old source owner key after cutover = %v, want pinned owner key error", err)
	}
	ch, _ = delivery.NewChallenge()
	swappedAfterCutover := node{owner: destNode.owner, recipient: sourceNode.recipient}
	if _, err := delivery.AdmitRecipient(ctx, scope, ch, swappedAfterCutover.statement(t, ch, time.Now())); !errors.Is(err, backupstore.ErrRecipientSubstituted) {
		t.Fatalf("old source recipient key after cutover = %v, want ErrRecipientSubstituted", err)
	}
}
