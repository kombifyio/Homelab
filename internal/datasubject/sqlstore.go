package datasubject

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/kombifyio/techstack/pkg/db"
)

const (
	pgFKViolation       = "23503"
	pgUndefinedFunction = "42883"
)

// Tables are discovered from the catalog by owner_subject_id (plus the lease
// table's subject_id), so a new owner-scoped table is covered without touching
// this code. Columns that name an acting operator (actor_subject_id,
// requested_by_subject_id, ...) are audit trail, not owner data, and stay out.
type tableColumn struct{ table, column string }

// evidenceTables hold provider execution evidence (cost-bearing capacity
// ledgers and identity mutation receipts). Erasure keeps them and reports them
// as retained.
var evidenceTables = map[string]bool{
	"managed_runtime_capacity_reservations":       true,
	"managed_runtime_server_slots":                true,
	"provider_stale_capacity_recovery_candidates": true,
	"homelab_identity_mutation_receipts":          true,
}

// SQLStore is the Postgres Store over the kombify_runtime database. Every
// statement runs in a transaction scoped to the principal's tenant, so row
// level security applies.
type SQLStore struct{ DB *sql.DB }

// sqlProcessorRoute inspects the selected driver's configuration, never an edition label.
func sqlProcessorRoute(ctx context.Context, database *sql.DB) (processorRoute, error) {
	if database == nil {
		return "", BlockedError{Reason: "processor_route_unqualified"}
	}
	conn, err := database.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	var route processorRoute
	err = conn.Raw(func(raw any) error {
		c, ok := raw.(*stdlib.Conn)
		if !ok {
			return BlockedError{Reason: "processor_route_unqualified"}
		}
		cfg := c.Conn().Config()
		route, err = postgresProcessorRoute(cfg.Host)
		if err != nil {
			return BlockedError{Reason: "processor_route_unqualified"}
		}
		for _, fallback := range cfg.Fallbacks {
			r, ferr := postgresProcessorRoute(fallback.Host)
			if ferr != nil || r != route {
				return BlockedError{Reason: "processor_route_unqualified"}
			}
		}
		return nil
	})
	return route, err
}

func (s *SQLStore) processorBinding(ctx context.Context) error {
	_, err := sqlProcessorRoute(ctx, s.DB)
	return err
}
func (d *SQLDirectory) processorBinding(ctx context.Context) error {
	_, err := sqlProcessorRoute(ctx, d.DB)
	return err
}

func ident(parts ...string) string { return pgx.Identifier(parts).Sanitize() }

func (s *SQLStore) tx(ctx context.Context, p Principal) (*sql.Tx, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("datasubject: database handle is nil")
	}
	return db.BeginTenantTransaction(ctx, s.DB, p.TenantID)
}

// subjectTables returns table/column pairs holding the subject.
func subjectTables(ctx context.Context, tx *sql.Tx) ([]tableColumn, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT c.table_name, c.column_name FROM information_schema.columns c
		JOIN information_schema.tables t
		  ON t.table_schema = c.table_schema AND t.table_name = c.table_name AND t.table_type = 'BASE TABLE'
		WHERE c.table_schema = 'public'
		  AND (c.column_name = 'owner_subject_id'
		       OR (c.table_name = 'techstack_vm_leases' AND c.column_name = 'subject_id'))
		ORDER BY c.table_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tableColumn
	for rows.Next() {
		var tc tableColumn
		if err := rows.Scan(&tc.table, &tc.column); err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}

// liveResources counts servers and leases that still map to provider capacity.
// A lease counts until its decommission is handed to provider control
// (desired_state 'absent', the indexed column's name for the archived lease);
// its server row then counts until provider absence is proven.
func liveResources(ctx context.Context, tx *sql.Tx, tables []tableColumn, sub string, onRead func()) (int64, error) {
	var total int64
	for _, tc := range tables {
		var query string
		switch tc.table {
		case "servers":
			query = `SELECT count(*) FROM servers WHERE owner_subject_id = $1
				AND (lifecycle_state IN ('provisioning', 'enrolling', 'active', 'decommissioning')
				     OR (provider_ref IS NOT NULL AND lifecycle_state <> 'decommissioned'))`
		case "techstack_vm_leases":
			query = `SELECT count(*) FROM techstack_vm_leases WHERE subject_id = $1 AND desired_state <> 'absent'`
		default:
			continue
		}
		var n int64
		if err := tx.QueryRowContext(ctx, query, sub).Scan(&n); err != nil {
			return 0, err
		}
		onRead()
		total += n
	}
	return total, nil
}

// Inventory counts the principal's rows per table and its live resources.
func (s *SQLStore) Inventory(ctx context.Context, p Principal) (Inventory, error) {
	route, err := sqlProcessorRoute(ctx, s.DB)
	if err != nil {
		return Inventory{}, err
	}
	tx, err := s.tx(ctx, p)
	if err != nil {
		return Inventory{}, err
	}
	defer func() { _ = tx.Rollback() }()
	tables, err := subjectTables(ctx, tx)
	if err != nil {
		return Inventory{}, err
	}
	recordProcessor(ctx, route, processorStoreRead, "")
	action := processorStoreRead
	if kind, _ := ctx.Value(processorInventoryKind{}).(bool); kind {
		action = processorExportInventoryRead
	}
	onRead := func() { recordProcessor(ctx, route, action, "") }
	var inv Inventory
	for _, tc := range tables {
		var n int64
		if err := tx.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s = $1`, ident(tc.table), ident(tc.column)), p.Subject).Scan(&n); err != nil {
			return Inventory{}, err
		}
		onRead()
		if n > 0 {
			inv.Tables = append(inv.Tables, TableRows{Table: tc.table, Rows: n, Evidence: evidenceTables[tc.table]})
		}
	}
	if inv.LiveResources, err = liveResources(ctx, tx, tables, p.Subject, onRead); err != nil {
		return Inventory{}, err
	}
	sort.Slice(inv.Tables, func(i, j int) bool { return inv.Tables[i].Table < inv.Tables[j].Table })
	return inv, nil
}

// Erase deletes the principal's non-evidence rows. Live resources are checked
// again inside the deleting transaction, so a server provisioned between the
// inventory and the delete still blocks.
func (s *SQLStore) Erase(ctx context.Context, p Principal) (map[string]int64, error) {
	route, err := sqlProcessorRoute(ctx, s.DB)
	if err != nil {
		return nil, err
	}
	tx, err := s.tx(ctx, p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	tables, err := subjectTables(ctx, tx)
	if err != nil {
		return nil, err
	}
	onRead := func() { recordProcessor(ctx, route, processorStoreRead, "") }
	onRead()
	live, err := liveResources(ctx, tx, tables, p.Subject, onRead)
	if err != nil {
		return nil, err
	}
	if live > 0 {
		return nil, BlockedError{Reason: ReasonLiveResources}
	}
	var pending []tableColumn
	for _, tc := range tables {
		if !evidenceTables[tc.table] {
			pending = append(pending, tc)
		}
	}
	deleted := map[string]int64{}
	for len(pending) > 0 {
		var next []tableColumn
		for _, tc := range pending {
			n, fkBlocked, err := deleteOwned(ctx, tx, tc, p.Subject)
			if err != nil {
				return nil, err
			}
			if fkBlocked {
				next = append(next, tc)
				continue
			}
			deleted[tc.table] += n
		}
		if len(next) == len(pending) {
			return nil, BlockedError{Reason: ReasonEraseIncomplete}
		}
		pending = next
	}
	// Once nothing at all remains (retained evidence rows still name the
	// subject), the directory entry goes too: it holds the raw subject id.
	remaining, err := remainingRows(ctx, tx, tables, p.Subject, onRead)
	if err != nil {
		return nil, err
	}
	if remaining == 0 {
		if _, err := tx.ExecContext(ctx, `SELECT techstack_forget_subject_directory($1, $2)`, p.TenantID, p.Subject); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for _, n := range deleted {
		if n > 0 {
			recordProcessor(ctx, route, processorDeleted, "")
			break
		}
	}
	return deleted, nil
}

func remainingRows(ctx context.Context, tx *sql.Tx, tables []tableColumn, sub string, onRead func()) (int64, error) {
	var total int64
	for _, tc := range tables {
		var n int64
		if err := tx.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s = $1`, ident(tc.table), ident(tc.column)), sub).Scan(&n); err != nil {
			return 0, err
		}
		onRead()
		total += n
	}
	return total, nil
}

// SQLDirectory resolves subject refs through the migration-owned subject
// directory. Its only read path is an exact-ref SECURITY DEFINER lookup, so
// row-level security stays in force for every other query. With the migration
// not applied the lookup fails with an undefined-function error, which the
// executor reports as blocked rather than as "nothing held".
type SQLDirectory struct{ DB *sql.DB }

func (d *SQLDirectory) Lookup(ctx context.Context, subjectRef string) ([]Principal, error) {
	if d == nil || d.DB == nil {
		return nil, errors.New("datasubject: database handle is nil")
	}
	route, err := sqlProcessorRoute(ctx, d.DB)
	if err != nil {
		return nil, err
	}
	rows, err := d.DB.QueryContext(ctx, `SELECT tenant_id, owner_subject_id FROM techstack_lookup_subject_directory($1)`, subjectRef)
	if err != nil {
		return nil, err
	}
	recordProcessor(ctx, route, processorSubjectLookup, "")
	defer rows.Close()
	var out []Principal
	for rows.Next() {
		var p Principal
		if err := rows.Scan(&p.TenantID, &p.Subject); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// deleteOwned deletes one table's rows inside a savepoint, so a foreign-key
// violation only skips that table for this pass.
func deleteOwned(ctx context.Context, tx *sql.Tx, tc tableColumn, sub string) (int64, bool, error) {
	if _, err := tx.ExecContext(ctx, "SAVEPOINT datasubject_erase"); err != nil {
		return 0, false, err
	}
	res, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE %s = $1`, ident(tc.table), ident(tc.column)), sub)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgFKViolation {
			if _, rbErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT datasubject_erase"); rbErr != nil {
				return 0, false, rbErr
			}
			return 0, true, nil
		}
		return 0, false, err
	}
	if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT datasubject_erase"); err != nil {
		return 0, false, err
	}
	n, _ := res.RowsAffected()
	return n, false, nil
}
