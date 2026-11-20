// Package routes provides REST API routes for kombifyHomelab.
// Auth routes have been refactored into the auth/ subpackage.
package routes

import (
	"github.com/kombifyio/homelab/internal/routes/auth"
	"github.com/kombifyio/homelab/pkg/config"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/kombifywallet"
)

type LocalOwnerSetup = auth.LocalOwnerSetup
type LocalSetupProvisioner = auth.LocalSetupProvisioner

type AuthRouteConfig struct {
	Runtime               auth.RuntimeStore
	PortalSession         auth.PortalSession
	LocalSetupProvisioner LocalSetupProvisioner
	LocalOwnerStore       auth.LocalOwnerLookup
	WalletSource          kombifywallet.Source
}

// RegisterAuthRoutesWithConfig registers auth routes with runtime-only
// dependencies such as the V2 local setup provisioner.
func RegisterRuntimeAuthRoutesWithConfig(r *httpx.Router, mode config.DeploymentMode, edition config.Edition, cfg AuthRouteConfig) {
	auth.RegisterRuntimeRoutesWithConfig(r, mode, edition, auth.RouteConfig{
		Runtime:               cfg.Runtime,
		PortalSession:         cfg.PortalSession,
		LocalSetupProvisioner: cfg.LocalSetupProvisioner,
		LocalOwnerStore:       cfg.LocalOwnerStore,
		WalletSource:          cfg.WalletSource,
	})
}

// RegisterInternalRoutes registers internal API routes for trusted service-to-service
// communication (Edge → Stack). Only active in SaaS deployment mode.
//
// Routes:
//   - POST /api/internal/sso/exchange - Exchange Edge identity for PB token
//   - POST /api/internal/feature-flags/apply - Push feature flag overrides
func RegisterInternalRoutes(r *httpx.Router, mode config.DeploymentMode) {
	auth.RegisterInternalRoutes(r, mode)
}
