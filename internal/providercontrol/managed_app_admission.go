package providercontrol

import (
	"context"
	"database/sql"
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
	"time"
)

type ManagedAppIdentity struct{ TenantID, SubjectID string }

// Cloud owns the allowance; Homelab owns durable reservations across placements.
type ManagedAppBudget struct {
	AccountID         string    `json:"account_id"`
	Subject           string    `json:"subject"`
	RequestedTenantID string    `json:"requested_tenant_id"`
	RequestID         string    `json:"request_id"`
	Limit             int       `json:"limit"`
	ValidUntil        time.Time `json:"valid_until"`
}

// All Managed App provider dispatches enter here before provider-specific work.
// The caller's tenant-bound transaction also reserves placement capacity, so
// admission and node capacity either commit together or neither commits.
type ManagedAppDispatch struct{ GenerationID, AppID, Placement, Action string }

func AdmitManagedAppDispatchTx(ctx context.Context, tx *sql.Tx, identity ManagedAppIdentity, request ManagedAppDispatch, budget ManagedAppBudget) error {
	if identity.TenantID == "" || identity.SubjectID == "" || request.GenerationID == "" || request.AppID == "" || (request.Placement != "ionos-shared-pool" && request.Placement != "fly" && request.Placement != "cloud-run") {
		return managedAppRefuse("ownership_refused", "use a verified identity and exact placement generation")
	}
	cleanup := IsManagedAppCleanup(request.Action)
	if !cleanup && (budget.AccountID == "" || len(budget.AccountID) > 256 || budget.Subject != identity.SubjectID || budget.RequestedTenantID != identity.TenantID || budget.RequestID == "" || budget.Limit < 0 || budget.Limit > 100000 || !budget.ValidUntil.After(time.Now()) || budget.ValidUntil.After(time.Now().Add(30*time.Second))) {
		return managedAppRefuse("budget_refused", "obtain a fresh account-bound Cloud allowance")
	}
	var account, subject, app, placement, state string
	err := tx.QueryRowContext(ctx, `SELECT account_id,subject_id,app_id,placement,state FROM managed_app_slot_reservations WHERE tenant_id=$1 AND generation_id=$2 FOR UPDATE`, identity.TenantID, request.GenerationID).Scan(&account, &subject, &app, &placement, &state)
	if err == nil {
		if subject != identity.SubjectID || app != request.AppID || placement != request.Placement || (account != budget.AccountID && !cleanup) {
			return managedAppRefuse("ownership_refused", "use the exact owning workload reservation")
		}
		if state == "released" && request.Action != "observe" && request.Action != "decommission" {
			return managedAppRefuse("generation_released", "admit a new generation")
		}
		if budget.Limit == 0 && !cleanup {
			return managedAppRefuse("entitlement_refused", "restore the account Managed App allowance")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if request.Action != "provision" || budget.Limit <= 0 {
		return managedAppRefuse("slot_admission_refused", "obtain a positive Managed App allowance before provisioning")
	}
	var locked bool
	if err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`, "managed-app-account:"+budget.AccountID).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return managedAppRefuse("admission_busy", "retry account admission after the concurrent transaction")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO managed_app_slot_reservations(tenant_id,generation_id,account_id,subject_id,app_id,placement,allowance_limit,allowance_until) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, identity.TenantID, request.GenerationID, budget.AccountID, identity.SubjectID, request.AppID, request.Placement, budget.Limit, budget.ValidUntil)
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) {
		if pgError.ConstraintName == "managed_app_slot_budget" {
			return managedAppRefuse("slot_limit_exhausted", "remove an existing Managed App with confirmed absence before reserving another slot")
		}
		if pgError.Code == "23505" {
			return managedAppRefuse("generation_unavailable", "use a new opaque generation identifier")
		}
	}
	return err
}

// Call only after the placement's authoritative readback and durable tombstone
// confirm absence of this exact generation, including its isolation resources.
func ReleaseManagedAppSlotTx(ctx context.Context, tx *sql.Tx, identity ManagedAppIdentity, generation, placement, evidence string) error {
	if identity.TenantID == "" || generation == "" || evidence == "" {
		return managedAppRefuse("absence_unconfirmed", "require exact-generation absence evidence")
	}
	result, err := tx.ExecContext(ctx, `UPDATE managed_app_slot_reservations SET state='released',absence_evidence_ref=$1,released_at=clock_timestamp(),updated_at=clock_timestamp() WHERE tenant_id=$2 AND generation_id=$3 AND placement=$4 AND state='held'`, evidence, identity.TenantID, generation, placement)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		var released bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM managed_app_slot_reservations WHERE tenant_id=$1 AND generation_id=$2 AND placement=$3 AND state='released' AND absence_evidence_ref=$4)`, identity.TenantID, generation, placement, evidence).Scan(&released); err != nil {
			return err
		}
		if !released {
			return managedAppRefuse("absence_unconfirmed", "require the exact held reservation")
		}
	}
	return nil
}

type ManagedAppRefusal struct{ Code, Repair string }

func (e *ManagedAppRefusal) Error() string { return "managed-app: " + e.Code + ": " + e.Repair }
func managedAppRefuse(code, repair string) error {
	return &ManagedAppRefusal{Code: code, Repair: repair}
}

func IsManagedAppCleanup(action string) bool {
	return action == "observe" || action == "stop" || action == "decommission"
}
