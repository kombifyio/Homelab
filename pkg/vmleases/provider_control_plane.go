package vmleases

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/kombifyio/homelab/internal/providercatalog"
)

var providerCatalogIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

var ErrProviderControlPlaneUnavailable = errors.New("vmleases: provider control plane unavailable")

func LoadHomelabLeasingProviderSet(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	if db == nil {
		return nil, ErrProviderControlPlaneUnavailable
	}

	rows, err := db.QueryContext(ctx, `
SELECT DISTINCT profile.provider_id
FROM provider_catalog_versions AS version
JOIN provider_catalog_profiles AS profile
  ON profile.catalog_version = version.catalog_version
WHERE version.status = 'active'
ORDER BY profile.provider_id`)
	if err != nil {
		return nil, fmt.Errorf("query provider control plane homelab_leasing exposures: %w", err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var providerID string
		if err := rows.Scan(&providerID); err != nil {
			return nil, fmt.Errorf("scan provider control plane homelab_leasing provider: %w", err)
		}
		providerID = strings.TrimSpace(providerID)
		if !providerCatalogIDPattern.MatchString(providerID) ||
			providercatalog.IsCompositeProviderAlias(providerID) {
			return nil, fmt.Errorf("invalid canonical provider catalog id %q", providerID)
		}
		out[providerID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read provider control plane homelab_leasing exposures: %w", err)
	}
	return out, nil
}
