package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	ksapi "github.com/kombifyio/homelab/pkg/api"
	"github.com/kombifyio/homelab/pkg/guardreach"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/middleware"
)

// serverReachStepUpMaxAge bounds the multi-factor sign-in behind publishing a
// server at a public address.
const serverReachStepUpMaxAge = 300 * time.Second

const (
	reachReasonStepUp      = "server_reach_step_up_required"
	reachReasonUnavailable = "guard_reach_unavailable"
	reachReasonInvalid     = "public_reach_invalid"
)

// ServerReachControl is the owner's side of kombify Guard reach: publishing one
// server at a public kombify.me address, or withdrawing it.
type ServerReachControl interface {
	SetPublic(ctx context.Context, tenantID, workerID, ownerSubject string, want guardreach.PublicReach) (*guardreach.Row, error)
}

type ServerPaperworkSignInControl interface {
	SetPaperworkSignIn(context.Context, string, string, string, bool) (*guardreach.Row, error)
}

func (h serverRuntimeHandlers) reachSetPaperworkSignIn(e *httpx.Event) error {
	const capability = "homelab.servers.reach.paperwork-sign-in"
	server, err := h.ownedServer(e, capability)
	if err != nil || server == nil {
		return err
	}
	ctx := e.Request.Context()
	var request struct {
		Enabled       *bool `json:"enabled"`
		OwnerApproved bool  `json:"owner_approved"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(e.Response, e.Request.Body, 256))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(new(json.RawMessage)) != io.EOF || request.Enabled == nil {
		return httpx.BadRequest(e, "Specify whether to enable linked Paperwork sign-in", nil)
	}
	if *request.Enabled {
		claims, verified := middleware.VerifiedStepUpFromContext(ctx)
		if !verified || claims.PrincipalType != middleware.PrincipalTypeUser || claims.AgentID != "" || !request.OwnerApproved {
			return httpx.Error(e, http.StatusForbidden, ksapi.ErrCodeForbidden, "The server owner must confirm this action directly", nil)
		}
	}
	workerID := strings.TrimSpace(server.WorkerID)
	if workerID == "" {
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "This server is not enrolled for reach yet", nil)
	}
	owner, _, _ := authenticatedUser(e)
	control, ok := h.reach.(ServerPaperworkSignInControl)
	if !ok {
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Linked Paperwork sign-in is unavailable", nil)
	}
	row, err := control.SetPaperworkSignIn(ctx, server.TenantID, workerID, owner, *request.Enabled)
	switch {
	case errors.Is(err, guardreach.ErrNotFound), errors.Is(err, guardreach.ErrPaperworkSignInNotReady):
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "The current Guard and its Paperwork route must be ready", nil)
	case err != nil:
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Linked Paperwork sign-in is unavailable", nil)
	default:
		slog.Warn("guard_paperwork_sign_in_set", "tenant_id", server.TenantID, "worker_id", workerID, "actor", owner, "enabled", *request.Enabled)
		return httpx.Success(e, http.StatusOK, guardreach.ViewOf(*row))
	}
}

type serverReachRequest struct {
	Enabled      bool   `json:"enabled"`
	Service      string `json:"service"`
	TargetPort   int    `json:"target_port"`
	TargetScheme string `json:"target_scheme"`
}

// reachSetPublic publishes or withdraws one server. Nothing is published unless
// the owner, or an agent acting for the owner, enables it here with an explicit
// service name and port: the relay authenticates the Guard, not the visitor, so
// that decision is what answers on that port. Enabling needs the maintenance
// entitlement and the person's fresh second factor, or an agent acting for the
// person (section 5e); withdrawing is never gated. Ownership of the server in the tenant
// (ownedServer) plus the signed entitlement is the whole authorization, as for
// owner maintenance: no inventory FGA `caller` tuple is required, because no
// writer provisions one for an owner's own server (live 2026-10-18: every
// owner got 403 inventory_access_denied before the step-up was reached).
func (h serverRuntimeHandlers) reachSetPublic(e *httpx.Event) error {
	server, err := h.ownedServer(e, "homelab.servers.reach.set")
	if err != nil || server == nil {
		return err
	}
	control, ok := h.reach.(ServerReachControl)
	if !ok {
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Guard reach is unavailable", map[string]any{"reason": reachReasonUnavailable})
	}
	if strings.TrimSpace(server.WorkerID) == "" {
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "This server has no kombify Guard", map[string]any{"reason": postureReasonNotReady})
	}
	ownerID, _, _ := authenticatedUser(e)
	raw, readErr := io.ReadAll(io.LimitReader(e.Request.Body, 4097))
	if readErr != nil || len(raw) > 4096 {
		return httpx.BadRequest(e, "Invalid request body", nil)
	}
	var request serverReachRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return httpx.BadRequest(e, "Invalid request body", nil)
	}
	want := guardreach.PublicReach{Enabled: request.Enabled, Service: request.Service, Scheme: request.TargetScheme, Port: request.TargetPort}
	if _, invalid := want.Normalize(); invalid != nil {
		return httpx.Error(e, http.StatusBadRequest, ksapi.ErrCodeBadRequest, invalid.Error(), map[string]any{
			"reason": reachReasonInvalid, "retryable": false,
			"guidance": "Name the service (lowercase letters, digits and hyphens, up to 14 characters) and the port it listens on.",
		})
	}
	agentID := ""
	if request.Enabled {
		gate := entitlementGateOrHosted(h.postureEntitlements)
		if !gate.SignedGrant(e.Request.Context(), ServerMaintenanceEntitlement) {
			return writeEntitlementDenial(e, "homelab.servers.reach.set", ServerMaintenanceEntitlement)
		}
		var admitted bool
		if agentID, admitted = freshPersonOrActingAgent(e.Request.Context(), gate, h.now().UTC(), serverReachStepUpMaxAge); !admitted {
			message, details := stepUpRefusal(e.Request.Context(), "A fresh multi-factor sign-in is required to publish a server at a public address", reachReasonStepUp)
			return httpx.Error(e, http.StatusForbidden, ksapi.ErrCodeForbidden, message, details)
		}
	}
	row, setErr := control.SetPublic(e.Request.Context(), server.TenantID, strings.TrimSpace(server.WorkerID), ownerID, want)
	switch {
	case errors.Is(setErr, guardreach.ErrPublicReachInvalid):
		return httpx.BadRequest(e, setErr.Error(), nil)
	case errors.Is(setErr, guardreach.ErrNotFound):
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "This server is not enrolled for reach yet", map[string]any{"reason": postureReasonNotReady})
	case setErr != nil:
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Guard reach is unavailable", map[string]any{"reason": reachReasonUnavailable})
	}
	slog.Warn("guard_reach_public_set", "tenant_id", server.TenantID, "worker_id", server.WorkerID, "enabled", request.Enabled,
		"service", want.Service, "target_port", want.Port, "actor", ownerID, "agent_id", agentID)
	return httpx.Success(e, http.StatusOK, guardreach.ViewOf(*row))
}
