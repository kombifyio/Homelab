package routes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kombifyio/homelab/internal/ionospooltunnel"
	"github.com/kombifyio/homelab/internal/providercontrol"
	"github.com/kombifyio/homelab/internal/routes/tenantguard"
	ksapi "github.com/kombifyio/homelab/pkg/api"
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/serviceregistry"
)

// ManagedPoolExecutor dispatches a tenant-scoped IONOS Shared Pool command
// through the admitted pool control plane (including slot admission and
// durable custody).
type ManagedPoolExecutor func(
	ctx context.Context,
	identity providercontrol.IONOSPoolIdentity,
	request providercontrol.IONOSPoolRequest,
) (providercontrol.IONOSSharedPoolObservation, error)

const (
	managedMigrationBlockAlreadyManaged = "already_managed_workload"
	managedMigrationBlockPendingR2      = "pending_r2_profile"
	managedMigrationBlockNotEligible    = "app_not_managed_eligible"
	managedMigrationBlockPoolDisabled   = "managed_pool_disabled"
)

type curatedManagedAppEntry struct {
	AppKey      string `json:"app_key"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
	Note        string `json:"note"`
}

type managedMigrationEligibilityResponse struct {
	ServiceID            string                   `json:"service_id"`
	AppKey               string                   `json:"app_key"`
	EligibleApp          bool                     `json:"eligible_app"`
	PoolEnabled          bool                     `json:"pool_enabled"`
	Eligible             bool                     `json:"eligible"`
	BlockCode            string                   `json:"block_code,omitempty"`
	Reason               string                   `json:"reason,omitempty"`
	CuratedSupportedApps []curatedManagedAppEntry `json:"curated_supported_apps"`
	TargetKind           string                   `json:"target_kind"`
}

type managedMigrationPlanRequest struct {
	OwnerApproved bool `json:"owner_approved"`
	Execute       bool `json:"execute,omitempty"`
}

type managedMigrationPlanStep struct {
	Order       int    `json:"order"`
	Key         string `json:"key"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Status      string `json:"status"`
}

type managedMigrationWorkloadReceipt struct {
	CommandID    string `json:"command_id"`
	GenerationID string `json:"generation_id"`
	NodeID       string `json:"node_id,omitempty"`
	State        string `json:"state"`
	EvidenceRef  string `json:"evidence_ref,omitempty"`
}

type managedMigrationPlanResponse struct {
	PlanID                   string                           `json:"plan_id"`
	IdempotencyKey           string                           `json:"idempotency_key"`
	ServiceID                string                           `json:"service_id"`
	SourceServerID           string                           `json:"source_server_id,omitempty"`
	AppKey                   string                           `json:"app_key"`
	TargetKind               string                           `json:"target_kind"`
	Status                   string                           `json:"status"`
	DryRun                   bool                             `json:"dry_run"`
	OwnerApproved            bool                             `json:"owner_approved"`
	ManagedWorkloadCommandID string                           `json:"managed_workload_command_id,omitempty"`
	ManagedWorkload          *managedMigrationWorkloadReceipt `json:"managed_workload,omitempty"`
	Steps                    []managedMigrationPlanStep       `json:"steps"`
	CreatedAt                time.Time                        `json:"created_at"`
}

func curatedManagedAppsCatalog() []curatedManagedAppEntry {
	return []curatedManagedAppEntry{
		{
			AppKey:      "vaultwarden",
			DisplayName: "Vaultwarden",
			Status:      "supported",
			Note:        "Curated Managed App eligible for dedicated-to-managed transfer when the shared pool is enabled.",
		},
		{
			AppKey:      "cloudreve-r2",
			DisplayName: "Files (Cloudreve R2)",
			Status:      "pending_r2_profile",
			Note:        "Queued for Managed Apps once the Cloudreve R2 profile ships (kombify-StackKits-fusz).",
		},
	}
}

func defaultManagedPoolEnabled() bool {
	return os.Getenv("HOMELAB_IONOS_SHARED_POOL_ENABLED") == "true"
}

func normalizeManagedMigrationAppKey(service controlplane.ServiceRuntime) string {
	appKey := strings.ToLower(strings.TrimSpace(applicationKeyForService(service)))
	if appKey != "" {
		return appKey
	}
	return strings.ToLower(strings.TrimSpace(service.ServiceKey))
}

func evaluateManagedMigrationEligibility(service controlplane.ServiceRuntime, poolEnabled bool) managedMigrationEligibilityResponse {
	appKey := normalizeManagedMigrationAppKey(service)
	resp := managedMigrationEligibilityResponse{
		ServiceID:            service.ID,
		AppKey:               appKey,
		PoolEnabled:          poolEnabled,
		CuratedSupportedApps: curatedManagedAppsCatalog(),
		TargetKind:           string(serviceregistry.TargetKindManagedWorkload),
	}

	placement := serviceregistry.NormalizePlacement(service.ServerID, service.Placement)
	if placement.TargetKind == serviceregistry.TargetKindManagedWorkload {
		resp.BlockCode = managedMigrationBlockAlreadyManaged
		resp.Reason = "Service is already running as a managed workload."
		return resp
	}
	if placement.TargetKind != serviceregistry.TargetKindServer || strings.TrimSpace(service.ServerID) == "" {
		resp.BlockCode = "source_server_required"
		resp.Reason = "Service must be bound to a dedicated server before migrating to Managed Apps."
		return resp
	}

	switch appKey {
	case "vaultwarden":
		resp.EligibleApp = true
	case "cloudreve", "cloudreve-r2":
		resp.BlockCode = managedMigrationBlockPendingR2
		resp.Reason = "Cloudreve Managed App transfer is queued until the Cloudreve R2 profile ships (kombify-StackKits-fusz)."
		return resp
	default:
		resp.BlockCode = managedMigrationBlockNotEligible
		resp.Reason = "Only curated Managed Apps (currently Vaultwarden) are eligible for transfer to Managed Apps."
		return resp
	}

	if !poolEnabled {
		resp.BlockCode = managedMigrationBlockPoolDisabled
		resp.Reason = "Managed App shared pool is disabled (HOMELAB_IONOS_SHARED_POOL_ENABLED is not enabled)."
		return resp
	}

	resp.Eligible = true
	return resp
}

func (h *serviceRuntimeHandlers) loadAuthorizedManagedMigrationService(
	e *httpx.Event,
	scope string,
) (string, string, *controlplane.ServiceRuntime, error) {
	ownerID, isInstanceOwner, ok := authenticatedUser(e)
	if !ok {
		return "", "", nil, httpx.RejectUnauthorized(e, "Authentication required")
	}
	serviceID := strings.TrimSpace(e.Request.PathValue("serviceId"))
	if serviceID == "" {
		return "", "", nil, httpx.Reject(e, http.StatusBadRequest, ksapi.ErrCodeBadRequest, "Service ID is required", nil)
	}
	tenantID, tenantErr := tenantguard.TenantScope(requestExplicitTenantID(e), ownerID, scope)
	if tenantErr != nil {
		return "", "", nil, tenantErr
	}
	service, err := h.loadServiceRuntime(e.Request.Context(), tenantID, serviceID)
	if errors.Is(err, controlplane.ErrNotFound) {
		return "", "", nil, httpx.Reject(e, http.StatusNotFound, ksapi.ErrCodeNotFound, "Service not found", nil)
	}
	if err != nil {
		httpx.RecordDependencyFailure(e, "services.managed_migration.runtime", err)
		return "", "", nil, httpx.Reject(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Service inventory is unavailable", nil)
	}
	stack, err := h.stacks.GetStack(e.Request.Context(), tenantID, service.StackID)
	if errors.Is(err, controlplane.ErrNotFound) || (err == nil && !isInstanceOwner && stack.OwnerSubjectID != ownerID) {
		return "", "", nil, httpx.Reject(e, http.StatusNotFound, ksapi.ErrCodeNotFound, "Service not found", nil)
	}
	if err != nil {
		httpx.RecordDependencyFailure(e, "services.managed_migration.stack", err)
		return "", "", nil, httpx.Reject(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Service authorization is unavailable", nil)
	}
	return tenantID, ownerID, service, nil
}

func (h *serviceRuntimeHandlers) managedMigrationEligibility(e *httpx.Event) error {
	_, _, service, err := h.loadAuthorizedManagedMigrationService(e, "homelab.services.managed_migration.read")
	if err != nil {
		return err
	}
	poolEnabled := h.managedPoolEnabled != nil && h.managedPoolEnabled()
	result := evaluateManagedMigrationEligibility(*service, poolEnabled)
	return httpx.Success(e, http.StatusOK, result)
}

func (h *serviceRuntimeHandlers) planManagedMigration(e *httpx.Event) error {
	tenantID, ownerID, service, err := h.loadAuthorizedManagedMigrationService(e, "homelab.services.managed_migration.plan")
	if err != nil {
		return err
	}
	idempotencyKey := strings.TrimSpace(e.Request.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" || len(idempotencyKey) > 128 {
		return httpx.BadRequest(e, "A bounded Idempotency-Key header is required", map[string]any{
			"code": "idempotency_key_required",
		})
	}
	if e.Request.Body == nil {
		return httpx.BadRequest(e, "Invalid managed migration request body", map[string]any{
			"code": "invalid_request_body",
		})
	}
	var req managedMigrationPlanRequest
	decoder := json.NewDecoder(io.LimitReader(e.Request.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return httpx.BadRequest(e, "Invalid managed migration request body", map[string]any{
			"code": "invalid_request_body",
		})
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return httpx.BadRequest(e, "Request body must contain one JSON document", map[string]any{
			"code": "invalid_request_body",
		})
	}
	if !req.OwnerApproved {
		return httpx.BadRequest(e, "owner_approved must be true to prepare a managed migration plan", map[string]any{
			"code": "owner_approval_required",
		})
	}

	poolEnabled := h.managedPoolEnabled != nil && h.managedPoolEnabled()
	eligibility := evaluateManagedMigrationEligibility(*service, poolEnabled)
	if !eligibility.Eligible {
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, eligibility.Reason, map[string]any{
			"code":        eligibility.BlockCode,
			"eligibility": eligibility,
		})
	}

	sum := sha256.Sum256([]byte(strings.Join([]string{tenantID, service.ID, idempotencyKey}, "\x00")))
	plan := managedMigrationPlanResponse{
		PlanID:         "mmp-" + hex.EncodeToString(sum[:8]),
		IdempotencyKey: idempotencyKey,
		ServiceID:      service.ID,
		SourceServerID: service.ServerID,
		AppKey:         eligibility.AppKey,
		TargetKind:     string(serviceregistry.TargetKindManagedWorkload),
		Status:         "planned",
		DryRun:         true,
		OwnerApproved:  true,
		Steps: []managedMigrationPlanStep{
			{
				Order:       1,
				Key:         "backup_export_source",
				Title:       "Backup and export source state",
				Description: "Create a verified backup export of the dedicated-server application state before provisioning.",
				Status:      "pending",
			},
			{
				Order:       2,
				Key:         "provision_managed_workload",
				Title:       "Provision managed workload",
				Description: "Admit and provision an isolated managed workload slot on the shared hosting pool.",
				Status:      "pending",
			},
			{
				Order:       3,
				Key:         "restore_verify_managed",
				Title:       "Restore and verify on managed workload",
				Description: "Restore the exported backup into the managed workload and verify health and ingress readiness.",
				Status:      "pending",
			},
			{
				Order:       4,
				Key:         "retire_source_retained",
				Title:       "Retire source while retaining recovery copy",
				Description: "Stop the dedicated-server instance after verification while retaining the source backup for rollback.",
				Status:      "pending",
			},
		},
		CreatedAt: h.now().UTC(),
	}
	if !req.Execute {
		return httpx.Success(e, http.StatusOK, plan)
	}

	if h.managedPoolExecutor == nil {
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Managed App shared pool executor is unavailable", map[string]any{
			"code": "managed_pool_executor_unavailable",
		})
	}
	if err := httpx.ExtendWriteDeadline(e.Response, providercontrol.IONOSPoolSynchronousBudget+30*time.Second); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Managed workload execution deadline is unavailable", map[string]any{
			"code": "command_deadline_unavailable",
		})
	}

	commandID := "mmc-" + hex.EncodeToString(sum[:12])
	generationID := "mmg-" + hex.EncodeToString(sum[12:24])
	e.Response.Header().Set("Cache-Control", "no-store")
	observation, execErr := h.managedPoolExecutor(
		e.Request.Context(),
		providercontrol.IONOSPoolIdentity{TenantID: tenantID, SubjectID: ownerID},
		providercontrol.IONOSPoolRequest{
			CommandID:    commandID,
			GenerationID: generationID,
			AppID:        eligibility.AppKey,
			Action:       providercontrol.IONOSPoolProvision,
		},
	)
	if execErr != nil {
		var readiness *ionospooltunnel.ReadinessError
		if errors.As(execErr, &readiness) {
			return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Managed App shared pool is not ready", map[string]any{
				"code": readiness.Code,
			})
		}
		var refusal *providercontrol.IONOSSharedPoolRefusal
		if errors.As(execErr, &refusal) {
			status := http.StatusForbidden
			apiCode := ksapi.ErrCodeForbidden
			if refusal.Code == "worker_busy" || refusal.Code == "admission_busy" || refusal.Code == "slot_limit_exhausted" || refusal.Code == "capacity_refused" {
				status = http.StatusConflict
				apiCode = ksapi.ErrCodeConflict
				if refusal.Code == "worker_busy" || refusal.Code == "admission_busy" {
					e.Response.Header().Set("Retry-After", "2")
				}
			}
			return httpx.Error(e, status, apiCode, refusal.Repair, map[string]any{
				"code":   refusal.Code,
				"repair": refusal.Repair,
			})
		}
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Managed workload dispatch outcome is unconfirmed; custody is retained", map[string]any{
			"code":   "workload_dispatch_unconfirmed",
			"repair": "Reconcile this generation; custody is retained.",
		})
	}

	provisionStatus := "in_progress"
	if observation.State == "running" || observation.State == "stopped" {
		provisionStatus = "completed"
	}
	plan.Status = "provisioning_managed"
	plan.DryRun = false
	plan.ManagedWorkloadCommandID = commandID
	plan.ManagedWorkload = &managedMigrationWorkloadReceipt{
		CommandID:    commandID,
		GenerationID: generationID,
		NodeID:       observation.Spec.NodeID,
		State:        observation.State,
		EvidenceRef:  observation.EvidenceRef,
	}
	// Source retirement remains strictly pending until destination restore and
	// verification complete.
	plan.Steps[1].Status = provisionStatus
	plan.Steps[2].Status = "pending"
	plan.Steps[3].Status = "pending"
	return httpx.Success(e, http.StatusOK, plan)
}
