package main

import (
	"context"
	"time"

	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/db/backgroundlock"
	"github.com/kombifyio/homelab/pkg/logger"
)

const deletedStackBindingReconciliationLock = "homelab/deleted-stack-binding-reconciliation"

// Each pass visits at most 32 tenants and repairs at most 25 deleted stacks
// per tenant. The existing secret-free registry directory supplies tenant IDs;
// all binding reads and writes run under each tenant's RLS context. lock keeps
// one replica reconciling at a time during a rolling deploy; a pass skipped for
// another replica restarts the tenant cursor, and a nil lock runs unlocked.
func runDeletedStackBindingReconciliation(ctx context.Context, store *controlplane.PostgresStore, lock *backgroundlock.Locker, log *logger.Logger) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	after := ""
	for {
		ran, lockErr := lock.Run(ctx, deletedStackBindingReconciliationLock, func(lockedCtx context.Context) {
			after = reconcileDeletedStackBindingPage(lockedCtx, store, after, log)
		})
		if lockErr != nil && ctx.Err() == nil {
			log.Warn("deleted_stack_binding_reconciliation_lock_failed", "error", lockErr)
		} else if !ran {
			after = ""
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// reconcileDeletedStackBindingPage repairs one bounded tenant page and returns
// the cursor of the next pass.
func reconcileDeletedStackBindingPage(ctx context.Context, store *controlplane.PostgresStore, after string, log *logger.Logger) string {
	passCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	tenants, err := store.ListServerProjectionTenants(passCtx, after, 32)
	if err != nil {
		log.Warn("deleted_stack_binding_reconciliation_failed", "error", err)
		return after
	}
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
	if len(tenants) == 0 || (len(tenants) < 32 && after == tenants[len(tenants)-1]) {
		after = ""
	}
	return after
}
