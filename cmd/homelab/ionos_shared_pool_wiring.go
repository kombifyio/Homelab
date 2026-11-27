package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/kombifyio/homelab/internal/ionospooltunnel"
	"github.com/kombifyio/homelab/internal/providercontrol"
	"github.com/kombifyio/homelab/internal/routes"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/middleware"
)

func registerIONOSSharedPool(router *httpx.Router, deps routeDeps) func() ionospooltunnel.Status {
	if os.Getenv("HOMELAB_IONOS_SHARED_POOL_ENABLED") != "true" {
		return nil
	}
	tunnel, err := configureIONOSSharedPool(router, deps)
	if err != nil {
		readiness := &ionospooltunnel.ReadinessError{Code: "pool_configuration_invalid"}
		_ = errors.As(err, &readiness)
		routes.RegisterIONOSSharedPoolRoutes(router, func(context.Context, providercontrol.IONOSPoolIdentity, providercontrol.IONOSPoolRequest) (providercontrol.IONOSSharedPoolObservation, error) {
			return providercontrol.IONOSSharedPoolObservation{}, readiness
		})
		if deps.log != nil {
			deps.log.Error("ionos_shared_pool_unavailable", "reason_code", readiness.Code)
		}
		return func() ionospooltunnel.Status {
			return ionospooltunnel.Status{Enabled: true, State: "unavailable", Error: readiness}
		}
	}
	deps.startup.poolTunnel = tunnel
	return tunnel.Status
}

func configureIONOSSharedPool(router *httpx.Router, deps routeDeps) (*ionospooltunnel.Tunnel, error) {
	if deps.startup == nil || !deps.startup.cfg.DeploymentMode.IsSaaS() || deps.v2 == nil || deps.v2.db == nil || deps.featureSvc == nil {
		return nil, fmt.Errorf("IONOS pool requires hosted database and feature authority")
	}
	var node providercontrol.IONOSSharedPoolNode
	var apps map[string]providercontrol.IONOSPoolApplication
	if json.Unmarshal([]byte(os.Getenv("HOMELAB_IONOS_SHARED_POOL_NODE")), &node) != nil || json.Unmarshal([]byte(os.Getenv("HOMELAB_IONOS_SHARED_POOL_APPS")), &apps) != nil {
		return nil, fmt.Errorf("IONOS pool requires admitted node and curated image configuration")
	}
	tunnel, client, err := ionospooltunnel.OpenHTTPClient(
		os.Getenv("HOMELAB_IONOS_SHARED_POOL_ORIGIN"),
		os.Getenv("HOMELAB_IONOS_SHARED_POOL_WIREGUARD_FILE"),
		os.Getenv("HOMELAB_IONOS_SHARED_POOL_CA_FILE"),
		os.Getenv("HOMELAB_IONOS_SHARED_POOL_CLIENT_CERT_FILE"),
		os.Getenv("HOMELAB_IONOS_SHARED_POOL_CLIENT_KEY_FILE"),
		providercontrol.IONOSSharedPoolMaxCommandDuration,
	)
	if err != nil {
		return nil, err
	}
	admitted := false
	defer func() {
		if !admitted {
			tunnel.Close()
		}
	}()
	runtime, err := providercontrol.NewIONOSSharedPoolHTTPRuntime(os.Getenv("HOMELAB_IONOS_SHARED_POOL_ORIGIN"), client)
	if err != nil {
		return nil, err
	}
	budgetReader, err := newManagedAppBudgetReader()
	if err != nil {
		return nil, err
	}
	admission := func(ctx context.Context, id providercontrol.IONOSPoolIdentity, req providercontrol.IONOSPoolRequest) (providercontrol.ManagedAppBudget, error) {
		grants, ok := middleware.SignedEntitlementsFromContext(ctx)
		if !ok || !grants.Has(routes.InventoryEntitlementOperate) || (req.Action == providercontrol.IONOSPoolProvision && !grants.Has(routes.InventoryEntitlementProvision)) {
			return providercontrol.ManagedAppBudget{}, &providercontrol.ManagedAppRefusal{Code: "entitlement_refused", Repair: "Obtain a current signed workload operation entitlement."}
		}
		// Cleanup still requires current signed identity/authority and an existing
		// exact reservation, but never creates capacity or calls Cloud commerce.
		if providercontrol.IsManagedAppCleanup(string(req.Action)) {
			return providercontrol.ManagedAppBudget{}, nil
		}
		enabled, err := deps.featureSvc.IsEnabled(ctx, providercontrol.IONOSSharedPoolFlag, id.SubjectID)
		if err != nil || !enabled {
			return providercontrol.ManagedAppBudget{}, &providercontrol.ManagedAppRefusal{Code: "module_disabled", Repair: "Enable the default-off IONOS Managed Apps module for this tester."}
		}
		return budgetReader.Read(ctx, id)
	}
	control, err := providercontrol.NewIONOSSharedPoolControl(deps.v2.db.DB, node, runtime, runtime, apps, admission)
	if err != nil {
		return nil, err
	}
	executor := routes.ManagedPoolExecutor(func(ctx context.Context, id providercontrol.IONOSPoolIdentity, request providercontrol.IONOSPoolRequest) (providercontrol.IONOSSharedPoolObservation, error) {
		if status := tunnel.Status(); status.Error != nil {
			return providercontrol.IONOSSharedPoolObservation{}, status.Error
		}
		return control.Execute(ctx, id, request)
	})
	routes.RegisterIONOSSharedPoolRoutes(router, executor)
	deps.startup.managedPoolExecutor = executor
	admitted = true
	return tunnel, nil
}
