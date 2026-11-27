package routes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/kombifyio/homelab/internal/routes/tenantguard"
	ksapi "github.com/kombifyio/homelab/pkg/api"
	"github.com/kombifyio/homelab/pkg/appcatalog"
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/httpx"
)

var defaultCatalogEnricher = sync.OnceValue(appcatalog.NewDefaultEnricher)

type catalogEnrichRequest struct {
	Query         string `json:"query,omitempty"`
	Image         string `json:"image,omitempty"`
	ContainerName string `json:"container_name,omitempty"`
}

func (h *serviceRuntimeHandlers) enrichService(ctx context.Context, service controlplane.ServiceRuntime, allowConnected bool) appcatalog.EnrichedMetadata {
	subject := serviceSubject(service)
	enricher := h.enricher
	if enricher == nil {
		enricher = defaultCatalogEnricher()
	}
	return enricher.Enrich(ctx, appcatalog.EnrichRequest{
		Query:          service.Name,
		Image:          subject.Image,
		ContainerName:  firstNonEmptyString(stackItContainerName(&service), subject.Name),
		Labels:         subject.Labels,
		AllowConnected: allowConnected,
	})
}

func (h *serviceRuntimeHandlers) getServiceEnrichment(e *httpx.Event) error {
	ownerID, isInstanceOwner, ok := authenticatedUser(e)
	if !ok {
		return httpx.RejectUnauthorized(e, "Authentication required")
	}
	tenantID, tenantErr := tenantguard.TenantScope(requestExplicitTenantID(e), ownerID, "homelab.services.read")
	if tenantErr != nil {
		return tenantErr
	}
	serviceID := strings.TrimSpace(e.Request.PathValue("serviceId"))
	if serviceID == "" {
		return httpx.BadRequest(e, "Service ID is required", nil)
	}
	service, err := h.store.GetServiceRuntime(e.Request.Context(), tenantID, serviceID)
	if errors.Is(err, controlplane.ErrNotFound) {
		return httpx.NotFound(e, "Service not found")
	}
	if err != nil {
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Service inventory is unavailable", nil)
	}
	stack, err := h.stacks.GetStack(e.Request.Context(), tenantID, service.StackID)
	if errors.Is(err, controlplane.ErrNotFound) || (err == nil && !isInstanceOwner && stack.OwnerSubjectID != ownerID) {
		return httpx.NotFound(e, "Service not found")
	}
	if err != nil {
		return httpx.Error(e, http.StatusInternalServerError, ksapi.ErrCodeInternal, "Failed to authorize service", nil)
	}
	meta := h.enrichService(e.Request.Context(), *service, true)
	return httpx.Success(e, http.StatusOK, meta)
}

func (h *serviceRuntimeHandlers) enrichCatalog(e *httpx.Event) error {
	if _, _, ok := authenticatedUser(e); !ok {
		return httpx.RejectUnauthorized(e, "Authentication required")
	}
	var req catalogEnrichRequest
	decoder := json.NewDecoder(io.LimitReader(e.Request.Body, 4097))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return httpx.Reject(e, http.StatusBadRequest, ksapi.ErrCodeBadRequest, "Invalid catalog enrichment request", nil)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return httpx.Reject(e, http.StatusBadRequest, ksapi.ErrCodeBadRequest, "One JSON document is required", nil)
	}
	req.Query = strings.TrimSpace(req.Query)
	req.Image = strings.TrimSpace(req.Image)
	req.ContainerName = strings.TrimSpace(req.ContainerName)
	if req.Query == "" && req.Image == "" && req.ContainerName == "" {
		return httpx.Reject(e, http.StatusBadRequest, ksapi.ErrCodeBadRequest, "At least one of query, image, or container_name is required", nil)
	}
	if len(req.Query) > 200 || len(req.Image) > 300 || len(req.ContainerName) > 200 {
		return httpx.Reject(e, http.StatusBadRequest, ksapi.ErrCodeBadRequest, "Enrichment query fields exceed maximum length", nil)
	}
	enricher := h.enricher
	if enricher == nil {
		enricher = defaultCatalogEnricher()
	}
	meta := enricher.Enrich(e.Request.Context(), appcatalog.EnrichRequest{
		Query:          req.Query,
		Image:          req.Image,
		ContainerName:  req.ContainerName,
		AllowConnected: true,
	})
	return httpx.Success(e, http.StatusOK, meta)
}
