package routes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/kombifyio/homelab/internal/gocommon/servicecall"

	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/orchestrator"
)

// The Loxone import route lets Cloud run `stackkit setup loxone-import` on the
// owner's node (the node of the home graph sync) and read the merged home
// graph back as a proposal. Cloud stores the proposal in the household home
// graph with a new revision (and sends it here again through the home graph
// route); Homelab never replaces the stored graph with it. The Miniserver
// credential stays on the node and never passes this route.
const (
	householdHomeGraphLoxoneImportPath = "/api/v1/internal/household/home-graph/loxone-import"

	// HomeGraphLoxoneImportSchema names the GET response.
	HomeGraphLoxoneImportSchema = "kombify.home_graph.loxone_import.v1"
)

var homeGraphLoxoneSpace = regexp.MustCompile(`^spc_[a-f0-9]{24}$`)

// HomeGraphLoxoneImporter starts and reads Loxone import runs. The
// orchestrator implements it; without it the route answers 503.
type HomeGraphLoxoneImporter interface {
	EnqueueHomeGraphLoxoneImport(ctx context.Context, tenantID, ownerID, spaceID string) (string, error)
	HomeGraphLoxoneImportStatus(ctx context.Context, tenantID, ownerID, spaceID, jobID string) (*orchestrator.HomeGraphLoxoneImport, error)
}

type homeGraphLoxoneImportBody struct {
	OwnerSubject string `json:"owner_subject"`
	SpaceID      string `json:"space_id"`
}

func (h householdHandlers) loxoneImporter(e *httpx.Event) (HomeGraphLoxoneImporter, bool) {
	importer, ok := h.sync.(HomeGraphLoxoneImporter)
	if !ok || importer == nil {
		_ = householdSurface.deny(e, http.StatusServiceUnavailable, householdSurface.unavailableReason, true, "Retry later.")
		return nil, false
	}
	return importer, true
}

func (h householdHandlers) homeGraphLoxoneImportStart(e *httpx.Event) error {
	return h.serve(e, HouseholdHomeGraphScope, func(e *httpx.Event, caller *servicecall.Caller) {
		raw, err := io.ReadAll(io.LimitReader(e.Request.Body, 4<<10+1))
		if err != nil || len(raw) > 4<<10 {
			_ = householdSurface.deny(e, http.StatusRequestEntityTooLarge, "body_too_large", false, "Send a smaller request.")
			return
		}
		var body homeGraphLoxoneImportBody
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF {
			_ = householdSurface.deny(e, http.StatusBadRequest, "body_invalid", false, "Send a valid JSON body with the documented fields.")
			return
		}
		body.OwnerSubject, body.SpaceID = strings.TrimSpace(body.OwnerSubject), strings.TrimSpace(body.SpaceID)
		if !homeGraphLoxoneSpace.MatchString(body.SpaceID) {
			_ = householdSurface.deny(e, http.StatusBadRequest, "home_graph_space_invalid", false, "Send the space_id of a stored home graph.")
			return
		}
		tenantID, ok := h.ownerTenant(e, caller, body.OwnerSubject)
		if !ok {
			return
		}
		importer, ok := h.loxoneImporter(e)
		if !ok {
			return
		}
		ctx := e.Request.Context()
		// The run belongs to the owner's node and must not stop because the
		// caller stopped waiting.
		jobID, err := importer.EnqueueHomeGraphLoxoneImport(context.WithoutCancel(ctx), tenantID, body.OwnerSubject, body.SpaceID)
		switch {
		case err == nil:
			_ = e.JSON(http.StatusAccepted, map[string]any{"status": "accepted", "job_id": jobID})
		case errors.Is(err, orchestrator.ErrHomeGraphNoConnector):
			_ = e.JSON(http.StatusOK, map[string]any{"status": "none"})
		case errors.Is(err, controlplane.ErrNotFound):
			_ = householdSurface.deny(e, http.StatusNotFound, "home_graph_not_found", false,
				"Store the home graph of the space first, then start the Loxone import.")
		case errors.Is(err, orchestrator.ErrHomeGraphFenced):
			_ = householdSurface.deny(e, http.StatusConflict, "home_graph_node_busy", true,
				"Another operation runs on the node; start the Loxone import again later.")
		default:
			h.writeError(e, err)
		}
	})
}

func (h householdHandlers) homeGraphLoxoneImportGet(e *httpx.Event) error {
	return h.serve(e, HouseholdHomeGraphScope, func(e *httpx.Event, caller *servicecall.Caller) {
		query := e.Request.URL.Query()
		owner := strings.TrimSpace(query.Get("owner_subject"))
		space := strings.TrimSpace(query.Get("space_id"))
		jobID := strings.TrimSpace(query.Get("job_id"))
		if owner == "" || !homeGraphLoxoneSpace.MatchString(space) || jobID == "" || len(jobID) > 256 || len(query) != 3 {
			_ = householdSurface.deny(e, http.StatusBadRequest, "home_graph_query_invalid", false, "Send exactly owner_subject, space_id and job_id.")
			return
		}
		tenantID, ok := h.ownerTenant(e, caller, owner)
		if !ok {
			return
		}
		importer, ok := h.loxoneImporter(e)
		if !ok {
			return
		}
		run, err := importer.HomeGraphLoxoneImportStatus(e.Request.Context(), tenantID, owner, space, jobID)
		if errors.Is(err, controlplane.ErrNotFound) {
			_ = householdSurface.deny(e, http.StatusNotFound, "loxone_import_not_found", false, "Send the job_id the import start returned.")
			return
		}
		if err != nil {
			h.writeError(e, err)
			return
		}
		out := map[string]any{
			"schema": HomeGraphLoxoneImportSchema, "job_id": run.JobID, "status": run.Status, "error_code": nil,
			"graph_revision": nil, "rooms_added": nil, "devices_added": nil, "graph": nil,
		}
		if run.ErrorCode != "" {
			out["error_code"] = run.ErrorCode
		}
		if run.Status == "completed" {
			out["graph_revision"], out["rooms_added"], out["devices_added"], out["graph"] = run.Revision, run.RoomsAdded, run.DevicesAdded, run.Graph
		}
		_ = e.JSON(http.StatusOK, out)
	})
}
