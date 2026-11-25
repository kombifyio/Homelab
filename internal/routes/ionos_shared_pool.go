package routes

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/kombifyio/homelab/internal/providercontrol"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/middleware"
)

// The dedicated workload route shares the canonical Edge identity chain. It
// never routes workloads through a fake server ID or a no-op service action.
func RegisterIONOSSharedPoolRoutes(router *httpx.Router, control *providercontrol.IONOSSharedPoolControl) {
	if control == nil {
		return
	}
	router.POST("/api/v1/runtime/managed-workloads/ionos/commands", func(e *httpx.Event) error {
		ctx := e.Request.Context()
		if !middleware.IsEdgeAuthenticated(ctx) || middleware.VerifiedEdgeEnvelopeFromContext(ctx) == nil {
			return e.JSON(http.StatusUnauthorized, map[string]string{"reason_code": "signed_identity_required"})
		}
		scope, err := inventoryScopeFromEvent(e)
		if err != nil || scope.householdOwnerID != "" {
			return e.JSON(http.StatusForbidden, map[string]string{"reason_code": "tenant_identity_required"})
		}
		var request providercontrol.IONOSPoolRequest
		decoder := json.NewDecoder(http.MaxBytesReader(e.Response, e.Request.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF {
			return e.JSON(http.StatusBadRequest, map[string]string{"reason_code": "command_malformed"})
		}
		if err := httpx.ExtendWriteDeadline(e.Response, providercontrol.IONOSPoolSynchronousBudget+30*time.Second); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return e.JSON(http.StatusServiceUnavailable, map[string]string{"reason_code": "command_deadline_unavailable"})
		}
		e.Response.Header().Set("Cache-Control", "no-store")
		observation, err := control.Execute(ctx, providercontrol.IONOSPoolIdentity{TenantID: scope.tenantID, SubjectID: scope.ownerID}, request)
		if err != nil {
			var refusal *providercontrol.IONOSSharedPoolRefusal
			if errors.As(err, &refusal) {
				status := http.StatusForbidden
				if refusal.Code == "worker_busy" || refusal.Code == "admission_busy" {
					status = http.StatusConflict
					e.Response.Header().Set("Retry-After", "2")
				}
				return e.JSON(status, map[string]string{"reason_code": refusal.Code, "repair": refusal.Repair})
			}
			return e.JSON(http.StatusServiceUnavailable, map[string]string{"reason_code": "workload_dispatch_unconfirmed", "repair": "Reconcile this generation; custody is retained."})
		}
		e.Response.Header().Set("Cache-Control", "no-store")
		return e.JSON(http.StatusOK, observation)
	})
}
