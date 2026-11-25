package providercontrol

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/kombifyio/homelab/pkg/db/tenantcontext"
)

type ionosPostgresCustody struct {
	connection   *sql.Conn
	tenant, node string
	backend      int
	mu           sync.Mutex
}

// A session advisory lock spans separate committed custody writes and agent
// calls. Transaction locks would roll back reservations on an uncertain call.
func withIONOSPoolNode(ctx context.Context, db *sql.DB, tenant, node string, fn func(context.Context, *ionosPostgresCustody) error) error {
	ctx, cancel := context.WithTimeout(ctx, ionosPoolClaimTTL)
	defer cancel()
	connection, err := tenantcontext.Database(ctx, db).Conn(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()
	// Discard the physical session even when unlock or cancellation fails.
	defer func() { _ = connection.Raw(func(any) error { return driver.ErrBadConn }) }()
	var locked bool
	var backend int
	if err := connection.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0)),pg_backend_pid()`, "ionos-pool:"+node).Scan(&locked, &backend); err != nil {
		return err
	}
	if !locked {
		return ionosPoolRefuse("worker_busy", "retry after the current worker command completes")
	}
	custody := &ionosPostgresCustody{connection: connection, tenant: tenant, node: node, backend: backend}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				custody.mu.Lock()
				err := verifyIONOSNodeLock(ctx, connection, node, backend)
				custody.mu.Unlock()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-done }()
	// Read the remaining lifetime from the database verifier, never from
	// untrusted token claims. Refuse before claiming or dispatching if the
	// synchronous budget plus receipt margin would exceed that identity.
	err = custody.transaction(ctx, func(tx *sql.Tx) error {
		var remaining float64
		if err := tx.QueryRowContext(ctx, `SELECT EXTRACT(EPOCH FROM ((kombify_ctx.verified_context()).expires_at-clock_timestamp()))::double precision`).Scan(&remaining); err != nil {
			return err
		}
		if remaining < (IONOSPoolSynchronousBudget + 30*time.Second).Seconds() {
			return ionosPoolRefuse("identity_refresh_required", "retry through Gateway with a fresh database identity before dispatch")
		}
		return nil
	})
	if err != nil {
		return err
	}
	return fn(ctx, custody)
}
func (s *ionosPostgresCustody) transaction(ctx context.Context, fn func(*sql.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.connection.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := tenantcontext.ApplyTenant(ctx, tx, s.tenant); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := verifyIONOSNodeLock(ctx, tx, s.node, s.backend); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *ionosPostgresCustody) WithNode(ctx context.Context, node string, fn func(IONOSSharedPoolLockedCustody) error) error {
	if s.node != node {
		return errors.New("worker lock mismatch")
	}
	return fn(s)
}
func (s *ionosPostgresCustody) Records(ctx context.Context) ([]IONOSSharedPoolRecord, error) {
	records := []IONOSSharedPoolRecord{}
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT spec,released,absence_evidence_ref FROM ionos_pool_generations WHERE tenant_id=$1 AND node_id=$2`, s.tenant, s.node)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var record IONOSSharedPoolRecord
			var data []byte
			if err := rows.Scan(&data, &record.Released, &record.AbsenceEvidenceRef); err != nil {
				return err
			}
			if err := json.Unmarshal(data, &record.Spec); err != nil {
				return err
			}
			records = append(records, record)
		}
		return rows.Err()
	})
	return records, err
}
func (s *ionosPostgresCustody) Capacity(ctx context.Context) (int, int, error) {
	var memory, slots int
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT memory_mb FROM ionos_pool_capacity WHERE node_id=$1),0),COALESCE((SELECT slots FROM ionos_pool_capacity WHERE node_id=$1),0)`, s.node).Scan(&memory, &slots)
	})
	return memory, slots, err
}
func (s *ionosPostgresCustody) Put(ctx context.Context, record IONOSSharedPoolRecord) error {
	if record.Spec.Workload.TenantID != s.tenant || record.Spec.NodeID != s.node {
		return errors.New("custody identity mismatch")
	}
	data, err := json.Marshal(record.Spec)
	if err != nil {
		return err
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `INSERT INTO ionos_pool_generations(tenant_id,node_id,generation_id,spec,memory_mb,released,absence_evidence_ref) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(tenant_id,generation_id) DO UPDATE SET released=EXCLUDED.released,absence_evidence_ref=EXCLUDED.absence_evidence_ref WHERE ionos_pool_generations.spec=EXCLUDED.spec`, s.tenant, s.node, record.Spec.Workload.Handle, string(data), record.Spec.Workload.MemoryMB, record.Released, record.AbsenceEvidenceRef)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ionosPoolRefuse("custody_refused", "repair the immutable generation binding")
		}
		return nil
	})
}
func (s *ionosPostgresCustody) claim(ctx context.Context, identity IONOSPoolIdentity, request IONOSPoolRequest, digest string, budget ManagedAppBudget, node IONOSSharedPoolNode, spec IONOSSharedPoolSpec) (*IONOSSharedPoolObservation, error) {
	var cached *IONOSSharedPoolObservation
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		var storedDigest string
		var result, refusal []byte
		err := tx.QueryRowContext(ctx, `SELECT request_digest,result,refusal FROM ionos_pool_commands WHERE tenant_id=$1 AND command_id=$2`, s.tenant, request.CommandID).Scan(&storedDigest, &result, &refusal)
		if err == nil {
			if storedDigest != digest {
				return ionosPoolRefuse("command_conflict", "use an immutable command ID for the exact workload action")
			}
			if len(refusal) > 0 {
				var rejected ManagedAppRefusal
				if err := json.Unmarshal(refusal, &rejected); err != nil {
					return err
				}
				return &rejected
			}
			if len(result) > 0 {
				cached = &IONOSSharedPoolObservation{}
				return json.Unmarshal(result, cached)
			}
			return ionosPoolRefuse("outcome_unknown", "observe or decommission this generation with a new command; never replay an uncertain dispatch")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var conflict bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ionos_pool_commands WHERE tenant_id=$1 AND generation_id=$2 AND result IS NULL AND NOT reconciled)`, s.tenant, request.GenerationID).Scan(&conflict); err != nil {
			return err
		}
		if conflict && request.Action != IONOSPoolObserve && request.Action != IONOSPoolDecommission {
			return ionosPoolRefuse("outcome_unknown", "observe or decommission the retained generation before another mutation")
		}
		// A generation belongs to its initial app and commercial allocation.
		var app, subject string
		err = tx.QueryRowContext(ctx, `SELECT app_id,subject_id FROM ionos_pool_commands WHERE tenant_id=$1 AND generation_id=$2 ORDER BY created_at LIMIT 1`, s.tenant, request.GenerationID).Scan(&app, &subject)
		if err == nil && (app != request.AppID || subject != identity.SubjectID) {
			return ionosPoolRefuse("ownership_refused", "use the generation's owning subject, app and allocated slot")
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := AdmitManagedAppDispatchTx(ctx, tx, identity, ManagedAppDispatch{GenerationID: request.GenerationID, AppID: request.AppID, Placement: "ionos-shared-pool", Action: string(request.Action)}, budget); err != nil {
			return err
		}
		if request.Action == IONOSPoolProvision {
			var exists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ionos_pool_generations WHERE tenant_id=$1 AND generation_id=$2)`, s.tenant, request.GenerationID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				var memory, slots int
				if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT memory_mb FROM ionos_pool_capacity WHERE node_id=$1),0),COALESCE((SELECT slots FROM ionos_pool_capacity WHERE node_id=$1),0)`, s.node).Scan(&memory, &slots); err != nil {
					return err
				}
				if slots >= node.Slots || memory > node.MemoryMB-node.ReservedMemoryMB-spec.Workload.MemoryMB {
					return ionosPoolRefuse("capacity_refused", "select an admitted worker with a free slot and enough RAM")
				}
				data, err := json.Marshal(spec)
				if err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO ionos_pool_generations(tenant_id,node_id,generation_id,spec,memory_mb) VALUES($1,$2,$3,$4,$5)`, s.tenant, s.node, request.GenerationID, string(data), spec.Workload.MemoryMB); err != nil {
					return err
				}
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO ionos_pool_commands(tenant_id,command_id,node_id,generation_id,subject_id,app_id,action,request_digest,claim_until) VALUES($1,$2,$3,$4,$5,$6,$7,$8,clock_timestamp()+interval '5 minutes')`, s.tenant, request.CommandID, s.node, request.GenerationID, identity.SubjectID, request.AppID, request.Action, digest)
		return err
	})
	return cached, err
}
func (s *ionosPostgresCustody) complete(ctx context.Context, request IONOSPoolRequest, digest string, observation IONOSSharedPoolObservation) error {
	data, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE ionos_pool_commands SET result=$1 WHERE tenant_id=$2 AND command_id=$3 AND request_digest=$4 AND result IS NULL AND claim_until>clock_timestamp()`, string(data), s.tenant, request.CommandID, digest)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrExecutionClaimLost
		}
		if observation.State == "absent" && observation.IsolationRemoved {
			var absent bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ionos_pool_generations WHERE tenant_id=$1 AND generation_id=$2 AND released AND absence_evidence_ref=$3)`, s.tenant, request.GenerationID, observation.EvidenceRef).Scan(&absent); err != nil {
				return err
			}
			if !absent {
				return ionosPoolRefuse("absence_unconfirmed", "require durable exact-generation absence")
			}
			if err := ReleaseManagedAppSlotTx(ctx, tx, ManagedAppIdentity{TenantID: s.tenant}, request.GenerationID, "ionos-shared-pool", observation.EvidenceRef); err != nil {
				return err
			}
		}
		if request.Action == IONOSPoolObserve || request.Action == IONOSPoolDecommission {
			_, err = tx.ExecContext(ctx, `UPDATE ionos_pool_commands SET reconciled=true WHERE tenant_id=$1 AND generation_id=$2 AND result IS NULL`, s.tenant, request.GenerationID)
		}
		return err
	})
}

// Verify both backend affinity and ownership of the exact 64-bit session lock.
// A transaction pooler is unsupported; changed backend or lost lock cancels dispatch.
func verifyIONOSNodeLock(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, node string, backend int) error {
	var held bool
	err := q.QueryRowContext(ctx, `SELECT pg_backend_pid()=$2 AND EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND pid=$2 AND granted AND objsubid=1 AND classid=((hashtextextended($1,0)>>32)&4294967295)::oid AND objid=(hashtextextended($1,0)&4294967295)::oid)`, "ionos-pool:"+node, backend).Scan(&held)
	if err != nil {
		return err
	}
	if !held {
		return ErrExecutionClaimLost
	}
	return nil
}

// A confirmed refusal before any mutating worker call is terminal and replayable.
func (s *ionosPostgresCustody) reject(ctx context.Context, request IONOSPoolRequest, digest string, refusal *ManagedAppRefusal) error {
	data, err := json.Marshal(refusal)
	if err != nil {
		return err
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE ionos_pool_commands SET refusal=$1,reconciled=true WHERE tenant_id=$2 AND command_id=$3 AND request_digest=$4 AND result IS NULL AND refusal IS NULL`, string(data), s.tenant, request.CommandID, digest)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrExecutionClaimLost
		}
		return nil
	})
}
