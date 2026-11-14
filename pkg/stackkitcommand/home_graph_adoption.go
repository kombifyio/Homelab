package stackkitcommand

import (
	"encoding/json"
	"fmt"
	"google.golang.org/protobuf/proto"
	"reflect"
	"regexp"

	"github.com/kombifyio/techstack/pkg/api/agentpb"
)

const HomeGraphAdoptionOperationName = "application.home-graph.adopt"
const HomeGraphAdoptionRequestSchema = "stackkit.home-graph-adoption-request/v1"
const HomeGraphAdoptionResultSchema = "stackkit.home-graph-adoption-result/v1"

var adoptionDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type HomeGraphAdoptionDecision struct {
	ConflictID string `json:"conflictId"`
	Side       string `json:"side"`
}

type HomeGraphAdoptionRequest struct {
	SchemaVersion string                      `json:"schemaVersion"`
	Mode          string                      `json:"mode"`
	Graph         json.RawMessage             `json:"graph"`
	PlanDigest    string                      `json:"planDigest,omitempty"`
	DecideAll     string                      `json:"decideAll,omitempty"`
	Decisions     []HomeGraphAdoptionDecision `json:"decisions,omitempty"`
}

func DecodeHomeGraphAdoptionRequest(raw []byte) (HomeGraphAdoptionRequest, HomeGraphHeader, error) {
	var r HomeGraphAdoptionRequest
	var h HomeGraphHeader
	if len(raw) == 0 || len(raw) > MaxHomeGraphSyncBytes || decodeConnectorJSON(raw, &r) != nil || r.SchemaVersion != HomeGraphAdoptionRequestSchema {
		return r, h, fmt.Errorf("invalid adoption request")
	}
	h, err := DecodeHomeGraphDocument(r.Graph)
	if err != nil {
		return r, h, err
	}
	switch r.Mode {
	case "inventory":
		if r.PlanDigest != "" || r.DecideAll != "" || len(r.Decisions) != 0 {
			return r, h, fmt.Errorf("inventory carries no decisions")
		}
	case "apply":
		if !adoptionDigest.MatchString(r.PlanDigest) || (r.DecideAll != "" && r.DecideAll != "kombify" && r.DecideAll != "home_assistant") || len(r.Decisions) > 4096 {
			return r, h, fmt.Errorf("invalid adoption decisions")
		}
		seen := map[string]bool{}
		for _, d := range r.Decisions {
			if d.ConflictID == "" || len(d.ConflictID) > 512 || seen[d.ConflictID] || (d.Side != "kombify" && d.Side != "home_assistant") {
				return r, h, fmt.Errorf("invalid adoption decision")
			}
			seen[d.ConflictID] = true
		}
	default:
		return r, h, fmt.Errorf("invalid adoption mode")
	}
	return r, h, nil
}

func IsHomeGraphAdoptionOperation(op agentpb.StackKitOperation) bool {
	return op == agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_HOME_GRAPH_ADOPT
}

func validateHomeGraphAdoptionCommand(c *agentpb.StackKitCommand) error {
	if _, _, err := DecodeHomeGraphAdoptionRequest(c.HomeGraphAdoptionRequest); err != nil {
		return err
	}
	// Reuse the connector/workspace guard with a validated sync envelope. The
	// adoption document has its own closed decoder above and its own wire field.
	r, _, _ := DecodeHomeGraphAdoptionRequest(c.HomeGraphAdoptionRequest)
	clone := proto.Clone(c).(*agentpb.StackKitCommand)
	clone.HomeGraphSyncRequest, _ = EncodeHomeGraphSyncRequest(r.Graph, nil)
	if c.OwnerApproved || len(c.HomeGraphDocument) != 0 || len(c.HomeGraphSyncRequest) != 0 {
		return fmt.Errorf("adoption carries only its confirmed plan")
	}
	return validateHomeGraphSyncCommand(clone)
}

type HomeGraphAdoptionResult struct {
	SchemaVersion     string             `json:"schemaVersion"`
	Mode              string             `json:"mode"`
	Status            string             `json:"status"`
	Revision          int64              `json:"revision"`
	LeadingSystem     string             `json:"leadingSystem"`
	FirstRun          bool               `json:"firstRun"`
	PlanDigest        string             `json:"planDigest"`
	Matched           []json.RawMessage  `json:"matched"`
	OnlyKombify       []json.RawMessage  `json:"onlyKombify"`
	OnlyHomeAssistant []json.RawMessage  `json:"onlyHomeAssistant"`
	Conflicts         []json.RawMessage  `json:"conflicts"`
	Truncated         bool               `json:"truncated"`
	Graph             json.RawMessage    `json:"graph"`
	Operations        int                `json:"operations"`
	Links             HomeGraphLinks     `json:"links"`
	Drift             []HomeGraphDrift   `json:"drift"`
	Backup            json.RawMessage    `json:"backup"`
	Inventory         HomeGraphInventory `json:"inventory"`
	SyncedAt          string             `json:"syncedAt"`
}

func ParseHomeGraphAdoptionResult(c *agentpb.StackKitCommand, result *agentpb.StackKitResult) (HomeGraphAdoptionResult, error) {
	var out HomeGraphAdoptionResult
	if c == nil || result == nil || !result.Success || !IsHomeGraphAdoptionOperation(c.Operation) {
		return out, fmt.Errorf("invalid adoption result")
	}
	req, h, err := DecodeHomeGraphAdoptionRequest(c.HomeGraphAdoptionRequest)
	if err != nil {
		return out, err
	}
	var envelope struct {
		SchemaVersion string          `json:"schemaVersion"`
		Command       string          `json:"command"`
		Status        string          `json:"status"`
		Data          json.RawMessage `json:"data"`
	}
	if len(result.CommandResultJson) > 2<<20 || json.Unmarshal(result.CommandResultJson, &envelope) != nil || envelope.SchemaVersion != CommandResultVersion || envelope.Command != ResultCommandName(c.Operation) || envelope.Status != "success" || decodeConnectorJSON(envelope.Data, &out) != nil {
		return out, fmt.Errorf("invalid adoption result envelope")
	}
	if out.SchemaVersion != HomeGraphAdoptionResultSchema || out.Mode != req.Mode || out.Revision != h.Revision || out.LeadingSystem != h.LeadingSystem || !adoptionDigest.MatchString(out.PlanDigest) || len(out.Conflicts) > 1024 || len(out.Matched) > 1024 || len(out.OnlyKombify) > 1024 || len(out.OnlyHomeAssistant) > 1024 || out.Operations < 0 {
		return out, fmt.Errorf("adoption result answers a different request")
	}
	if req.Mode == "inventory" {
		if out.Status != "planned" || out.Operations != 0 || (len(out.Graph) != 0 && string(out.Graph) != "null") {
			return out, fmt.Errorf("inventory must be observation only")
		}
	} else {
		merged, err := DecodeHomeGraphDocument(out.Graph)
		if err != nil || merged.SpaceID != h.SpaceID || merged.Revision != h.Revision || merged.LeadingSystem != h.LeadingSystem || out.PlanDigest != req.PlanDigest || len(out.Backup) == 0 || string(out.Backup) == "null" || (h.LeadingSystem == "kombify" && out.Status != "applied") || (h.LeadingSystem == "home_assistant" && out.Status != "mirrored") {
			return out, fmt.Errorf("adoption apply result is incomplete")
		}
	}
	return out, nil
}

// Leadership is chosen after inventory; every other graph value must still be current.
func SameHomeGraphContent(a, b json.RawMessage) bool {
	var left, right map[string]json.RawMessage
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	delete(left, "leading_system")
	delete(right, "leading_system")
	l, _ := json.Marshal(left)
	r, _ := json.Marshal(right)
	var lv, rv any
	_ = json.Unmarshal(l, &lv)
	_ = json.Unmarshal(r, &rv)
	return reflect.DeepEqual(lv, rv)
}

func HomeGraphAdoptionRefusal(result *agentpb.StackKitResult) string {
	if result == nil {
		return ""
	}
	var e struct {
		Data struct {
			SchemaVersion string `json:"schemaVersion"`
			ReasonCode    string `json:"reasonCode"`
		} `json:"data"`
	}
	if json.Unmarshal(result.CommandResultJson, &e) != nil || e.Data.SchemaVersion != "stackkit.actionable-error/v1" {
		return ""
	}
	switch e.Data.ReasonCode {
	case "home_graph_connector_unavailable", "home_graph_request_invalid", "home_graph_apply_failed", "home_graph_adoption_stale", "home_graph_backup_failed", "home_graph_adoption_pending":
		return e.Data.ReasonCode
	}
	return ""
}
