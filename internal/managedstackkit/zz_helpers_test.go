package managedstackkit

// Helpers of retired unit tests that the remaining boundary tests of this package still use.

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"github.com/kombifyio/homelab/pkg/backupstore"
	"github.com/kombifyio/stackkits/pkg/hostdelivery"
	"testing"
	"time"
)

type fakeDeliveryCustody struct {
	pin     *backupstore.RecipientPin
	binding *backupstore.NodeBinding
	staged  map[string]*backupstore.ReplacementRecipientPin
	stagedB map[string]*backupstore.NodeBinding
	status  map[string]string
}

func (*fakeDeliveryCustody) Ensure(context.Context, string, string) (backupstore.CustodyEvidence, error) {
	return backupstore.CustodyEvidence{BindingEvidence: []byte("b"), TargetEvidence: []byte("t"), ObservedAt: time.Now()}, nil
}

func (*fakeDeliveryCustody) Get(context.Context, string, string) (backupstore.Credentials, error) {
	return backupstore.Credentials{TenantID: "tenant-a", StackID: "stack-a", Bucket: "skbk-stack-a", Endpoint: "https://acct.r2.cloudflarestorage.com", TokenID: "token-a", AccessKeyID: "access-a", SecretAccessKey: "SECRET-ACCESS-KEY", RepositoryPassword: "REPO-PASSPHRASE"}, nil
}

func (*fakeDeliveryCustody) Evidence(context.Context, string, string) (backupstore.CustodyEvidence, error) {
	return backupstore.CustodyEvidence{BindingEvidence: []byte("b"), TargetEvidence: []byte("t")}, nil
}

func (*fakeDeliveryCustody) RecordAttestation(_ context.Context, _, _ string, evidence backupstore.CustodyEvidence) (backupstore.CustodyEvidence, error) {
	return evidence, nil
}

func (f *fakeDeliveryCustody) PinRecipient(_ context.Context, pin backupstore.RecipientPin) error {
	if f.pin != nil && (f.pin.Fingerprint != pin.Fingerprint || f.pin.OwnerKey != pin.OwnerKey || f.pin.Generation != pin.Generation) {
		return backupstore.ErrRecipientSubstituted
	}
	f.pin = &pin
	return nil
}

func (f *fakeDeliveryCustody) Recipient(context.Context, string, string) (backupstore.RecipientPin, bool, error) {
	if f.pin == nil {
		return backupstore.RecipientPin{}, false, nil
	}
	return *f.pin, true, nil
}

func (f *fakeDeliveryCustody) RecordNodeBinding(_ context.Context, _, _ string, binding backupstore.NodeBinding) error {
	f.binding = &binding
	return nil
}

func (f *fakeDeliveryCustody) NodeBinding(context.Context, string, string) (backupstore.NodeBinding, error) {
	if f.binding == nil {
		return backupstore.NodeBinding{}, backupstore.ErrNodeBindingMissing
	}
	return *f.binding, nil
}

func (f *fakeDeliveryCustody) StageReplacementRecipient(_ context.Context, pin backupstore.ReplacementRecipientPin) error {
	if f.staged == nil {
		f.staged = map[string]*backupstore.ReplacementRecipientPin{}
	}
	if f.status == nil {
		f.status = map[string]string{}
	}
	key := pin.TenantID + "/" + pin.StackID + "/" + pin.ReplacementID
	if existing, ok := f.staged[key]; ok {
		if f.status[key] != "staged" {
			return backupstore.ErrInvalidCustody
		}
		if existing.Fingerprint != pin.Fingerprint || existing.OwnerKey != pin.OwnerKey || existing.Generation != pin.Generation {
			return backupstore.ErrRecipientSubstituted
		}
		return nil
	}
	copyPin := pin
	f.staged[key] = &copyPin
	f.status[key] = "staged"
	return nil
}

func (f *fakeDeliveryCustody) ReplacementRecipient(_ context.Context, tenantID, stackID, replacementID string) (backupstore.ReplacementRecipientPin, bool, error) {
	key := tenantID + "/" + stackID + "/" + replacementID
	if f.staged == nil || f.status[key] != "staged" || f.staged[key] == nil {
		return backupstore.ReplacementRecipientPin{}, false, nil
	}
	return *f.staged[key], true, nil
}

func (f *fakeDeliveryCustody) RecordReplacementNodeBinding(_ context.Context, tenantID, stackID, replacementID string, binding backupstore.NodeBinding) error {
	key := tenantID + "/" + stackID + "/" + replacementID
	if f.status[key] != "staged" {
		return backupstore.ErrCustodyNotFound
	}
	if f.stagedB == nil {
		f.stagedB = map[string]*backupstore.NodeBinding{}
	}
	if existing := f.stagedB[key]; existing != nil && *existing != binding {
		return backupstore.ErrInvalidCustody
	}
	copyBinding := binding
	f.stagedB[key] = &copyBinding
	return nil
}

func (f *fakeDeliveryCustody) ReplacementNodeBinding(_ context.Context, tenantID, stackID, replacementID string) (backupstore.NodeBinding, error) {
	key := tenantID + "/" + stackID + "/" + replacementID
	if f.stagedB == nil || f.stagedB[key] == nil {
		return backupstore.NodeBinding{}, backupstore.ErrNodeBindingMissing
	}
	return *f.stagedB[key], nil
}

func (f *fakeDeliveryCustody) CommitReplacementCustody(_ context.Context, tenantID, stackID, replacementID string) error {
	key := tenantID + "/" + stackID + "/" + replacementID
	if f.status[key] == "committed" {
		return nil
	}
	if f.status[key] != "staged" || f.staged[key] == nil || f.stagedB[key] == nil {
		return backupstore.ErrCustodyNotFound
	}
	pin := f.staged[key].RecipientPin
	binding := *f.stagedB[key]
	f.pin = &pin
	f.binding = &binding
	f.status[key] = "committed"
	return nil
}

func (f *fakeDeliveryCustody) DiscardReplacementCustody(_ context.Context, tenantID, stackID, replacementID string) error {
	key := tenantID + "/" + stackID + "/" + replacementID
	if f.status[key] == "committed" {
		return backupstore.ErrInvalidCustody
	}
	if f.status[key] == "staged" {
		f.status[key] = "rolled_back"
	}
	return nil
}

// node is a StackKits node: an owner signing key and a separate recipient key.
type node struct {
	owner     ed25519.PrivateKey
	recipient *ecdh.PrivateKey
}

func newNode(t *testing.T) node {
	t.Helper()
	_, owner, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return node{owner: owner, recipient: recipient}
}

func (n node) statement(t *testing.T, challenge string, issuedAt time.Time) []byte {
	t.Helper()
	public := n.recipient.PublicKey().Bytes()
	statement := hostdelivery.Statement{
		APIVersion: hostdelivery.StatementAPIVersion, SiteRef: cloudOperationsSite, NodeRef: cloudOperationsNode,
		RecipientKey: base64.RawStdEncoding.EncodeToString(public), Fingerprint: hostdelivery.Fingerprint(public), Generation: 1,
		Challenge: challenge, IssuedAt: issuedAt.UTC().Format(time.RFC3339Nano), OwnerKeyID: "owner-1",
		OwnerPublic: base64.RawStdEncoding.EncodeToString(n.owner.Public().(ed25519.PublicKey)),
	}
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
