package stackkitcommand

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kombifyio/homelab/pkg/api/agentpb"
)

// The home graph sync is the typed Homelab side of the catalog operation
// application.home-graph.sync. The node runs it against its own Home Assistant
// connector credential; the graph is public household structure, never a
// credential. The command is idempotent: a re-run of the same graph converges
// to the same Home Assistant state, so a lost acknowledgement is safe to repeat.
const (
	HomeGraphSyncOperationName = "application.home-graph.sync"
	HomeGraphSyncRequestSchema = "stackkit.home-graph-sync-request/v1"
	HomeGraphSyncResultSchema  = "stackkit.home-graph-sync-result/v1"
	HomeGraphSchemaID          = "kombify.home_graph.v1"
	MaxHomeGraphSyncBytes      = 512 << 10
	// MaxHomeGraphDocumentBytes caps the graph itself so the wrapped request
	// (schema, up to 256 restore items, about 20 KiB) always fits
	// MaxHomeGraphSyncBytes.
	MaxHomeGraphDocumentBytes = 480 << 10
	MaxHomeGraphRestoreItems  = 256

	maxHomeGraphResultLinks   = 4096
	maxHomeGraphResultDrift   = 256
	maxHomeGraphDriftRunes    = 255
	maxHomeGraphInventoryFlr  = 32
	maxHomeGraphInventoryArea = 256
)

// Stable failure codes of the StackKits command error path.
var HomeGraphErrorCodes = []string{"home_graph_connector_unavailable", "home_graph_request_invalid", "home_graph_apply_failed", "home_graph_adoption_pending"}

var (
	homeGraphSpacePattern = regexp.MustCompile(`^spc_[a-f0-9]{24}$`)
	homeGraphFloorID      = regexp.MustCompile(`^flr_[a-f0-9]{24}$`)
	homeGraphRoomID       = regexp.MustCompile(`^rom_[a-f0-9]{24}$`)
	homeGraphDeviceID     = regexp.MustCompile(`^hdv_[a-f0-9]{24}$`)
)

// HomeGraphRestoreItem is one drift item the owner chose to resolve with the
// kombify value.
type HomeGraphRestoreItem struct {
	Kind      string `json:"kind"`
	KombifyID string `json:"kombifyId"`
	Field     string `json:"field"`
}

// HomeGraphSyncRequest is stackkit.home-graph-sync-request/v1.
type HomeGraphSyncRequest struct {
	SchemaVersion string                 `json:"schemaVersion"`
	Graph         json.RawMessage        `json:"graph"`
	Restore       []HomeGraphRestoreItem `json:"restore"`
}

// HomeGraphHeader is the part of a kombify.home_graph.v1 document Homelab
// reads. The document itself stays opaque: the shared schema owns its shape.
type HomeGraphHeader struct {
	Schema        string            `json:"schema"`
	SpaceID       string            `json:"space_id"`
	Revision      int64             `json:"revision"`
	LeadingSystem string            `json:"leading_system"`
	Floors        []json.RawMessage `json:"floors"`
	Rooms         []json.RawMessage `json:"rooms"`
	Devices       []json.RawMessage `json:"devices"`
	Assignments   []json.RawMessage `json:"assignments"`
}

// DecodeHomeGraphDocument validates the bounded header of a home graph.
func DecodeHomeGraphDocument(raw []byte) (HomeGraphHeader, error) {
	var h HomeGraphHeader
	if len(raw) == 0 || len(raw) > MaxHomeGraphDocumentBytes || !json.Valid(raw) {
		return h, fmt.Errorf("home graph exceeds its bound or is not JSON")
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return h, fmt.Errorf("home graph is not a document")
	}
	if h.Schema != HomeGraphSchemaID || !homeGraphSpacePattern.MatchString(h.SpaceID) || h.Revision < 0 || h.Revision > 9007199254740991 ||
		(h.LeadingSystem != "kombify" && h.LeadingSystem != "home_assistant") ||
		len(h.Floors) > 32 || len(h.Rooms) > 256 || len(h.Devices) > 1024 || len(h.Assignments) > 4096 {
		return h, fmt.Errorf("home graph has no closed schema, space or revision binding")
	}
	return h, nil
}

// DedupeHomeGraphRestore drops repeated restore items, keeping the first
// occurrence and the order. A POST that names an item twice is not an error.
func DedupeHomeGraphRestore(items []HomeGraphRestoreItem) []HomeGraphRestoreItem {
	seen := make(map[HomeGraphRestoreItem]bool, len(items))
	out := make([]HomeGraphRestoreItem, 0, len(items))
	for _, item := range items {
		if !seen[item] {
			seen[item] = true
			out = append(out, item)
		}
	}
	return out
}

// ValidateHomeGraphRestore bounds the restore list: each item names one drift
// field of a floor, area (room) or device.
func ValidateHomeGraphRestore(items []HomeGraphRestoreItem) error {
	if len(items) > MaxHomeGraphRestoreItems {
		return fmt.Errorf("home graph restore list exceeds its bound")
	}
	seen := map[HomeGraphRestoreItem]bool{}
	for _, item := range items {
		var id *regexp.Regexp
		var fields []string
		switch item.Kind {
		case "floor":
			id, fields = homeGraphFloorID, []string{"name", "level"}
		case "area":
			id, fields = homeGraphRoomID, []string{"name", "floor"}
		case "device":
			id, fields = homeGraphDeviceID, []string{"area"}
		default:
			return fmt.Errorf("home graph restore item has an unknown kind")
		}
		ok := false
		for _, f := range fields {
			ok = ok || f == item.Field
		}
		if !ok || !id.MatchString(item.KombifyID) || seen[item] {
			return fmt.Errorf("home graph restore item is not closed")
		}
		seen[item] = true
	}
	return nil
}

// EncodeHomeGraphSyncRequest builds the exact request document, bounded.
func EncodeHomeGraphSyncRequest(graph json.RawMessage, restore []HomeGraphRestoreItem) ([]byte, error) {
	if _, err := DecodeHomeGraphDocument(graph); err != nil {
		return nil, err
	}
	if err := ValidateHomeGraphRestore(restore); err != nil {
		return nil, err
	}
	if restore == nil {
		restore = []HomeGraphRestoreItem{}
	}
	raw, err := json.Marshal(HomeGraphSyncRequest{SchemaVersion: HomeGraphSyncRequestSchema, Graph: graph, Restore: restore})
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxHomeGraphSyncBytes {
		return nil, fmt.Errorf("home graph sync request exceeds its bound")
	}
	return raw, nil
}

// DecodeHomeGraphSyncRequest strictly decodes and validates a request document.
func DecodeHomeGraphSyncRequest(raw []byte) (HomeGraphSyncRequest, HomeGraphHeader, error) {
	var r HomeGraphSyncRequest
	var h HomeGraphHeader
	if len(raw) == 0 || len(raw) > MaxHomeGraphSyncBytes {
		return r, h, fmt.Errorf("home graph sync request exceeds its bound")
	}
	if err := decodeConnectorJSON(raw, &r); err != nil || r.SchemaVersion != HomeGraphSyncRequestSchema {
		return r, h, fmt.Errorf("home graph sync request is not one %s document", HomeGraphSyncRequestSchema)
	}
	h, err := DecodeHomeGraphDocument(r.Graph)
	if err != nil {
		return r, h, err
	}
	return r, h, ValidateHomeGraphRestore(r.Restore)
}

func IsHomeGraphSyncOperation(op agentpb.StackKitOperation) bool {
	return op == agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_HOME_GRAPH_SYNC
}

func validateHomeGraphSyncCommand(c *agentpb.StackKitCommand) error {
	r, err := DecodeConnectorRequest(c.ApplicationConnectorRequestJson)
	if err != nil {
		return err
	}
	if r.Connector.Action != "ensure" {
		return fmt.Errorf("home graph sync binds the active connector (action ensure)")
	}
	if _, _, err = DecodeHomeGraphSyncRequest(c.HomeGraphSyncRequest); err != nil {
		return err
	}
	// OwnerApproved is carried: the owner's own write to the graph is the
	// approval, and the node still needs its own connector custody.
	if c.ServiceKey != "smart-home" || c.StackkitInstanceId == "" || !commandIDPattern.MatchString(c.ApplicationOperationId) || c.ApplicationContainerId != "" || c.ApplicationPlanDigest != "" || c.ApplicationAction != "" || handoffFieldsPresent(c) || c.LogTail != 0 || c.LogCursor != "" || c.WorkloadRef != "" || c.DryRun || c.Offline || c.TargetRelease != "" || len(c.InventoryJson) != 0 || len(c.CandidateSpecJson) != 0 || len(c.SealedDelivery) != 0 || c.ExpectedPlanHash != "" {
		return fmt.Errorf("home graph sync requires only exact workspace, connector binding and graph")
	}
	if err = validateLocalExecutionPlacement(c, "home graph sync"); err != nil {
		return err
	}
	if r.Connector.Binding.Node != (ConnectorNode{c.LocalSiteRef, c.LocalNodeRef, c.LocalExecutionChannelRef}) {
		return fmt.Errorf("home graph sync node differs from admitted node")
	}
	return nil
}

// HomeGraphSyncResult is stackkit.home-graph-sync-result/v1.
type HomeGraphSyncResult struct {
	SchemaVersion string             `json:"schemaVersion"`
	Revision      int64              `json:"revision"`
	LeadingSystem string             `json:"leadingSystem"`
	Status        string             `json:"status"`
	Operations    int                `json:"operations"`
	Links         HomeGraphLinks     `json:"links"`
	Drift         []HomeGraphDrift   `json:"drift"`
	Inventory     HomeGraphInventory `json:"inventory"`
	SyncedAt      time.Time          `json:"syncedAt"`
	Graph         json.RawMessage    `json:"graph"`
}

type HomeGraphLinks struct {
	Floors  map[string]string `json:"floors"`
	Areas   map[string]string `json:"areas"`
	Devices map[string]string `json:"devices"`
}

type HomeGraphDrift struct {
	Kind      string `json:"kind"`
	Field     string `json:"field"`
	KombifyID string `json:"kombifyId"`
	HAID      string `json:"haId"`
	Graph     string `json:"graph"`
	HA        string `json:"ha"`
}

type HomeGraphInventoryFloor struct {
	HAID  string `json:"haId"`
	Name  string `json:"name"`
	Level *int   `json:"level"`
}

type HomeGraphInventoryArea struct {
	HAID      string `json:"haId"`
	Name      string `json:"name"`
	FloorHAID string `json:"floorHaId"`
}

type HomeGraphInventory struct {
	Floors          []HomeGraphInventoryFloor `json:"floors"`
	Areas           []HomeGraphInventoryArea  `json:"areas"`
	UnmappedDevices int                       `json:"unmappedDevices"`
}

func homeGraphText(v string, max int) bool {
	return len(v) <= max && !strings.ContainsAny(v, "\x00\r\n")
}

// validHomeGraphDrift checks one drift item against its own closed schema: the
// node CLI emits kind floor|area|device, a field of the drift enum, a kombify id
// of the kind's pattern (empty for an object without a mapping) and string
// values clipped to 255 runes.
func validHomeGraphDrift(d HomeGraphDrift) bool {
	var id *regexp.Regexp
	switch d.Kind {
	case "floor":
		id = homeGraphFloorID
	case "area":
		id = homeGraphRoomID
	case "device":
		id = homeGraphDeviceID
	default:
		return false
	}
	switch d.Field {
	case "name", "level", "floor", "area", "deleted", "name_collision":
	default:
		return false
	}
	if d.KombifyID != "" && !id.MatchString(d.KombifyID) {
		return false
	}
	for _, v := range []string{d.HAID, d.Graph, d.HA} {
		if utf8.RuneCountInString(v) > maxHomeGraphDriftRunes || !utf8.ValidString(v) || strings.ContainsRune(v, 0) {
			return false
		}
	}
	return true
}

// Validate re-checks a decoded result against the closed C1 shape and its bounds.
func (r HomeGraphSyncResult) Validate() error {
	if r.SchemaVersion != HomeGraphSyncResultSchema || r.Revision < 0 || r.Revision > 9007199254740991 || r.Operations < 0 || r.Operations > 1<<20 ||
		(r.LeadingSystem != "kombify" && r.LeadingSystem != "home_assistant") ||
		(r.LeadingSystem == "kombify" && r.Status != "applied") || (r.Status != "applied" && r.Status != "observed") || r.SyncedAt.IsZero() ||
		(r.Status == "observed" && r.Operations != 0) {
		return fmt.Errorf("home graph result has no closed identity")
	}
	checkLinks := func(m map[string]string, id *regexp.Regexp) bool {
		if len(m) > maxHomeGraphResultLinks {
			return false
		}
		for k, v := range m {
			if !id.MatchString(k) || v == "" || !homeGraphText(v, 256) {
				return false
			}
		}
		return true
	}
	if !checkLinks(r.Links.Floors, homeGraphFloorID) || !checkLinks(r.Links.Areas, homeGraphRoomID) || !checkLinks(r.Links.Devices, homeGraphDeviceID) {
		return fmt.Errorf("home graph result links are not closed")
	}
	if len(r.Drift) > maxHomeGraphResultDrift {
		return fmt.Errorf("home graph result drift exceeds its bound")
	}
	for _, d := range r.Drift {
		if !validHomeGraphDrift(d) {
			return fmt.Errorf("home graph result drift item is not closed")
		}
	}
	inv := r.Inventory
	if len(inv.Floors) > maxHomeGraphInventoryFlr || len(inv.Areas) > maxHomeGraphInventoryArea || inv.UnmappedDevices < 0 || inv.UnmappedDevices > 1<<20 {
		return fmt.Errorf("home graph result inventory exceeds its bound")
	}
	for _, f := range inv.Floors {
		if f.HAID == "" || !homeGraphText(f.HAID, 256) || !homeGraphText(f.Name, 256) {
			return fmt.Errorf("home graph result inventory is not closed")
		}
	}
	for _, a := range inv.Areas {
		if a.HAID == "" || !homeGraphText(a.HAID, 256) || !homeGraphText(a.Name, 256) || !homeGraphText(a.FloorHAID, 256) {
			return fmt.Errorf("home graph result inventory is not closed")
		}
	}
	return nil
}

// HomeGraphRefusal is the stable error code of a failed run, empty when the
// command-result carries none of the documented codes.
func HomeGraphRefusal(result *agentpb.StackKitResult) string {
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
	for _, code := range HomeGraphErrorCodes {
		if refusal.Data.ReasonCode == code {
			return code
		}
	}
	return ""
}

// ParseHomeGraphSyncResult relies on the exact pinned CLI's successful
// command-result. The result must answer the graph revision that was sent.
func ParseHomeGraphSyncResult(c *agentpb.StackKitCommand, result *agentpb.StackKitResult) (HomeGraphSyncResult, error) {
	var out HomeGraphSyncResult
	if c == nil || result == nil || !IsHomeGraphSyncOperation(c.Operation) || !result.Success {
		return out, fmt.Errorf("home graph result differs from the dispatched sync")
	}
	_, header, err := DecodeHomeGraphSyncRequest(c.HomeGraphSyncRequest)
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
		return out, fmt.Errorf("home graph result envelope is invalid")
	}
	if err = decodeConnectorJSON(envelope.Data, &out); err != nil {
		return HomeGraphSyncResult{}, fmt.Errorf("home graph result is not one %s document", HomeGraphSyncResultSchema)
	}
	if err = out.Validate(); err != nil {
		return HomeGraphSyncResult{}, err
	}
	if out.LeadingSystem == "home_assistant" {
		merged, err := DecodeHomeGraphDocument(out.Graph)
		if err != nil || merged.SpaceID != header.SpaceID || merged.Revision != header.Revision || merged.LeadingSystem != header.LeadingSystem {
			return out, fmt.Errorf("invalid mirrored graph")
		}
	}
	if out.Revision != header.Revision || out.LeadingSystem != header.LeadingSystem {
		return HomeGraphSyncResult{}, fmt.Errorf("home graph result answers a different graph")
	}
	return out, nil
}

// MarshalHomeGraphSyncResult is the canonical stored form of a validated result.
func MarshalHomeGraphSyncResult(r HomeGraphSyncResult) ([]byte, error) {
	if r.Links.Floors == nil {
		r.Links.Floors = map[string]string{}
	}
	if r.Links.Areas == nil {
		r.Links.Areas = map[string]string{}
	}
	if r.Links.Devices == nil {
		r.Links.Devices = map[string]string{}
	}
	if r.Drift == nil {
		r.Drift = []HomeGraphDrift{}
	}
	if r.Inventory.Floors == nil {
		r.Inventory.Floors = []HomeGraphInventoryFloor{}
	}
	if r.Inventory.Areas == nil {
		r.Inventory.Areas = []HomeGraphInventoryArea{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}
