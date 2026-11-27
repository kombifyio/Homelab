package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kombifyio/homelab/internal/gocommon/identity"
	"github.com/kombifyio/homelab/internal/gocommon/servicecall"
	"github.com/kombifyio/homelab/internal/gocommon/tenantdb"
	"github.com/kombifyio/homelab/internal/providercontrol"
	"github.com/kombifyio/homelab/internal/routes/tenantguard"
	"github.com/kombifyio/homelab/pkg/httpx"
)

func TestRequireAuthAcceptsSignedEdgeIdentity(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/v1/discovery/networks", nil)
	req = req.WithContext(identity.NewContext(context.Background(), &identity.Identity{UserID: "api-key:homelab"}))
	e := &httpx.Event{
		Request:  req,
		Response: httptest.NewRecorder(),
	}

	got, err := requireAuth(e)
	if err != nil {
		t.Fatalf("requireAuth returned error: %v", err)
	}
	if got != "api-key:homelab" {
		t.Fatalf("requireAuth returned %q, want edge identity user ID", got)
	}
}

func TestRequireAuthRejectsMissingIdentity(t *testing.T) {
	rec := httptest.NewRecorder()
	e := &httpx.Event{
		Request:  httptest.NewRequest("GET", "/api/v1/discovery/networks", nil),
		Response: rec,
	}

	got, err := requireAuth(e)
	if err == nil {
		t.Fatal("requireAuth must return an error once it has written the refusal")
	}
	if got != "" {
		t.Fatalf("requireAuth returned %q, want empty user ID", got)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("requireAuth status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestAuthenticatedUserDetectsSelfHostedOwnerRole(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/v1/features", nil)
	req = req.WithContext(identity.NewContext(context.Background(), &identity.Identity{
		UserID: "breakglass:local-owner",
		Roles:  []string{"operator", "admin"},
	}))
	e := &httpx.Event{
		Request:  req,
		Response: httptest.NewRecorder(),
	}

	got, isOwner, ok := authenticatedUser(e)
	if !ok {
		t.Fatal("authenticatedUser rejected identity")
	}
	if got != "breakglass:local-owner" {
		t.Fatalf("authenticatedUser returned %q, want identity user ID", got)
	}
	if !isOwner {
		t.Fatal("authenticatedUser did not detect the self-hosted owner")
	}
}

// A kombify platform role is never owner authority: not on a self-hosted
// installation beyond its own local "admin" membership, and not at all in the
// hosted product.
func TestAuthenticatedUserIgnoresPlatformRoles(t *testing.T) {
	platformRoles := []string{"super_admin", "global_admin", "developer", "manager"}
	for _, hosted := range []bool{false, true} {
		tenantguard.Configure(hosted)
		t.Cleanup(func() { tenantguard.Configure(false) })
		roleSets := [][]string{platformRoles}
		if hosted {
			roleSets = append(roleSets, []string{"admin"}, []string{"admin", "global_admin"})
		}
		for _, roles := range roleSets {
			req := httptest.NewRequest("GET", "/api/v1/features", nil)
			req = req.WithContext(identity.NewContext(context.Background(), &identity.Identity{
				UserID: "auth0|platform-user",
				Roles:  roles,
			}))
			e := &httpx.Event{Request: req, Response: httptest.NewRecorder()}

			_, isOwner, ok := authenticatedUser(e)
			if !ok {
				t.Fatal("authenticatedUser rejected identity")
			}
			if isOwner {
				t.Fatalf("hosted=%v roles=%v counted as instance owner", hosted, roles)
			}
		}
	}
}

// A lease read or write whose Edge-signed database context names another
// subject or organization than the tenant it asks for is the caller's missing
// reach. Every monthly-runtime route maps it to a structured 403, never to the
// 502 of a runtime failure.
func TestMonthlyRuntimeErrorRefusesTenantContextMismatch(t *testing.T) {
	mismatch := fmt.Errorf("lease read: %w", tenantdb.ErrContextMismatch)
	for name, mapper := range map[string]func(*httpx.Event) error{
		"operation error":    func(e *httpx.Event) error { return monthlyRuntimeError(e, mismatch) },
		"decommission error": func(e *httpx.Event) error { return monthlyRuntimeDecommissionError(e, nil, mismatch, "lease-1") },
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			e := &httpx.Event{Request: httptest.NewRequest(http.MethodGet, "/api/v1/monthly-runtimes/lease-1", nil), Response: rec}
			_ = mapper(e)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Error struct {
					Details map[string]any `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if got := body.Error.Details["reason_code"]; got != "tenant_context_mismatch" {
				t.Fatalf("reason_code = %v, want tenant_context_mismatch", got)
			}
		})
	}
}

type stubSupportReplacementController struct {
	rec providercontrol.SupportReplacementRecord
}

func (s *stubSupportReplacementController) Authorize(_ context.Context, req providercontrol.AuthorizeSupportReplacementRequest) (providercontrol.SupportReplacementRecord, error) {
	if !req.NoChargeConfirmed {
		return providercontrol.SupportReplacementRecord{}, fmt.Errorf("%w: no_charge_confirmed required", providercontrol.ErrInvalidRequest)
	}
	if req.TargetProductSlug != "" && req.TargetProductSlug != providercontrol.SupportReplacementTargetProductSlug {
		return providercontrol.SupportReplacementRecord{}, fmt.Errorf("%w: wrong target", providercontrol.ErrInvalidRequest)
	}
	s.rec = providercontrol.SupportReplacementRecord{
		TenantID:          req.TenantID,
		ReplacementID:     "sr-1",
		OwnerSubjectID:    "owner-1",
		StackID:           req.StackID,
		SourceLeaseID:     req.SourceLeaseID,
		NoChargeConfirmed: true,
		TargetProductSlug: providercontrol.SupportReplacementTargetProductSlug,
		Phase:             providercontrol.SupportReplacementPhaseAuthorized,
		Created:           true,
	}
	return s.rec, nil
}
func (s *stubSupportReplacementController) Accept(_ context.Context, req providercontrol.AcceptSupportReplacementRequest) (providercontrol.SupportReplacementRecord, error) {
	if s.rec.ReplacementID == "" || s.rec.OwnerSubjectID != req.OwnerSubjectID {
		return providercontrol.SupportReplacementRecord{}, fmt.Errorf("%w: owner mismatch", providercontrol.ErrNativeAdmissionConflict)
	}
	s.rec.Phase = providercontrol.SupportReplacementPhaseAccepted
	return s.rec, nil
}
func (s *stubSupportReplacementController) Get(_ context.Context, _, _ string) (providercontrol.SupportReplacementRecord, error) {
	return s.rec, nil
}
func (s *stubSupportReplacementController) List(_ context.Context, _, _ string) ([]providercontrol.SupportReplacementRecord, error) {
	return []providercontrol.SupportReplacementRecord{s.rec}, nil
}
func (s *stubSupportReplacementController) EnqueueRun(_ context.Context, _, _, _, _, _ string, _ bool, _ string) (string, error) {
	return "job-sr-1", nil
}
func (s *stubSupportReplacementController) ReconcileConflict(_ context.Context, _, _, targetPhase, _ string) (providercontrol.SupportReplacementRecord, error) {
	s.rec.Phase = targetPhase
	return s.rec, nil
}

func TestSupportReplacementRoutesEnforceEd25519StaffAuthAndTargetPinning(t *testing.T) {
	opsPriv, opsPub, err := servicecall.GenerateSigningKey(SupportReplacementStaffCaller, "ops-1")
	if err != nil {
		t.Fatal(err)
	}
	gwPriv, gwPub, err := servicecall.GenerateSigningKey(SupportReplacementGatewayCaller, "gw-1")
	if err != nil {
		t.Fatal(err)
	}
	pubKeys, err := servicecall.ParseCallerPublicKeys(opsPub + "," + gwPub)
	if err != nil {
		t.Fatal(err)
	}
	authCfg := servicecall.Config{
		ServiceName:      "homelab",
		Enabled:          true,
		RequireEd25519:   true,
		CallerPublicKeys: pubKeys,
	}
	ctrl := &stubSupportReplacementController{}
	router := httpx.NewRouter()
	RegisterSupportReplacementRoutes(router, SupportReplacementRouteConfig{
		Controller: ctrl,
		AuthConfig: &authCfg,
	})

	callSigned := func(caller, priv, scope, method, path string, body []byte) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		if priv != "" {
			kid, edKey, parseErr := servicecall.ParseSigningKey(priv)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			binding, bErr := servicecall.NewRequestBinding(method, path, body)
			if bErr != nil {
				t.Fatal(bErr)
			}
			issuerCfg := servicecall.Config{
				ServiceName:  caller,
				Enabled:      true,
				SigningKey:   edKey,
				SigningKeyID: kid,
				Scope:        scope,
				TokenTTL:     30 * time.Second,
			}
			tok, tokErr := servicecall.IssueBoundToken(issuerCfg, "homelab", binding, "req-1")
			if tokErr != nil {
				t.Fatal(tokErr)
			}
			req.Header.Set(servicecall.HeaderServiceAuth, tok)
		}
		router.ServeHTTP(rec, req)
		return rec
	}

	validAuthBody := []byte(`{"tenantId":"tenant-1","sourceLeaseId":"lease-1","stackId":"stack-1","caseRef":"CASE-1","reason":"free upgrade","authorizedBy":"staff@kombify.io","evidenceRef":"ev://1","idempotencyKey":"idem-1","noChargeConfirmed":true}`)

	if got := callSigned("", "", "", http.MethodPost, SupportReplacementAuthorizePath, validAuthBody).Code; got != http.StatusUnauthorized {
		t.Fatalf("unauthenticated authorize = %d, want 401", got)
	}
	if got := callSigned(SupportReplacementGatewayCaller, gwPriv, SupportReplacementAuthorizeScope, http.MethodPost, SupportReplacementAuthorizePath, validAuthBody).Code; got != http.StatusForbidden {
		t.Fatalf("gateway caller authorize = %d, want 403", got)
	}
	badNoCharge := []byte(`{"tenantId":"tenant-1","sourceLeaseId":"lease-1","stackId":"stack-1","caseRef":"CASE-1","reason":"free upgrade","authorizedBy":"staff@kombify.io","evidenceRef":"ev://1","idempotencyKey":"idem-2","noChargeConfirmed":false}`)
	if got := callSigned(SupportReplacementStaffCaller, opsPriv, SupportReplacementAuthorizeScope, http.MethodPost, SupportReplacementAuthorizePath, badNoCharge).Code; got != http.StatusBadRequest {
		t.Fatalf("noChargeConfirmed=false = %d, want 400", got)
	}
	if got := callSigned(SupportReplacementStaffCaller, opsPriv, SupportReplacementAuthorizeScope, http.MethodPost, SupportReplacementAuthorizePath, validAuthBody).Code; got != http.StatusCreated {
		t.Fatalf("valid staff authorize = %d, want 201", got)
	}
	acceptBody := []byte(`{"tenantId":"tenant-1","replacementId":"sr-1","ownerSubjectId":"owner-1","acceptedBy":"owner-1"}`)
	if got := callSigned(SupportReplacementGatewayCaller, gwPriv, SupportReplacementAcceptScope, http.MethodPost, SupportReplacementAcceptPath, acceptBody).Code; got != http.StatusOK {
		t.Fatalf("valid gateway accept = %d, want 200", got)
	}
	acceptNoOwner := []byte(`{"tenantId":"tenant-1","replacementId":"sr-1","acceptedBy":"attacker"}`)
	if got := callSigned(SupportReplacementGatewayCaller, gwPriv, SupportReplacementAcceptScope, http.MethodPost, SupportReplacementAcceptPath, acceptNoOwner).Code; got != http.StatusForbidden {
		t.Fatalf("gateway accept missing ownerSubjectId = %d, want 403", got)
	}
	if got := callSigned(SupportReplacementStaffCaller, opsPriv, SupportReplacementAcceptScope, http.MethodPost, SupportReplacementAcceptPath, acceptNoOwner).Code; got != http.StatusForbidden {
		t.Fatalf("staff accept missing ownerSubjectId = %d, want 403", got)
	}
	acceptMismatchedPrincipal := []byte(`{"tenantId":"tenant-1","replacementId":"sr-1","ownerSubjectId":"owner-1","acceptedBy":"attacker"}`)
	if got := callSigned(SupportReplacementGatewayCaller, gwPriv, SupportReplacementAcceptScope, http.MethodPost, SupportReplacementAcceptPath, acceptMismatchedPrincipal).Code; got != http.StatusForbidden {
		t.Fatalf("gateway accept mismatched acceptedBy = %d, want 403", got)
	}
	if got := callSigned(SupportReplacementStaffCaller, opsPriv, SupportReplacementAcceptScope, http.MethodPost, SupportReplacementAcceptPath, acceptMismatchedPrincipal).Code; got != http.StatusForbidden {
		t.Fatalf("staff accept mismatched acceptedBy = %d, want 403", got)
	}
	acceptWrongOwner := []byte(`{"tenantId":"tenant-1","replacementId":"sr-1","ownerSubjectId":"owner-2","acceptedBy":"owner-2"}`)
	if got := callSigned(SupportReplacementGatewayCaller, gwPriv, SupportReplacementAcceptScope, http.MethodPost, SupportReplacementAcceptPath, acceptWrongOwner).Code; got != http.StatusConflict {
		t.Fatalf("gateway accept wrong ownerSubjectId = %d, want 409", got)
	}
	reconcileBody := []byte(`{"tenantId":"tenant-1","replacementId":"sr-1","reconcilePhase":"completed","reason":"verified"}`)
	if got := callSigned(SupportReplacementGatewayCaller, gwPriv, SupportReplacementOperateScope, http.MethodPost, SupportReplacementRunPath, reconcileBody).Code; got != http.StatusForbidden {
		t.Fatalf("gateway conflict reconcile = %d, want 403", got)
	}
	gwRunBody := []byte(`{"tenantId":"tenant-1","replacementId":"sr-1","ownerSubjectId":"owner-1","idempotencyKey":"run-gw-1"}`)
	if got := callSigned(SupportReplacementGatewayCaller, gwPriv, SupportReplacementOperateScope, http.MethodPost, SupportReplacementRunPath, gwRunBody).Code; got != http.StatusForbidden {
		t.Fatalf("gateway caller run = %d, want 403", got)
	}
	gwForceRollbackBody := []byte(`{"tenantId":"tenant-1","replacementId":"sr-1","ownerSubjectId":"owner-1","idempotencyKey":"run-gw-2","forcePreWriteRollback":true,"reason":"customer cancel"}`)
	if got := callSigned(SupportReplacementGatewayCaller, gwPriv, SupportReplacementOperateScope, http.MethodPost, SupportReplacementRunPath, gwForceRollbackBody).Code; got != http.StatusForbidden {
		t.Fatalf("gateway caller forcePreWriteRollback = %d, want 403", got)
	}
	runMissingIdem := []byte(`{"tenantId":"tenant-1","replacementId":"sr-1"}`)
	if got := callSigned(SupportReplacementStaffCaller, opsPriv, SupportReplacementOperateScope, http.MethodPost, SupportReplacementRunPath, runMissingIdem).Code; got != http.StatusBadRequest {
		t.Fatalf("staff run missing idempotencyKey = %d, want 400", got)
	}
	runBody := []byte(`{"tenantId":"tenant-1","replacementId":"sr-1","idempotencyKey":"run-1"}`)
	if got := callSigned(SupportReplacementStaffCaller, opsPriv, SupportReplacementOperateScope, http.MethodPost, SupportReplacementRunPath, runBody).Code; got != http.StatusAccepted {
		t.Fatalf("valid staff run = %d, want 202", got)
	}
}
