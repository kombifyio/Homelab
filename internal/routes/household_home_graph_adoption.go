package routes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/kombifyio/techstack/internal/gocommon/servicecall"
	"github.com/kombifyio/techstack/pkg/controlplane"
	"github.com/kombifyio/techstack/pkg/httpx"
	"github.com/kombifyio/techstack/pkg/orchestrator"
	"github.com/kombifyio/techstack/pkg/stackkitcommand"
)

const householdHomeGraphAdoptionPath = "/api/v1/internal/household/home-graph/adoption"

type HomeGraphAdopter interface {
	EnqueueHomeGraphAdoption(context.Context, string, string, string, string, json.RawMessage) (string, error)
	HomeGraphAdoptionStatus(context.Context, string, string, string, string) (*orchestrator.HomeGraphAdoption, error)
}

func (h householdHandlers) homeGraphAdoptionStart(e *httpx.Event) error {
	return h.serve(e, HouseholdHomeGraphScope, func(e *httpx.Event, caller *servicecall.Caller) {
		var body struct {
			OwnerSubject string          `json:"owner_subject"`
			SpaceID      string          `json:"space_id"`
			ServiceID    string          `json:"service_id"`
			Request      json.RawMessage `json:"request"`
		}
		raw, err := io.ReadAll(io.LimitReader(e.Request.Body, stackkitcommand.MaxHomeGraphSyncBytes+4097))
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		if err != nil || len(raw) > stackkitcommand.MaxHomeGraphSyncBytes+4096 || decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF || len(body.ServiceID) > 256 {
			_ = householdSurface.deny(e, http.StatusBadRequest, "body_invalid", false, "Send the documented adoption request.")
			return
		}
		_, header, err := stackkitcommand.DecodeHomeGraphAdoptionRequest(body.Request)
		if err != nil || header.SpaceID != body.SpaceID {
			_ = householdSurface.deny(e, http.StatusBadRequest, "home_graph_request_invalid", false, "Send an adoption request for this space.")
			return
		}
		tenant, ok := h.ownerTenant(e, caller, body.OwnerSubject)
		if !ok {
			return
		}
		adopter, ok := h.sync.(HomeGraphAdopter)
		if !ok {
			_ = householdSurface.deny(e, 503, "home_graph_adoption_unavailable", true, "Retry later.")
			return
		}
		job, err := adopter.EnqueueHomeGraphAdoption(context.WithoutCancel(e.Request.Context()), tenant, body.OwnerSubject, body.SpaceID, body.ServiceID, body.Request)
		switch {
		case err == nil:
			_ = e.JSON(http.StatusAccepted, map[string]any{"status": "accepted", "job_id": job})
		case errors.Is(err, orchestrator.ErrHomeGraphAdoptionStale):
			_ = householdSurface.deny(e, 409, "home_graph_adoption_stale", false, "Inventory the current graph again.")
		case errors.Is(err, orchestrator.ErrHomeGraphNoConnector):
			_ = householdSurface.deny(e, 409, "home_graph_no_connector", false, "Activate a Home Assistant connector first.")
		case errors.Is(err, orchestrator.ErrHomeGraphFenced):
			_ = householdSurface.deny(e, 409, "home_graph_node_busy", true, "Retry when the node is idle.")
		default:
			h.writeError(e, err)
		}
	})
}

func (h householdHandlers) homeGraphAdoptionGet(e *httpx.Event) error {
	return h.serve(e, HouseholdHomeGraphScope, func(e *httpx.Event, caller *servicecall.Caller) {
		q := e.Request.URL.Query()
		owner, space, job := q.Get("owner_subject"), q.Get("space_id"), q.Get("job_id")
		if len(q) != 3 || len(q["owner_subject"]) != 1 || len(q["space_id"]) != 1 || len(q["job_id"]) != 1 || !homeGraphLoxoneSpace.MatchString(space) || job == "" || len(job) > 256 {
			_ = householdSurface.deny(e, 400, "home_graph_query_invalid", false, "Send exactly owner_subject, space_id and job_id.")
			return
		}
		tenant, ok := h.ownerTenant(e, caller, owner)
		if !ok {
			return
		}
		adopter, ok := h.sync.(HomeGraphAdopter)
		if !ok {
			_ = householdSurface.deny(e, 503, "home_graph_adoption_unavailable", true, "Retry later.")
			return
		}
		run, err := adopter.HomeGraphAdoptionStatus(e.Request.Context(), tenant, owner, space, job)
		if errors.Is(err, controlplane.ErrNotFound) {
			_ = householdSurface.deny(e, 404, "home_graph_adoption_not_found", false, "Send the adoption job_id.")
			return
		}
		if err != nil {
			h.writeError(e, err)
			return
		}
		_ = e.JSON(200, map[string]any{"schema": "kombify.home_graph.adoption.v1", "job_id": run.JobID, "service_id": run.ServiceID, "status": run.Status, "error_code": run.ErrorCode, "result": run.Result})
	})
}
