package routes

import (
	"net/http"

	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/httpx"
)

// The diagnostic reads an owner and the owner's agents use to answer "why is
// my backup failing" and "what can I update" (AP16.1). They serve the same
// projections as the private Cloud monitoring reads (runtimeBackupsFor,
// runtimeUpdatesFor) and authorize like every other inventory read: the
// authenticated owner's own servers under the inventory read policy
// (`homelab.inventory.read`). Nothing runs on a node to answer them.

// GET /api/v1/inventory/backups
func (h inventoryHandlers) httpInventoryBackups(e *httpx.Event) error {
	if err := rejectInventoryScopeOverrides(e, nil); err != nil {
		return writeInventoryHTTPError(e, err)
	}
	scope, err := inventoryScopeFromEvent(e)
	if err != nil {
		return writeInventoryHTTPError(e, err)
	}
	ctx := e.Request.Context()
	reader, ok := h.app.read.(controlplane.InventoryMonitoringReader)
	if !ok {
		return writeInventoryHTTPError(e, &inventoryError{status: http.StatusServiceUnavailable, reasonCode: "runtime_backups_unavailable", message: "Backup facts are unavailable"})
	}
	readScope, err := h.app.authorize(ctx, scope, InventoryActionRead, controlplane.InventoryReadTargetServerCollection, "")
	if err != nil {
		return writeInventoryHTTPError(e, err)
	}
	response, err := h.runtimeBackupsFor(ctx, h.app, reader, readScope, scope)
	if err != nil {
		return writeInventoryHTTPError(e, err)
	}
	e.Response.Header().Set("Cache-Control", "private, no-store")
	return httpx.Success(e, http.StatusOK, response)
}

// GET /api/v1/inventory/updates
func (h inventoryHandlers) httpInventoryUpdates(e *httpx.Event) error {
	if err := rejectInventoryScopeOverrides(e, nil); err != nil {
		return writeInventoryHTTPError(e, err)
	}
	scope, err := inventoryScopeFromEvent(e)
	if err != nil {
		return writeInventoryHTTPError(e, err)
	}
	response, err := h.runtimeUpdatesFor(e.Request.Context(), h.app, scope)
	if err != nil {
		return writeInventoryHTTPError(e, err)
	}
	e.Response.Header().Set("Cache-Control", "private, no-store")
	return httpx.Success(e, http.StatusOK, response)
}
