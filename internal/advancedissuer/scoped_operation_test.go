package advancedissuer

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	wire "github.com/kombifyio/stackkits/contracts/advancedcapability"
)

// A scoped release update or own-application action is one exact grant: the
// signed document carries exactly the admitted scope, and a widened request
// (another operation, a second scope, a longer life) is never signed.
func TestScopedOperationsSignExactlyOneScope(t *testing.T) {
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 12, 19, 0, 0, 0, time.UTC)
	issuer, err := New(Config{IssuerID: "techstack", PrivateKey: key, Recorder: &memoryRecorder{}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	base := Request{TenantID: "tenant-1", DeploymentID: "deployment-1", StackID: "stack-1", OwnerRef: "owner/local/00112233445566778899aabbccddeeff", TTL: 15 * time.Minute}
	release := wire.ApplicationRelease{ReleaseKind: "untested", TargetDigest: "sha256:" + strings.Repeat("b", 64), TargetRelease: "1.37.5", UseCaseRef: "vault"}
	lifecycle := wire.OwnApplicationLifecycle{Action: "update", Name: "mail", OperationID: "job-1", ReviewDigest: "sha256:" + strings.Repeat("c", 64)}

	releaseRequest := base
	releaseRequest.Operations = []string{OperationApplicationReleaseUpdate}
	releaseRequest.ApplicationRelease = &release
	issued, err := issuer.Issue(t.Context(), releaseRequest)
	if err != nil {
		t.Fatal(err)
	}
	document, err := wire.DecodeCapability(issued.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if document.ApplicationRelease == nil || *document.ApplicationRelease != release || document.OwnApplicationLifecycle != nil || len(document.AllowedOperations) != 1 {
		t.Fatal("release capability does not carry exactly the approved release")
	}
	if err = document.VerifySignature(key.Public().(ed25519.PublicKey)); err != nil {
		t.Fatal(err)
	}

	lifecycleRequest := base
	lifecycleRequest.Operations = []string{OperationOwnApplicationLifecycle}
	lifecycleRequest.OwnApplicationLifecycle = &lifecycle
	issued, err = issuer.Issue(t.Context(), lifecycleRequest)
	if err != nil {
		t.Fatal(err)
	}
	if document, err = wire.DecodeCapability(issued.Raw); err != nil {
		t.Fatal(err)
	}
	if document.OwnApplicationLifecycle == nil || *document.OwnApplicationLifecycle != lifecycle || document.ApplicationRelease != nil {
		t.Fatal("lifecycle capability does not carry exactly the reviewed action")
	}

	widened := []Request{
		func() Request {
			r := releaseRequest
			r.Operations = []string{OperationApplicationReleaseUpdate, OperationRollbackCoordinated}
			return r
		}(),
		func() Request { r := releaseRequest; r.OwnApplicationLifecycle = &lifecycle; return r }(),
		func() Request { r := releaseRequest; r.ApplicationRelease = nil; return r }(),
		func() Request { r := lifecycleRequest; r.TTL = 16 * time.Minute; return r }(),
		func() Request { r := lifecycleRequest; r.Operations = []string{OperationRollbackCoordinated}; return r }(),
	}
	for index, request := range widened {
		if _, err := issuer.Issue(t.Context(), request); err == nil {
			t.Fatalf("widened request %d was signed", index)
		}
	}
}
