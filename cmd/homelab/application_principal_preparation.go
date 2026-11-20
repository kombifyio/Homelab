package main

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/kombifyio/homelab/pkg/db"
	"net/url"
)

// The existing ordered producer owns this one-shot stage. No owner credential
// reaches the authenticated app pool or ordinary runtime.
func prepareApplicationPrincipal(ctx context.Context, authority *db.DB, cfg advancedIssuerBootstrapConfig) error {
	u, parseErr := url.Parse(cfg.application.DSN)
	if parseErr != nil || len(u.Query()["sslmode"]) != 1 || u.Query().Get("sslmode") != "verify-full" {
		return errors.New("application preparation requires verified transport")
	}
	app, err := pgx.ParseConfig(cfg.application.DSN)
	if err != nil || app.User == "" || app.User != cfg.appRole || app.Password == "" || app.User == providerControlRuntimeRoleName {
		return errors.New("application preparation requires its explicit distinct purpose login")
	}
	migration, err := pgx.ParseConfig(cfg.migration.DSN)
	if err != nil || app.User == migration.User {
		return errors.New("application preparation refuses migration/legacy owner alias")
	}
	owner, err := db.ApplicationAuthorityOwner(ctx, authority.DB)
	if err != nil || owner != migration.User {
		return errors.New("application preparation requires the declared existing schema authority")
	}
	principal := db.ApplicationPrincipal{Role: app.User, AuthorityOwner: owner}
	if err = ensureProviderControlRuntimeLogin(ctx, authority.DB, app.User, app.Password); err != nil {
		return err
	}
	// The hosted system login (platform-3r69a) is ensured beside the application
	// login from its own DSN, so a release never waits for a manual role.
	var systemConfig db.Config
	hostedSystem := cfg.appRole == db.HostedApplicationRole
	if hostedSystem {
		if systemConfig, err = db.RuntimeSystemConfig(cfg.application); err != nil {
			return err
		}
		system, parseErr := pgx.ParseConfig(systemConfig.DSN)
		if parseErr != nil || system.User == migration.User || system.User == app.User {
			return errors.New("application preparation refuses a system login alias")
		}
		if err = ensureProviderControlRuntimeLogin(ctx, authority.DB, db.RuntimeSystemRole, system.Password); err != nil {
			return err
		}
	}
	tx, err := authority.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SET LOCAL lock_timeout='5s'"); err != nil {
		return err
	}
	if err = db.InstallApplicationCapabilities(ctx, tx, principal); err != nil {
		return err
	}
	// Login creation and password custody stay with the existing governed
	// infrastructure bootstrap. Product preparation grants only its finite
	// capabilities, without receiving the worker credential.
	if cfg.appRole == db.HostedApplicationRole {
		if err = db.InstallApplicationCapabilities(ctx, tx, db.ApplicationPrincipal{Role: db.BackupWorkerRole, AuthorityOwner: owner}); err != nil {
			return err
		}
		// The hosted system login holds the same finite application capability.
		if err = db.InstallApplicationCapabilities(ctx, tx, db.ApplicationPrincipal{Role: db.RuntimeSystemRole, AuthorityOwner: owner}); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	application, err := db.OpenContext(ctx, cfg.application)
	if err != nil {
		return errors.New("prepared application login authentication failed")
	}
	defer application.Close()
	if err = db.VerifyExpectedDatabaseIdentity(ctx, application.DB); err != nil {
		return err
	}
	var login, session string
	if err = application.QueryRowContext(ctx, "SELECT current_user,session_user").Scan(&login, &session); err != nil || login != app.User || session != login {
		return errors.New("prepared application session differs from the declared login")
	}
	if err = db.VerifyApplicationPrivileges(ctx, application.DB, principal, true); err != nil {
		return err
	}
	if !hostedSystem {
		return nil
	}
	system, err := db.OpenContext(ctx, systemConfig)
	if err != nil {
		return errors.New("prepared runtime system login authentication failed")
	}
	defer system.Close()
	return db.VerifyRuntimeSystemLogin(ctx, system)
}
