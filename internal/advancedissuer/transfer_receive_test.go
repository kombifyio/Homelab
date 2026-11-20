package advancedissuer

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"

	wire "github.com/kombifyio/stackkits/contracts/advancedcapability"
)

func TestTransferReceiveRequestKeepsOtherIssuanceBodiesUnchanged(t *testing.T) {
	other, err := json.Marshal(Request{TenantID: "tenant-1", DeploymentID: "deployment-1", StackID: "stack-1", OwnerRef: "owner/local/00112233445566778899aabbccddeeff", Operations: []string{OperationRollbackCoordinated}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(other), "TransferReceive") {
		t.Fatal("a request that grants another operation names the receive scope")
	}
	scope := wire.TransferReceive{CandidateSpecHash: "sha256:" + strings.Repeat("a", 64), SourceOwnerRef: "owner/local/00112233445566778899aabbccddeeff", Step: "prepare"}
	receive, err := json.Marshal(Request{TransferReceive: &scope})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(receive), `"TransferReceive":{"candidateSpecHash":"sha256:`) {
		t.Fatal("the forward receives no receiving scope")
	}
}

func TestTransferReceiveIssuanceRetainsExactScopeAndRejectsWidening(t *testing.T) {
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	recorder := &memoryRecorder{}
	issuer, err := New(Config{IssuerID: "homelab", PrivateKey: key, Recorder: recorder, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	scope := wire.TransferReceive{CandidateSpecHash: "sha256:" + strings.Repeat("a", 64), SourceOwnerRef: "owner/local/00112233445566778899aabbccddeeff", Step: "admit"}
	request := Request{TenantID: "tenant-1", DeploymentID: "destination-1", StackID: "destination-stack", OwnerRef: "owner/local/ffeeddccbbaa99887766554433221100", Operations: []string{OperationApplicationTransferReceive}, TTL: time.Duration(wire.TransferReceiveMaxLifetimeSeconds) * time.Second}
	issued, err := issuer.IssueTransferReceive(t.Context(), request, scope)
	if err != nil {
		t.Fatal(err)
	}
	document, err := wire.DecodeCapability(issued.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if document.TransferReceive == nil || *document.TransferReceive != scope {
		t.Fatal("issuer changed the admitted receiving scope")
	}
	if document.TransferRetirement != nil || document.BackupRenewal != nil || len(document.AllowedOperations) != 1 {
		t.Fatal("a receive capability carries another authority")
	}
	if err = document.VerifySignature(key.Public().(ed25519.PublicKey)); err != nil {
		t.Fatal(err)
	}
	if len(recorder.issuances) != 1 || recorder.issuances[0].CapabilityID != document.CapabilityID || recorder.issuances[0].DeploymentID != request.DeploymentID {
		t.Fatal("authority escaped without its existing issuance record")
	}
	retirement := wire.TransferRetirement{BaselineSpecHash: "sha256:" + strings.Repeat("b", 64), CandidateSpecHash: scope.CandidateSpecHash, OperationID: "transfer-1", OutcomeDigest: "sha256:" + strings.Repeat("c", 64), Role: "source", TransferID: "sha256:" + strings.Repeat("d", 64)}
	for name, change := range map[string]func(*Request){
		"scope omitted":   func(r *Request) { r.TransferReceive = nil },
		"other operation": func(r *Request) { r.Operations = []string{OperationRollbackCoordinated} },
		"both operations": func(r *Request) {
			r.Operations = []string{OperationApplicationTransferReceive, OperationApplicationTransferRetire}
		},
		"long lifetime":       func(r *Request) { r.TTL = 16 * time.Minute },
		"another step":        func(r *Request) { r.TransferReceive.Step = "serve" },
		"another source":      func(r *Request) { r.TransferReceive.SourceOwnerRef = "owner/local/not-a-local-owner" },
		"retirement combined": func(r *Request) { r.TransferRetirement = &retirement },
	} {
		t.Run(name, func(t *testing.T) {
			next := request
			copy := scope
			next.TransferReceive = &copy
			change(&next)
			if _, err := issuer.Issue(t.Context(), next); err == nil {
				t.Fatal("issuer accepted a widened receive grant")
			}
			if len(recorder.issuances) != 1 {
				t.Fatal("denied issuance changed the ledger")
			}
		})
	}
}
