package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"github.com/kombifyio/homelab/pkg/config"
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/db"
)

// The governed Stage run checks that the previous source's public self-hosted
// build starts on the head-migrated Local schema. It starts on a secondary
// channel so it runs no shared work, and a secondary channel only reads its
// default tenant (ensureDefaultControlPlaneTenant). On a self-hosted install
// the primary's first Stable boot creates that row; kombify-stg-pg has no such
// primary, so the Local preparation creates the same row once. A repeat
// verifies the existing row and changes nothing.
func prepareStagingLocalTenant(ctx context.Context) error {
	if os.Getenv("HOMELAB_DATABASE_CONTEXT") != "stage-development" ||
		os.Getenv(db.EnvAllowDBGenesis) != "false" || os.Getenv(db.EnvDatabaseMigrationMode) != "verify" ||
		config.RejectRetiredReleaseChannel() != nil {
		return errDatabaseDeliveryAdmission
	}
	for _, name := range []string{db.EnvMigrationDatabaseURL, "HOMELAB_ADVANCED_ISSUER_DATABASE_URL",
		"HOMELAB_PROVIDER_CONTROL_RUNTIME_DATABASE_URL", "HOMELAB_ENCRYPTION_KEY"} {
		if os.Getenv(name) != "" {
			return errDatabaseDeliveryAdmission
		}
	}
	if err := stagingDeliveryBinding(os.Getenv("DATABASE_URL"), db.HostedApplicationRole); err != nil {
		return err
	}
	cfg, err := db.ConfigFromEnv()
	if err != nil {
		return err
	}
	application, err := db.OpenContext(ctx, cfg)
	if err != nil {
		return err
	}
	defer application.Close()
	if err := db.VerifyExpectedDatabaseIdentity(ctx, application.DB); err != nil {
		return err
	}
	if err := db.VerifyRuntimeRolePosture(ctx, application.DB, db.RuntimeRoleHosted); err != nil {
		return err
	}
	if err := application.VerifyMigrations(ctx); err != nil {
		return err
	}
	tenantID := v2DefaultTenantIDFromEnv()
	store := controlplane.NewPostgresStore(application.DB)
	tenant, err := store.GetTenant(ctx, tenantID)
	created := false
	if errors.Is(err, controlplane.ErrNotFound) {
		// The exact row a self-hosted primary's startup ensures.
		tenant, err = store.EnsureTenant(ctx, controlplane.Tenant{
			ID: tenantID, DisplayName: tenantID, Kind: "self_hosted", Status: "active",
		})
		created = err == nil
	}
	if err != nil {
		return err
	}
	if tenant.ID != tenantID || tenant.Status != "active" {
		return errors.New("staging Local default tenant is not active")
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"schema": "homelab.database-delivery/v1", "operation": "local-tenant", "status": "acknowledged",
		"source_sha": buildRevision, "manifest_sha256": db.DeliveryForwardManifestDigest(),
		"phase": "staging", "context": "stage-development", "database_identity": stagingDeliveryIdentity,
		"tenant_id": tenantID, "tenant_kind": tenant.Kind, "application_role": db.HostedApplicationRole,
		"migration_mode": "verify", "genesis": false, "database_effects": created,
		"plan_digest": "", "release_id": "",
	})
}
