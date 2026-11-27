package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/kombifyio/homelab/internal/providercontrol"
	"github.com/kombifyio/homelab/internal/routes"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/middleware"
)

func registerFlyMachines(router *httpx.Router, deps routeDeps) func() providercontrol.FlyMachinesStatus {
	if os.Getenv("HOMELAB_FLY_MACHINES_ENABLED") != "true" {
		return nil
	}
	control, err := configureFlyMachines(router, deps)
	if err != nil {
		readiness := &providercontrol.FlyMachinesReadinessError{Code: "fly_configuration_invalid"}
		_ = errors.As(err, &readiness)
		routes.RegisterFlyMachinesRoutes(router, deps.log, func(context.Context, providercontrol.FlyMachinesIdentity, providercontrol.FlyMachinesRequest) (providercontrol.FlyMachineCommandResult, error) {
			return providercontrol.FlyMachineCommandResult{}, readiness
		})
		if deps.log != nil {
			deps.log.Error("fly_machines_unavailable", "reason_code", readiness.Code)
		}
		return func() providercontrol.FlyMachinesStatus {
			return providercontrol.FlyMachinesStatus{Enabled: true, State: "unavailable", Error: readiness}
		}
	}
	return control.Status
}

func configureFlyMachines(router *httpx.Router, deps routeDeps) (*providercontrol.FlyMachinesControl, error) {
	if deps.startup == nil || !deps.startup.cfg.DeploymentMode.IsSaaS() || deps.v2 == nil || deps.v2.db == nil || deps.featureSvc == nil {
		return nil, &providercontrol.FlyMachinesReadinessError{Code: "fly_authority_unavailable"}
	}
	region := strings.TrimSpace(os.Getenv("HOMELAB_FLY_MACHINES_REGION"))
	if region != providercontrol.FlyMachinesRegion {
		return nil, &providercontrol.FlyMachinesReadinessError{Code: "fly_region_refused"}
	}
	var tenants map[string]providercontrol.FlyMachinesTenantBinding
	if json.Unmarshal([]byte(os.Getenv("HOMELAB_FLY_MACHINES_TENANT_APPS")), &tenants) != nil || len(tenants) == 0 {
		return nil, &providercontrol.FlyMachinesReadinessError{Code: "fly_tenant_isolation_invalid"}
	}
	var apps map[string]providercontrol.FlyMachinesApplication
	if json.Unmarshal([]byte(os.Getenv("HOMELAB_FLY_MACHINES_APPS")), &apps) != nil {
		return nil, &providercontrol.FlyMachinesReadinessError{Code: "fly_catalog_invalid"}
	}
	r2Store, err := flyMachinesR2FromEnv()
	if err != nil {
		return nil, err
	}
	budgetReader, err := newManagedAppBudgetReader()
	if err != nil {
		return nil, &providercontrol.FlyMachinesReadinessError{Code: "fly_budget_reader_unavailable"}
	}
	admission := func(ctx context.Context, id providercontrol.FlyMachinesIdentity, req providercontrol.FlyMachinesRequest) (providercontrol.ManagedAppBudget, error) {
		grants, ok := middleware.SignedEntitlementsFromContext(ctx)
		if !ok {
			return providercontrol.ManagedAppBudget{}, &providercontrol.ManagedAppRefusal{Code: "entitlement_refused", Repair: "Obtain a current signed workload identity decision."}
		}
		// Cleanup (observe, stop, decommission) remains available to the owning
		// authenticated tenant/subject even if a paid operate entitlement has
		// lapsed, so running machines can always be stopped and decommissioned.
		// Postgres RLS and AdmitManagedAppDispatchTx enforce exact ownership and
		// cleanup never creates capacity or calls Cloud commerce.
		if providercontrol.IsManagedAppCleanup(string(req.Action)) {
			return providercontrol.ManagedAppBudget{}, nil
		}
		if !grants.Has(routes.InventoryEntitlementOperate) || (req.Action == providercontrol.FlyMachineActionProvision && !grants.Has(routes.InventoryEntitlementProvision)) {
			return providercontrol.ManagedAppBudget{}, &providercontrol.ManagedAppRefusal{Code: "entitlement_refused", Repair: "Obtain a current signed workload operation entitlement."}
		}
		enabled, err := deps.featureSvc.IsEnabled(ctx, providercontrol.FlyMachinesFlag, id.SubjectID)
		if err != nil || !enabled {
			return providercontrol.ManagedAppBudget{}, &providercontrol.ManagedAppRefusal{Code: "module_disabled", Repair: "Enable the default-off Fly.io Managed Apps module for this tester."}
		}
		return budgetReader.Read(ctx, id)
	}
	control, err := providercontrol.NewFlyMachinesControl(providercontrol.FlyMachinesConfig{
		Database: deps.v2.db.DB,
		Region:   region,
		Tenants:  tenants,
		Apps:     apps,
		R2:       r2Store,
		Admit:    admission,
	})
	if err != nil {
		return nil, err
	}
	routes.RegisterFlyMachinesRoutes(router, deps.log, control.Execute)
	return control, nil
}

func flyMachinesR2FromEnv() (providercontrol.FlyMachineR2Store, error) {
	accountID := strings.TrimSpace(os.Getenv("HOMELAB_FLY_MACHINES_R2_ACCOUNT_ID"))
	bucket := strings.TrimSpace(os.Getenv("HOMELAB_FLY_MACHINES_R2_BUCKET"))
	accessKeyID := strings.TrimSpace(os.Getenv("HOMELAB_FLY_MACHINES_R2_ACCESS_KEY_ID"))
	secretAccessKey := strings.TrimSpace(os.Getenv("HOMELAB_FLY_MACHINES_R2_SECRET_ACCESS_KEY"))
	if accountID == "" && bucket == "" && accessKeyID == "" && secretAccessKey == "" {
		return nil, nil
	}
	// PayloadBridge remains nil until a real workload data-plane snapshot bridge
	// is wired: PurgeWorkload works on decommission while Backup/Restore fail
	// closed and Status().BackupConfigured honestly reports false.
	return providercontrol.NewFlyMachinesR2Store(providercontrol.FlyMachinesR2Config{
		AccountID:       accountID,
		Bucket:          bucket,
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretAccessKey,
	})
}
