package routes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/kombifyio/homelab/internal/gocommon/servicecall"

	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/logger"
	"github.com/kombifyio/homelab/pkg/orchestrator"
	"github.com/kombifyio/homelab/pkg/stackkitcommand"
)

// The home graph route is the Cloud to Homelab half of the home graph sync
// (HOME-GRAPH-TARGET.md, D-2026-11-09-03): Cloud stores the household's graph
// and Homelab keeps the latest one per owner and space, projects it through
// the node's Home Assistant connector (application_home_graph_sync) and reports
// the sync state back. Homelab never holds the connector credential.
const (
	householdHomeGraphPath = "/api/v1/internal/household/home-graph"

	// HouseholdHomeGraphScope is the scope the Cloud service token must carry
	// for both calls.
	HouseholdHomeGraphScope = "household:home_graph"

	homeGraphBodyLimit = stackkitcommand.MaxHomeGraphSyncBytes + 64<<10
	// HomeGraphSyncStatusSchema names the GET response.
	HomeGraphSyncStatusSchema = "kombify.home_graph.sync_status.v1"
)

// HomeGraphSyncer enqueues the sync run of one stored space. The orchestrator
// implements it; a nil value stores the graph and leaves the run to the
// reconcile loop.
type HomeGraphSyncer interface {
	EnqueueHomeGraphSync(ctx context.Context, tenantID, ownerID, spaceID string) (string, error)
	HasHomeGraphConnector(ctx context.Context, tenantID, ownerID string) bool
}

type homeGraphBody struct {
	OwnerSubject string                                 `json:"owner_subject"`
	SpaceID      string                                 `json:"space_id"`
	Graph        json.RawMessage                        `json:"graph"`
	Restore      []stackkitcommand.HomeGraphRestoreItem `json:"restore"`
}

func (h householdHandlers) ownerTenant(e *httpx.Event, caller *servicecall.Caller, owner string) (string, bool) {
	sub, org := onBehalfOf(caller)
	if sub == "" || owner == "" || sub != owner {
		_ = householdSurface.deny(e, http.StatusForbidden, "principal_mismatch", false,
			"Read and write the home graph on behalf of the homelab owner.")
		return "", false
	}
	if h.svc == nil || h.svc.Tenants == nil || h.graphs == nil {
		_ = householdSurface.deny(e, http.StatusServiceUnavailable, householdSurface.unavailableReason, true, "Retry later.")
		return "", false
	}
	tenantID, err := h.svc.Tenants.ResolveOwnerTenant(e.Request.Context(), org, owner)
	if err != nil {
		h.writeError(e, err)
		return "", false
	}
	return tenantID, true
}

func (h householdHandlers) homeGraphPut(e *httpx.Event) error {
	return h.serve(e, HouseholdHomeGraphScope, func(e *httpx.Event, caller *servicecall.Caller) {
		raw, err := io.ReadAll(io.LimitReader(e.Request.Body, homeGraphBodyLimit+1))
		if err != nil || len(raw) > homeGraphBodyLimit {
			_ = householdSurface.deny(e, http.StatusRequestEntityTooLarge, "body_too_large", false, "Send a smaller request.")
			return
		}
		var body homeGraphBody
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF {
			_ = householdSurface.deny(e, http.StatusBadRequest, "body_invalid", false, "Send a valid JSON body with the documented fields.")
			return
		}
		body.OwnerSubject = strings.TrimSpace(body.OwnerSubject)
		if len(body.Graph) > stackkitcommand.MaxHomeGraphDocumentBytes {
			_ = householdSurface.deny(e, http.StatusRequestEntityTooLarge, "body_too_large", false, "Send a smaller request.")
			return
		}
		header, err := stackkitcommand.DecodeHomeGraphDocument(body.Graph)
		if err != nil || header.SpaceID != strings.TrimSpace(body.SpaceID) {
			_ = householdSurface.deny(e, http.StatusBadRequest, "home_graph_invalid", false,
				"Send a kombify.home_graph.v1 document within its bounds for the named space.")
			return
		}
		body.Restore = stackkitcommand.DedupeHomeGraphRestore(body.Restore)
		if err = stackkitcommand.ValidateHomeGraphRestore(body.Restore); err != nil {
			_ = householdSurface.deny(e, http.StatusBadRequest, "home_graph_restore_invalid", false,
				"Send at most 256 restore items of kind floor, area or device with a known field.")
			return
		}
		tenantID, ok := h.ownerTenant(e, caller, body.OwnerSubject)
		if !ok {
			return
		}
		restore, _ := json.Marshal(body.Restore)
		ctx := e.Request.Context()
		stored, outcome, err := h.graphs.PutHomeGraph(ctx, tenantID, body.OwnerSubject, header.SpaceID, body.Graph, header.Revision, restore)
		if err != nil {
			h.writeError(e, err)
			return
		}
		if outcome == controlplane.HomeGraphPutStale {
			// An older revision never replaces the stored graph.
			// The answer is the contract's "accepted" with the stored revision.
			_ = e.JSON(http.StatusAccepted, map[string]any{"status": "accepted", "revision": stored.GraphRevision})
			return
		}
		if outcome == controlplane.HomeGraphPutUnchanged {
			// The same revision again (Cloud may push it twice): nothing new,
			// nothing enqueued, and the same answer as the first call.
			if h.sync != nil && !h.sync.HasHomeGraphConnector(ctx, tenantID, body.OwnerSubject) {
				_ = e.JSON(http.StatusOK, map[string]any{"status": "none"})
				return
			}
			_ = e.JSON(http.StatusAccepted, map[string]any{"status": "accepted", "revision": stored.GraphRevision})
			return
		}
		if h.sync == nil {
			_ = e.JSON(http.StatusAccepted, map[string]any{"status": "accepted", "revision": stored.GraphRevision})
			return
		}
		// The run belongs to the owner's node and must not stop because the
		// caller stopped waiting; the stored state is already durable.
		_, enqueueErr := h.sync.EnqueueHomeGraphSync(context.WithoutCancel(ctx), tenantID, body.OwnerSubject, header.SpaceID)
		switch {
		case errors.Is(enqueueErr, orchestrator.ErrHomeGraphNoConnector):
			_ = e.JSON(http.StatusOK, map[string]any{"status": "none"})
		default:
			if enqueueErr != nil && !errors.Is(enqueueErr, orchestrator.ErrHomeGraphFenced) {
				logger.Default().Warn("home_graph_sync_enqueue_deferred",
					"request_id", e.Request.Header.Get("X-Request-ID"), "error", enqueueErr.Error())
			}
			// Fenced or deferred: the reconcile loop runs it after the next
			// Guard inventory.
			_ = e.JSON(http.StatusAccepted, map[string]any{"status": "accepted", "revision": stored.GraphRevision})
		}
	})
}

func (h householdHandlers) homeGraphGet(e *httpx.Event) error {
	return h.serve(e, HouseholdHomeGraphScope, func(e *httpx.Event, caller *servicecall.Caller) {
		query := e.Request.URL.Query()
		owner := strings.TrimSpace(query.Get("owner_subject"))
		space := strings.TrimSpace(query.Get("space_id"))
		if owner == "" || space == "" || len(query) != 2 {
			_ = householdSurface.deny(e, http.StatusBadRequest, "home_graph_query_invalid", false, "Send exactly owner_subject and space_id.")
			return
		}
		tenantID, ok := h.ownerTenant(e, caller, owner)
		if !ok {
			return
		}
		ctx := e.Request.Context()
		state, err := h.graphs.GetHomeGraph(ctx, tenantID, owner, space)
		if errors.Is(err, controlplane.ErrNotFound) {
			_ = e.JSON(http.StatusOK, homeGraphStatusNone())
			return
		}
		if err != nil {
			h.writeError(e, err)
			return
		}
		if h.sync != nil && !h.sync.HasHomeGraphConnector(ctx, tenantID, owner) {
			none := homeGraphStatusNone()
			none["graph_revision"] = state.GraphRevision
			_ = e.JSON(http.StatusOK, none)
			return
		}
		_ = e.JSON(http.StatusOK, homeGraphStatusWire(state))
	})
}

func homeGraphStatusNone() map[string]any {
	return map[string]any{
		"schema": HomeGraphSyncStatusSchema, "status": "none", "graph_revision": nil, "synced_revision": nil,
		"synced_at": nil, "leading_system": nil, "operations": nil, "drift": []any{}, "inventory": nil, "error_code": nil, "graph": nil,
	}
}

// homeGraphStatusWire is the exact C2 GET shape. Drift, inventory, operations
// and the leading system come from the stored, validated node result.
func homeGraphStatusWire(state *controlplane.HomeGraphState) map[string]any {
	out := homeGraphStatusNone()
	status := state.Status
	var restore []json.RawMessage
	_ = json.Unmarshal(state.Restore, &restore)
	if status == controlplane.HomeGraphStatusSynced && (state.GraphRevision > state.SyncedRevision || len(restore) > 0) {
		status = controlplane.HomeGraphStatusPending
	}
	out["status"], out["graph_revision"] = status, state.GraphRevision
	if state.SyncedRevision >= 0 {
		out["synced_revision"] = state.SyncedRevision
	}
	if state.SyncedAt != nil {
		out["synced_at"] = state.SyncedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	if state.ErrorCode != "" {
		out["error_code"] = state.ErrorCode
	}
	if header, err := stackkitcommand.DecodeHomeGraphDocument(state.Graph); err == nil {
		out["leading_system"] = header.LeadingSystem
	}
	if len(state.Result) != 0 {
		var result stackkitcommand.HomeGraphSyncResult
		if json.Unmarshal(state.Result, &result) == nil && result.Validate() == nil {
			out["leading_system"], out["operations"] = result.LeadingSystem, result.Operations
			out["drift"], out["inventory"], out["graph"] = result.Drift, result.Inventory, result.Graph
		}
	}
	return out
}
