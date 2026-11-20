package routes

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/kombifyio/homelab/internal/portinventory"
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/httpx"
)

// The private Cloud monitoring reads. They sit next to the runtime summary and
// share its servicecall boundary (serveCloudRead): the signed Cloud principal
// is verified first, owner and tenant come only from the signed on_behalf_of
// identity, and the stored membership authorization is resolved before any
// data is read.
//
// Authorization is the same collection-only owner read the summary uses
// (ownerInventoryReadPolicy admits server_collection and service_collection
// reads of the signed owner's own rows and nothing else). The monitoring reads
// are exactly such collection reads - the metric history, state changes, ports
// and backup facts of the owner's own servers - so the policy needs no new
// capability, and it stays owner-scoped and read-only: no object read, operate,
// write, provision or admin action becomes reachable through these routes.
// Every failed or denied read answers with the surface's error envelope;
// nothing is ever reported as empty data in place of a failure.
//
// What is absent stays absent. A server without samples has an empty series, a
// server without a port observation has no `ports` field, a backup fact the
// control plane does not know is omitted. Nothing is derived or invented.

const (
	runtimeMonitoringVersion = "1"
	runtimeEventsQueryLimit  = "limit"
	runtimeHistoryQueryRange = "range"
)

var (
	runtimeHistorySurface = cloudReadSurface{
		errorPrefix: "homelab.runtime_history.", unavailableReason: "runtime_history_unavailable",
		title: "Runtime history unavailable", body: "Homelab Runtime could not resolve this runtime history safely.",
		queryKeys: []string{runtimeHistoryQueryRange},
	}
	runtimeEventsSurface = cloudReadSurface{
		errorPrefix: "homelab.runtime_events.", unavailableReason: "runtime_events_unavailable",
		title: "Runtime events unavailable", body: "Homelab Runtime could not resolve these runtime events safely.",
		queryKeys: []string{runtimeEventsQueryLimit},
	}
	runtimePortsSurface = cloudReadSurface{
		errorPrefix: "homelab.runtime_ports.", unavailableReason: "runtime_ports_unavailable",
		title: "Runtime ports unavailable", body: "Homelab Runtime could not resolve these open ports safely.",
	}
	runtimeBackupsSurface = cloudReadSurface{
		errorPrefix: "homelab.runtime_backups.", unavailableReason: "runtime_backups_unavailable",
		title: "Runtime backups unavailable", body: "Homelab Runtime could not resolve these backup facts safely.",
	}
)

type runtimeMonitoringEnvelope struct {
	Version     string `json:"version"`
	GeneratedAt string `json:"generated_at"`
	// "ok" means authorization succeeded, including an actually empty result.
	ScopeState string `json:"scope_state"`
}

func newRuntimeMonitoringEnvelope(now time.Time) runtimeMonitoringEnvelope {
	return runtimeMonitoringEnvelope{Version: runtimeMonitoringVersion, GeneratedAt: now.UTC().Format(time.RFC3339Nano), ScopeState: "ok"}
}

// ownerReadApplication is the application the Cloud owner reads run on. Once
// the signed subject's active membership is verified the read is that owner's
// own collections only, under ownerInventoryReadPolicy.
func (h inventoryHandlers) ownerReadApplication() *inventoryApplication {
	if h.runtimeSummaryContext == nil {
		return h.app
	}
	owned := *h.app
	owned.policy = ownerInventoryReadPolicy{}
	return &owned
}

// monitoringReader authorizes the owner's server collection and returns the
// monitoring reader of the store with the policy-issued scope. It writes the
// response itself when it fails.
func (h inventoryHandlers) monitoringReader(
	e *httpx.Event, ctx context.Context, surface cloudReadSurface, scope inventoryScope,
) (*inventoryApplication, controlplane.InventoryMonitoringReader, controlplane.InventoryReadScope, bool) {
	app := h.ownerReadApplication()
	reader, ok := app.read.(controlplane.InventoryMonitoringReader)
	if !ok {
		_ = surface.deny(e, http.StatusServiceUnavailable, surface.unavailableReason, true,
			"Retry after the Homelab Runtime monitoring store is available.")
		return nil, nil, controlplane.InventoryReadScope{}, false
	}
	readScope, err := app.authorize(ctx, scope, InventoryActionRead, controlplane.InventoryReadTargetServerCollection, "")
	if err != nil {
		h.writeCloudReadError(e, surface, err)
		return nil, nil, controlplane.InventoryReadScope{}, false
	}
	return app, reader, readScope, true
}

// ---------------------------------------------------------------------------
// GET /api/v1/internal/runtime/history?range=1h|24h|7d|30d
// ---------------------------------------------------------------------------

type runtimeHistoryPoint struct {
	T      string   `json:"t"`
	CPU    *float64 `json:"cpu,omitempty"`
	Memory *float64 `json:"memory,omitempty"`
	Disk   *float64 `json:"disk,omitempty"`
}

type runtimeHistoryServer struct {
	ServerID string                `json:"server_id"`
	Points   []runtimeHistoryPoint `json:"points"`
}

type runtimeHistory struct {
	runtimeMonitoringEnvelope
	Range       string                 `json:"range"`
	StepSeconds int                    `json:"step_seconds"`
	Servers     []runtimeHistoryServer `json:"servers"`
}

func (h inventoryHandlers) httpInternalRuntimeHistory(e *httpx.Event) error {
	return h.serveCloudRead(e, runtimeHistorySurface, h.serveRuntimeHistory)
}

func (h inventoryHandlers) serveRuntimeHistory(e *httpx.Event, ctx context.Context, scope inventoryScope) {
	window, ok := controlplane.ParseMetricRange(e.Request.URL.Query().Get(runtimeHistoryQueryRange))
	if !ok {
		_ = runtimeHistorySurface.deny(e, http.StatusBadRequest, "invalid_range", false,
			"Ask for range 1h, 24h, 7d or 30d.")
		return
	}
	app, reader, readScope, ok := h.monitoringReader(e, ctx, runtimeHistorySurface, scope)
	if !ok {
		return
	}
	now := app.now().UTC()
	series, err := reader.ListInventoryMetricHistory(ctx, readScope, window, now)
	if err != nil {
		h.writeCloudReadError(e, runtimeHistorySurface, inventoryStoreError(err))
		return
	}
	response := runtimeHistory{
		runtimeMonitoringEnvelope: newRuntimeMonitoringEnvelope(now),
		Range:                     window.Name, StepSeconds: int(window.Step / time.Second),
		Servers: make([]runtimeHistoryServer, 0, len(series)),
	}
	for _, item := range series {
		server := runtimeHistoryServer{ServerID: item.ServerID, Points: make([]runtimeHistoryPoint, 0, len(item.Points))}
		for _, point := range item.Points {
			server.Points = append(server.Points, runtimeHistoryPoint{
				T: point.At.UTC().Format(time.RFC3339), CPU: point.CPU, Memory: point.Memory, Disk: point.Disk,
			})
		}
		response.Servers = append(response.Servers, server)
	}
	_ = e.JSON(http.StatusOK, response)
}

// ---------------------------------------------------------------------------
// GET /api/v1/internal/runtime/events?limit=50
// ---------------------------------------------------------------------------

type runtimeEvent struct {
	At         string `json:"at"`
	Kind       string `json:"kind"`
	ID         string `json:"id"`
	Name       string `json:"name"`
	ServerID   string `json:"server_id"`
	Field      string `json:"field"`
	From       string `json:"from"`
	To         string `json:"to"`
	ReasonCode string `json:"reason_code,omitempty"`
}

type runtimeEvents struct {
	runtimeMonitoringEnvelope
	Events []runtimeEvent `json:"events"`
}

func (h inventoryHandlers) httpInternalRuntimeEvents(e *httpx.Event) error {
	return h.serveCloudRead(e, runtimeEventsSurface, h.serveRuntimeEvents)
}

func (h inventoryHandlers) serveRuntimeEvents(e *httpx.Event, ctx context.Context, scope inventoryScope) {
	limit := controlplane.DefaultInventoryTransitionEvents
	if raw := e.Request.URL.Query().Get(runtimeEventsQueryLimit); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > controlplane.MaxInventoryTransitionEvents {
			_ = runtimeEventsSurface.deny(e, http.StatusBadRequest, "invalid_limit", false,
				"Ask for a limit between 1 and "+strconv.Itoa(controlplane.MaxInventoryTransitionEvents)+".")
			return
		}
		limit = parsed
	}
	app, reader, readScope, ok := h.monitoringReader(e, ctx, runtimeEventsSurface, scope)
	if !ok {
		return
	}
	now := app.now().UTC()
	events, err := reader.ListInventoryTransitions(ctx, readScope, now.Add(-controlplane.InventoryTransitionWindow), limit)
	if err != nil {
		h.writeCloudReadError(e, runtimeEventsSurface, inventoryStoreError(err))
		return
	}
	response := runtimeEvents{runtimeMonitoringEnvelope: newRuntimeMonitoringEnvelope(now), Events: make([]runtimeEvent, 0, len(events))}
	for _, event := range events {
		response.Events = append(response.Events, runtimeEvent{
			At: event.At.UTC().Format(time.RFC3339Nano), Kind: event.Kind, ID: event.ID,
			Name: safeLabel(event.Name, 256), ServerID: event.ServerID, Field: event.Field,
			From: safeState(event.From, monitoringStatusUnknown), To: safeState(event.To, monitoringStatusUnknown),
			ReasonCode: safeReasonToken(event.ReasonCode),
		})
	}
	_ = e.JSON(http.StatusOK, response)
}

// ---------------------------------------------------------------------------
// GET /api/v1/internal/runtime/ports
// ---------------------------------------------------------------------------

type runtimePort struct {
	Port        uint16 `json:"port"`
	Protocol    string `json:"protocol"`
	BindAddress string `json:"bind_address"`
	// Exposure is the port inventory's own vocabulary: local, lan,
	// remote-private or public.
	Exposure string `json:"exposure"`
	// ExposedState says whether the Guard also observed the port as exposed
	// beyond the host: present, missing, unknown or stale.
	ExposedState string `json:"exposed_state"`
	// Desired is true when a deployed stack claims the port; an observed port
	// nobody claims is false.
	Desired bool `json:"desired"`
}

type runtimePortsServer struct {
	ServerID string `json:"server_id"`
	// State is observed, stale (the last observation expired) or unknown (the
	// Guard never reported listeners).
	State             string     `json:"state"`
	ObservedAt        *time.Time `json:"observed_at,omitempty"`
	ListenersComplete bool       `json:"listeners_complete"`
	// Ports is absent unless State is observed; an observed host with no open
	// port has an empty list.
	Ports *[]runtimePort `json:"ports,omitempty"`
}

type runtimePorts struct {
	runtimeMonitoringEnvelope
	Servers []runtimePortsServer `json:"servers"`
}

func (h inventoryHandlers) httpInternalRuntimePorts(e *httpx.Event) error {
	return h.serveCloudRead(e, runtimePortsSurface, h.serveRuntimePorts)
}

func (h inventoryHandlers) serveRuntimePorts(e *httpx.Event, ctx context.Context, scope inventoryScope) {
	app := h.ownerReadApplication()
	if app.ports == nil {
		_ = runtimePortsSurface.deny(e, http.StatusServiceUnavailable, runtimePortsSurface.unavailableReason, true,
			"Retry after the Homelab Runtime port inventory is available.")
		return
	}
	readScope, err := app.authorize(ctx, scope, InventoryActionRead, controlplane.InventoryReadTargetServerCollection, "")
	if err != nil {
		h.writeCloudReadError(e, runtimePortsSurface, err)
		return
	}
	now := app.now().UTC()
	page, err := app.read.ListInventoryServers(ctx, readScope, controlplane.InventoryPageRequest{Limit: runtimeSummaryPageSize})
	if err != nil {
		h.writeCloudReadError(e, runtimePortsSurface, inventoryStoreError(err))
		return
	}
	response := runtimePorts{runtimeMonitoringEnvelope: newRuntimeMonitoringEnvelope(now), Servers: make([]runtimePortsServer, 0, len(page.Servers))}
	for _, row := range page.Servers {
		// The server came out of the owner-scoped collection read above, so its
		// owner is the signed owner; the authority still receives that owner as
		// a predicate.
		inventory, readErr := app.ports.ReadCurrent(ctx, portinventory.InventoryRequest{
			TenantID: scope.tenantID, ServerID: row.ID, OwnerSubjectID: row.OwnerSubjectID,
		}, now)
		if errors.Is(readErr, sql.ErrNoRows) {
			continue
		}
		if readErr != nil {
			h.writeCloudReadError(e, runtimePortsSurface, inventoryStoreError(readErr))
			return
		}
		response.Servers = append(response.Servers, runtimePortsServerFrom(row.ID, inventory, now))
	}
	_ = e.JSON(http.StatusOK, response)
}

func runtimePortsServerFrom(serverID string, inventory portinventory.Inventory, now time.Time) runtimePortsServer {
	server := runtimePortsServer{ServerID: serverID, State: "unknown", ListenersComplete: inventory.ListenersComplete}
	if inventory.ObservedAt == nil {
		return server
	}
	observedAt := inventory.ObservedAt.UTC()
	server.ObservedAt = &observedAt
	if inventory.ExpiresAt != nil && !now.Before(*inventory.ExpiresAt) {
		server.State = "stale"
		return server
	}
	server.State = "observed"
	ports := make([]runtimePort, 0, len(inventory.Allocations))
	for _, allocation := range inventory.Allocations {
		if allocation.ObservedState != portinventory.EvidencePresent {
			continue
		}
		ports = append(ports, runtimePort{
			Port: allocation.Port, Protocol: string(allocation.Transport), BindAddress: allocation.BindAddress,
			Exposure: string(allocation.Exposure), ExposedState: string(allocation.ExposedState), Desired: allocation.Desired,
		})
	}
	server.Ports = &ports
	return server
}
