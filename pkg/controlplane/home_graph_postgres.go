package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// homeGraphRestoreItem mirrors one restore item; the caller validated its shape.
type homeGraphRestoreItem struct {
	Kind      string `json:"kind"`
	KombifyID string `json:"kombifyId"`
	Field     string `json:"field"`
}

const maxHomeGraphRestore = 256

const homeGraphColumns = `tenant_id, owner_subject_id, space_id, graph::text, graph_revision, restore::text, status,
	synced_revision, result::text, error_code, failure_count, synced_at, last_attempt_at, created_at, updated_at`

func homeGraphScope(tenantID, ownerSubjectID, spaceID string) (string, string, string, error) {
	tenantID, ownerSubjectID, spaceID = strings.TrimSpace(tenantID), strings.TrimSpace(ownerSubjectID), strings.TrimSpace(spaceID)
	if tenantID == "" || ownerSubjectID == "" || spaceID == "" {
		return "", "", "", fmt.Errorf("controlplane: tenant, owner and space are required for a home graph")
	}
	return tenantID, ownerSubjectID, spaceID, nil
}

func scanHomeGraph(row rowScanner) (*HomeGraphState, error) {
	var (
		out                   HomeGraphState
		graph, restore        string
		result, errorCode     sql.NullString
		syncedAt, lastAttempt sql.NullTime
	)
	if err := row.Scan(&out.TenantID, &out.OwnerSubjectID, &out.SpaceID, &graph, &out.GraphRevision, &restore, &out.Status,
		&out.SyncedRevision, &result, &errorCode, &out.FailureCount, &syncedAt, &lastAttempt, &out.CreatedAt, &out.UpdatedAt); err != nil {
		return nil, err
	}
	out.Graph, out.Restore = json.RawMessage(graph), json.RawMessage(restore)
	if result.Valid {
		out.Result = json.RawMessage(result.String)
	}
	out.ErrorCode = errorCode.String
	if syncedAt.Valid {
		t := syncedAt.Time.UTC()
		out.SyncedAt = &t
	}
	if lastAttempt.Valid {
		t := lastAttempt.Time.UTC()
		out.LastAttemptAt = &t
	}
	return &out, nil
}

func decodeHomeGraphRestore(raw json.RawMessage) ([]homeGraphRestoreItem, error) {
	items := []homeGraphRestoreItem{}
	if len(raw) == 0 {
		return items, nil
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("controlplane: home graph restore list is not an array: %w", err)
	}
	return items, nil
}

func encodeHomeGraphRestore(items []homeGraphRestoreItem) string {
	if items == nil {
		items = []homeGraphRestoreItem{}
	}
	raw, _ := json.Marshal(items)
	return string(raw)
}

func (s *PostgresStore) PutHomeGraph(ctx context.Context, tenantID, ownerSubjectID, spaceID string, graph json.RawMessage, revision int64, restore json.RawMessage) (*HomeGraphState, HomeGraphPutOutcome, error) {
	if s == nil || s.db == nil {
		return nil, "", fmt.Errorf("controlplane: database not configured")
	}
	tenantID, ownerSubjectID, spaceID, err := homeGraphScope(tenantID, ownerSubjectID, spaceID)
	if err != nil {
		return nil, "", err
	}
	if len(graph) == 0 || len(graph) > MaxHomeGraphBytes || revision < 0 {
		return nil, "", fmt.Errorf("controlplane: home graph is empty, too large or has no revision")
	}
	incoming, err := decodeHomeGraphRestore(restore)
	if err != nil {
		return nil, "", err
	}
	var out *HomeGraphState
	outcome := HomeGraphPutChanged
	err = s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		// Concurrent first writes for a new space: the loser of the insert
		// race waits on the winner's row, does nothing and then locks and
		// merges into it like any later write.
		inserted, err := tx.ExecContext(ctx, `INSERT INTO home_graph_state
				(tenant_id, owner_subject_id, space_id, graph, graph_revision, restore, status)
			VALUES ($1, $2, $3, $4::jsonb, $5, $6::jsonb, 'pending')
			ON CONFLICT (tenant_id, owner_subject_id, space_id) DO NOTHING`,
			tenantID, ownerSubjectID, spaceID, string(graph), revision, encodeHomeGraphRestore(dedupeHomeGraphRestore(nil, incoming)))
		if err != nil {
			return err
		}
		created, _ := inserted.RowsAffected()
		current, err := scanHomeGraph(tx.QueryRowContext(ctx, `SELECT `+homeGraphColumns+`
			FROM home_graph_state WHERE tenant_id = $1 AND owner_subject_id = $2 AND space_id = $3 FOR UPDATE`,
			tenantID, ownerSubjectID, spaceID))
		if err != nil {
			return err
		}
		if created == 1 {
			out = current
			return nil
		}
		if revision < current.GraphRevision {
			out, outcome = current, HomeGraphPutStale
			return nil
		}
		waiting, err := decodeHomeGraphRestore(current.Restore)
		if err != nil {
			return err
		}
		known := map[homeGraphRestoreItem]bool{}
		for _, item := range waiting {
			known[item] = true
		}
		added := false
		for _, item := range incoming {
			added = added || !known[item]
		}
		if revision == current.GraphRevision && !added {
			out, outcome = current, HomeGraphPutUnchanged
			return nil
		}
		// An equal revision keeps the stored graph; only a higher one replaces it.
		newGraph := string(current.Graph)
		if revision > current.GraphRevision {
			newGraph = string(graph)
		}
		out, err = scanHomeGraph(tx.QueryRowContext(ctx, `UPDATE home_graph_state SET
				graph = $4::jsonb, graph_revision = $5, restore = $6::jsonb, status = 'pending',
				error_code = NULL, failure_count = 0, updated_at = now()
			WHERE tenant_id = $1 AND owner_subject_id = $2 AND space_id = $3 RETURNING `+homeGraphColumns,
			tenantID, ownerSubjectID, spaceID, newGraph, revision, encodeHomeGraphRestore(dedupeHomeGraphRestore(waiting, incoming))))
		return err
	})
	if err != nil {
		return nil, "", err
	}
	return out, outcome, nil
}

// dedupeHomeGraphRestore merges the incoming items after the waiting ones,
// dedupes them and drops the oldest beyond the cap.
func dedupeHomeGraphRestore(waiting, incoming []homeGraphRestoreItem) []homeGraphRestoreItem {
	seen := map[homeGraphRestoreItem]bool{}
	var out []homeGraphRestoreItem
	for _, list := range [][]homeGraphRestoreItem{waiting, incoming} {
		for _, item := range list {
			if seen[item] {
				continue
			}
			seen[item] = true
			out = append(out, item)
		}
	}
	if len(out) > maxHomeGraphRestore {
		out = out[len(out)-maxHomeGraphRestore:]
	}
	return out
}

func (s *PostgresStore) GetHomeGraph(ctx context.Context, tenantID, ownerSubjectID, spaceID string) (*HomeGraphState, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("controlplane: database not configured")
	}
	tenantID, ownerSubjectID, spaceID, err := homeGraphScope(tenantID, ownerSubjectID, spaceID)
	if err != nil {
		return nil, err
	}
	var out *HomeGraphState
	err = s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		row, err := scanHomeGraph(tx.QueryRowContext(ctx, `SELECT `+homeGraphColumns+`
			FROM home_graph_state WHERE tenant_id = $1 AND owner_subject_id = $2 AND space_id = $3`,
			tenantID, ownerSubjectID, spaceID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		out = row
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PostgresStore) ListHomeGraphs(ctx context.Context, tenantID, ownerSubjectID string) ([]HomeGraphState, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("controlplane: database not configured")
	}
	tenantID, ownerSubjectID = strings.TrimSpace(tenantID), strings.TrimSpace(ownerSubjectID)
	if tenantID == "" || ownerSubjectID == "" {
		return nil, fmt.Errorf("controlplane: tenant and owner are required for home graphs")
	}
	var out []HomeGraphState
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+homeGraphColumns+`
			FROM home_graph_state WHERE tenant_id = $1 AND owner_subject_id = $2 ORDER BY space_id LIMIT 64`,
			tenantID, ownerSubjectID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			row, err := scanHomeGraph(rows)
			if err != nil {
				return err
			}
			out = append(out, *row)
		}
		return rows.Err()
	})
	return out, err
}

func (s *PostgresStore) MarkHomeGraphAttempt(ctx context.Context, tenantID, ownerSubjectID, spaceID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("controlplane: database not configured")
	}
	tenantID, ownerSubjectID, spaceID, err := homeGraphScope(tenantID, ownerSubjectID, spaceID)
	if err != nil {
		return err
	}
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE home_graph_state SET last_attempt_at = now()
			WHERE tenant_id = $1 AND owner_subject_id = $2 AND space_id = $3`, tenantID, ownerSubjectID, spaceID)
		return err
	})
}

func (s *PostgresStore) CompleteHomeGraphSync(ctx context.Context, tenantID, ownerSubjectID, spaceID string, revision int64, sentRestore, result json.RawMessage) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("controlplane: database not configured")
	}
	tenantID, ownerSubjectID, spaceID, err := homeGraphScope(tenantID, ownerSubjectID, spaceID)
	if err != nil {
		return err
	}
	if len(result) == 0 || len(result) > MaxHomeGraphBytes {
		return fmt.Errorf("controlplane: home graph result is empty or too large")
	}
	sent, err := decodeHomeGraphRestore(sentRestore)
	if err != nil {
		return err
	}
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		current, err := scanHomeGraph(tx.QueryRowContext(ctx, `SELECT `+homeGraphColumns+`
			FROM home_graph_state WHERE tenant_id = $1 AND owner_subject_id = $2 AND space_id = $3 FOR UPDATE`,
			tenantID, ownerSubjectID, spaceID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		waiting, err := decodeHomeGraphRestore(current.Restore)
		if err != nil {
			return err
		}
		sentSet := map[homeGraphRestoreItem]bool{}
		for _, item := range sent {
			sentSet[item] = true
		}
		remaining := make([]homeGraphRestoreItem, 0, len(waiting))
		for _, item := range waiting {
			if !sentSet[item] {
				remaining = append(remaining, item)
			}
		}
		syncedRevision := revision
		if current.SyncedRevision > syncedRevision {
			syncedRevision = current.SyncedRevision
		}
		status := HomeGraphStatusSynced
		if current.GraphRevision > syncedRevision || len(remaining) > 0 {
			status = HomeGraphStatusPending
		}
		_, err = tx.ExecContext(ctx, `UPDATE home_graph_state SET
				status = $4, synced_revision = $5, restore = $6::jsonb, result = $7::jsonb, error_code = NULL, failure_count = 0,
				synced_at = now(), last_attempt_at = now(), updated_at = now()
			WHERE tenant_id = $1 AND owner_subject_id = $2 AND space_id = $3`,
			tenantID, ownerSubjectID, spaceID, status, syncedRevision, encodeHomeGraphRestore(remaining), string(result))
		return err
	})
}

func (s *PostgresStore) FailHomeGraphSync(ctx context.Context, tenantID, ownerSubjectID, spaceID string, revision int64, sentRestore json.RawMessage, errorCode string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("controlplane: database not configured")
	}
	tenantID, ownerSubjectID, spaceID, err := homeGraphScope(tenantID, ownerSubjectID, spaceID)
	if err != nil {
		return err
	}
	sent, err := decodeHomeGraphRestore(sentRestore)
	if err != nil {
		return err
	}
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		current, err := scanHomeGraph(tx.QueryRowContext(ctx, `SELECT `+homeGraphColumns+`
			FROM home_graph_state WHERE tenant_id = $1 AND owner_subject_id = $2 AND space_id = $3 FOR UPDATE`,
			tenantID, ownerSubjectID, spaceID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		waiting, err := decodeHomeGraphRestore(current.Restore)
		if err != nil {
			return err
		}
		sentSet := map[homeGraphRestoreItem]bool{}
		for _, item := range sent {
			sentSet[item] = true
		}
		superseded := current.GraphRevision > revision
		for _, item := range waiting {
			superseded = superseded || !sentSet[item]
		}
		if superseded {
			// A newer revision or a newly added restore waits: the failure
			// belongs to a superseded request and must not hide it.
			_, err = tx.ExecContext(ctx, `UPDATE home_graph_state SET last_attempt_at = now()
				WHERE tenant_id = $1 AND owner_subject_id = $2 AND space_id = $3`, tenantID, ownerSubjectID, spaceID)
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE home_graph_state SET status = 'failed', error_code = $4,
				failure_count = LEAST(failure_count, 1000) + 1, last_attempt_at = now(), updated_at = now()
			WHERE tenant_id = $1 AND owner_subject_id = $2 AND space_id = $3`, tenantID, ownerSubjectID, spaceID, errorCode)
		return err
	})
}

// HomeGraphRetryDelay is the wait after the given number of consecutive
// failures: base, doubling per failure, capped at HomeGraphRetryMax.
func HomeGraphRetryDelay(base time.Duration, failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	delay := base
	for i := 1; i < failures && delay < HomeGraphRetryMax; i++ {
		delay *= 2
	}
	if delay > HomeGraphRetryMax {
		delay = HomeGraphRetryMax
	}
	return delay
}

// HomeGraphRetryDue reports whether a stored state needs a sync run now, per
// the scheduling rules: a pending revision or restore, a failed run retried
// after retryAfter doubling per consecutive failure up to HomeGraphRetryMax
// (never for home_graph_request_invalid), and a synced state older than driftEvery (drift check).
func HomeGraphRetryDue(state HomeGraphState, now time.Time, retryAfter, driftEvery time.Duration) bool {
	if state.LastAttemptAt != nil && now.Sub(*state.LastAttemptAt) < time.Minute {
		return false
	}
	switch state.Status {
	case HomeGraphStatusPending:
		return true
	case HomeGraphStatusFailed:
		if state.ErrorCode == HomeGraphRequestInvalidCode {
			// The node refused the request itself; only a new revision or
			// restore (which resets the state to pending) changes that.
			return false
		}
		return state.LastAttemptAt == nil || now.Sub(*state.LastAttemptAt) >= HomeGraphRetryDelay(retryAfter, state.FailureCount)
	case HomeGraphStatusSynced:
		return state.SyncedAt == nil || now.Sub(*state.SyncedAt) >= driftEvery
	}
	return false
}

var _ HomeGraphStore = (*PostgresStore)(nil)
