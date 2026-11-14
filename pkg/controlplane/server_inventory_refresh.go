package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const serverInventoryRefreshKey = "inventory_refresh"

// ServerInventoryRefreshRequester asks for an observation of the exact server
// placement. It grants no host mutation or command execution authority.
type ServerInventoryRefreshRequester interface {
	RequestServerInventoryRefresh(context.Context, string, string, string, time.Time) error
}

// PendingServerInventoryRefresh returns a request only until a newer inventory
// observation is accepted. Unchanged content keeps its material revision, while
// inventory_observed_at advances; heartbeats do not change that inventory field.
func PendingServerInventoryRefresh(server ServerRuntime, now time.Time) string {
	request, _ := server.Metadata[serverInventoryRefreshKey].(map[string]any)
	id, _ := request["request_id"].(string)
	stackID, _ := request["stack_id"].(string)
	expires, _ := request["expires_at"].(string)
	deadline, err := time.Parse(time.RFC3339Nano, expires)
	revision, ok := inventoryMetadataInt64(request, "inventory_revision")
	previousObservation, _ := request["observed_at"].(string)
	observation, _ := server.Metadata["inventory_observed_at"].(string)
	if id == "" || stackID != server.StackID || err != nil || !now.Before(deadline) || !ok || server.InventoryRevision > revision ||
		(observation != "" && observation != previousObservation) {
		return ""
	}
	return id
}

func (s *PostgresStore) RequestServerInventoryRefresh(ctx context.Context, tenantID, serverID, stackID string, deadline time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("controlplane: database not configured")
	}
	if err := validateInventoryRefresh(tenantID, serverID, stackID, deadline); err != nil {
		return err
	}
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE servers SET
			metadata_json = COALESCE(metadata_json, '{}'::jsonb) || jsonb_build_object('inventory_refresh',
				jsonb_build_object('request_id', $4::text, 'expires_at', $5::text, 'stack_id', stack_id,
					'inventory_revision', inventory_revision, 'observed_at', COALESCE(metadata_json->>'inventory_observed_at', ''))),
			updated_at = now()
			WHERE tenant_id = $1 AND id = $2 AND stack_id = $3`, tenantID, serverID, stackID, uuid.NewString(), deadline.UTC().Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err == nil && count == 0 {
			return ErrNotFound
		}
		return err
	})
}

func validateInventoryRefresh(tenantID, serverID, stackID string, deadline time.Time) error {
	now := time.Now()
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(serverID) == "" || strings.TrimSpace(stackID) == "" ||
		!deadline.After(now) || deadline.After(now.Add(2*time.Minute)) {
		return fmt.Errorf("inventory refresh requires exact tenant, server, stack and a deadline within two minutes")
	}
	return nil
}

func (s *MemoryStore) RequestServerInventoryRefresh(_ context.Context, tenantID, serverID, stackID string, deadline time.Time) error {
	if err := validateInventoryRefresh(tenantID, serverID, stackID, deadline); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	server, ok := s.servers[serverID]
	if !ok || server.TenantID != tenantID || server.StackID != stackID {
		return ErrNotFound
	}
	server.Metadata = cloneMap(server.Metadata)
	if server.Metadata == nil {
		server.Metadata = map[string]any{}
	}
	server.Metadata[serverInventoryRefreshKey] = map[string]any{
		"request_id": uuid.NewString(), "expires_at": deadline.UTC().Format(time.RFC3339Nano),
		"stack_id": stackID, "inventory_revision": server.InventoryRevision,
		"observed_at": stringFromMap(server.Metadata, "inventory_observed_at"),
	}
	server.UpdatedAt = s.now()
	s.servers[serverID] = server
	return nil
}
