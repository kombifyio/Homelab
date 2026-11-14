package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const creationDraftColumns = `id, tenant_id, owner_subject_id, draft_key, process, step, furthest,
		answers_json::text, expires_at, created_at, updated_at`

func creationDraftScope(tenantID, ownerSubjectID, key string) (string, string, error) {
	tenantID = strings.TrimSpace(tenantID)
	ownerSubjectID = strings.TrimSpace(ownerSubjectID)
	if tenantID == "" || ownerSubjectID == "" || !ValidCreationDraftKey(key) {
		return "", "", fmt.Errorf("controlplane: tenant, owner and a valid draft key are required")
	}
	return tenantID, ownerSubjectID, nil
}

func (s *PostgresStore) GetCreationDraft(ctx context.Context, tenantID, ownerSubjectID, key string) (*CreationDraft, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("controlplane: database not configured")
	}
	tenantID, ownerSubjectID, err := creationDraftScope(tenantID, ownerSubjectID, key)
	if err != nil {
		return nil, err
	}
	var out *CreationDraft
	err = s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		row, err := scanCreationDraft(tx.QueryRowContext(ctx, `
			SELECT `+creationDraftColumns+`
			FROM creation_drafts
			WHERE tenant_id = $1 AND owner_subject_id = $2 AND draft_key = $3
			  AND expires_at > now()
		`, tenantID, ownerSubjectID, key))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		out = row
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PostgresStore) PutCreationDraft(ctx context.Context, draft CreationDraft) (*CreationDraft, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("controlplane: database not configured")
	}
	normalized, err := normalizeCreationDraft(draft)
	if err != nil {
		return nil, err
	}
	ttl := fmt.Sprintf("%d seconds", int64(CreationDraftTTL.Seconds()))
	var out *CreationDraft
	err = s.withTenant(ctx, normalized.TenantID, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM creation_drafts
			WHERE tenant_id = $1 AND owner_subject_id = $2 AND expires_at <= now()
		`, normalized.TenantID, normalized.OwnerSubjectID); err != nil {
			return err
		}
		row, err := scanCreationDraft(tx.QueryRowContext(ctx, `
			INSERT INTO creation_drafts (
				id, tenant_id, owner_subject_id, draft_key, schema_version,
				process, step, furthest, answers_json, expires_at
			) VALUES ($1, $2, $3, $4, 1, $5, $6, $7, $8::jsonb, now() + $9::interval)
			ON CONFLICT (tenant_id, owner_subject_id, draft_key)
			DO UPDATE SET
				process = EXCLUDED.process,
				step = EXCLUDED.step,
				-- A frame that shows fewer steps (one step alone) never takes
				-- back how far the owner got.
				furthest = CASE
					WHEN array_position(ARRAY['goals', 'server', 'access', 'users', 'login'], EXCLUDED.furthest)
						>= array_position(ARRAY['goals', 'server', 'access', 'users', 'login'], creation_drafts.furthest)
					THEN EXCLUDED.furthest ELSE creation_drafts.furthest END,
				answers_json = EXCLUDED.answers_json,
				expires_at = EXCLUDED.expires_at,
				updated_at = now()
			RETURNING `+creationDraftColumns,
			normalized.ID, normalized.TenantID, normalized.OwnerSubjectID, normalized.Key,
			normalized.Process, normalized.Step, normalized.Furthest, string(normalized.Answers), ttl))
		if err != nil {
			return err
		}
		out = row
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PostgresStore) DeleteCreationDraft(ctx context.Context, tenantID, ownerSubjectID, key string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("controlplane: database not configured")
	}
	tenantID, ownerSubjectID, err := creationDraftScope(tenantID, ownerSubjectID, key)
	if err != nil {
		return err
	}
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			DELETE FROM creation_drafts
			WHERE tenant_id = $1 AND owner_subject_id = $2 AND draft_key = $3
		`, tenantID, ownerSubjectID, key)
		return err
	})
}

func scanCreationDraft(row rowScanner) (*CreationDraft, error) {
	var (
		out         CreationDraft
		answersJSON string
	)
	if err := row.Scan(
		&out.ID, &out.TenantID, &out.OwnerSubjectID, &out.Key, &out.Process, &out.Step, &out.Furthest,
		&answersJSON, &out.ExpiresAt, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		return nil, err
	}
	out.Answers = []byte(answersJSON)
	return &out, nil
}

var _ CreationDraftStore = (*PostgresStore)(nil)
