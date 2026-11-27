package providercontrol

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kombifyio/homelab/pkg/db/tenantcontext"
)

type flyMachinesPostgresCustody struct {
	database     *sql.DB
	gateMu       sync.Mutex
	maxTotal     int
	maxPerTenant int
	activeTotal  int
	activeTenant map[string]int
}

func newFlyMachinesPostgresCustody(database *sql.DB, maxConcurrent int) FlyMachinesCustody {
	if maxConcurrent <= 0 {
		maxConcurrent = flyMachinesDefaultMaxWorkers
	}
	return &flyMachinesPostgresCustody{
		database:     database,
		maxTotal:     maxConcurrent,
		maxPerTenant: 1,
		activeTenant: make(map[string]int),
	}
}

func (p *flyMachinesPostgresCustody) acquireSlot(tenantID string) bool {
	p.gateMu.Lock()
	defer p.gateMu.Unlock()
	if p.activeTotal >= p.maxTotal || p.activeTenant[tenantID] >= p.maxPerTenant {
		return false
	}
	p.activeTotal++
	p.activeTenant[tenantID]++
	return true
}

func (p *flyMachinesPostgresCustody) releaseSlot(tenantID string) {
	p.gateMu.Lock()
	defer p.gateMu.Unlock()
	if p.activeTotal > 0 {
		p.activeTotal--
	}
	if p.activeTenant[tenantID] <= 1 {
		delete(p.activeTenant, tenantID)
	} else {
		p.activeTenant[tenantID]--
	}
}

type flyMachinesPostgresSession struct {
	connection *sql.Conn
	tenant     string
	lockKey    string
	backend    int
	mu         sync.Mutex
}

// A session advisory lock spans separate committed custody writes and Fly API
// calls. Transaction locks would roll back generation claims or recorded machine
// IDs if a later readback or receipt step failed. A bounded total + per-tenant
// concurrency gate prevents one slow tenant from exhausting the shared Postgres
// pool or starving other tenants.
func (p *flyMachinesPostgresCustody) WithGeneration(ctx context.Context, tenantID, generationID string, fn func(context.Context, FlyMachinesLockedCustody) error) error {
	if !p.acquireSlot(tenantID) {
		return managedAppRefuse("worker_busy", "retry after an active Fly Machine command completes")
	}
	defer p.releaseSlot(tenantID)
	ctx, cancel := context.WithTimeout(ctx, flyMachinesClaimTTL)
	defer cancel()
	connection, err := tenantcontext.Database(ctx, p.database).Conn(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()
	// Discard the physical session even when unlock or cancellation fails.
	defer func() { _ = connection.Raw(func(any) error { return driver.ErrBadConn }) }()
	// Scope the advisory lock key to the authenticated tenant so another tenant
	// cannot cause worker_busy DoS even if it learns an opaque generation ID.
	lockKey := "fly-machine:" + tenantID + "\x00" + generationID
	var locked bool
	var backend int
	if err := connection.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0)),pg_backend_pid()`, lockKey).Scan(&locked, &backend); err != nil {
		return err
	}
	if !locked {
		return managedAppRefuse("worker_busy", "retry after the current Fly Machine command completes")
	}
	session := &flyMachinesPostgresSession{connection: connection, tenant: tenantID, lockKey: lockKey, backend: backend}
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
				session.mu.Lock()
				err := verifyFlyGenerationLock(ctx, connection, lockKey, backend)
				session.mu.Unlock()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-done }()
	// Read remaining lifetime from the database verifier, never from untrusted
	// token claims. Refuse before claiming or dispatching if the synchronous
	// budget plus receipt margin would exceed the verified database identity.
	err = session.transaction(ctx, func(tx *sql.Tx) error {
		var remaining float64
		if err := tx.QueryRowContext(ctx, `SELECT EXTRACT(EPOCH FROM ((kombify_ctx.verified_context()).expires_at-clock_timestamp()))::double precision`).Scan(&remaining); err != nil {
			return err
		}
		if remaining < (FlyMachinesSynchronousBudget + 30*time.Second).Seconds() {
			return managedAppRefuse("identity_refresh_required", "retry through Gateway with a fresh database identity before dispatch")
		}
		return nil
	})
	if err != nil {
		return err
	}
	return fn(ctx, session)
}

func (s *flyMachinesPostgresSession) transaction(ctx context.Context, fn func(*sql.Tx) error) error {
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
	if err := verifyFlyGenerationLock(ctx, tx, s.lockKey, s.backend); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *flyMachinesPostgresSession) Claim(ctx context.Context, in FlyMachineClaimRequest) (FlyMachineClaimOutcome, error) {
	var outcome FlyMachineClaimOutcome
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		var storedDigest string
		var result, refusal []byte
		err := tx.QueryRowContext(ctx, `SELECT request_digest,result,refusal FROM fly_machine_commands WHERE tenant_id=$1 AND command_id=$2`, s.tenant, in.Request.CommandID).Scan(&storedDigest, &result, &refusal)
		if err == nil {
			if storedDigest != in.RequestDigest {
				return managedAppRefuse("command_conflict", "use an immutable command ID for the exact workload action")
			}
			if len(refusal) > 0 {
				var rejected ManagedAppRefusal
				if err := json.Unmarshal(refusal, &rejected); err != nil {
					return err
				}
				return &rejected
			}
			if len(result) > 0 {
				cached := &FlyMachineCommandResult{}
				if err := json.Unmarshal(result, cached); err != nil {
					return err
				}
				outcome.Cached = cached
				return nil
			}
			return managedAppRefuse("outcome_unknown", "observe, stop or decommission this generation with a new command; never replay an uncertain dispatch")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var conflict bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM fly_machine_commands WHERE tenant_id=$1 AND generation_id=$2 AND result IS NULL AND NOT reconciled)`, s.tenant, in.Request.GenerationID).Scan(&conflict); err != nil {
			return err
		}
		if conflict && !IsManagedAppCleanup(string(in.Request.Action)) {
			return managedAppRefuse("outcome_unknown", "observe, stop or decommission the retained generation before another mutation")
		}
		var gen FlyMachineGeneration
		var specBytes []byte
		err = tx.QueryRowContext(ctx, `SELECT tenant_id,generation_id,subject_id,app_id,fly_app,machine_id,custody_hash,spec,released,absence_evidence_ref FROM fly_machine_generations WHERE tenant_id=$1 AND generation_id=$2 FOR UPDATE`, s.tenant, in.Request.GenerationID).Scan(
			&gen.TenantID, &gen.GenerationID, &gen.SubjectID, &gen.AppID, &gen.FlyApp, &gen.MachineID, &gen.CustodyHash, &specBytes, &gen.Released, &gen.AbsenceEvidenceRef,
		)
		if in.Request.Action == FlyMachineActionProvision {
			if err == nil {
				return managedAppRefuse("provision_already_claimed", "observe or decommission the existing generation, or allocate a new generation ID")
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err := AdmitManagedAppDispatchTx(ctx, tx, in.Identity, ManagedAppDispatch{
				GenerationID: in.Request.GenerationID, AppID: in.Request.AppID, Placement: "fly", Action: string(in.Request.Action),
			}, in.Budget); err != nil {
				return err
			}
			encodedSpec, err := json.Marshal(in.Spec)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO fly_machine_generations(tenant_id,generation_id,subject_id,app_id,fly_app,machine_id,custody_hash,spec) VALUES($1,$2,$3,$4,$5,'',$6,$7)`,
				s.tenant, in.Request.GenerationID, in.Identity.SubjectID, in.Request.AppID, in.FlyApp, in.CustodyHash, string(encodedSpec)); err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == "23505" {
					return managedAppRefuse("generation_unavailable", "use a new opaque generation identifier")
				}
				return err
			}
			gen = FlyMachineGeneration{
				TenantID:     s.tenant,
				GenerationID: in.Request.GenerationID,
				SubjectID:    in.Identity.SubjectID,
				AppID:        in.Request.AppID,
				FlyApp:       in.FlyApp,
				CustodyHash:  in.CustodyHash,
				Spec:         in.Spec,
			}
		} else {
			if errors.Is(err, sql.ErrNoRows) {
				return managedAppRefuse("ownership_refused", "use a Fly Machine generation owned by the authenticated tenant")
			}
			if err != nil {
				return err
			}
			if err := json.Unmarshal(specBytes, &gen.Spec); err != nil {
				return err
			}
			if gen.SubjectID != in.Identity.SubjectID || gen.AppID != in.Request.AppID || gen.FlyApp != in.FlyApp {
				return managedAppRefuse("ownership_refused", "use the generation's owning subject, app and Fly deployment")
			}
			if in.Request.MachineID != "" && (gen.MachineID == "" || in.Request.MachineID != gen.MachineID) {
				return managedAppRefuse("handle_mismatch", "use the ledger-owned Fly Machine handle for this generation")
			}
			if gen.Released {
				if in.Request.Action != FlyMachineActionObserve && in.Request.Action != FlyMachineActionDecommission {
					return managedAppRefuse("generation_released", "admit a new generation; released handles cannot be reused")
				}
			} else if gen.MachineID == "" {
				if !IsManagedAppCleanup(string(in.Request.Action)) {
					return managedAppRefuse("outcome_unknown", "observe, stop or decommission the unconfirmed Fly Machine generation to reconcile it")
				}
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM fly_machine_commands WHERE tenant_id=$1 AND generation_id=$2 AND action='provision' AND claim_until>clock_timestamp())`, s.tenant, in.Request.GenerationID).Scan(&gen.ProvisionInFlight); err != nil {
					return err
				}
			}
			if err := AdmitManagedAppDispatchTx(ctx, tx, in.Identity, ManagedAppDispatch{
				GenerationID: in.Request.GenerationID, AppID: in.Request.AppID, Placement: "fly", Action: string(in.Request.Action),
			}, in.Budget); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO fly_machine_commands(tenant_id,command_id,fly_app,generation_id,subject_id,app_id,action,request_digest,claim_until) VALUES($1,$2,$3,$4,$5,$6,$7,$8,clock_timestamp()+interval '3 minutes')`,
			s.tenant, in.Request.CommandID, in.FlyApp, in.Request.GenerationID, in.Identity.SubjectID, in.Request.AppID, string(in.Request.Action), in.RequestDigest); err != nil {
			return err
		}
		outcome.Generation = gen
		return nil
	})
	return outcome, err
}

func (s *flyMachinesPostgresSession) RecordMachineID(ctx context.Context, tenantID, generationID, machineID string) error {
	if tenantID != s.tenant || !flyMachinePathID(machineID) {
		return managedAppRefuse("custody_refused", "record a valid Fly Machine ID for the owning tenant")
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE fly_machine_generations SET machine_id=$1,updated_at=clock_timestamp() WHERE tenant_id=$2 AND generation_id=$3 AND (machine_id='' OR machine_id=$1)`, machineID, s.tenant, generationID)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return managedAppRefuse("custody_refused", "repair the immutable Fly Machine generation handle")
		}
		return nil
	})
}

func (s *flyMachinesPostgresSession) MarkReleased(ctx context.Context, tenantID, generationID, evidenceRef string) error {
	if tenantID != s.tenant || evidenceRef == "" {
		return managedAppRefuse("absence_unconfirmed", "require deterministic exact-generation absence evidence")
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE fly_machine_generations SET released=true,absence_evidence_ref=$1,updated_at=clock_timestamp() WHERE tenant_id=$2 AND generation_id=$3 AND ((NOT released) OR (released AND absence_evidence_ref=$1))`, evidenceRef, s.tenant, generationID)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return managedAppRefuse("absence_unconfirmed", "require durable exact-generation absence")
		}
		return nil
	})
}

func (s *flyMachinesPostgresSession) Complete(ctx context.Context, _ FlyMachinesIdentity, request FlyMachinesRequest, digest string, observation FlyMachineCommandResult) error {
	data, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE fly_machine_commands SET result=$1 WHERE tenant_id=$2 AND command_id=$3 AND request_digest=$4 AND result IS NULL AND claim_until>clock_timestamp()`, string(data), s.tenant, request.CommandID, digest)
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
		if observation.State == "absent" {
			var absent bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM fly_machine_generations WHERE tenant_id=$1 AND generation_id=$2 AND released AND absence_evidence_ref=$3)`, s.tenant, request.GenerationID, observation.EvidenceRef).Scan(&absent); err != nil {
				return err
			}
			if !absent {
				return managedAppRefuse("absence_unconfirmed", "require durable exact-generation absence")
			}
			if err := ReleaseManagedAppSlotTx(ctx, tx, ManagedAppIdentity{TenantID: s.tenant}, request.GenerationID, "fly", observation.EvidenceRef); err != nil {
				return err
			}
		}
		if request.Action == FlyMachineActionObserve || request.Action == FlyMachineActionDecommission {
			_, err = tx.ExecContext(ctx, `UPDATE fly_machine_commands SET reconciled=true WHERE tenant_id=$1 AND generation_id=$2 AND result IS NULL`, s.tenant, request.GenerationID)
		}
		return err
	})
}

func (s *flyMachinesPostgresSession) Reject(ctx context.Context, _ FlyMachinesIdentity, request FlyMachinesRequest, digest string, refusal *ManagedAppRefusal) error {
	data, err := json.Marshal(refusal)
	if err != nil {
		return err
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE fly_machine_commands SET refusal=$1,reconciled=true WHERE tenant_id=$2 AND command_id=$3 AND request_digest=$4 AND result IS NULL AND refusal IS NULL`, string(data), s.tenant, request.CommandID, digest)
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

// RejectAndRelease atomically tombstones an uncreated generation (machine_id=”),
// releases its Managed App slot reservation, and records the command refusal.
func (s *flyMachinesPostgresSession) RejectAndRelease(ctx context.Context, _ FlyMachinesIdentity, request FlyMachinesRequest, digest, evidenceRef string, refusal *ManagedAppRefusal) error {
	if evidenceRef == "" || refusal == nil {
		return managedAppRefuse("absence_unconfirmed", "require deterministic absence evidence and refusal")
	}
	data, err := json.Marshal(refusal)
	if err != nil {
		return err
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		genRes, err := tx.ExecContext(ctx, `UPDATE fly_machine_generations SET released=true,absence_evidence_ref=$1,updated_at=clock_timestamp() WHERE tenant_id=$2 AND generation_id=$3 AND machine_id='' AND NOT released`, evidenceRef, s.tenant, request.GenerationID)
		if err != nil {
			return err
		}
		genCount, err := genRes.RowsAffected()
		if err != nil {
			return err
		}
		if genCount != 1 {
			return managedAppRefuse("absence_unconfirmed", "cannot release generation with an existing machine handle")
		}
		if err := ReleaseManagedAppSlotTx(ctx, tx, ManagedAppIdentity{TenantID: s.tenant}, request.GenerationID, "fly", evidenceRef); err != nil {
			return err
		}
		cmdRes, err := tx.ExecContext(ctx, `UPDATE fly_machine_commands SET refusal=$1,reconciled=true WHERE tenant_id=$2 AND command_id=$3 AND request_digest=$4 AND result IS NULL AND refusal IS NULL`, string(data), s.tenant, request.CommandID, digest)
		if err != nil {
			return err
		}
		cmdCount, err := cmdRes.RowsAffected()
		if err != nil {
			return err
		}
		if cmdCount != 1 {
			return ErrExecutionClaimLost
		}
		return nil
	})
}

func verifyFlyGenerationLock(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, lockKey string, backend int) error {
	var held bool
	err := q.QueryRowContext(ctx, `SELECT pg_backend_pid()=$2 AND EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND pid=$2 AND granted AND objsubid=1 AND classid=((hashtextextended($1,0)>>32)&4294967295)::oid AND objid=(hashtextextended($1,0)&4294967295)::oid)`, lockKey, backend).Scan(&held)
	if err != nil {
		return err
	}
	if !held {
		return ErrExecutionClaimLost
	}
	return nil
}
