package main

import (
	"context"
	"time"

	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/logger"
)

// Each pass visits at most 32 tenants and repairs at most 25 deleted stacks
// per tenant. The existing secret-free registry directory supplies tenant IDs;
// all binding reads and writes run under each tenant's RLS context.
func runDeletedStackBindingReconciliation(ctx context.Context, store *controlplane.PostgresStore, log *logger.Logger) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	after := ""
	for {
		passCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		tenants, err := store.ListServerProjectionTenants(passCtx, after, 32)
		if err == nil {
			for _, tenant := range tenants {
				tenantCtx, tenantCancel := context.WithTimeout(passCtx, 2*time.Second)
				repairErr := store.ReconcileDeletedStackBindings(tenantCtx, tenant, 25)
				tenantCancel()
				if repairErr != nil {
					log.Warn("deleted_stack_binding_tenant_reconciliation_failed", "tenant_id", tenant, "error", repairErr)
				}
				after = tenant
				if passCtx.Err() != nil {
					break
				}
			}
		}
		cancel()
		if err != nil {
			log.Warn("deleted_stack_binding_reconciliation_failed", "error", err)
		} else if len(tenants) == 0 || (len(tenants) < 32 && after == tenants[len(tenants)-1]) {
			after = ""
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
