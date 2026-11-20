package stackkitcommand

import (
	"encoding/json"
	"fmt"

	"github.com/kombifyio/homelab/pkg/api/agentpb"
)

// The Loxone import is the typed Homelab side of the catalog operation
// application.home-graph.loxone-import. The node reads the Miniserver
// structure file read-only with the Miniserver credential it keeps in its own
// workspace (the dispatch never carries it) and answers the home graph merged
// with the Miniserver's rooms and controls as a proposal. Nothing is written
// to the Miniserver, so a re-run is safe.
const (
	HomeGraphLoxoneImportOperationName = "application.home-graph.loxone-import"
	HomeGraphLoxoneImportResultSchema  = "stackkit.loxone-import-result/v1"
)

// Stable failure codes of the StackKits loxone-import error path.
var HomeGraphLoxoneImportErrorCodes = []string{"loxone_miniserver_unavailable", "loxone_import_request_invalid"}

func IsHomeGraphLoxoneImportOperation(op agentpb.StackKitOperation) bool {
	return op == agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_HOME_GRAPH_LOXONE_IMPORT
}

// validateHomeGraphLoxoneImportCommand binds the import to the same node as the
// home graph sync: the active connector setup names the node, the graph is the
// only payload.
func validateHomeGraphLoxoneImportCommand(c *agentpb.StackKitCommand) error {
	r, err := DecodeConnectorRequest(c.ApplicationConnectorRequestJson)
	if err != nil {
		return err
	}
	if r.Connector.Action != "ensure" {
		return fmt.Errorf("loxone import binds the active connector (action ensure)")
	}
	if _, err = DecodeHomeGraphDocument(c.HomeGraphDocument); err != nil {
		return err
	}
	if c.ServiceKey != "smart-home" || c.StackkitInstanceId == "" || !commandIDPattern.MatchString(c.ApplicationOperationId) || c.ApplicationContainerId != "" || c.ApplicationPlanDigest != "" || c.ApplicationAction != "" || handoffFieldsPresent(c) || c.LogTail != 0 || c.LogCursor != "" || c.WorkloadRef != "" || c.DryRun || c.Offline || c.TargetRelease != "" || len(c.InventoryJson) != 0 || len(c.CandidateSpecJson) != 0 || len(c.SealedDelivery) != 0 || c.ExpectedPlanHash != "" || len(c.HomeGraphSyncRequest) != 0 || c.OwnerApproved {
		return fmt.Errorf("loxone import requires only exact workspace, connector binding and graph")
	}
	if err = validateLocalExecutionPlacement(c, "loxone import"); err != nil {
		return err
	}
	if r.Connector.Binding.Node != (ConnectorNode{c.LocalSiteRef, c.LocalNodeRef, c.LocalExecutionChannelRef}) {
		return fmt.Errorf("loxone import node differs from admitted node")
	}
	return nil
}

// HomeGraphLoxoneImportResult is stackkit.loxone-import-result/v1. Graph stays
// opaque: the shared schema owns its shape and Cloud stores it.
type HomeGraphLoxoneImportResult struct {
	SchemaVersion string          `json:"schemaVersion"`
	Graph         json.RawMessage `json:"graph"`
	RoomsAdded    int             `json:"roomsAdded"`
	DevicesAdded  int             `json:"devicesAdded"`
	ReadOnly      bool            `json:"readOnly"`
}

// HomeGraphLoxoneImportRefusal is the stable error code of a failed run, empty
// when the command-result carries none of the documented codes.
func HomeGraphLoxoneImportRefusal(result *agentpb.StackKitResult) string {
	if result == nil {
		return ""
	}
	var refusal struct {
		Data struct {
			SchemaVersion string `json:"schemaVersion"`
			ReasonCode    string `json:"reasonCode"`
		} `json:"data"`
	}
	if json.Unmarshal(result.CommandResultJson, &refusal) != nil || refusal.Data.SchemaVersion != "stackkit.actionable-error/v1" {
		return ""
	}
	for _, code := range HomeGraphLoxoneImportErrorCodes {
		if refusal.Data.ReasonCode == code {
			return code
		}
	}
	return ""
}

// ParseHomeGraphLoxoneImportResult relies on the exact pinned CLI's successful
// command-result. The merged graph must stay the sent space and revision.
func ParseHomeGraphLoxoneImportResult(c *agentpb.StackKitCommand, result *agentpb.StackKitResult) (HomeGraphLoxoneImportResult, error) {
	var out HomeGraphLoxoneImportResult
	if c == nil || result == nil || !IsHomeGraphLoxoneImportOperation(c.Operation) || !result.Success {
		return out, fmt.Errorf("loxone import result differs from the dispatched import")
	}
	sent, err := DecodeHomeGraphDocument(c.HomeGraphDocument)
	if err != nil {
		return out, err
	}
	var envelope struct {
		SchemaVersion string          `json:"schemaVersion"`
		Command       string          `json:"command"`
		Status        string          `json:"status"`
		Data          json.RawMessage `json:"data"`
	}
	if len(result.CommandResultJson) > MaxHomeGraphSyncBytes+4096 || json.Unmarshal(result.CommandResultJson, &envelope) != nil ||
		envelope.SchemaVersion != CommandResultVersion || envelope.Command != ResultCommandName(c.Operation) || envelope.Status != "success" {
		return out, fmt.Errorf("loxone import result envelope is invalid")
	}
	if err = decodeConnectorJSON(envelope.Data, &out); err != nil || out.SchemaVersion != HomeGraphLoxoneImportResultSchema || !out.ReadOnly ||
		out.RoomsAdded < 0 || out.RoomsAdded > 256 || out.DevicesAdded < 0 || out.DevicesAdded > 1024 {
		return HomeGraphLoxoneImportResult{}, fmt.Errorf("loxone import result is not one %s document", HomeGraphLoxoneImportResultSchema)
	}
	merged, err := DecodeHomeGraphDocument(out.Graph)
	if err != nil {
		return HomeGraphLoxoneImportResult{}, err
	}
	if merged.SpaceID != sent.SpaceID || merged.Revision != sent.Revision || merged.LeadingSystem != sent.LeadingSystem ||
		len(merged.Rooms) != len(sent.Rooms)+out.RoomsAdded || len(merged.Devices) != len(sent.Devices)+out.DevicesAdded {
		return HomeGraphLoxoneImportResult{}, fmt.Errorf("loxone import result answers a different graph")
	}
	return out, nil
}
