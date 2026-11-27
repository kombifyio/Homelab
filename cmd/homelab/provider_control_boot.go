package main

import (
	"database/sql"

	productnotifications "github.com/kombifyio/homelab/internal/notifications"
	"github.com/kombifyio/homelab/internal/providercontrol"
	"github.com/kombifyio/homelab/pkg/config"
	"github.com/kombifyio/homelab/pkg/jobs"
	"github.com/kombifyio/homelab/pkg/logger"
	"github.com/kombifyio/homelab/pkg/ril/workflow"
)

// The public build retains the same native custody, ledger, admission and
// cleanup implementation. No commercial adapter or credential resolver is linked.
func composeProviderControl(database *sql.DB, edition config.Edition, log *logger.Logger,
	_ *productnotifications.Outbox, onParkedOperation func(providercontrol.OperationRef),
	_ ...*workflow.Engine,
) (*providercontrol.Runtime, jobs.RuntimeActions, error) {
	composed, err := composeProviderControlParts(database, edition, log, nil, onParkedOperation)
	return composed.runtime, composed.actions, err
}

// composeProviderControlParts is the startup composition the private wrapper
// also provides (startup_provider.go). Selfhost injects no commercial adapter
// registration and no incident or capacity observer.
func composeProviderControlParts(database *sql.DB, edition config.Edition, log *logger.Logger,
	_ *productnotifications.Outbox, onParkedOperation func(providercontrol.OperationRef),
	_ ...*workflow.Engine,
) (nativeProviderControl, error) {
	return composeNativeProviderControlParts(database, edition, log, nil, onParkedOperation, nil)
}

// composeProviderControlForRetenant composes the same provider control for the
// managed-runtime retenant command. No worker starts and no notification
// observer is attached. Selfhost links no managed provider, so a
// managed lease cannot be re-admitted here.
func composeProviderControlForRetenant(database *sql.DB, edition config.Edition, log *logger.Logger) (nativeProviderControl, error) {
	return composeNativeProviderControlParts(database, edition, log, nil, nil, nil)
}
