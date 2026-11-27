package main

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/kombifyio/homelab/pkg/auth/serverownerfga"
	"github.com/kombifyio/homelab/pkg/config"
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/logger"
	"github.com/kombifyio/homelab/pkg/serverregistry"
	"github.com/kombifyio/homelab/pkg/serviceauth"
)

const (
	envServerOwnerFGAProjectionEnabled  = "HOMELAB_SERVER_OWNER_FGA_PROJECTION_ENABLED"
	envServerOwnerFGAProjectionInterval = "HOMELAB_SERVER_OWNER_FGA_PROJECTION_INTERVAL"

	defaultServerOwnerFGAProjectionInterval = 2 * time.Minute
)

// serverOwnerProjectionSource reads canonical server-registry heads for
// periodic full-aggregate FGA owner reconciliation under tenant RLS.
type serverOwnerProjectionSource struct {
	store *controlplane.PostgresStore
}

func (s serverOwnerProjectionSource) ListServerProjectionTenants(ctx context.Context, afterTenantID string, limit int) ([]string, error) {
	return s.store.ListServerProjectionTenants(ctx, afterTenantID, limit)
}

func (s serverOwnerProjectionSource) ListProjectedServers(ctx context.Context, tenantID string) ([]serverregistry.ProjectedServer, error) {
	runtimes, err := s.store.ListServerRuntimesByTenant(ctx, tenantID, "")
	if err != nil {
		return nil, err
	}
	out := make([]serverregistry.ProjectedServer, 0, len(runtimes))
	for _, rt := range runtimes {
		out = append(out, projectedServerForFGA(rt))
	}
	return out, nil
}

func projectedServerForFGA(rt controlplane.ServerRuntime) serverregistry.ProjectedServer {
	return serverregistry.ProjectedServer{
		ServerID:          strings.TrimSpace(rt.ID),
		TenantID:          strings.TrimSpace(rt.TenantID),
		OwnerID:           strings.TrimSpace(rt.OwnerSubjectID),
		LifecycleState:    strings.TrimSpace(rt.LifecycleState),
		AggregateRevision: rt.Revision,
		Retired:           rt.DecommissionedAt != nil,
	}
}

// bootServerOwnerFGAProjection wires Homelab's per-server FGA owner projector
// (`server#owner` tuples in Administration). It stays safely inert in
// standalone/self-hosted deployments, secondary channels without shared work,
// explicit opt-out, or local SaaS without an explicit Administration URL or
// service-auth signing credential.
func bootServerOwnerFGAProjection(cfg *config.Config, boot *v2Boot, log *logger.Logger) (*serverregistry.PlatformProjector, *serverownerfga.EventDispatcher) {
	if cfg == nil || !cfg.DeploymentMode.IsSaaS() || !config.RunsSharedWork() {
		return nil, nil
	}
	if !envBoolDefault(envServerOwnerFGAProjectionEnabled, true) {
		log.Info("server_owner_fga_projection_disabled", "reason", "explicit_env_disabled")
		return nil, nil
	}
	if boot == nil || boot.db == nil || boot.db.DB == nil {
		log.Warn("server_owner_fga_projection_disabled", "reason", "database_unavailable")
		return nil, nil
	}
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("HOMELAB_ADMINISTRATION_URL")), "/")
	if baseURL == "" {
		if cfg.IsLocal() {
			log.Info("server_owner_fga_projection_disabled", "reason", "local_administration_url_unset")
			return nil, nil
		}
		baseURL = serverownerfga.DefaultAdministrationURL
	}
	secret := os.Getenv("SERVICE_AUTH_SECRET")
	if !serviceauth.CanSign(secret) {
		log.Warn("server_owner_fga_projection_disabled", "reason", "service_auth_signing_unavailable")
		return nil, nil
	}
	client, err := serverownerfga.NewClient(serverownerfga.ClientConfig{
		BaseURL:           baseURL,
		ServiceAuthSecret: secret,
	})
	if err != nil {
		log.Error("server_owner_fga_projection_config_invalid", "error", err)
		return nil, nil
	}
	store := controlPlanePostgresStore(boot)
	projector, err := serverregistry.NewPlatformProjector(serverregistry.PlatformProjectorConfig{
		Source:    serverOwnerProjectionSource{store: store},
		Publisher: client,
		Interval:  durationFromEnv(envServerOwnerFGAProjectionInterval, defaultServerOwnerFGAProjectionInterval),
		BatchSize: serverownerfga.MaxBatchSize,
		Logger:    log.Logger,
		OnError: func(err error) {
			log.Warn("server_owner_fga_projection_failed", "error", err)
		},
	})
	if err != nil {
		log.Error("server_owner_fga_projector_init_failed", "error", err)
		return nil, nil
	}
	dispatcher, err := serverownerfga.NewEventDispatcher(serverownerfga.EventDispatcherConfig{
		Projector: client,
		Logger:    log.Logger,
	})
	if err != nil {
		log.Error("server_owner_fga_dispatcher_init_failed", "error", err)
		return nil, nil
	}
	log.Info("server_owner_fga_projection_enabled", "endpoint", baseURL+serverownerfga.EndpointPath)
	return projector, dispatcher
}
