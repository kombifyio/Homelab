package routes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kombifyio/homelab/internal/gocommon/servicecall"
	"github.com/kombifyio/homelab/internal/providercontrol"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/serviceauth"
)

const (
	SupportReplacementAuthorizePath = "/api/v1/internal/managed-runtime/support-replacements/authorize"
	SupportReplacementAcceptPath    = "/api/v1/internal/managed-runtime/support-replacements/accept"
	SupportReplacementRunPath       = "/api/v1/internal/managed-runtime/support-replacements/run"
	SupportReplacementListPath      = "/api/v1/internal/managed-runtime/support-replacements"

	SupportReplacementAuthorizeScope = "homelab:support-replacement-authorize"
	SupportReplacementAcceptScope    = "homelab:support-replacement-accept"
	SupportReplacementOperateScope   = "homelab:support-replacement-operate"
	SupportReplacementReadScope      = "homelab:support-replacement-read"

	SupportReplacementStaffCaller   = "homelab-operations"
	SupportReplacementGatewayCaller = "gateway"
)

type SupportReplacementController interface {
	Authorize(ctx context.Context, req providercontrol.AuthorizeSupportReplacementRequest) (providercontrol.SupportReplacementRecord, error)
	Accept(ctx context.Context, req providercontrol.AcceptSupportReplacementRequest) (providercontrol.SupportReplacementRecord, error)
	Get(ctx context.Context, tenantID, replacementID string) (providercontrol.SupportReplacementRecord, error)
	List(ctx context.Context, tenantID, phase string) ([]providercontrol.SupportReplacementRecord, error)
	EnqueueRun(ctx context.Context, tenantID, ownerID, stackID, replacementID, idempotencyKey string, forcePreWriteRollback bool, reason string) (string, error)
	ReconcileConflict(ctx context.Context, tenantID, replacementID, targetPhase, reason string) (providercontrol.SupportReplacementRecord, error)
}

type SupportReplacementRouteConfig struct {
	Controller SupportReplacementController
	// AuthConfig optionally overrides serviceauth.Apply for tests.
	AuthConfig *servicecall.Config
}

func RegisterSupportReplacementRoutes(r *httpx.Router, cfg SupportReplacementRouteConfig) {
	if r == nil {
		return
	}
	r.POST(SupportReplacementAuthorizePath, supportReplacementAuthorizedHandler(cfg, SupportReplacementAuthorizeScope, []string{SupportReplacementStaffCaller}, 8192, serveSupportReplacementAuthorize))
	r.POST(SupportReplacementAcceptPath, supportReplacementAuthorizedHandler(cfg, SupportReplacementAcceptScope, []string{SupportReplacementStaffCaller, SupportReplacementGatewayCaller}, 4096, serveSupportReplacementAccept))
	r.POST(SupportReplacementRunPath, supportReplacementAuthorizedHandler(cfg, SupportReplacementOperateScope, []string{SupportReplacementStaffCaller}, 4096, serveSupportReplacementRun))
	r.GET(SupportReplacementListPath, supportReplacementAuthorizedHandler(cfg, SupportReplacementReadScope, []string{SupportReplacementStaffCaller, SupportReplacementGatewayCaller}, 1024, serveSupportReplacementList))
}

func supportReplacementAuthorizedHandler(
	cfg SupportReplacementRouteConfig,
	scope string,
	callers []string,
	maxBody int64,
	next func(http.ResponseWriter, *http.Request, SupportReplacementRouteConfig, *servicecall.Caller),
) func(*httpx.Event) error {
	return func(e *httpx.Event) error {
		if cfg.Controller == nil {
			http.Error(e.Response, "support replacement route unavailable", http.StatusServiceUnavailable)
			return nil
		}
		var auth servicecall.Config
		if cfg.AuthConfig != nil {
			auth = *cfg.AuthConfig
		} else {
			applied, err := serviceauth.Apply(servicecall.Config{
				ServiceName:    "homelab",
				Enabled:        true,
				RequireEd25519: true,
				AllowedCallers: callers,
			})
			if err != nil {
				http.Error(e.Response, "support replacement auth unavailable", http.StatusServiceUnavailable)
				return nil
			}
			auth = applied
		}
		auth.ServiceName = "homelab"
		auth.Enabled = true
		auth.RequireEd25519 = true
		auth.AllowedCallers = callers
		auth.Secret, auth.SecretNext = "", ""

		servicecall.RequireBoundServiceAuth(servicecall.BoundRequestConfig{
			Config:        auth,
			RequiredScope: scope,
			MaxBodyBytes:  maxBody,
		})(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			claims := servicecall.FromContext(request.Context())
			if claims == nil || claims.ExpiresAt.Sub(claims.IssuedAt) <= 0 || claims.ExpiresAt.Sub(claims.IssuedAt) > time.Minute {
				http.Error(w, "identity denied", http.StatusForbidden)
				return
			}
			next(w, request, cfg, claims)
		})).ServeHTTP(e.Response, e.Request)
		return nil
	}
}

func serveSupportReplacementAuthorize(w http.ResponseWriter, request *http.Request, cfg SupportReplacementRouteConfig, claims *servicecall.Caller) {
	if claims.Service != SupportReplacementStaffCaller {
		http.Error(w, "staff caller required", http.StatusForbidden)
		return
	}
	var body struct {
		TenantID               string `json:"tenantId"`
		ReplacementID          string `json:"replacementId"`
		OwnerSubjectID         string `json:"ownerSubjectId"`
		StackID                string `json:"stackId"`
		SourceLeaseID          string `json:"sourceLeaseId"`
		CaseRef                string `json:"caseRef"`
		Reason                 string `json:"reason"`
		AuthorizedBy           string `json:"authorizedBy"`
		EvidenceRef            string `json:"evidenceRef"`
		IdempotencyKey         string `json:"idempotencyKey"`
		NoChargeConfirmed      bool   `json:"noChargeConfirmed"`
		TargetProvider         string `json:"targetProvider"`
		TargetPackageID        string `json:"targetPackageId"`
		TargetVariantName      string `json:"targetVariantName"`
		TargetProductSlug      string `json:"targetProductSlug"`
		TargetExecutionProfile string `json:"targetExecutionProfile"`
		TargetProviderProfile  string `json:"targetProviderProfile"`
	}
	if !decodeStrictSupportJSON(w, request.Body, 8192, &body) {
		return
	}
	rec, err := cfg.Controller.Authorize(request.Context(), providercontrol.AuthorizeSupportReplacementRequest{
		TenantID:               body.TenantID,
		ReplacementID:          body.ReplacementID,
		OwnerSubjectID:         body.OwnerSubjectID,
		StackID:                body.StackID,
		SourceLeaseID:          body.SourceLeaseID,
		CaseRef:                body.CaseRef,
		Reason:                 body.Reason,
		AuthorizedBy:           body.AuthorizedBy,
		EvidenceRef:            body.EvidenceRef,
		IdempotencyKey:         body.IdempotencyKey,
		NoChargeConfirmed:      body.NoChargeConfirmed,
		TargetProvider:         body.TargetProvider,
		TargetPackageID:        body.TargetPackageID,
		TargetVariantName:      body.TargetVariantName,
		TargetProductSlug:      body.TargetProductSlug,
		TargetExecutionProfile: body.TargetExecutionProfile,
		TargetProviderProfile:  body.TargetProviderProfile,
	})
	if err != nil {
		writeSupportReplacementError(w, err)
		return
	}
	status := http.StatusOK
	if rec.Created {
		status = http.StatusCreated
	}
	writeSupportReplacementJSON(w, status, map[string]any{"ok": true, "replacement": rec})
}

func serveSupportReplacementAccept(w http.ResponseWriter, request *http.Request, cfg SupportReplacementRouteConfig, claims *servicecall.Caller) {
	var body struct {
		TenantID       string `json:"tenantId"`
		ReplacementID  string `json:"replacementId"`
		OwnerSubjectID string `json:"ownerSubjectId"`
		AcceptedBy     string `json:"acceptedBy"`
	}
	if !decodeStrictSupportJSON(w, request.Body, 4096, &body) {
		return
	}
	ownerSubjectID := strings.TrimSpace(body.OwnerSubjectID)
	acceptedBy := strings.TrimSpace(body.AcceptedBy)
	if ownerSubjectID == "" {
		http.Error(w, "ownerSubjectId is required for customer acceptance", http.StatusForbidden)
		return
	}
	if acceptedBy != ownerSubjectID {
		http.Error(w, "acceptedBy must match ownerSubjectId", http.StatusForbidden)
		return
	}
	rec, err := cfg.Controller.Accept(request.Context(), providercontrol.AcceptSupportReplacementRequest{
		TenantID:       body.TenantID,
		ReplacementID:  body.ReplacementID,
		OwnerSubjectID: ownerSubjectID,
		AcceptedBy:     acceptedBy,
	})
	if err != nil {
		writeSupportReplacementError(w, err)
		return
	}
	writeSupportReplacementJSON(w, http.StatusOK, map[string]any{"ok": true, "replacement": rec})
}

func serveSupportReplacementRun(w http.ResponseWriter, request *http.Request, cfg SupportReplacementRouteConfig, claims *servicecall.Caller) {
	if claims.Service != SupportReplacementStaffCaller {
		http.Error(w, "staff caller required", http.StatusForbidden)
		return
	}
	var body struct {
		TenantID              string `json:"tenantId"`
		ReplacementID         string `json:"replacementId"`
		OwnerSubjectID        string `json:"ownerSubjectId"`
		IdempotencyKey        string `json:"idempotencyKey"`
		ForcePreWriteRollback bool   `json:"forcePreWriteRollback"`
		ReconcilePhase        string `json:"reconcilePhase"`
		Reason                string `json:"reason"`
	}
	if !decodeStrictSupportJSON(w, request.Body, 4096, &body) {
		return
	}
	if strings.TrimSpace(body.ReconcilePhase) != "" {
		rec, err := cfg.Controller.ReconcileConflict(request.Context(), body.TenantID, body.ReplacementID, body.ReconcilePhase, body.Reason)
		if err != nil {
			writeSupportReplacementError(w, err)
			return
		}
		writeSupportReplacementJSON(w, http.StatusOK, map[string]any{"ok": true, "replacement": rec})
		return
	}
	if strings.TrimSpace(body.IdempotencyKey) == "" {
		http.Error(w, "idempotencyKey is required", http.StatusBadRequest)
		return
	}
	rec, err := cfg.Controller.Get(request.Context(), body.TenantID, body.ReplacementID)
	if err != nil {
		writeSupportReplacementError(w, err)
		return
	}
	if owner := strings.TrimSpace(body.OwnerSubjectID); owner != "" && owner != rec.OwnerSubjectID {
		http.Error(w, "owner mismatch", http.StatusConflict)
		return
	}
	jobID, err := cfg.Controller.EnqueueRun(
		request.Context(), rec.TenantID, rec.OwnerSubjectID, rec.StackID,
		rec.ReplacementID, body.IdempotencyKey, body.ForcePreWriteRollback, body.Reason,
	)
	if err != nil {
		writeSupportReplacementError(w, err)
		return
	}
	writeSupportReplacementJSON(w, http.StatusAccepted, map[string]any{"ok": true, "job_id": jobID, "replacement": rec})
}

func serveSupportReplacementList(w http.ResponseWriter, request *http.Request, cfg SupportReplacementRouteConfig, claims *servicecall.Caller) {
	q := request.URL.Query()
	tenantID := strings.TrimSpace(q.Get("tenantId"))
	replacementID := strings.TrimSpace(q.Get("replacementId"))
	phase := strings.TrimSpace(q.Get("phase"))
	ownerID := strings.TrimSpace(q.Get("ownerSubjectId"))
	if tenantID == "" {
		http.Error(w, "tenantId query parameter is required", http.StatusBadRequest)
		return
	}
	if claims.Service != SupportReplacementStaffCaller && ownerID == "" {
		http.Error(w, "ownerSubjectId is required", http.StatusForbidden)
		return
	}
	if replacementID != "" {
		rec, err := cfg.Controller.Get(request.Context(), tenantID, replacementID)
		if err != nil {
			writeSupportReplacementError(w, err)
			return
		}
		if claims.Service != SupportReplacementStaffCaller && rec.OwnerSubjectID != ownerID {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		writeSupportReplacementJSON(w, http.StatusOK, map[string]any{"ok": true, "replacement": rec})
		return
	}
	items, err := cfg.Controller.List(request.Context(), tenantID, phase)
	if err != nil {
		writeSupportReplacementError(w, err)
		return
	}
	if claims.Service != SupportReplacementStaffCaller {
		filtered := items[:0]
		for _, item := range items {
			if item.OwnerSubjectID == ownerID {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}
	writeSupportReplacementJSON(w, http.StatusOK, map[string]any{"ok": true, "replacements": items})
}

func decodeStrictSupportJSON(w http.ResponseWriter, body io.Reader, maxBytes int64, dst any) bool {
	decoder := json.NewDecoder(io.LimitReader(body, maxBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return false
	}
	return true
}

func writeSupportReplacementError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, providercontrol.ErrInvalidRequest):
		status = http.StatusBadRequest
	case errors.Is(err, providercontrol.ErrOperationNotFound):
		status = http.StatusNotFound
	case errors.Is(err, providercontrol.ErrNativeAdmissionConflict):
		status = http.StatusConflict
	case errors.Is(err, providercontrol.ErrProfileUnavailable):
		status = http.StatusServiceUnavailable
	}
	writeSupportReplacementJSON(w, status, map[string]any{"ok": false, "error": err.Error()})
}

func writeSupportReplacementJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
