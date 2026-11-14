package db

import (
	"fmt"
	"os"
	"strings"

	"github.com/kombifyio/techstack/pkg/config"
)

// EnvReleaseChannel is the retired channel selector (config.EnvReleaseChannel).
// A deployment that still names a channel other than stable refuses to start.
const EnvReleaseChannel = config.EnvReleaseChannel

// ReleaseChannel reports the one channel this deployment serves: stable.
func ReleaseChannel() string {
	return config.ReleaseChannel()
}

// refusePreviewMigration keeps a serving process beside the one channel
// verify-only with genesis disabled. No admitted process is one
// (config.RejectRetiredReleaseChannel refuses it), so this admits the
// self-hosted and hosted boots unchanged.
func refusePreviewMigration() error {
	if !config.SecondaryChannel() {
		return nil
	}
	if strings.TrimSpace(os.Getenv(EnvDatabaseMigrationMode)) != "verify" || genesisOptInFromEnv() {
		return fmt.Errorf("%s=%s refuses to migrate: set %s=verify and no genesis opt-in", EnvReleaseChannel, ReleaseChannel(), EnvDatabaseMigrationMode)
	}
	return nil
}
