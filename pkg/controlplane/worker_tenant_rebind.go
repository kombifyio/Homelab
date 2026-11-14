package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// WorkerTenantRebindResourceKey holds the operator-staged same-subject tenant
// rebind of a worker (guardidentity.TenantRebindDirective). Heartbeat and
// inventory upserts preserve it; only the methods below change it.
const WorkerTenantRebindResourceKey = "guard_tenant_rebind"

// WorkerTenantRebindStore is the operator and heartbeat authority over the
// staged directive. It is separate from WorkerStore so read-only consumers do
// not acquire it.
type WorkerTenantRebindStore interface {
	// StageWorkerTenantRebind stores directive on the worker when it holds no
	// directive yet, the same directive id (an idempotent re-stage), or exactly
	// replaceDirectiveID (an expired directive the operator replaces).
	StageWorkerTenantRebind(ctx context.Context, tenantID, workerID, directiveID, replaceDirectiveID string, directive map[string]any) error
	// RecordWorkerTenantRebindStaged records the Guard's acknowledgement of
	// directiveID once; a later or other acknowledgement changes nothing.
	RecordWorkerTenantRebindStaged(ctx context.Context, tenantID, workerID, directiveID string, at time.Time) (bool, error)
	// ClearWorkerTenantRebind removes exactly directiveID from the worker.
	ClearWorkerTenantRebind(ctx context.Context, tenantID, workerID, directiveID string) error
}

var _ WorkerTenantRebindStore = (*PostgresStore)(nil)

// StageWorkerTenantRebind implements WorkerTenantRebindStore.
func (s *PostgresStore) StageWorkerTenantRebind(ctx context.Context, tenantID, workerID, directiveID, replaceDirectiveID string, directive map[string]any) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("controlplane: database not configured")
	}
	tenantID, workerID, directiveID = strings.TrimSpace(tenantID), strings.TrimSpace(workerID), strings.TrimSpace(directiveID)
	if tenantID == "" || workerID == "" || directiveID == "" || directive["directive_id"] != directiveID {
		return fmt.Errorf("%w: tenant, worker and a matching directive are required", ErrConflict)
	}
	raw, err := json.Marshal(directive)
	if err != nil {
		return err
	}
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		result, execErr := tx.ExecContext(ctx, `
			UPDATE workers
			SET resources_json = COALESCE(resources_json, '{}'::jsonb) || jsonb_build_object($3::text, $5::jsonb),
				updated_at = now()
			WHERE tenant_id = $1 AND id = $2
			  AND (
			      NOT (COALESCE(resources_json, '{}'::jsonb) ? $3::text)
			      OR resources_json->$3::text->>'directive_id' = $4
			      OR ($6 <> '' AND resources_json->$3::text->>'directive_id' = $6)
			  )
		`, tenantID, workerID, WorkerTenantRebindResourceKey, directiveID, string(raw), strings.TrimSpace(replaceDirectiveID))
		if execErr != nil {
			return execErr
		}
		if rows, rowsErr := result.RowsAffected(); rowsErr == nil && rows == 0 {
			return fmt.Errorf("%w: worker is missing or holds another tenant rebind", ErrConflict)
		}
		return nil
	})
}

// RecordWorkerTenantRebindStaged implements WorkerTenantRebindStore.
func (s *PostgresStore) RecordWorkerTenantRebindStaged(ctx context.Context, tenantID, workerID, directiveID string, at time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("controlplane: database not configured")
	}
	tenantID, workerID, directiveID = strings.TrimSpace(tenantID), strings.TrimSpace(workerID), strings.TrimSpace(directiveID)
	if tenantID == "" || workerID == "" || directiveID == "" {
		return false, nil
	}
	recorded := false
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		result, execErr := tx.ExecContext(ctx, `
			UPDATE workers
			SET resources_json = jsonb_set(resources_json, ARRAY[$3::text, 'staged_at'], to_jsonb($5::text), true),
				updated_at = now()
			WHERE tenant_id = $1 AND id = $2
			  AND resources_json->$3::text->>'directive_id' = $4
			  AND NOT (resources_json->$3::text ? 'staged_at')
		`, tenantID, workerID, WorkerTenantRebindResourceKey, directiveID, at.UTC().Format(time.RFC3339Nano))
		if execErr != nil {
			return execErr
		}
		rows, rowsErr := result.RowsAffected()
		recorded = rowsErr == nil && rows == 1
		return rowsErr
	})
	return recorded, err
}

// ClearWorkerTenantRebind implements WorkerTenantRebindStore.
func (s *PostgresStore) ClearWorkerTenantRebind(ctx context.Context, tenantID, workerID, directiveID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("controlplane: database not configured")
	}
	tenantID, workerID, directiveID = strings.TrimSpace(tenantID), strings.TrimSpace(workerID), strings.TrimSpace(directiveID)
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		result, execErr := tx.ExecContext(ctx, `
			UPDATE workers
			SET resources_json = resources_json - $3::text, updated_at = now()
			WHERE tenant_id = $1 AND id = $2
			  AND resources_json->$3::text->>'directive_id' = $4
		`, tenantID, workerID, WorkerTenantRebindResourceKey, directiveID)
		if execErr != nil {
			return execErr
		}
		if rows, rowsErr := result.RowsAffected(); rowsErr == nil && rows == 0 {
			return fmt.Errorf("%w: worker holds no such tenant rebind", ErrNotFound)
		}
		return nil
	})
}
