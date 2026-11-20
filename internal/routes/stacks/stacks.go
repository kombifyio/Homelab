// Package stacks provides the main entry point for stack-related API routes.
// It aggregates all stack handlers including CRUD operations and import/export.
package stacks

import (
	"github.com/kombifyio/homelab/internal/routes/auth"

	productnotifications "github.com/kombifyio/homelab/internal/notifications"
	"github.com/kombifyio/homelab/pkg/config"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/jobs"
	"github.com/kombifyio/homelab/pkg/orchestrator"
)

func RegisterRuntimeRoutesWithModeAndFeatures(r *httpx.Router, runtime auth.RuntimeStore, orch *orchestrator.Orchestrator, mode config.DeploymentMode, featureChecker managedRuntimeFeatureChecker, managedLeases jobs.ManagedLeaseManager, notificationOutbox productnotifications.ProductEventEnqueuer) {
	// Register CRUD routes for stack management
	// Handles: POST /api/v1/stacks, POST /api/v1/stacks/{id}/provision, etc.
	RegisterCRUDRuntimeRoutes(r, runtime, orch, mode, featureChecker, managedLeases, notificationOutbox)

	// Register import/export routes for configuration management
	// Handles: GET /api/v1/stacks/{id}/export, POST /api/v1/stacks/import
	RegisterImportExportRuntimeRoutes(r, runtime, orch, mode, featureChecker)

	// Register spec access routes for persisted specs
	// Handles: GET /api/v1/stacks/{id}/requirements, GET /api/v1/stacks/{id}/unified, etc.
	RegisterSpecRoutes(r)
}
