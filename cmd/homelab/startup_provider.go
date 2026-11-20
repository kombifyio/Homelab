package main

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	"github.com/kombifyio/homelab/internal/executioncustody"
	"github.com/kombifyio/homelab/internal/notifications"
	"github.com/kombifyio/homelab/internal/providercontrol"
	"github.com/kombifyio/homelab/pkg/config"
	"github.com/kombifyio/homelab/pkg/db"
	"github.com/kombifyio/homelab/pkg/jobs"
	"github.com/kombifyio/homelab/pkg/logger"
	"github.com/kombifyio/homelab/pkg/orchestrator"
	"github.com/kombifyio/homelab/pkg/ril/workflow"
)

// Both builds run the same native authority under its restricted database role.
// The public composition wrapper registers only the customer's Proxmox adapter.
func init() {
	runHostedProviderControlBootstrap = runProviderControlRuntimeBootstrap
	composeHostedProviderRuntime = func(
		ctx context.Context,
		database *sql.DB,
		edition config.Edition,
		log *logger.Logger,
		notificationOutbox *notifications.Outbox,
		orch *orchestrator.Orchestrator,
		workflowEngine *workflow.Engine,
	) (providerRuntimeState, error) {
		// A deployment beside this one would open no Execution connection and
		// sweep no wait projection. The product has one channel, so this is the
		// production boot with both.
		if config.SecondaryChannel() {
			log.Info("provider_control_inactive", "reason", "early_access_runs_no_background_work")
			return providerRuntimeState{}, nil
		}
		mode, err := advancedIssuerCustodyMode()
		if err != nil {
			return providerRuntimeState{}, err
		}
		stageDatabase, err := db.IsStagingApplicationDatabase(ctx, database)
		if err != nil {
			return providerRuntimeState{}, err
		}
		if stageDatabase {
			// Only a positively verified Stage database under its exact restricted
			// application role reaches here (an unverifiable identity or role
			// already returned an error). Stage has no owner mechanism and no
			// provider custody, so provider control stays off: no Execution
			// connection, no provider pool, no provider side effect. The rest of
			// the control plane serves the development loop on that real database.
			log.Warn("provider_control_disabled", "reason", "stage_database_has_no_provider_custody")
			return providerRuntimeState{}, nil
		}

		if mode == advancedIssuerCustodyRemote {
			remote, err := executioncustody.OpenRemote(ctx)
			if err != nil {
				// Provider Control is an independently gated surface: an unreachable
				// Execution process disables it, never the rest of the control plane.
				log.Warn("provider_control_disabled", "reason", "execution_process_unavailable")
				return providerRuntimeState{}, nil
			}
			managedDay2.setPower(remote)
			return providerRuntimeState{runner: &remoteProviderRecovery{orchestrator: orch, remote: remote}, actions: jobs.RuntimeActions{LeaseManager: remote, LeaseDecommissioner: remote, RuntimeTargetResolver: remote}, cleanupReadback: remote, resolution: remote}, nil
		}
		providerDatabase, providerDatabaseErr := openProviderControlRuntimeDatabase(ctx, database)
		if providerDatabaseErr != nil {
			// Provider Control is an independently gated product surface. An absent
			// or unsafe runtime role disables that surface without taking unrelated
			// self-hosted control-plane APIs down.
			log.Warn("provider_control_disabled", "reason", "dedicated_runtime_database_unavailable_or_posture_invalid")
			return providerRuntimeState{}, nil
		}

		providerRuntime, providerActions, err := composeProviderControl(
			providerDatabase.DB,
			edition,
			log,
			notificationOutbox,
			func(operation providercontrol.OperationRef) {
				if recoveryErr := orch.RehydrateProviderProvisionWait(ctx, operation); recoveryErr != nil {
					log.Warn("provider_provision_wait_rehydration_failed",
						"tenant", operation.TenantID, "operation", operation.OperationID, "error", recoveryErr)
				}
			},
			workflowEngine,
		)
		if err != nil {
			_ = providerDatabase.Close()
			return providerRuntimeState{}, err
		}

		// Queue timers are process-local. Keep provider-decommission wait
		// recovery inside the same activation-gated runtime.
		runtimeForRecovery := providerRuntime
		if err := runtimeForRecovery.RegisterAuxiliaryWorker(func(recoveryCtx context.Context) {
			orch.RunDueProviderDecommissionRecovery(recoveryCtx, runtimeForRecovery.Ledger())
		}); err != nil {
			_ = providerDatabase.Close()
			return providerRuntimeState{}, fmt.Errorf("register provider-decommission wait recovery: %w", err)
		}
		if err := runtimeForRecovery.RegisterAuxiliaryWorker(func(recoveryCtx context.Context) {
			orch.RunProviderProvisionRecovery(recoveryCtx, runtimeForRecovery.Ledger())
		}); err != nil {
			_ = providerDatabase.Close()
			return providerRuntimeState{}, fmt.Errorf("register provider-provision wait recovery: %w", err)
		}

		return providerRuntimeState{
			database:        providerDatabase,
			runner:          providerRuntime,
			actions:         providerActions,
			cleanupReadback: providerRuntime,
			resolution:      providerRuntime,
		}, nil
	}
}

// The existing lifecycle owns and drains the same recovery loops. It carries
// no provider pool or side-effect worker into the user-facing process.
type remoteProviderRecovery struct {
	orchestrator *orchestrator.Orchestrator
	remote       *executioncustody.Remote
}

func (r *remoteProviderRecovery) Run(ctx context.Context) {
	var workers sync.WaitGroup
	workers.Add(2)
	go func() { defer workers.Done(); r.orchestrator.RunProviderProvisionRecovery(ctx, r.remote) }()
	go func() { defer workers.Done(); r.orchestrator.RunDueProviderDecommissionRecovery(ctx, r.remote) }()
	workers.Wait()
}
