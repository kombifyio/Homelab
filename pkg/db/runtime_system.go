package db

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/kombifyio/techstack/internal/gocommon/tenantdb"
	"github.com/jackc/pgx/v5"
)

// RuntimeSystemRole is the hosted login for Techstack system work that carries
// no Edge-signed database context: background loops, Guard ingress, Cloud
// service-to-service routes and the Guard gRPC listener. It holds the same
// finite object capability as HostedApplicationRole, is registered as a system
// role in kombify_ctx.role_class and is never a context-token audience. The
// hosted application login therefore serves only verified user requests on
// the inventory tables; a runtime-chosen app.tenant_id alone grants it nothing
// there (contract migration 191).
const RuntimeSystemRole = "techstack_runtime_system"

// EnvRuntimeSystemDatabaseURL is the system login's DSN in every hosted
// runtime. Native Local and Guard serve through their single product-native
// login and never receive it.
const EnvRuntimeSystemDatabaseURL = "TECHSTACK_RUNTIME_SYSTEM_DATABASE_URL"

var errRuntimeSystemCustody = errors.New("runtime system login requires its distinct verified-transport database custody")

// RuntimeSystemConfig derives the system pool from the hosted application
// configuration. It refuses any DSN that is not the exact system login over
// verified TLS, or that would switch the session's role.
func RuntimeSystemConfig(application Config) (Config, error) {
	dsn := strings.TrimSpace(os.Getenv(EnvRuntimeSystemDatabaseURL))
	parsed, err := pgx.ParseConfig(dsn)
	if dsn == "" || err != nil || parsed.User != RuntimeSystemRole || parsed.Password == "" ||
		parsed.TLSConfig == nil || parsed.TLSConfig.InsecureSkipVerify || len(parsed.Fallbacks) != 0 {
		return Config{}, errRuntimeSystemCustody
	}
	for key := range parsed.RuntimeParams {
		if key == "role" || key == "session_authorization" || key == "options" {
			return Config{}, errors.New("runtime system login refuses session authority overrides")
		}
	}
	if application.Backend != StoreBackendPostgres || strings.TrimSpace(application.DSN) == dsn {
		return Config{}, errRuntimeSystemCustody
	}
	system := application
	system.DSN = dsn
	return system, nil
}

// VerifyRuntimeSystemLogin is the boot assertion on the opened system pool:
// the hosted role posture, the pinned physical database, the exact
// application capability and the system class registration.
func VerifyRuntimeSystemLogin(ctx context.Context, system *DB) error {
	if system == nil || system.DB == nil {
		return errRuntimeSystemCustody
	}
	if err := verifyRuntimeRolePosture(ctx, system.DB, RuntimeRoleHosted, RuntimeSystemRole); err != nil {
		return err
	}
	if err := VerifyExpectedDatabaseIdentity(ctx, system.DB); err != nil {
		return err
	}
	owner, err := ApplicationAuthorityOwner(ctx, system.DB)
	if err != nil {
		return err
	}
	if err := VerifyApplicationPrivileges(ctx, system.DB, ApplicationPrincipal{Role: RuntimeSystemRole, AuthorityOwner: owner}, true); err != nil {
		return err
	}
	if err := tenantdb.AssertSystemRole(ctx, system.DB, RuntimeSystemRole); err != nil {
		return errors.New("runtime system login is not registered as a system role")
	}
	return nil
}
