package main

import (
	commonobservability "github.com/kombifyio/homelab/internal/gocommon/observability"

	"github.com/kombifyio/homelab/pkg/fulllocal"
	"github.com/kombifyio/homelab/pkg/logger"
)

// initErrorReporting starts Sentry error reporting. Under Full Local it starts
// nothing, even when a DSN in the homelab is set; the Full Local build does not
// contain the Sentry SDK at all.
func initErrorReporting(cfg commonobservability.Config) func() {
	if fulllocal.Active() {
		logger.Default().Info("error_reporting_disabled", "reason", "data profile full-local")
		return func() {}
	}
	return commonobservability.MustInit(cfg)
}
