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

	"github.com/kombifyio/homelab/internal/executionchannel"
	"github.com/kombifyio/homelab/internal/guardbootstrap"
	"github.com/kombifyio/homelab/pkg/config"
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/monthlyruntime"
	"github.com/kombifyio/homelab/pkg/serviceauth"
)

// GuardReenrollPath runs the managed Guard re-enrollment for one node
// (executionchannel.Channel.ReenrollGuard). It is a machine route: only the
// operations service identity may call it, with an Ed25519 token bound to this
// exact request, so an agent triggers it without any owner session.
const (
	GuardReenrollPath   = "/api/v1/internal/managed-runtime/guard-reenroll"
	GuardReenrollCaller = "homelab-operations"
	GuardReenrollScope  = "homelab:guard-reenroll"
)

// GuardReenrollRouteConfig carries the composed execution channel and the
// capability issuer of the serving process.
type GuardReenrollRouteConfig struct {
	Channel *executionchannel.Channel
	// Issuer opens the enrollment issuer on the application database.
	Issuer func() (*guardbootstrap.EnrollmentIssuer, error)
	// Version is the serving release; the node is brought to the Guard of the
	// same release its install.sh serves.
	Version string
}

// RegisterGuardReenrollRoute exposes POST GuardReenrollPath.
func RegisterGuardReenrollRoute(r *httpx.Router, cfg GuardReenrollRouteConfig) {
	if r == nil {
		return
	}
	r.POST(GuardReenrollPath, func(e *httpx.Event) error {
		auth, err := serviceauth.Apply(servicecall.Config{ServiceName: "homelab", Enabled: true, RequireEd25519: true, AllowedCallers: []string{GuardReenrollCaller}})
		if err != nil || cfg.Channel == nil || cfg.Issuer == nil {
			http.Error(e.Response, "guard re-enrollment unavailable", http.StatusServiceUnavailable)
			return nil
		}
		auth.Secret, auth.SecretNext = "", ""
		servicecall.RequireBoundServiceAuth(servicecall.BoundRequestConfig{Config: auth, RequiredScope: GuardReenrollScope, MaxBodyBytes: 1024})(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			serveGuardReenroll(w, request, cfg)
		})).ServeHTTP(e.Response, e.Request)
		return nil
	})
}

func serveGuardReenroll(w http.ResponseWriter, request *http.Request, cfg GuardReenrollRouteConfig) {
	caller := servicecall.FromContext(request.Context())
	if caller == nil || caller.Service != GuardReenrollCaller || caller.ExpiresAt.Sub(caller.IssuedAt) <= 0 || caller.ExpiresAt.Sub(caller.IssuedAt) > time.Minute {
		http.Error(w, "identity denied", http.StatusForbidden)
		return
	}
	var body struct {
		TenantID string `json:"tenantId"`
		LeaseID  string `json:"leaseId"`
	}
	decoder := json.NewDecoder(io.LimitReader(request.Body, 1025))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&body) != nil || decoder.Decode(&struct{}{}) != io.EOF || !guardReenrollID(body.TenantID) || !guardReenrollID(body.LeaseID) {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	issuer, err := cfg.Issuer()
	if err != nil {
		http.Error(w, "enrollment issuer unavailable", http.StatusServiceUnavailable)
		return
	}
	// The installer run outlives a client that gives up: the node must not be
	// left half-moved because a caller's connection dropped.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(request.Context()), 20*time.Minute)
	defer cancel()
	record, err := cfg.Channel.ReenrollGuard(ctx, executionchannel.GuardReenrollRequest{
		TenantID: body.TenantID, LeaseID: body.LeaseID, Actor: "service:" + caller.Service,
		TargetVersion: cfg.Version, ControlPlaneURL: config.PublicOriginFromEnv(),
		Capability: func(ctx context.Context, server controlplane.ServerRuntime, attemptID string) (string, error) {
			return issuer.IssueReenrollCapability(ctx, guardbootstrap.ReenrollRequest{
				TenantID: server.TenantID, LeaseID: server.LeaseID, StackID: server.StackID, ServerID: server.ID, AttemptID: attemptID,
			})
		},
	})
	status := http.StatusOK
	switch {
	case err == nil:
	case errors.Is(err, executionchannel.ErrGuardReenrollRefused), errors.Is(err, executionchannel.ErrGuardReenrollInProgress):
		// A run already holds the node, or the node cannot be re-enrolled;
		// in both cases nothing ran on the host for this call.
		status = http.StatusConflict
	case errors.Is(err, monthlyruntime.ErrNodeChannelUnavailable), errors.Is(err, executionchannel.ErrGuardReenrollUnconfirmed):
		// The node was unreachable, the installer failed, or the new Guard
		// gave no accepted heartbeat at the target version in time
		// (state installed_unconfirmed).
		status = http.StatusBadGateway
	default:
		status = http.StatusInternalServerError
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"result": record, "ok": err == nil})
}

func guardReenrollID(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && len(value) <= 256 && !strings.ContainsAny(value, "\r\n\t ")
}
