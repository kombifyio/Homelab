package db

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/kombifyio/homelab/internal/gocommon/identity"
	"github.com/kombifyio/homelab/internal/gocommon/tenantdb"
	"github.com/kombifyio/homelab/pkg/db/tenantcontext"
)

func TestWithTenantEmptyTenant(t *testing.T) {
	d := &DB{}
	if err := d.WithTenant(context.Background(), "", nil); !errors.Is(err, ErrNoTenant) {
		t.Fatalf("empty: got %v, want ErrNoTenant", err)
	}
	if err := d.WithTenant(context.Background(), "   ", nil); !errors.Is(err, ErrNoTenant) {
		t.Fatalf("whitespace: got %v, want ErrNoTenant", err)
	}
}

func TestWithTenantNilHandle(t *testing.T) {
	var d *DB
	if err := d.WithTenant(context.Background(), "tenant-1", nil); err == nil {
		t.Fatal("expected error for nil DB handle")
	}
}

// Runtime-selected identifiers cannot be supplied through the public user
// journey. These narrow auth-boundary cases exercise that internal failure;
// managed Stage journeys separately prove actual policies and persistence.
func TestTenantContextRejectsRuntimeRebinding(t *testing.T) {
	for _, scenario := range []struct {
		name, requestedTenant, requestSubject string
	}{
		{"foreign tenant", "tenant-b", "user-a"},
		{"foreign user", "tenant-a", "user-b"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			pool, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			ctx := identity.NewContext(context.Background(), &identity.Identity{UserID: scenario.requestSubject, OrgID: "tenant-a"})
			ctx = tenantdb.NewContextWithToken(ctx, "opaque-gateway-context-fixture")
			mock.ExpectBegin()
			mock.ExpectExec(regexp.QuoteMeta("SELECT set_config('kombify.ctx_token', $1, true)")).
				WithArgs("opaque-gateway-context-fixture").WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectQuery(regexp.QuoteMeta("SELECT subject, org_id FROM kombify_ctx.verified_context()")).
				WillReturnRows(sqlmock.NewRows([]string{"subject", "org_id"}).AddRow("user-a", "tenant-a"))
			mock.ExpectRollback()
			_, err = BeginTenantTransaction(ctx, pool, scenario.requestedTenant)
			if !errors.Is(err, tenantdb.ErrContextMismatch) {
				t.Fatalf("runtime rebinding was not refused: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserContextRejectsRuntimeChosenSubject(t *testing.T) {
	pool, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx := tenantdb.NewContextWithToken(context.Background(), "opaque-gateway-context-fixture")
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT set_config('kombify.ctx_token', $1, true)")).
		WithArgs("opaque-gateway-context-fixture").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT subject, org_id FROM kombify_ctx.verified_context()")).
		WillReturnRows(sqlmock.NewRows([]string{"subject", "org_id"}).AddRow("user-a", "tenant-a"))
	mock.ExpectRollback()
	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = tenantcontext.ApplyUser(ctx, tx, "user-b")
	_ = tx.Rollback()
	if !errors.Is(err, tenantdb.ErrContextMismatch) {
		t.Fatalf("foreign self-account lookup was not refused: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// System work begins on the system login, masks a request token and never
// redirects a pool other than the application pool (platform-3r69a).
func TestSystemContextRoutesOnlyTheApplicationPool(t *testing.T) {
	application, applicationMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	system, systemMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer system.Close()
	provider, providerMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	request := tenantdb.NewContextWithToken(context.Background(), "opaque-gateway-context-fixture")
	ctx := tenantcontext.WithSystemDatabase(request, application, system)
	if !tenantcontext.IsSystem(ctx) || tenantdb.ContextTokenFromContext(ctx) != "" {
		t.Fatal("system work kept the request token or lost its route")
	}
	scope := regexp.QuoteMeta("SELECT set_config($1, $2, true), set_config('kombify.ctx_token', '', true)")
	for _, mock := range []sqlmock.Sqlmock{systemMock, providerMock} {
		mock.ExpectBegin()
		mock.ExpectExec(scope).WithArgs("app.tenant_id", "tenant-a").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectRollback()
	}
	for _, pool := range []*sql.DB{application, provider} {
		tx, err := BeginTenantTransaction(ctx, pool, "tenant-a")
		if err != nil {
			t.Fatal(err)
		}
		_ = tx.Rollback()
	}
	for _, mock := range []sqlmock.Sqlmock{applicationMock, systemMock, providerMock} {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	}
	if tenantcontext.Database(tenantcontext.WithSystemDatabase(context.Background(), nil, nil), application) != application {
		t.Fatal("a runtime without a system login must keep its single serving login")
	}
}
