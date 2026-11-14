package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// ReconcileDeletedStackBindings releases one bounded batch of historical
// soft-deleted stacks in a tenant. Inventory readers remain read-only and
// never need a wizard request to make these servers visible again.
func (s *PostgresStore) ReconcileDeletedStackBindings(ctx context.Context, tenantID string, limit int) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("controlplane: database not configured")
	}
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" || limit < 1 || limit > 100 {
		return fmt.Errorf("controlplane: tenant and binding reconciliation limit from 1 to 100 required")
	}
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
            SELECT s.id FROM stacks s
            WHERE s.tenant_id = $1 AND s.deleted_at IS NOT NULL
              AND (EXISTS (SELECT 1 FROM servers v WHERE v.tenant_id = $1 AND v.stack_id = s.id)
                OR EXISTS (SELECT 1 FROM workers w WHERE w.tenant_id = $1 AND w.stack_id = s.id)
                OR EXISTS (SELECT 1 FROM nodes n WHERE n.tenant_id = $1 AND n.stack_id = s.id))
            ORDER BY s.id LIMIT $2 FOR UPDATE OF s SKIP LOCKED
        `, tenantID, limit)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, id := range ids {
			if err := releaseDeletedStackBindingsTx(ctx, tx, tenantID, id); err != nil {
				return err
			}
		}
		return nil
	})
}
