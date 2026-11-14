package stacks

import (
	"errors"
	"net/http"
	"strings"

	"github.com/kombifyio/techstack/internal/routes/tenantguard"
	ksapi "github.com/kombifyio/techstack/pkg/api"
	"github.com/kombifyio/techstack/pkg/controlplane"
	"github.com/kombifyio/techstack/pkg/httpx"
	"github.com/kombifyio/techstack/pkg/jobs"
	"github.com/kombifyio/techstack/pkg/orchestrator"
	"github.com/kombifyio/techstack/pkg/stackkitcommand"
)

// Called from the existing CRUD route composition with its configured stores.
func (h crudRouteHandlers) registerWorkloadRelocationRoutes(r *httpx.Router) {
	r.POST("/api/v1/stacks/{id}/relocations", h.startWorkloadRelocation)
	r.GET("/api/v1/stacks/{id}/relocations/{jobId}", h.inspectWorkloadRelocation)
	r.POST("/api/v1/stacks/{id}/relocations/{jobId}/resolve", h.resolveWorkloadRelocation)
	r.POST("/api/v1/stacks/{id}/relocations/{jobId}/abort", h.abortWorkloadRelocation)
}

func (h crudRouteHandlers) inspectWorkloadRelocation(e *httpx.Event) error {
	return h.readbackWorkloadRelocation(e, false)
}
func (h crudRouteHandlers) resolveWorkloadRelocation(e *httpx.Event) error {
	return h.readbackWorkloadRelocation(e, true)
}
func (h crudRouteHandlers) abortWorkloadRelocation(e *httpx.Event) error {
	return h.readbackWorkloadRelocation(e, true)
}
func (h crudRouteHandlers) readbackWorkloadRelocation(e *httpx.Event, resolve bool) error {
	owner, err := requireStackAuth(e)
	if err != nil {
		return err
	}
	tenant, err := tenantguard.TenantScope(tenantIDFromRequest(e), owner, "techstack.workload.relocation")
	if err != nil {
		return err
	}
	if h.orch == nil {
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Relocation observation is unavailable", nil)
	}
	var result map[string]any
	if strings.HasSuffix(e.Request.URL.Path, "/abort") {
		var body struct {
			OwnerApproved bool `json:"owner_approved"`
		}
		if decodeStrictJSONBody(e.Request.Body, &body) != nil || !body.OwnerApproved {
			return httpx.BadRequest(e, "Explicit Owner approval is required to resume the source")
		}
		result, err = h.orch.AbortWorkloadRelocation(e.Request.Context(), tenant, owner, e.Request.PathValue("id"), e.Request.PathValue("jobId"))
	} else {
		result, err = h.orch.InspectWorkloadRelocation(e.Request.Context(), tenant, owner, e.Request.PathValue("id"), e.Request.PathValue("jobId"), resolve)
	}
	if errors.Is(err, controlplane.ErrNotFound) {
		return httpx.NotFound(e, "Relocation not found")
	}
	if err != nil {
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "Relocation remains fenced until both nodes verify the exact transfer", map[string]any{"reason": err.Error()})
	}
	return httpx.Success(e, http.StatusOK, result)
}

func (h crudRouteHandlers) startWorkloadRelocation(e *httpx.Event) error {
	owner, err := requireStackAuth(e)
	if err != nil {
		return err
	}
	tenant, err := tenantguard.TenantScope(tenantIDFromRequest(e), owner, "techstack.workload.relocation")
	if err != nil {
		return err
	}
	if h.orch == nil {
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Workload relocation is unavailable", nil)
	}
	var body struct {
		Engine             string   `json:"engine,omitempty"`
		UseCase            string   `json:"use_case,omitempty"`
		Workloads          []string `json:"workloads,omitempty"`
		SourceAgentID      string   `json:"source_agent_id"`
		DestinationStackID string   `json:"destination_stack_id"`
		DestinationAgentID string   `json:"destination_agent_id"`
		Projects           []string `json:"projects,omitempty"`
		ResourceUUIDs      []string `json:"resource_uuids,omitempty"`
		WalletItemID       string   `json:"wallet_item_id"`
		DomainSwitches     []string `json:"domain_switches,omitempty"`
		OwnerApproved      bool     `json:"owner_approved"`
	}
	if err := decodeStrictJSONBody(e.Request.Body, &body); err != nil {
		return httpx.BadRequest(e, "Invalid relocation request")
	}
	if len(e.Request.Header.Values("Idempotency-Key")) != 1 {
		return httpx.BadRequest(e, "Exactly one Idempotency-Key is required")
	}
	input := orchestrator.WorkloadRelocationInput{Engine: strings.TrimSpace(body.Engine), UseCase: strings.TrimSpace(body.UseCase), Workloads: body.Workloads, SourceStackID: strings.TrimSpace(e.Request.PathValue("id")), SourceAgentID: body.SourceAgentID, DestinationStackID: body.DestinationStackID, DestinationAgentID: body.DestinationAgentID, Projects: body.Projects, ResourceUUIDs: body.ResourceUUIDs, WalletItemID: body.WalletItemID, DomainSwitches: body.DomainSwitches, OwnerApproved: body.OwnerApproved}
	if !input.OwnerApproved || input.SourceStackID == "" || input.DestinationStackID == "" {
		return httpx.BadRequest(e, "Source, destination and explicit Owner approval are required")
	}
	switch input.Engine {
	case jobs.RelocationEngineNative:
		if len(input.Projects) != 0 || len(input.ResourceUUIDs) != 0 || input.WalletItemID != "" || len(input.DomainSwitches) != 0 {
			return httpx.BadRequest(e, "A native move takes a use case or one application, not Coolify projects, a Wallet item or domain switches")
		}
		if err := stackkitcommand.ValidateApplicationTransferSelection(input.UseCase, input.Workloads); err != nil {
			return httpx.BadRequest(e, err.Error())
		}
	case "", jobs.RelocationEngineCoolify:
		if input.UseCase != "" || len(input.Workloads) != 0 {
			return httpx.BadRequest(e, "Use cases and applications move with engine native")
		}
		if err := jobs.ValidateWorkloadRelocationSelection(input.Projects, input.ResourceUUIDs); err != nil {
			return httpx.BadRequest(e, err.Error())
		}
	default:
		return httpx.BadRequest(e, "engine must be coolify or native")
	}
	for _, target := range []struct{ stack, agent string }{{input.SourceStackID, input.SourceAgentID}, {input.DestinationStackID, input.DestinationAgentID}} {
		if h.stackStore == nil {
			return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Stack ownership is unavailable", nil)
		}
		stack, err := h.stackStore.GetStack(e.Request.Context(), tenant, target.stack)
		if err != nil || stack == nil || stack.OwnerSubjectID != owner {
			return httpx.NotFound(e, "Relocation target not found")
		}
		if locked, err := h.lockedStackServices(e, tenant, target.stack, jobs.StackKitLifecycleAdvancedChangeSet); err != nil {
			return err
		} else if len(locked) > 0 {
			return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "A relocation target contains locked services", map[string]any{"locked_service_ids": locked})
		}
		if err := h.refuseDuringServerMaintenance(e, tenant, target.stack, target.agent); err != nil {
			return err
		}
	}
	id, err := h.orch.EnqueueWorkloadRelocation(e.Request.Context(), tenant, owner, e.Request.Header.Get("Idempotency-Key"), input)
	if errors.Is(err, controlplane.ErrNotFound) {
		return httpx.NotFound(e, "Relocation target not found")
	}
	if errors.Is(err, controlplane.ErrConflict) {
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "Relocation identity conflicts with an existing operation", nil)
	}
	if err != nil {
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Relocation prerequisites are unavailable", map[string]any{"reason": err.Error()})
	}
	engine := input.Engine
	if engine == "" {
		engine = jobs.RelocationEngineCoolify
	}
	return httpx.Success(e, http.StatusAccepted, map[string]any{"job_id": id, "kit_deployment_id": input.SourceStackID, "operation": "workload_relocation", "engine": engine, "source_retained": true})
}
