package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/kombifyio/homelab/internal/runtimeproduct/vmlease"
	"github.com/kombifyio/homelab/pkg/vmleases"
)

// Sensitive migration boundary: the shared staging ledger must not be edited to
// manufacture partial-prefix, hole, duplicate or foreign-ledger failure cases.
func TestForwardBatchRejectsChangedLedger(t *testing.T) {
	batch, err := DeliveryForwardBatch()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	pending := map[string]bool{}
	for _, artifact := range batch {
		pending[artifact.Version] = true
	}
	basis := []string{}
	for _, entry := range entries {
		if !entry.IsDir() && !pending[entry.Name()] {
			basis = append(basis, entry.Name())
		}
	}
	check := func(applied []string) (int, error) {
		expected := map[string]bool{}
		for _, version := range basis {
			expected[version] = true
		}
		return compareForwardBatchLedger(expected, applied, batch)
	}
	for prefix := 0; prefix <= len(batch); prefix++ {
		applied := append([]string{}, basis...)
		for _, artifact := range batch[:prefix] {
			applied = append(applied, artifact.Version)
		}
		if observed, err := check(applied); err != nil || observed != prefix {
			t.Fatalf("committed prefix %d refused: %d %v", prefix, observed, err)
		}
	}
	for _, applied := range [][]string{append(append([]string{}, basis...), batch[1].Version),
		append(append([]string{}, basis...), batch[0].Version, batch[2].Version),
		append(append([]string{}, basis...), batch[0].Version, batch[0].Version),
		append(append([]string{}, basis...), "unexpected.sql"), basis[1:],
		append(append([]string{}, basis...), batch[0].Version, "999_newer.sql")} {
		if _, err := check(applied); !errors.Is(err, ErrForwardBatchLedger) {
			t.Fatalf("changed ledger admitted: %v", err)
		}
	}
	newer := append([]string{}, basis...)
	for _, artifact := range batch {
		newer = append(newer, artifact.Version)
	}
	if observed, err := check(append(newer, "999_newer.sql")); err != nil || observed != len(batch) {
		t.Fatalf("ledger of a newer deploy refused: %d %v", observed, err)
	}
	badHash := append([]ForwardMigrationArtifact{}, batch...)
	badHash[0].SHA256 = strings.Repeat("0", 64)
	for _, invalid := range [][]ForwardMigrationArtifact{nil, batch[:len(batch)-1],
		append(append([]ForwardMigrationArtifact{}, batch[:1]...), batch[2:]...), badHash,
		{{Version: "../164.sql", SHA256: strings.Repeat("0", 64)}}} {
		if _, err := forwardBatchBodies(invalid); err == nil {
			t.Fatal("invalid or incomplete manifest admitted")
		}
	}
}

// Sensitive migration boundary: the previous binary keeps serving after a
// database leg only when every newer ledger entry is a proven additive
// migration. Every embedded *.additive.sql must pass the allowlist.
func TestAdditiveMigrationsStayAdditive(t *testing.T) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), AdditiveMigrationSuffix) {
			continue
		}
		if _, ok := migrationNumber(entry.Name()); !ok {
			t.Errorf("%s: additive migration needs a numbered lower-case name", entry.Name())
		}
		body, err := migrationsFS.ReadFile(path.Join("migrations", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateAdditiveMigrationSQL(string(body)); err != nil {
			t.Errorf("%s: %v", entry.Name(), err)
		}
	}

	admitted := []string{
		"SET LOCAL lock_timeout = '5s';\nCREATE TABLE guard_notes (id uuid PRIMARY KEY, note text NOT NULL);\nCREATE UNIQUE INDEX guard_notes_note ON guard_notes (note);\nALTER TABLE guard_notes ENABLE ROW LEVEL SECURITY;\nCREATE POLICY guard_notes_owner ON guard_notes USING (true);\nGRANT SELECT, INSERT ON guard_notes TO techstack_application;\nCOMMENT ON TABLE guard_notes IS 'a; b';",
		"-- nullable and constant defaults neither rewrite nor break old inserts\nALTER TABLE servers ADD COLUMN label text, ADD COLUMN IF NOT EXISTS retries integer NOT NULL DEFAULT 0;",
		"ALTER TABLE servers ADD COLUMN settings jsonb DEFAULT '{}'::jsonb;",
		"ALTER TABLE servers ADD COLUMN owner_id uuid REFERENCES users(id);",
		`CREATE TABLE "Servers" (id integer); ALTER TABLE "Servers" ADD COLUMN name text;`,
	}
	for _, body := range admitted {
		if err := ValidateAdditiveMigrationSQL(body); err != nil {
			t.Errorf("additive migration refused: %v", err)
		}
	}
	refused := []string{
		"",
		`CREATE TABLE "Servers" (id integer); ALTER TABLE servers DROP COLUMN name;`,
		`CREATE TABLE "a  b" (id integer); ALTER TABLE "a b" DROP COLUMN name;`,
		`CREATE TABLE public.servers (id integer); ALTER TABLE servers DROP COLUMN name;`,
		`CREATE TABLE servers (id integer); SET LOCAL search_path = other; ALTER TABLE servers DROP COLUMN name;`,
		`CREATE TABLE servers (id integer); SELECT pg_catalog.set_config('search_path', 'other', true); ALTER TABLE servers DROP COLUMN name;`,
		`GRANT SELECT ON servers TO public;`,
		`CREATE TABLE new_table (id integer); GRANT SELECT ON new_table TO public;`,
		`GRANT SELECT ON servers TO techstack_application;`,
		"DROP TABLE servers;",
		"ALTER TABLE servers DROP COLUMN label;",
		"ALTER TABLE servers RENAME COLUMN label TO name;",
		"ALTER TABLE servers ALTER COLUMN label SET NOT NULL;",
		"ALTER TABLE servers ADD CONSTRAINT servers_label CHECK (label <> '');",
		"ALTER TABLE servers ADD COLUMN label text NOT NULL;",
		"ALTER TABLE servers ADD COLUMN created timestamptz DEFAULT now();",
		"ALTER TABLE servers ADD COLUMN id2 uuid NOT NULL DEFAULT gen_random_uuid();",
		"ALTER TABLE servers ADD COLUMN label text UNIQUE;",
		"ALTER TABLE servers ENABLE ROW LEVEL SECURITY;",
		"CREATE INDEX servers_label ON servers (label);",
		"CREATE UNIQUE INDEX servers_label ON servers (label);",
		"CREATE POLICY servers_all ON servers USING (true);",
		"CREATE TABLE IF NOT EXISTS servers (id uuid);",
		"CREATE TABLE servers_copy AS SELECT * FROM servers;",
		"CREATE OR REPLACE FUNCTION f() RETURNS int AS $$ SELECT 1 $$ LANGUAGE sql;",
		"DO $$ BEGIN END $$;",
		"CREATE TRIGGER t BEFORE INSERT ON servers FOR EACH ROW EXECUTE FUNCTION f();",
		"INSERT INTO servers (id) VALUES ('x');",
		"UPDATE servers SET label = 'x';",
		"REVOKE SELECT ON servers FROM techstack_application;",
		"SET search_path = public;",
	}
	for _, body := range refused {
		if err := ValidateAdditiveMigrationSQL(body); err == nil {
			t.Errorf("non-additive migration admitted: %q", body)
		}
	}
}

func TestForwardExpandHoldsContractAndLaterMigrations(t *testing.T) {
	migrations := []forwardExpandInput{
		{name: "177_first.additive.sql", body: []byte("SET LOCAL lock_timeout = '5s'; SET LOCAL statement_timeout = '2min'; ALTER TABLE servers ADD COLUMN label text;")},
		{name: "178_contract.sql", body: []byte("ALTER TABLE servers DROP COLUMN old_label;")},
		{name: "179_later.additive.sql", body: []byte("SET LOCAL lock_timeout = '5s'; SET LOCAL statement_timeout = '2min'; ALTER TABLE servers ADD COLUMN note text;")},
	}
	limit, err := forwardExpandLimit(migrations, 0)
	if err != nil || limit != 1 {
		t.Fatalf("expand limit = %d, error = %v; want first migration only", limit, err)
	}
	limit, err = forwardExpandLimit(migrations, 2)
	if err != nil || limit != 3 {
		t.Fatalf("expand limit after applied contract = %d, error = %v; want 3", limit, err)
	}
	limit, err = forwardExpandLimit(migrations, 1)
	if err != nil || limit != 1 {
		t.Fatalf("contract was not held: limit = %d, error = %v", limit, err)
	}
	if _, err = forwardExpandLimit(migrations, 4); err == nil {
		t.Fatal("out-of-range ledger prefix admitted")
	}
	unsafe := []forwardExpandInput{{name: "180_unsafe.additive.sql", body: []byte("SET LOCAL lock_timeout = '5s'; SET LOCAL statement_timeout = '2min'; DROP TABLE servers;")}}
	if limit, err = forwardExpandLimit(unsafe, 0); err != nil || limit != 0 {
		t.Fatalf("unsafe marked migration admitted: limit = %d, error = %v", limit, err)
	}
	unsafe = []forwardExpandInput{{name: "180_unsafe.additive.sql", body: []byte("SET LOCAL lock_timeout = '5s'; SET LOCAL statement_timeout = '2min'; CREATE TABLE x (tenant_id text); CREATE POLICY x_all ON x USING (true);")}}
	if err := ValidateAdditiveMigrationSQL(string(unsafe[0].body)); err != nil {
		t.Fatalf("control no longer demonstrates the additional shared classifier: %v", err)
	}
	if limit, err = forwardExpandLimit(unsafe, 0); err != nil || limit != 0 {
		t.Fatalf("unconfined tenant policy admitted: limit = %d, error = %v", limit, err)
	}
}

func TestEmbeddedForwardExpandHoldsLegacyTail(t *testing.T) {
	batch, err := DeliveryForwardBatch()
	if err != nil {
		t.Fatal(err)
	}
	limit, err := embeddedForwardExpandLimit(batch, 0)
	if err != nil || limit != 0 {
		t.Fatalf("legacy forward tail: limit = %d, error = %v; want hold", limit, err)
	}
}

func TestForwardExpandRefusesSelectedTail(t *testing.T) {
	batch, err := DeliveryForwardBatch()
	if err != nil || len(batch) < 2 {
		t.Fatalf("delivery batch unavailable: %v", err)
	}
	t.Setenv(EnvDatabaseMigrationMode, "apply")
	t.Setenv(EnvAllowDBGenesis, "false")
	var target *DB
	_, err = target.MigrateForwardExpand(context.Background(), batch[1:])
	if err == nil || !strings.Contains(err.Error(), "complete embedded delivery manifest") {
		t.Fatalf("selected tail error = %v", err)
	}
}

// Sensitive migration boundary: boot admits a ledger that meets the schema
// floor, whatever is newer or not applied above it (D-2026-10-27-06).
func TestSchemaFloorAdmitsAnyOrder(t *testing.T) {
	expected := map[string]bool{"163_servers.sql": true, "164_routine.sql": true, "165_notes.sql": true}
	applied := func(versions ...string) map[string]bool {
		set := map[string]bool{}
		for _, version := range versions {
			set[version] = true
		}
		return set
	}
	for _, ledger := range []map[string]bool{
		applied("163_servers.sql", "164_routine.sql", "165_notes.sql"),
		applied("163_servers.sql", "164_routine.sql"),
		applied("163_servers.sql", "164_routine.sql", "166_drop.sql", "170_labels.sql"),
	} {
		if err := verifySchemaFloor(expected, ledger, 164); err != nil {
			t.Errorf("floor 164 refused %v: %v", ledger, err)
		}
	}
	for _, ledger := range []map[string]bool{
		applied("163_servers.sql", "165_notes.sql"),
		applied("164_routine.sql"),
	} {
		if err := verifySchemaFloor(expected, ledger, 164); !errors.Is(err, ErrSchemaFloorUnmet) {
			t.Errorf("floor 164 admitted %v: %v", ledger, err)
		}
	}
	if state := EmbeddedSchemaState(); state.Floor < 1 || state.Floor > state.EmbeddedMax {
		t.Errorf("SchemaFloor %d must name an embedded migration (newest %d)", state.Floor, state.EmbeddedMax)
	}
}

// Sensitive migration boundary (kombify-Techstack-qhrf): the forward batch
// names its locks from the pending bodies, comments excluded and string
// literals included, so a FOREACH list of tables is locked up front.
func TestForwardLockCandidatesNameTablesOutsideComments(t *testing.T) {
	body := []byte(`-- servers is only mentioned here
/* and services
   only here */
CREATE POLICY p ON "Odd Name" FOR SELECT USING (true);
ALTER TABLE household_identity_refs ADD COLUMN x int;
GRANT SELECT ON kombify_ctx.role_class TO someone;
DO $$ BEGIN FOREACH t IN ARRAY ARRAY['workers','stacks'] LOOP NULL; END LOOP; END $$;`)
	candidates := forwardLockCandidates([][]byte{body})
	has := func(name string) bool {
		for _, candidate := range candidates {
			if candidate == name {
				return true
			}
		}
		return false
	}
	for _, name := range []string{`"Odd Name"`, "household_identity_refs", "kombify_ctx.role_class", "workers", "stacks"} {
		if !has(name) {
			t.Fatalf("candidate %s missing from %v", name, candidates)
		}
	}
	for _, name := range []string{"servers", "services"} {
		if has(name) {
			t.Fatalf("commented name %s became a candidate", name)
		}
	}
	if forwardLockName("Servers") != "servers" || forwardLockName(`"Odd ""Name"""`) != `Odd "Name"` {
		t.Fatal("candidates fold like the parser: unquoted to lower case, quoted exactly")
	}
	order := forwardLockOrder([]string{"public.a", "public.b", "public.c"}, []string{"public.c", "public.x", "public.c"})
	if strings.Join(order, ",") != "public.c,public.a,public.b" {
		t.Fatalf("refused tables must lead, the rest stay sorted: %v", order)
	}
	if !retryableForwardLock(fmt.Errorf("w: %w", &pgconn.PgError{Code: "55P03"})) ||
		!retryableForwardLock(&pgconn.PgError{Code: "40P01"}) || !retryableForwardLock(errForwardPreLockLimit) {
		t.Fatal("lock timeout, deadlock and the attempt limit must retry")
	}
	if retryableForwardLock(forwardCommitError{&pgconn.PgError{Code: "40P01"}}) ||
		retryableForwardLock(&pgconn.PgError{Code: "42P01"}) || retryableForwardLock(errors.New("x")) {
		t.Fatal("a commit failure or any other error must never retry")
	}
}

// Sensitive migration boundary (kombify-Techstack-qhrf): the real pending
// tail (165-173) against a synthetic load that holds short locks on the same
// tables. The unchanged single attempt fails as production did (55P03 on an
// application wait cycle, 40P01 as the deadlock victim); the batch with
// up-front locks and bounded retry commits, with its ledger complete.
func TestIntegrationForwardBatchSurvivesConcurrentShortLocks(t *testing.T) {
	dsn := integrationDSN()
	if dsn == "" {
		t.Skip("HOMELAB_TEST_POSTGRES_URL not set; skipping Postgres integration test")
	}
	batch, err := DeliveryForwardBatch()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, refusal string
		load          func(context.Context, *sql.DB) error
	}{
		// A transaction holds servers and waits in the application for another
		// connection that reads jobs: Postgres cannot see this cycle.
		{name: "application wait cycle", refusal: "55P03", load: func(ctx context.Context, db *sql.DB) error {
			holder, err := db.Conn(ctx)
			if err != nil {
				return err
			}
			defer holder.Close()
			reader, err := db.Conn(ctx)
			if err != nil {
				return err
			}
			defer reader.Close()
			tx, err := holder.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(ctx, "LOCK TABLE servers IN ROW EXCLUSIVE MODE"); err != nil {
				return err
			}
			readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			if _, err := reader.ExecContext(readCtx, "SELECT count(*) FROM jobs"); err != nil {
				return err
			}
			return tx.Commit()
		}},
		// The same transaction waits on jobs itself, a cycle Postgres detects;
		// its long deadlock_timeout makes the migration the victim.
		{name: "deadlock", refusal: "40P01", load: func(ctx context.Context, db *sql.DB) error {
			conn, err := db.Conn(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()
			if _, err := conn.ExecContext(ctx, "SET deadlock_timeout = '60s'"); err != nil {
				return err
			}
			tx, err := conn.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(ctx, "LOCK TABLE servers IN ROW EXCLUSIVE MODE"); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "SELECT count(*) FROM jobs"); err != nil {
				return err
			}
			return tx.Commit()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
			defer cancel()
			target, load, name := newForwardBatchDatabase(t, ctx, dsn)
			applyForwardBatchBasis(t, ctx, target, batch)
			var system string
			if err := target.QueryRowContext(ctx, "SELECT system_identifier::text FROM pg_catalog.pg_control_system()").Scan(&system); err != nil {
				t.Fatal(err)
			}
			t.Setenv(EnvDatabaseMigrationMode, "apply")
			t.Setenv(EnvAllowDBGenesis, "false")
			t.Setenv(EnvExpectedDatabaseIdentity, name+"@"+system)
			verify := func(context.Context, *sql.Tx) error { return nil }
			for _, statement := range []string{
				"CREATE SCHEMA forward_private", "CREATE TABLE forward_private.hidden (id int)",
				"CREATE TABLE public.forward_readonly (id int)", "GRANT SELECT ON public.forward_readonly TO PUBLIC",
			} {
				if _, err := load.ExecContext(ctx, statement); err != nil {
					t.Fatal(err)
				}
			}
			if tx, err := target.BeginTx(ctx, &sql.TxOptions{ReadOnly: true}); err == nil {
				tables, err := forwardLockSet(ctx, tx, [][]byte{[]byte("SELECT * FROM forward_private.hidden, forward_readonly, servers")})
				_ = tx.Rollback()
				if err != nil || strings.Join(tables, ",") != "public.servers" {
					t.Fatalf("only tables the owner may lock become locks, without raising: %v %v", tables, err)
				}
			}
			if tx, err := target.BeginTx(ctx, &sql.TxOptions{ReadOnly: true}); err == nil {
				bodies, _ := forwardBatchBodies(batch)
				prefix, err := forwardBatchPrefix(ctx, tx, batch)
				if err != nil {
					t.Fatal(err)
				}
				tables, err := forwardLockSet(ctx, tx, bodies[prefix:])
				_ = tx.Rollback()
				if err != nil || len(tables) == 0 {
					t.Fatalf("the pending tail must name its existing tables: %v %v", tables, err)
				}
				t.Logf("pending tail locks %d tables up front: %s", len(tables), strings.Join(tables, ", "))
			}
			preLock, attempts, retryLog := forwardPreLock, forwardMaxAttempts, forwardRetryLog
			t.Cleanup(func() { forwardPreLock, forwardMaxAttempts, forwardRetryLog = preLock, attempts, retryLog })

			// The unchanged runner: one attempt, no up-front locks, under the
			// case's cycle alone, so its refusal is the production one.
			stopCycle, cycleRounds := startForwardBatchLoad(ctx, load, tc.load, 0)
			defer stopCycle()
			time.Sleep(300 * time.Millisecond)
			forwardPreLock, forwardMaxAttempts = false, 1
			err := target.MigrateForwardBatch(ctx, batch, verify)
			var refusal *pgconn.PgError
			if !errors.As(err, &refusal) || refusal.Code != tc.refusal {
				t.Fatalf("the unchanged single attempt must fail with %s under this load, got %v", tc.refusal, err)
			}
			if state, err := target.InspectForwardBatch(ctx, batch); err != nil || len(state.Pending) != len(batch)-state.AppliedPrefix || len(state.Pending) == 0 {
				t.Fatalf("the refused attempt must leave its whole tail pending: %+v %v", state, err)
			}

			// The runner as shipped, under the same cycle plus four workers that
			// hold short row locks on the policy tables in random order.
			stopNoise, noiseRounds := startForwardBatchLoad(ctx, load, nil, 4)
			defer stopNoise()
			var log strings.Builder
			forwardPreLock, forwardMaxAttempts, forwardRetryLog = true, attempts, &log
			started := time.Now()
			if err := target.MigrateForwardBatch(ctx, batch, verify); err != nil {
				t.Fatalf("the batch must survive the load: %v\n%s", err, log.String())
			}
			t.Logf("committed after %s with %d cycle and %d noise rounds; retries:\n%s",
				time.Since(started).Round(time.Millisecond), cycleRounds.Load(), noiseRounds.Load(), log.String())
			state, err := target.InspectForwardBatch(ctx, batch)
			if err != nil || len(state.Pending) != 0 || state.AppliedPrefix != len(batch) {
				t.Fatalf("the ledger must hold the whole batch: %+v %v", state, err)
			}
			before := cycleRounds.Load() + noiseRounds.Load()
			time.Sleep(300 * time.Millisecond)
			if cycleRounds.Load()+noiseRounds.Load() == before {
				t.Fatal("the application load must keep running after the commit")
			}
		})
	}
}

// newForwardBatchDatabase creates an empty database per case, owned by a
// non-bypass migration owner as production's is (several migrations refuse a
// superuser owner, and name public explicitly, so a schema per test cannot
// hold them). The superuser prepares only what production's prerequisites
// prepare: the two NOLOGIN roles the tail would otherwise create, the grant
// option on pg_control_system and the public schema owner.
func newForwardBatchDatabase(t *testing.T, ctx context.Context, dsn string) (*DB, *sql.DB, string) {
	t.Helper()
	adminConfig, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*adminConfig)
	t.Cleanup(func() { _ = admin.Close() })
	var superuser bool
	if err := admin.QueryRowContext(ctx, "SELECT rolsuper FROM pg_roles WHERE rolname = current_user").Scan(&superuser); err != nil || !superuser {
		t.Skip("the forward batch case prepares a migration owner and needs a superuser test cluster")
	}
	id := strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	name, owner, password := "homelab_forward_"+id, "homelab_forward_owner_"+id, uuid.NewString()
	statements := []string{"CREATE ROLE " + owner + " LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE PASSWORD '" + password + "'"}
	created := []string{}
	for _, role := range []string{"techstack_household_worker", "techstack_platform_projection_system"} {
		var exists bool
		if err := admin.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", role).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			statements = append(statements, "CREATE ROLE "+role+" NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION")
			created = append(created, role)
		}
	}
	statements = append(statements,
		"GRANT EXECUTE ON FUNCTION pg_catalog.pg_control_system() TO "+owner+" WITH GRANT OPTION",
		"CREATE DATABASE "+name+" OWNER "+owner)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = admin.ExecContext(cleanupCtx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_, _ = admin.ExecContext(cleanupCtx, "REVOKE EXECUTE ON FUNCTION pg_catalog.pg_control_system() FROM "+owner+" CASCADE")
		for _, role := range append(created, owner) {
			_, _ = admin.ExecContext(cleanupCtx, "DROP ROLE IF EXISTS "+role)
		}
	})
	for _, statement := range statements {
		if _, err := admin.ExecContext(ctx, statement); err != nil {
			t.Fatalf("prepare the forward batch database: %v", err)
		}
	}
	config := adminConfig.Copy()
	config.Database, config.User, config.Password = name, owner, password
	sqlDB := stdlib.OpenDB(*config)
	sqlDB.SetMaxOpenConns(4)
	// The synthetic application load holds only locks; it connects as the
	// cluster superuser so the deadlock case may set its deadlock_timeout.
	loadConfig := adminConfig.Copy()
	loadConfig.Database = name
	loadDB := stdlib.OpenDB(*loadConfig)
	loadDB.SetMaxOpenConns(16)
	t.Cleanup(func() { _ = loadDB.Close(); _ = sqlDB.Close() })
	// Production's public schema belongs to the migration owner.
	if _, err := loadDB.ExecContext(ctx, "ALTER SCHEMA public OWNER TO "+owner); err != nil {
		t.Fatalf("prepare the forward batch schema owner: %v", err)
	}
	return &DB{DB: sqlDB, backend: StoreBackendPostgres}, loadDB, name
}

// applyForwardBatchBasis applies every embedded migration before the tail
// production still has pending (165), as the normal runner does.
func applyForwardBatchBasis(t *testing.T, ctx context.Context, target *DB, batch []ForwardMigrationArtifact) {
	t.Helper()
	if _, err := target.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	pendingFrom := "165_"
	if !strings.HasPrefix(batch[len(batch)-1].Version, "179_") {
		t.Fatalf("the case is calibrated on the 165-179 tail, the manifest ends at %s", batch[len(batch)-1].Version)
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() >= pendingFrom {
			continue
		}
		body, err := migrationsFS.ReadFile(path.Join("migrations", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		tx, err := target.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := executePendingMigration(ctx, tx, entry.Name(), body); err != nil {
			_ = tx.Rollback()
			t.Fatalf("basis migration %s: %v", entry.Name(), err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version) VALUES($1)", entry.Name()); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
}

// startForwardBatchLoad runs the cycle worker, when given, without pause and
// noise workers that hold short row locks on the policy tables in random order.
func startForwardBatchLoad(ctx context.Context, db *sql.DB, cycle func(context.Context, *sql.DB) error, noise int) (func(), *atomic.Int64) {
	loadCtx, cancel := context.WithCancel(ctx)
	rounds := &atomic.Int64{}
	var wg sync.WaitGroup
	run := func(round func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for loadCtx.Err() == nil {
				if round() == nil {
					rounds.Add(1)
				}
			}
		}()
	}
	if cycle != nil {
		run(func() error { return cycle(loadCtx, db) })
	}
	tables := []string{"servers", "services", "stacks", "workers", "homelabs", "typed_agent_commands"}
	for worker := 0; worker < noise; worker++ {
		run(func() error {
			tx, err := db.BeginTx(loadCtx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			for _, index := range mathrand.Perm(len(tables))[:3] {
				if _, err := tx.ExecContext(loadCtx, "LOCK TABLE "+tables[index]+" IN ROW EXCLUSIVE MODE"); err != nil {
					return err
				}
				time.Sleep(time.Duration(20+mathrand.IntN(180)) * time.Millisecond)
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			time.Sleep(time.Duration(mathrand.IntN(150)) * time.Millisecond)
			return nil
		})
	}
	return func() { cancel(); wg.Wait() }, rounds
}

// Production created provider_resource_free_terminalization_validate_insert
// from a CRLF migration file, so the exact LF patch targets of migration 190
// never matched there and Homelab 0.48.3 stopped at the release preparation
// (2026-11-10). The bounded correction folds the read definition to LF before
// patching and stays bound to the shipped SQL, CRLF checkout or not.
func TestSubstrateAbsenceMigrationFoldsCRLFDefinition(t *testing.T) {
	body, err := migrationsFS.ReadFile("migrations/190_substrate_failed_provision_guest_absence.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range [][]byte{body, []byte(strings.ReplaceAll(string(body), "\n", "\r\n"))} {
		effective, err := substrateAbsenceEffectiveBody(source)
		if err != nil {
			t.Fatalf("correction refused the shipped migration: %v", err)
		}
		fold := "    ) INTO function_definition;\n    function_definition := pg_catalog.replace(function_definition, E'\\r\\n', E'\\n');\n"
		if strings.Count(string(effective), fold) != 1 || strings.Contains(string(effective), "\r") {
			t.Fatal("effective migration does not fold the definition to LF exactly once")
		}
	}
	if _, err := substrateAbsenceEffectiveBody(append([]byte("-- changed\n"), body...)); err == nil {
		t.Fatal("correction admitted a changed migration")
	}
}

const forwardNamesMigration = "199_homelab_forward_names.sql"

// Sensitive migration boundary (platform-2jlnx7.14): migration 199 moves the
// values a 0.49.19 server stored to the spelling 0.50.x reads, in place, with
// no second reader. The rows are seeded on the schema of migration 198 as the
// 0.49.19 server wrote them, then the migration runs as the non-superuser
// migration owner under FORCE ROW LEVEL SECURITY, exactly as the release's
// database leg does.
func TestIntegrationHomelabForwardNames(t *testing.T) {
	dsn := forwardNamesDSN(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	target, admin, _ := newForwardBatchDatabase(t, ctx, dsn)
	prepareForwardNamesRoles(t, ctx, admin)
	pinApplyMigrationsBefore(t, ctx, target, "199_")

	seed := seedForwardNameRows(t, ctx, admin)
	// The 0.50.x boot check refuses the 0.49.19-era ledger and admits it once
	// 199 is applied.
	var pin ExpectedDatabaseIdentity
	if err := admin.QueryRowContext(ctx, `SELECT current_database(), system_identifier::text FROM pg_control_system()`).Scan(
		&pin.DatabaseName, &pin.SystemIdentifier); err != nil {
		t.Fatal(err)
	}
	if err := target.VerifyMigrationsAtFloor(ctx, pin, SchemaFloor); !errors.Is(err, ErrSchemaFloorUnmet) {
		t.Fatalf("schema floor %d before 199 = %v, want ErrSchemaFloorUnmet", SchemaFloor, err)
	}
	pinApplyMigration(t, ctx, target, forwardNamesMigration)
	if err := target.VerifyMigrationsAtFloor(ctx, pin, SchemaFloor); err != nil {
		t.Fatalf("schema floor %d after 199: %v", SchemaFloor, err)
	}

	var keyIssuer, bindingIssuer, procedure, source, journey string
	var stateJSON, resultJSON, payloadJSON []byte
	var stateUpdated time.Time
	if err := admin.QueryRowContext(ctx, `SELECT
			(SELECT issuer_id FROM advanced_issuer_keys WHERE singleton),
			(SELECT issuer_id FROM advanced_trust_bindings WHERE tenant_id = $1),
			(SELECT procedure_ref FROM failure_resolution_outcomes WHERE tenant_id = $1),
			(SELECT source FROM operator_self_disclosures WHERE tenant_id = $1),
			(SELECT journey_id FROM user_onboarding_state WHERE tenant_id = $1),
			(SELECT state_json::text FROM user_onboarding_state WHERE tenant_id = $1),
			(SELECT updated_at FROM user_onboarding_state WHERE tenant_id = $1),
			(SELECT result_json::text FROM jobs WHERE id = $2),
			(SELECT payload_json::text FROM jobs WHERE id = $2)`, seed.tenantID, seed.jobID).Scan(
		&keyIssuer, &bindingIssuer, &procedure, &source, &journey, &stateJSON, &stateUpdated, &resultJSON, &payloadJSON); err != nil {
		t.Fatal(err)
	}
	// The binding keeps the bare issuer 0.49.19 bound; its bundle digest names
	// that issuer, so the host's trust re-import creates it again.
	if want := "homelab." + seed.issuerUUID; keyIssuer != want || bindingIssuer != "techstack" {
		t.Fatalf("issuer labels = %q, %q; want %q and the bound techstack", keyIssuer, bindingIssuer, want)
	}
	if procedure != "homelab:stackkit/repair-volume" || source != "homelab" || journey != "homelab.platform" {
		t.Fatalf("procedure=%q source=%q journey=%q", procedure, source, journey)
	}
	if !stateUpdated.Equal(seed.stateUpdated) {
		t.Fatalf("the rewrite fired the updated_at trigger: %s, seeded %s", stateUpdated, seed.stateUpdated)
	}
	var state struct {
		JourneyID string `json:"journey_id"`
		Completed []struct {
			StepID string `json:"step_id"`
		} `json:"completed"`
		SeenSteps []string `json:"seen_steps"`
	}
	if err := json.Unmarshal(stateJSON, &state); err != nil {
		t.Fatal(err)
	}
	if state.JourneyID != "homelab.platform" || len(state.Completed) != 1 || state.Completed[0].StepID != "homelab.configure_intent" ||
		strings.Join(state.SeenSteps, ",") != "homelab.configure_intent,homelab.add_server" {
		t.Fatalf("onboarding state = %s", stateJSON)
	}
	var result map[string]any
	if err := json.Unmarshal(resultJSON, &result); err != nil {
		t.Fatal(err)
	}
	receipt, _ := result["service_action_receipt"].(map[string]any)
	switch {
	case result["schema_version"] != "homelab.stackkit-lifecycle-result/v1", result["kit_deployment_id"] != "stack-forward",
		result["techstack_id"] != nil, receipt == nil, receipt["schema_version"] != "homelab.service-action-receipt/v1",
		receipt["kit_deployment_id"] != "stack-forward", receipt["techstack_id"] != nil:
		t.Fatalf("moved job result = %s", resultJSON)
	case result["kept"] != "techstack.provider-control-operation/v1",
		result["note"] != `the old id "techstack.wizard-intent/v1" inside a sentence stays`,
		result["techstack_id_note"] != "techstack_id":
		t.Fatalf("a value outside the move changed: %s", resultJSON)
	}
	if !strings.Contains(string(payloadJSON), `"homelab_enrollment"`) || strings.Contains(string(payloadJSON), "techstack_enrollment") {
		t.Fatalf("job payload = %s", payloadJSON)
	}

	// The lease digest: every stored copy of the 0.49.19 digest is now the
	// digest 0.50.x computes for the same lease generation.
	var leaseJSON []byte
	var journalDigest string
	if err := admin.QueryRowContext(ctx, `SELECT
			(SELECT lease_json::text FROM techstack_vm_leases WHERE id = $1),
			(SELECT resource_generation_digest FROM techstack_vm_lease_operation_journal WHERE lease_id = $1)`, seed.leaseID).Scan(
		&leaseJSON, &journalDigest); err != nil {
		t.Fatal(err)
	}
	var lease vmlease.Lease
	if err := json.Unmarshal(leaseJSON, &lease); err != nil {
		t.Fatal(err)
	}
	newDigest, err := vmleases.ResourceGenerationDigest(seed.tenantID, lease)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatal(err)
	}
	if newDigest == seed.oldLeaseDigest || lease.Metadata[vmleases.MetadataKeyDecommissionClaimDigest] != newDigest ||
		journalDigest != newDigest || payload["resource_generation_digest"] != newDigest {
		t.Fatalf("lease digest not moved: claim=%q journal=%q payload=%v want %q (old %q)",
			lease.Metadata[vmleases.MetadataKeyDecommissionClaimDigest], journalDigest, payload["resource_generation_digest"],
			newDigest, seed.oldLeaseDigest)
	}

	var forcedTables int
	var forced, triggerEnabled, journalGuarded bool
	if err := admin.QueryRowContext(ctx, `SELECT
			count(*) FILTER (WHERE c.relname IN ('jobs', 'user_onboarding_state', 'operator_self_disclosures',
				'failure_resolution_outcomes', 'advanced_trust_bindings', 'techstack_vm_leases', 'techstack_vm_lease_operation_journal')),
			coalesce(bool_and(c.relforcerowsecurity) FILTER (WHERE c.relname IN ('jobs', 'user_onboarding_state', 'operator_self_disclosures',
				'failure_resolution_outcomes', 'advanced_trust_bindings', 'techstack_vm_leases', 'techstack_vm_lease_operation_journal')), false),
			(SELECT tgenabled = 'O' FROM pg_trigger WHERE tgname = 'set_user_onboarding_state_updated_at'),
			(SELECT tgenabled = 'O' FROM pg_trigger WHERE tgname = 'techstack_vm_lease_operation_journal_reject_mutation')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public'`).Scan(
		&forcedTables, &forced, &triggerEnabled, &journalGuarded); err != nil {
		t.Fatal(err)
	}
	if forcedTables != 7 || !forced || !triggerEnabled || !journalGuarded {
		t.Fatalf("row level security tables=%d forced=%t, updated_at trigger=%t, journal append-only trigger=%t after the migration",
			forcedTables, forced, triggerEnabled, journalGuarded)
	}

	// The checks admit exactly what 0.50.x writes.
	for _, statement := range []string{
		`INSERT INTO failure_resolution_outcomes (tenant_id, resolution_id, receipt_kind, receipt_ref, signature, procedure_ref, outcome, completed_at)
			VALUES ($1, gen_random_uuid()::text, 'job', 'job-new', repeat('b', 64), 'techstack:stackkit/repair-volume', 'failed', now())`,
		`INSERT INTO operator_self_disclosures (id, tenant_id, owner_subject_id, source, disclosure_json)
			VALUES ('disclosure-old-source', $1, 'subject-old-source', 'techstack', '{}')`,
	} {
		if _, err := admin.ExecContext(ctx, statement, seed.tenantID); err == nil || !strings.Contains(err.Error(), "check") {
			t.Fatalf("an old spelling was admitted after the move: %v", err)
		}
	}
	// Whatever 0.49.19 would still write under an old spelling before 0.50.x
	// serves is refused at write, never stored for 0.50.x to misread.
	for _, refused := range []struct {
		statement string
		args      []any
		want      string
	}{
		{`UPDATE jobs SET result_json = '{"schema_version":"techstack.stackkit-lifecycle-result/v1"}' WHERE id = $1`, []any{seed.jobID}, "check"},
		{`UPDATE jobs SET payload_json = '{"techstack_id":"stack-forward"}' WHERE id = $1`, []any{seed.jobID}, "check"},
		{`UPDATE user_onboarding_state SET journey_id = 'techstack.platform' WHERE tenant_id = $1`, []any{seed.tenantID}, "check"},
		{`UPDATE advanced_issuer_keys SET issuer_id = 'techstack.' || $1 WHERE singleton`, []any{seed.issuerUUID}, "check"},
		{`UPDATE techstack_vm_leases SET lease_json = jsonb_set(lease_json, '{metadata,resource_decommission_claim_digest}', to_jsonb($1::text))
			WHERE id = $2`, []any{seed.oldLeaseDigest, seed.leaseID}, "check"},
		{`INSERT INTO techstack_vm_lease_operation_journal (tenant_id, lease_id, event_type, status, resource_generation_digest)
			VALUES ($1, $2, 'decommission', 'decommissioned', $3)`, []any{seed.tenantID, seed.leaseID, seed.oldLeaseDigest},
			"refuses a 0.49.19 resource generation digest"},
	} {
		if _, err := admin.ExecContext(ctx, refused.statement, refused.args...); err == nil || !strings.Contains(err.Error(), refused.want) {
			t.Fatalf("a 0.49.19 write was admitted after the move (%s): %v", refused.statement, err)
		}
	}
	// 0.50.x keeps writing its own spelling and digest.
	pinExecInTenant(t, ctx, admin, seed.tenantID, `INSERT INTO techstack_vm_lease_operation_journal (tenant_id, lease_id, event_type, status, resource_generation_digest)
		VALUES ($1, $2, 'decommission', 'decommissioned', $3)`, seed.tenantID, seed.leaseID, newDigest)
	pinExecInTenant(t, ctx, admin, seed.tenantID, `UPDATE jobs SET result_json = '{"schema_version":"homelab.stackkit-lifecycle-result/v1"}' WHERE id = $1`, seed.jobID)
	pinExec(t, ctx, admin, `INSERT INTO failure_resolution_outcomes (tenant_id, resolution_id, receipt_kind, receipt_ref, signature, procedure_ref, outcome, completed_at)
		VALUES ($1, gen_random_uuid()::text, 'job', 'job-new', repeat('b', 64), 'homelab:stackkit/repair-volume', 'failed', now())`, seed.tenantID)
	for _, value := range []string{"homelab", "link", "cloud"} {
		pinExec(t, ctx, admin, `INSERT INTO operator_self_disclosures (id, tenant_id, owner_subject_id, source, disclosure_json)
			VALUES ($2, $1, $2, $3, '{}')`, seed.tenantID, "subject-"+value, value)
	}
}

// An object that would carry both the old and the new key refuses the whole
// migration: nothing is moved and the release's database leg fails.
func TestIntegrationHomelabForwardNamesKeepsTheNewKeyOfATwin(t *testing.T) {
	dsn := forwardNamesDSN(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	target, admin, _ := newForwardBatchDatabase(t, ctx, dsn)
	prepareForwardNamesRoles(t, ctx, admin)
	pinApplyMigrationsBefore(t, ctx, target, "199_")

	seed := seedForwardNameRows(t, ctx, admin)
	// Production held objects that carry the new key next to the old one.
	pinExecInTenant(t, ctx, admin, seed.tenantID, `UPDATE jobs SET result_json = result_json || '{"kit_deployment_id":"other","nested":[{"techstack_id":"a","kit_deployment_id":"b"}]}'::jsonb WHERE id = $1`, seed.jobID)
	if err := applyForwardNamesMigration(ctx, target); err != nil {
		t.Fatalf("twin apply = %v", err)
	}
	var top, nested string
	var old bool
	if err := admin.QueryRowContext(ctx, `SELECT result_json->>'kit_deployment_id', result_json->'nested'->0->>'kit_deployment_id',
			result_json::text ~ '"techstack_id"[[:space:]]*:' FROM jobs WHERE id = $1`, seed.jobID).Scan(&top, &nested, &old); err != nil {
		t.Fatal(err)
	}
	if top != "other" || nested != "b" || old {
		t.Fatalf("twin kept top=%q nested=%q old key left=%t", top, nested, old)
	}
	var helper bool
	if err := admin.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'homelab_forward_drop_twin_key')`).Scan(&helper); err != nil {
		t.Fatal(err)
	}
	if helper {
		t.Fatal("the twin helper function outlived the migration")
	}
}

// forwardNamesDSN prefers the throwaway superuser server of CI and falls back
// to a private embedded server under the embeddedpg build tag.
func forwardNamesDSN(t *testing.T) string {
	t.Helper()
	dsn := integrationDSN()
	if dsn == "" {
		dsn = forwardNamesEmbeddedDSN(t)
	}
	if dsn == "" {
		t.Skip("HOMELAB_TEST_POSTGRES_URL not set and no embeddedpg tag; skipping Postgres integration test")
	}
	return dsn
}

// prepareForwardNamesRoles creates the NOLOGIN roles production prepares
// before the migration chain (scripts/schema-old-binary-check.sh) when the
// test cluster lacks them, and removes only what it created.
func prepareForwardNamesRoles(t *testing.T, ctx context.Context, admin *sql.DB) {
	t.Helper()
	for _, role := range []string{"techstack_application", "techstack_backup_worker"} {
		var exists bool
		if err := admin.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", role).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			continue
		}
		pinExec(t, ctx, admin, "CREATE ROLE "+role+" NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION")
		t.Cleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, _ = admin.ExecContext(cleanupCtx, "DROP OWNED BY "+role)
			_, _ = admin.ExecContext(cleanupCtx, "DROP ROLE IF EXISTS "+role)
		})
	}
}

type forwardNameSeed struct {
	tenantID       string
	jobID          string
	issuerUUID     string
	leaseID        string
	oldLeaseDigest string
	stateUpdated   time.Time
}

// forwardNamesOldLeaseDigest is the 0.49.19 ResourceGenerationDigest: the
// same fixed struct and steps as pkg/vmleases/resource_generation.go under the
// old domain marker techstack.vmlease.resource-generation/v1.
func forwardNamesOldLeaseDigest(t *testing.T, lease vmlease.Lease) string {
	t.Helper()
	payload, err := json.Marshal(struct {
		Version              string `json:"version"`
		TenantID             string `json:"tenant_id"`
		LeaseID              string `json:"lease_id"`
		ResourceGenerationID string `json:"resource_generation_id"`
		ProviderID           string `json:"provider_id"`
		EngineVMID           string `json:"engine_vm_id"`
		SimulationID         string `json:"simulation_id"`
		VMID                 string `json:"vm_id"`
	}{
		Version:              "techstack.vmlease.resource-generation/v1",
		TenantID:             strings.TrimSpace(lease.Subject.OrgID),
		LeaseID:              strings.TrimSpace(string(lease.ID)),
		ResourceGenerationID: strings.TrimSpace(lease.Metadata[vmleases.MetadataKeyResourceGenerationID]),
		ProviderID:           strings.ToLower(strings.TrimSpace(lease.Resource.ProviderID)),
		EngineVMID:           strings.TrimSpace(lease.Resource.EngineVMID),
		SimulationID:         strings.TrimSpace(lease.Resource.SimulationID),
		VMID:                 strings.TrimSpace(lease.Resource.VMID),
	})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

// seedForwardNameRows writes the rows as the 0.49.19 server stored them. The
// superuser connection bypasses row level security only for the seed.
func seedForwardNameRows(t *testing.T, ctx context.Context, admin *sql.DB) forwardNameSeed {
	t.Helper()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	seed := forwardNameSeed{tenantID: "forward-names-" + suffix, jobID: "job-forward-" + suffix, issuerUUID: uuid.NewString(),
		leaseID: "lease-forward-" + suffix, stateUpdated: time.Date(2026, 11, 20, 9, 26, 0, 0, time.UTC)}
	keyID := "ed25519://sha256/" + strings.Repeat("c", 64)
	pinExec(t, ctx, admin, `INSERT INTO techstack_tenants (id, display_name, kind, status) VALUES ($1, $1, 'saas', 'active')`, seed.tenantID)
	pinExec(t, ctx, admin, `INSERT INTO advanced_issuer_keys (singleton, issuer_id, key_id, public_key, private_key_enc)
		VALUES (true, $1, $2, 'public-key', 'enc:v1:sealed')`, "techstack."+seed.issuerUUID, keyID)
	pinExecInTenant(t, ctx, admin, seed.tenantID, `INSERT INTO advanced_trust_bindings (tenant_id, stack_id, owner_ref, bundle_sha256, issuer_id, key_id, imported_at)
		VALUES ($1, 'stack-forward', $2, $3, $4, $5, now())`,
		seed.tenantID, "owner/local/"+strings.Repeat("d", 32), "sha256:"+strings.Repeat("e", 64), "techstack", keyID)
	pinExecInTenant(t, ctx, admin, seed.tenantID, `INSERT INTO failure_resolution_outcomes (tenant_id, resolution_id, receipt_kind, receipt_ref, signature, procedure_ref, outcome, completed_at)
		VALUES ($1, gen_random_uuid()::text, 'job', 'job-old', repeat('a', 64), 'techstack:stackkit/repair-volume', 'succeeded', now())`, seed.tenantID)
	pinExecInTenant(t, ctx, admin, seed.tenantID, `INSERT INTO operator_self_disclosures (id, tenant_id, owner_subject_id, source, disclosure_json)
		VALUES ($2, $1, 'subject-typed', 'techstack', '{"experience":"technical"}')`, seed.tenantID, "disclosure-"+suffix)
	pinExecInTenant(t, ctx, admin, seed.tenantID, `INSERT INTO user_onboarding_state (id, tenant_id, owner_subject_id, product, journey_id, journey_version, state_json, status, updated_at)
		VALUES ($2, $1, 'subject-typed', 'techstack', 'techstack.platform', 3, $3::jsonb, 'active', $4)`, seed.tenantID, "onboarding-"+suffix,
		`{"schema_version":1,"journey_id":"techstack.platform","journey_version":3,"completed":[{"step_id":"techstack.configure_intent","at":"2026-11-16T09:25:00Z","source":"derived"}],"seen_steps":["techstack.configure_intent","techstack.add_server"],"status":"active","dismissed_at":null,"reopened_at":null,"revision":2,"updated_at":"2026-11-20T09:26:00Z"}`,
		seed.stateUpdated)
	// A lease whose decommission 0.49.19 claimed. The fields exercise the
	// encoding the SQL must share with Go: surrounding Unicode space, HTML
	// escaping, a line separator, a quote and a backslash.
	generation := uuid.NewString()
	lease := vmlease.Lease{
		ID:           vmlease.LeaseID(seed.leaseID),
		Subject:      vmlease.Subject{Kind: vmlease.SubjectUser, ID: "subject-typed", OrgID: seed.tenantID},
		Resource:     vmlease.ResourceRef{ProviderID: "centron", EngineVMID: "engine-7", SimulationID: `sim "quoted" \\ <&>`, VMID: "\u00a0 vm\u2028x\t"},
		DesiredState: vmlease.DesiredStateRunning,
		Metadata:     map[string]string{vmleases.MetadataKeyResourceGenerationID: generation},
	}
	seed.oldLeaseDigest = forwardNamesOldLeaseDigest(t, lease)
	lease.Metadata[vmleases.MetadataKeyDecommissionClaimDigest] = seed.oldLeaseDigest
	leaseJSON, err := json.Marshal(lease)
	if err != nil {
		t.Fatal(err)
	}
	pinExecInTenant(t, ctx, admin, seed.tenantID, `INSERT INTO techstack_vm_leases
		(id, tenant_id, subject_id, org_id, provider_id, engine_vm_id, desired_state, resource_generation_id, lease_json)
		VALUES ($1, $2, 'subject-typed', $2, 'centron', 'engine-7', 'running', $3::uuid, $4::jsonb)`,
		seed.leaseID, seed.tenantID, generation, string(leaseJSON))
	pinExecInTenant(t, ctx, admin, seed.tenantID, `INSERT INTO techstack_vm_lease_operation_journal
		(tenant_id, lease_id, event_type, status, resource_generation_digest)
		VALUES ($1, $2, 'decommission', 'decommissioned', $3)`, seed.tenantID, seed.leaseID, seed.oldLeaseDigest)
	pinExecInTenant(t, ctx, admin, seed.tenantID, `INSERT INTO jobs (id, tenant_id, type, state, payload_json, result_json)
		VALUES ($2, $1, 'deploy', 'completed', $3::jsonb, $4::jsonb)`, seed.tenantID, seed.jobID,
		`{"techstack_enrollment":{"worker_id":"worker-forward"},"resource_generation_digest":"`+seed.oldLeaseDigest+`"}`,
		`{"schema_version":"techstack.stackkit-lifecycle-result/v1","techstack_id":"stack-forward","service_action_receipt":{"schema_version":"techstack.service-action-receipt/v1","techstack_id":"stack-forward"},"kept":"techstack.provider-control-operation/v1","note":"the old id \"techstack.wizard-intent/v1\" inside a sentence stays","techstack_id_note":"techstack_id"}`)
	return seed
}

func applyForwardNamesMigration(ctx context.Context, target *DB) error {
	body, err := migrationsFS.ReadFile(path.Join("migrations", forwardNamesMigration))
	if err != nil {
		return err
	}
	tx, err := target.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := executePendingMigration(ctx, tx, forwardNamesMigration, body); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version) VALUES($1)", forwardNamesMigration); err != nil {
		return err
	}
	return tx.Commit()
}
