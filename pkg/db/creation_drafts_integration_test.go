package db

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kombifyio/homelab/pkg/controlplane"
)

// Sensitive migration boundary (kombify-StackKits-6u4m): migration 188 adds
// the server-side creation draft, one per tenant, owner and process key, so a
// creation started in the Cloud popup continues on the Homelab page or on
// another device. The draft goes through the real store exactly as the
// /api/v1/creation/drafts routes use it.
func TestIntegrationCreationDraftsMigration(t *testing.T) {
	dsn := integrationDSN()
	if dsn == "" {
		t.Skip("HOMELAB_TEST_POSTGRES_URL not set; skipping Postgres integration test")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	target, _, _ := newForwardBatchDatabase(t, ctx, dsn)
	pinApplyMigrationsBefore(t, ctx, target, "188_")

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	tenant, otherTenant := "draft-tenant-"+suffix, "draft-other-"+suffix
	for _, id := range []string{tenant, otherTenant} {
		pinExecInTenant(t, ctx, target.DB, id, `INSERT INTO techstack_tenants (id, display_name) VALUES ($1, $1)`, id)
	}
	store := controlplane.NewPostgresStore(target.DB)
	draft := func(tenantID, owner, key, step string, answers string) controlplane.CreationDraft {
		return controlplane.CreationDraft{
			ID: uuid.NewString(), TenantID: tenantID, OwnerSubjectID: owner, Key: key,
			Process: "add-use-case", Step: step, Furthest: "goals", Answers: json.RawMessage(answers),
		}
	}
	key := "add-use-case-stack-" + suffix

	// Through migration 187 there is nowhere to keep a draft.
	if _, err := store.PutCreationDraft(ctx, draft(tenant, "owner-a", key, "goals", `{}`)); err == nil ||
		!strings.Contains(err.Error(), "creation_drafts") {
		t.Fatalf("pre-188 draft write error = %v", err)
	}

	pinApplyMigration(t, ctx, target, "188_creation_drafts.additive.sql")

	// The hosted application login reaches the table through the canonical
	// capability selection, under forced row security with the verified-identity
	// policy of the personal settings.
	policy, err := LoadApplicationPolicy(ctx, target.DB, false)
	if err != nil {
		t.Fatal(err)
	}
	granted := map[string]bool{}
	for oid, name := range policy.TableNames {
		if name == "creation_drafts" {
			granted = policy.Tables[oid]
		}
	}
	for _, privilege := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
		if !granted[privilege] {
			t.Fatalf("application capability on creation_drafts = %v", granted)
		}
	}
	var forced bool
	var restrictive int
	if err := target.QueryRowContext(ctx, `SELECT c.relrowsecurity AND c.relforcerowsecurity,
		(SELECT count(*) FROM pg_policies p WHERE p.tablename = 'creation_drafts'
		   AND p.policyname = 'creation_drafts_verified_identity' AND p.permissive = 'RESTRICTIVE'
		   AND p.qual LIKE '%kombify_ctx.subject()%' AND p.with_check LIKE '%kombify_ctx.org_id()%')
		FROM pg_class c WHERE c.oid = 'public.creation_drafts'::regclass`).Scan(&forced, &restrictive); err != nil {
		t.Fatal(err)
	}
	if !forced || restrictive != 1 {
		t.Fatalf("creation_drafts row security forced=%v verified-identity policies=%d", forced, restrictive)
	}

	// A write keeps the step and the answers; the next one replaces the draft.
	saved, err := store.PutCreationDraft(ctx, draft(tenant, "owner-a", key, "goals", `{"goals":{"photos":true}}`))
	if err != nil {
		t.Fatalf("write draft: %v", err)
	}
	if saved.Step != "goals" || !saved.ExpiresAt.After(time.Now().Add(controlplane.CreationDraftTTL-time.Hour)) {
		t.Fatalf("saved draft = %+v", saved)
	}
	replaced, err := store.PutCreationDraft(ctx, draft(tenant, "owner-a", key, "server", `{"goals":{"photos":false}}`))
	if err != nil {
		t.Fatalf("replace draft: %v", err)
	}
	if replaced.ID != saved.ID || replaced.Step != "server" || replaced.Furthest != "server" {
		t.Fatalf("replaced draft = %+v (first %s)", replaced, saved.ID)
	}
	read, err := store.GetCreationDraft(ctx, tenant, "owner-a", key)
	if err != nil {
		t.Fatalf("read draft: %v", err)
	}
	var answers map[string]map[string]bool
	if err := json.Unmarshal(read.Answers, &answers); err != nil || answers["goals"]["photos"] {
		t.Fatalf("read answers = %s, %v", read.Answers, err)
	}
	// A frame with fewer steps writes its own step; how far the owner got stays.
	narrowed, err := store.PutCreationDraft(ctx, draft(tenant, "owner-a", key, "goals", `{"goals":{"photos":false}}`))
	if err != nil || narrowed.Step != "goals" || narrowed.Furthest != "server" {
		t.Fatalf("narrowed draft = %+v, %v", narrowed, err)
	}

	// Another owner of the tenant and every other tenant see nothing.
	if _, err := store.GetCreationDraft(ctx, tenant, "owner-b", key); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("other owner read error = %v", err)
	}
	tx := pinBeginInTenant(t, ctx, target.DB, otherTenant)
	var visible int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM creation_drafts`).Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("drafts visible to another tenant = %d, %v", visible, err)
	}
	_ = tx.Rollback()

	// A draft never holds a secret; a blanked field is no secret.
	if _, err := store.PutCreationDraft(ctx, draft(tenant, "owner-a", key, "server",
		`{"serverProvisioning":{"remote":{"sshPassword":"hunter2"}}}`)); !errors.Is(err, controlplane.ErrCreationDraftSecret) ||
		strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("secret draft error = %v", err)
	}
	if _, err := store.PutCreationDraft(ctx, draft(tenant, "owner-a", key, "server",
		`{"serverProvisioning":{"remote":{"sshPassword":""}},"auth":{"requirePassword":true}}`)); err != nil {
		t.Fatalf("blanked draft: %v", err)
	}

	// An expired draft is gone for every reader; the owner's next write removes it.
	pinExecInTenant(t, ctx, target.DB, tenant, `UPDATE creation_drafts
		SET created_at = now() - interval '9 days', expires_at = now() - interval '1 day'
		WHERE tenant_id = $1 AND draft_key = $2`, tenant, key)
	if _, err := store.GetCreationDraft(ctx, tenant, "owner-a", key); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("expired draft read error = %v", err)
	}
	if _, err := store.PutCreationDraft(ctx, draft(tenant, "owner-a", "stack-create", "goals", `{}`)); err != nil {
		t.Fatalf("write second draft: %v", err)
	}
	tx = pinBeginInTenant(t, ctx, target.DB, tenant)
	var keys []string
	rows, err := tx.QueryContext(ctx, `SELECT draft_key FROM creation_drafts WHERE owner_subject_id = 'owner-a'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, name)
	}
	rows.Close()
	_ = tx.Rollback()
	if len(keys) != 1 || keys[0] != "stack-create" {
		t.Fatalf("owner drafts after expiry = %v", keys)
	}

	// An explicit discard ends the draft.
	if err := store.DeleteCreationDraft(ctx, tenant, "owner-a", "stack-create"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetCreationDraft(ctx, tenant, "owner-a", "stack-create"); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("discarded draft read error = %v", err)
	}

	// The process vocabulary is closed in the schema too.
	tx = pinBeginInTenant(t, ctx, target.DB, tenant)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO creation_drafts
		(id, tenant_id, owner_subject_id, draft_key, process, step, furthest, answers_json, expires_at)
		VALUES ('draft-unknown', $1, 'owner-a', 'stack-create', 'unknown', 'goals', 'goals', '{}', now() + interval '1 day')`, tenant)
	var refusal *pgconn.PgError
	if !errors.As(err, &refusal) || refusal.Code != "23514" {
		t.Fatalf("unknown process insert error = %v", err)
	}
}
