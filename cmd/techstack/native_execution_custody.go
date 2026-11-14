package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/kombifyio/techstack/internal/advancedissuer"
	"github.com/kombifyio/techstack/internal/executioncustody"
	"github.com/kombifyio/techstack/internal/localdb"
	"github.com/kombifyio/techstack/pkg/auth"
	"github.com/kombifyio/techstack/pkg/db"
)

// A native installation whose operator supplies a loopback server certificate
// (localdb) gets the two execution authorities a managed deployment prepares
// with provider-control-bootstrap: the provider-control runtime login and the
// Advanced issuer's signing-only login (bound custody, verified TLS). The
// supervising serve is the native operator process: it holds the bootstrap
// and migration custody, gives both logins a fresh credential on every start,
// installs their exact capabilities through the shared preparation and proves
// them through the posture gates startup uses. The serving child receives only
// the two bounded connections.

// nativeExecutionOperatorNames select or substitute execution custody; the
// supervisor prepares it itself, so none of them may come from the operator.
var nativeExecutionOperatorNames = []string{
	providerControlRuntimeDatabaseURLEnv, db.EnvAdvancedIssuerDatabaseURL, envAdvancedIssuerCustody,
	advancedissuer.EnvIssuerKey, executioncustody.EnvRemoteURL,
}

func nativeExecutionDBConfig(dsn string) db.Config {
	config := db.DefaultConfig()
	config.Backend, config.DriverName, config.DSN = db.StoreBackendPostgres, db.PostgresDriverName, dsn
	config.MaxOpenConns, config.MaxIdleConns = 2, 1
	return config
}

// prepareNativeExecutionCustody returns the serving child's execution
// connections, or nil when the installation serves no verified loopback TLS.
func prepareNativeExecutionCustody(ctx context.Context, pg *localdb.EmbeddedPostgres, applicationDSN string) (map[string]string, error) {
	if !pg.VerifiedLoopback() {
		return nil, nil
	}
	for _, name := range nativeExecutionOperatorNames {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return nil, fmt.Errorf("the native supervisor prepares this installation's execution custody; remove %s", name)
		}
	}
	encryptor := auth.GetEncryptor()
	if encryptor == nil {
		return nil, errors.New("native execution custody requires the installation's 32-byte TECHSTACK_ENCRYPTION_KEY")
	}
	owner, err := db.OpenContext(ctx, nativeExecutionDBConfig(pg.DSN()))
	if err != nil {
		return nil, errors.New("native migration connection failed")
	}
	defer owner.Close()

	// Provider control first: its preparation also withdraws PUBLIC object
	// rights, which the signing login's admission requires to be absent.
	providerDSN, err := pg.PrepareExecutionLogin(ctx, providerControlRuntimeRoleName)
	if err != nil {
		return nil, err
	}
	if err := installProviderControlRuntimeGrants(ctx, owner.DB, providerControlRuntimeRoleName); err != nil {
		return nil, err
	}
	provider, err := db.OpenContext(ctx, nativeExecutionDBConfig(providerDSN))
	if err != nil {
		return nil, errors.New("native provider-control login failed")
	}
	defer provider.Close()
	if err := verifyProviderControlRuntimeRolePosture(ctx, provider.DB, owner.DB); err != nil {
		return nil, err
	}

	issuerDSN, err := pg.PrepareExecutionLogin(ctx, localdb.AdvancedIssuerSigningRole)
	if err != nil {
		return nil, err
	}
	signingLogin, err := pgx.ParseConfig(issuerDSN)
	if err != nil {
		return nil, errors.New("native Advanced issuer login is invalid")
	}
	verifier, err := postgresSCRAMVerifier(signingLogin.Password)
	if err != nil {
		return nil, err
	}
	if err := db.PrepareAdvancedIssuerKeyCustody(ctx, owner, issuerDSN, localdb.NativeApplicationRole, verifier); err != nil {
		return nil, err
	}
	application, err := db.OpenContext(ctx, nativeExecutionDBConfig(applicationDSN))
	if err != nil {
		return nil, errors.New("native application connection failed")
	}
	defer application.Close()
	signing, err := db.OpenContext(ctx, nativeExecutionDBConfig(issuerDSN))
	if err != nil {
		return nil, errors.New("native Advanced issuer login failed")
	}
	defer signing.Close()
	store, err := advancedissuer.NewPostgresStoreWithKeyCustody(application, signing, encryptor)
	if err != nil {
		return nil, err
	}
	// The installation's issuer identity is created on its first prepared
	// start; every later start only loads it and proves the wrapping key.
	if _, _, err := store.LoadOrCreateKey(ctx); err != nil {
		return nil, errors.New("native Advanced issuer identity could not be created or unwrapped with this TECHSTACK_ENCRYPTION_KEY")
	}
	return map[string]string{providerControlRuntimeDatabaseURLEnv: providerDSN, db.EnvAdvancedIssuerDatabaseURL: issuerDSN}, nil
}
