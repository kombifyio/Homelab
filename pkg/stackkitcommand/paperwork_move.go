package stackkitcommand

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"

	"github.com/kombifyio/homelab/pkg/api/agentpb"
)

// PaperworkMoveEvidence is the closed, secret-free status the node may return.
// The Wallet item binds it to the dispatched encrypted admission.
type PaperworkMoveEvidence struct {
	Format    string                `json:"format"`
	MoveID    string                `json:"move_id"`
	PinSHA256 string                `json:"pin_sha256"`
	ItemRef   string                `json:"wallet_item_ref"`
	TenantRef string                `json:"tenant_ref"`
	State     string                `json:"state"`
	Result    *PaperworkMoveReceipt `json:"result,omitempty"`
}

type PaperworkMoveReceipt struct {
	Kind          string `json:"kind"`
	State         string `json:"state"`
	ReceiptSHA256 string `json:"receipt_sha256"`
	Manifest      *struct {
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	} `json:"manifest"`
	Tables int   `json:"tables,omitempty"`
	Rows   int64 `json:"rows,omitempty"`
	Blobs  int   `json:"blobs,omitempty"`
}

var paperworkMoveIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func ParsePaperworkMoveEvidence(command *agentpb.StackKitCommand, result *agentpb.StackKitResult) (PaperworkMoveEvidence, error) {
	var invalid PaperworkMoveEvidence
	if command == nil || result == nil || !result.Success || command.Operation != agentpb.StackKitOperation_STACKKIT_OPERATION_WALLET_ITEM_IMPORT || command.WorkloadRef != "paperwork-move" || len(result.EventsJsonl) != 0 {
		return invalid, fmt.Errorf("successful closed Paperwork move evidence is required")
	}
	var envelope struct {
		SchemaVersion string `json:"schemaVersion"`
		Command       string `json:"command"`
		Status        string `json:"status"`
		Data          struct {
			Move PaperworkMoveEvidence `json:"paperwork_move"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(bytes.NewReader(result.CommandResultJson))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&envelope) != nil || decoder.Decode(new(any)) != io.EOF || envelope.SchemaVersion != CommandResultVersion || envelope.Command != walletItemDeliveryCommandName(command.Operation) || envelope.Status != commandResultStatusSuccess {
		return invalid, fmt.Errorf("paperwork move envelope is invalid")
	}
	move := envelope.Data.Move
	if move.Format != "kombify-paperwork-node-move-status/1" || !paperworkMoveIDPattern.MatchString(move.MoveID) || !sha256Pattern.MatchString(move.PinSHA256) || move.ItemRef != command.WalletItemRef || move.TenantRef != command.DeliveryTenantRef || move.State != "verified" || move.Result == nil {
		return invalid, fmt.Errorf("paperwork move status differs from the dispatched admission")
	}
	receipt := move.Result
	if receipt.Kind != "result" || receipt.State != "verified" || !sha256Pattern.MatchString(receipt.ReceiptSHA256) || receipt.Manifest == nil || !sha256Pattern.MatchString(receipt.Manifest.SHA256) || receipt.Manifest.Size < 1 || receipt.Manifest.Size > 32<<20 || receipt.Tables < 0 || receipt.Rows < 0 || receipt.Blobs < 0 {
		return invalid, fmt.Errorf("paperwork move receipt is invalid")
	}
	return move, nil
}
