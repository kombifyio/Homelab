package routes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/kombifyio/homelab/internal/providercontrol"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/logger"
	"github.com/kombifyio/homelab/pkg/middleware"
)

// RegisterFlyMachinesRoutes exposes the dedicated Fly.io fra Managed Apps command
// route on the canonical Edge identity chain. It is registered only when
// HOMELAB_FLY_MACHINES_ENABLED=true.
func RegisterFlyMachinesRoutes(router *httpx.Router, log *logger.Logger, execute func(context.Context, providercontrol.FlyMachinesIdentity, providercontrol.FlyMachinesRequest) (providercontrol.FlyMachineCommandResult, error)) {
	if execute == nil {
		return
	}
	router.POST("/api/v1/runtime/managed-workloads/fly/commands", func(e *httpx.Event) error {
		ctx := e.Request.Context()
		if !middleware.IsEdgeAuthenticated(ctx) || middleware.VerifiedEdgeEnvelopeFromContext(ctx) == nil {
			return e.JSON(http.StatusUnauthorized, map[string]string{"reason_code": "signed_identity_required"})
		}
		scope, err := inventoryScopeFromEvent(e)
		if err != nil || scope.householdOwnerID != "" {
			return e.JSON(http.StatusForbidden, map[string]string{"reason_code": "tenant_identity_required"})
		}
		var request providercontrol.FlyMachinesRequest
		decoder := json.NewDecoder(http.MaxBytesReader(e.Response, e.Request.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF {
			return e.JSON(http.StatusBadRequest, map[string]string{"reason_code": "command_malformed"})
		}
		if err := httpx.ExtendWriteDeadline(e.Response, providercontrol.FlyMachinesSynchronousBudget+30*time.Second); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return e.JSON(http.StatusServiceUnavailable, map[string]string{"reason_code": "command_deadline_unavailable"})
		}
		e.Response.Header().Set("Cache-Control", "no-store")
		result, err := execute(ctx, providercontrol.FlyMachinesIdentity{TenantID: scope.tenantID, SubjectID: scope.ownerID}, request)
		if err != nil {
			var readiness *providercontrol.FlyMachinesReadinessError
			if errors.As(err, &readiness) {
				return e.JSON(http.StatusServiceUnavailable, readiness)
			}
			var refusal *providercontrol.ManagedAppRefusal
			if errors.As(err, &refusal) {
				status := http.StatusForbidden
				switch refusal.Code {
				case "worker_busy", "admission_busy", "absence_pending":
					status = http.StatusConflict
					e.Response.Header().Set("Retry-After", "2")
				case "outcome_unknown":
					status = http.StatusConflict
				case "backup_unavailable", "tenant_isolation_unavailable":
					status = http.StatusServiceUnavailable
				case "provider_provision_refused":
					status = http.StatusBadGateway
				}
				return e.JSON(status, map[string]string{"reason_code": refusal.Code, "repair": refusal.Repair})
			}
			if log != nil {
				log.Error("fly_machines_command_unconfirmed",
					"tenant_id", scope.tenantID,
					"generation_id", request.GenerationID,
					"command_id", request.CommandID,
					"action", string(request.Action),
					"error", err.Error(),
				)
			}
			return e.JSON(http.StatusServiceUnavailable, map[string]string{"reason_code": "workload_dispatch_unconfirmed", "repair": "Reconcile this generation; custody is retained."})
		}
		return e.JSON(http.StatusOK, result)
	})
}
