package stackkitcommand

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kombifyio/techstack/pkg/api/agentpb"
)

// BackupStatusEvidence is the closed reading of a successful `stackkit backup
// status`: whether the node's repository is ready and the state of the newest
// snapshot and staged-restore receipts. It carries no path, credential or
// repository detail; the node read them from its own custody and reports only
// these states.
type BackupStatusEvidence struct {
	RepositoryReady bool
	// Snapshot and StagedRestore are one of "recorded", "unverified" or
	// "clock-skew": "recorded" is an authenticated receipt, "unverified" none
	// (or one that could not be authenticated), "clock-skew" a receipt dated
	// after the node's clock.
	Snapshot      string
	StagedRestore string
}

// ParseBackupStatusEvidence admits the node's successful backup status result
// for the dispatched command. It never reads an unknown state as healthy: a
// state outside the closed set is "unverified".
func ParseBackupStatusEvidence(command *agentpb.StackKitCommand, result *agentpb.StackKitResult) (BackupStatusEvidence, error) {
	if command == nil || result == nil || !result.Success || command.Operation != agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_STATUS ||
		result.CommandId != command.CommandId {
		return BackupStatusEvidence{}, fmt.Errorf("bound successful StackKit backup status result is required")
	}
	var envelope struct {
		SchemaVersion string `json:"schemaVersion"`
		Command       string `json:"command"`
		Status        string `json:"status"`
		Data          struct {
			Ready   *bool `json:"ready"`
			History *struct {
				Snapshot struct {
					State string `json:"state"`
				} `json:"snapshot"`
				StagedRestore struct {
					State string `json:"state"`
				} `json:"stagedRestore"`
			} `json:"history"`
		} `json:"data"`
	}
	if len(result.CommandResultJson) == 0 || len(result.CommandResultJson) > maxResultBytes || json.Unmarshal(result.CommandResultJson, &envelope) != nil {
		return BackupStatusEvidence{}, fmt.Errorf("decode StackKit backup status evidence")
	}
	if envelope.SchemaVersion != CommandResultVersion || envelope.Command != ResultCommandName(command.Operation) || envelope.Status != "success" ||
		envelope.Data.Ready == nil || envelope.Data.History == nil {
		return BackupStatusEvidence{}, fmt.Errorf("StackKit backup status evidence identity is invalid")
	}
	return BackupStatusEvidence{
		RepositoryReady: *envelope.Data.Ready,
		Snapshot:        closedReceiptState(envelope.Data.History.Snapshot.State),
		StagedRestore:   closedReceiptState(envelope.Data.History.StagedRestore.State),
	}, nil
}

func closedReceiptState(value string) string {
	switch state := strings.TrimSpace(value); state {
	case "recorded", "clock-skew":
		return state
	default:
		return "unverified"
	}
}
