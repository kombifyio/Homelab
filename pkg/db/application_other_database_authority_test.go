package db

import (
	"context"
	"database/sql/driver"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5"
)

// Managed Postgres keeps PUBLIC CONNECT/TEMPORARY on provider databases a
// tenant role cannot revoke; only the role's own authority there is excess.
func TestApplicationOtherDatabaseAuthorityIgnoresOnlyPublicInheritance(t *testing.T) {
	providerPublic := []otherDatabaseGrant{{PublicGrantee: true}, {PublicGrantee: true}}
	if otherDatabaseAuthorityExcess(providerPublic) {
		t.Fatal("PUBLIC-inherited CONNECT/TEMPORARY on a provider database was rejected")
	}
	for name, grant := range map[string]otherDatabaseGrant{
		"explicit grant":   {RoleGrantee: true},
		"membership grant": {RoleGrantee: true},
		"owned database":   {OwnedByRole: true, PublicGrantee: true},
	} {
		if !otherDatabaseAuthorityExcess(append(providerPublic, grant)) {
			t.Fatalf("%s on another database was admitted", name)
		}
	}
}

// Routing through the canonical operator tunnel must not change database/TLS
// authority. This parses connection configuration only; it opens no database.
func TestOperatorEgressPreservesDatabaseTLSAuthority(t *testing.T) {
	const dsn = "postgres://app:fixture@stage.example/kombify_runtime?sslmode=verify-full"
	const mapping = `{"stage.example":"127.0.0.5"}`
	original, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := operatorEgressConfig(dsn, mapping)
	if err != nil || mapped == nil {
		t.Fatalf("mapped config: %v", err)
	}
	addresses, err := mapped.LookupFunc(context.Background(), "stage.example")
	if err != nil || len(addresses) != 1 || addresses[0] != "127.0.0.5" {
		t.Fatalf("canonical local route: %v %v", addresses, err)
	}
	if mapped.Host != original.Host || mapped.Port != original.Port || mapped.Database != original.Database ||
		mapped.User != original.User || mapped.Password != original.Password ||
		mapped.TLSConfig.ServerName != original.TLSConfig.ServerName || mapped.TLSConfig.InsecureSkipVerify ||
		mapped.TLSConfig.RootCAs != original.TLSConfig.RootCAs || len(mapped.Fallbacks) != len(original.Fallbacks) {
		t.Fatal("operator routing changed parsed database or TLS authority")
	}
	for _, raw := range []string{"", `{}`, `{"other.example":"127.0.0.2"}`} {
		if config, err := operatorEgressConfig(dsn, raw); err != nil || config != nil {
			t.Fatalf("unmapped connection changed: %v", err)
		}
	}
	for _, raw := range []string{
		`null`, `[]`, `{`, `{"stage.example":"127.0.0.5"} {}`,
		`{"stage.example":"127.0.0.5","stage.example":"127.0.0.2"}`,
		`{"stage.example":"203.0.113.1"}`, `{"stage.example":"0.0.0.0"}`,
		`{"stage.example":"127.0.0.1"}`, `{"stage.example":"127.0.0.5:5432"}`,
		`{"stage.example":null}`, `{"stage.example":5}`, `{"https://stage.example":"127.0.0.5"}`,
	} {
		if _, err := operatorEgressConfig(dsn, raw); err == nil {
			t.Fatalf("unsafe/ambiguous routing admitted: %s", raw)
		}
	}
	for _, sslmode := range []string{"disable", "allow", "prefer", "require", "verify-ca"} {
		if _, err := operatorEgressConfig("postgres://app:fixture@stage.example/kombify_runtime?sslmode="+sslmode, mapping); err == nil {
			t.Fatalf("unverified TLS mode admitted: %s", sslmode)
		}
	}
}

// The native Windows login keeps its attempts in a table without tenant and row
// security. The application role holds exactly the capabilities this policy
// names, so a table missing here is unusable to the runtime: it answered
// POST /api/v2/auth/native/start with 503 while the table existed.
func TestApplicationPolicyGrantsTheNativeLoginAttempts(t *testing.T) {
	// The sequence lookup binds slices, which only a pgx connection converts.
	sqlDB, mock, err := sqlmock.New(sqlmock.ValueConverterOption(passthroughConverter{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	mock.ExpectQuery("FROM pg_class c JOIN pg_namespace n").WillReturnRows(
		sqlmock.NewRows([]string{"oid", "relname", "rls", "tenant"}).
			AddRow(int64(41), "native_login_handoffs", false, false).
			AddRow(int64(42), "an_unlisted_table_without_tenant", false, false))
	mock.ExpectQuery("unnest").WillReturnRows(sqlmock.NewRows([]string{"oid", "relname"}))
	for range applicationCapabilityFunctions {
		mock.ExpectQuery("to_regprocedure").WillReturnRows(sqlmock.NewRows([]string{"oid"}).AddRow(nil))
	}
	mock.ExpectQuery("to_regnamespace").WillReturnRows(sqlmock.NewRows([]string{"oid"}).AddRow(nil))

	policy, err := loadApplicationPolicy(context.Background(), sqlDB, false)
	if err != nil {
		t.Fatal(err)
	}
	granted := policy.Tables[41]
	for _, privilege := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
		if !granted[privilege] {
			t.Fatalf("native_login_handoffs lacks %s, got %v", privilege, granted)
		}
	}
	if len(granted) != 4 {
		t.Fatalf("native_login_handoffs privileges = %v, want exactly SELECT, INSERT, UPDATE, DELETE", granted)
	}
	if _, ok := policy.Tables[42]; ok {
		t.Fatal("a table the policy does not name must stay without privileges")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

type passthroughConverter struct{}

func (passthroughConverter) ConvertValue(v any) (driver.Value, error) { return v, nil }
