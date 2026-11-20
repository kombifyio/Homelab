package executioncustody

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kombifyio/homelab/internal/gocommon/servicecall"
	wire "github.com/kombifyio/stackkits/contracts/advancedcapability"

	"github.com/kombifyio/homelab/internal/advancedissuer"
)

// The delegated issuer path must forward the receiving scope of one step in the
// same recorded issuance request, and keep every other issuance body unchanged.
func TestRemoteIssueTransferReceiveForwardsExactScope(t *testing.T) {
	scope := wire.TransferReceive{CandidateSpecHash: "sha256:" + strings.Repeat("a", 64), SourceOwnerRef: "owner/local/00112233445566778899aabbccddeeff", Step: "import"}
	if _, err := (*Remote)(nil).IssueTransferReceive(context.Background(), Request{}, scope); !errors.Is(err, advancedissuer.ErrUnavailable) {
		t.Fatal("a delegated issuer without custody must fail closed")
	}
	if _, err := (*Remote)(nil).Issue(context.Background(), Request{}); !errors.Is(err, advancedissuer.ErrUnavailable) {
		t.Fatal("a delegated issuer without custody must fail closed on any issuance")
	}
	var body []byte
	var other []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "TransferReceive") {
			body = raw
		} else {
			other = raw
		}
		_, _ = w.Write([]byte(`{"ID":"cap-1","IssuerID":"homelab","Operations":["application-transfer.receive"]}`))
	}))
	defer server.Close()
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &Remote{endpoint: server.URL, auth: servicecall.Config{ServiceName: "homelab-core", SigningKey: key, SigningKeyID: "caller-1", Scope: remoteScope, TokenTTL: time.Minute}, http: server.Client()}
	issued, err := client.IssueTransferReceive(t.Context(), Request{TenantID: "tenant-1", DeploymentID: "destination-1", StackID: "destination-stack", OwnerRef: "owner/local/ffeeddccbbaa99887766554433221100", Operations: []string{wire.OperationApplicationTransferReceive}}, scope)
	if err != nil {
		t.Fatal(err)
	}
	if issued.ID != "cap-1" {
		t.Fatal("the delegated issuer response is not the issued capability")
	}
	var forwarded struct {
		TransferReceive *wire.TransferReceive
		Operations      []string
	}
	if json.Unmarshal(body, &forwarded) != nil || forwarded.TransferReceive == nil || *forwarded.TransferReceive != scope {
		t.Fatal("the receiving scope did not reach the issuer that holds the key")
	}
	if len(forwarded.Operations) != 1 || forwarded.Operations[0] != wire.OperationApplicationTransferReceive {
		t.Fatal("the forwarded step grants another operation")
	}
	if _, err := client.Issue(t.Context(), Request{TenantID: "tenant-1", DeploymentID: "destination-1", StackID: "destination-stack", OwnerRef: "owner/local/ffeeddccbbaa99887766554433221100", Operations: []string{wire.OperationRollbackCoordinated}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(other), "TransferReceive") {
		t.Fatal("another issuance now names the receiving scope")
	}
}
