package localdb

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The operator's server certificate for the loopback address. With it the
// embedded PostgreSQL serves verified TLS on 127.0.0.1 and the supervisor can
// give the installation's execution authorities their own verify-full logins.
// Without it nothing changes: the native installation keeps its plain loopback
// connections and runs no provider control or bound Advanced issuer.
const (
	EnvEmbeddedPostgresTLSCertFile     = "TECHSTACK_EMBEDDED_POSTGRES_TLS_CERT_FILE"
	EnvEmbeddedPostgresTLSKeyFile      = "TECHSTACK_EMBEDDED_POSTGRES_TLS_KEY_FILE"
	EnvEmbeddedPostgresTLSRootCertFile = "TECHSTACK_EMBEDDED_POSTGRES_TLS_ROOT_CERT_FILE"
)

// The two execution authorities that hold their own login on a native
// installation. Their grants come from the shared governed preparation, which
// the supervisor runs as the migration owner.
const (
	ProviderControlRuntimeRole = "techstack_provider_control_runtime"
	AdvancedIssuerSigningRole  = "techstack_advanced_issuer"
)

type loopbackTLS struct {
	cert, key, root string
}

func (t loopbackTLS) enabled() bool { return t.cert != "" }

// loopbackTLSFromEnv admits all three files or none. PostgreSQL itself refuses
// a key file readable by others; the paths must be absolute, regular files
// and free of the characters a shell would interpret inside the double-quoted
// start options pg_ctl passes on.
func loopbackTLSFromEnv() (loopbackTLS, error) {
	t := loopbackTLS{
		cert: strings.TrimSpace(os.Getenv(EnvEmbeddedPostgresTLSCertFile)),
		key:  strings.TrimSpace(os.Getenv(EnvEmbeddedPostgresTLSKeyFile)),
		root: strings.TrimSpace(os.Getenv(EnvEmbeddedPostgresTLSRootCertFile)),
	}
	if t.cert == "" && t.key == "" && t.root == "" {
		return loopbackTLS{}, nil
	}
	// pg_ctl runs the start options through /bin/sh on Unix and CMD on Windows.
	interpreted := "\"'`$\\"
	if runtime.GOOS == "windows" {
		interpreted = "\"%"
	}
	for _, path := range []string{t.cert, t.key, t.root} {
		if path == "" {
			return loopbackTLS{}, errors.New("embedded PostgreSQL TLS needs its certificate, key and root certificate files together")
		}
		info, err := os.Stat(path)
		if !filepath.IsAbs(path) || strings.ContainsAny(path, interpreted) || err != nil || !info.Mode().IsRegular() {
			return loopbackTLS{}, errors.New("embedded PostgreSQL TLS files must be existing absolute regular files")
		}
	}
	if info, _ := os.Stat(t.key); runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return loopbackTLS{}, errors.New("embedded PostgreSQL TLS key file must be private to its owner (0600)")
	}
	return t, nil
}

func (t loopbackTLS) startParameters(clusterName string) map[string]string {
	parameters := map[string]string{"cluster_name": clusterName}
	if t.enabled() {
		parameters["ssl"] = "on"
		parameters["ssl_cert_file"] = t.cert
		parameters["ssl_key_file"] = t.key
		parameters["ssl_min_protocol_version"] = "TLSv1.2"
	}
	return parameters
}

// VerifiedLoopback reports whether this installation serves verified TLS.
func (pg *EmbeddedPostgres) VerifiedLoopback() bool {
	return pg != nil && pg.tls.enabled()
}

// PrepareExecutionLogin gives one execution authority its own NOINHERIT login
// with a fresh credential for this start and returns its verify-full loopback
// connection. It runs only in the supervisor, with the bootstrap role, and
// never grants anything: the shared preparation installs the exact
// capabilities as the migration owner afterwards. An existing login with
// memberships or elevated attributes is refused, not repaired.
func (pg *EmbeddedPostgres) PrepareExecutionLogin(ctx context.Context, role string) (string, error) {
	if pg == nil || pg.custody == nil || !pg.tls.enabled() {
		return "", errors.New("native execution logins require verified loopback TLS")
	}
	if role != ProviderControlRuntimeRole && role != AdvancedIssuerSigningRole {
		return "", errors.New("unknown native execution login")
	}
	password, err := randomCredential()
	if err != nil {
		return "", err
	}
	verifier, err := scramVerifier(password)
	if err != nil {
		return "", err
	}
	conn, err := pgx.Connect(ctx, pg.adminDSN)
	if err != nil {
		return "", redactConnectError(err)
	}
	defer conn.Close(context.Background())
	var elevated bool
	err = conn.QueryRow(ctx, `SELECT r.rolsuper OR r.rolbypassrls OR r.rolcreaterole OR r.rolcreatedb OR r.rolreplication
	 OR r.rolinherit OR NOT r.rolcanlogin OR EXISTS(SELECT 1 FROM pg_catalog.pg_auth_members WHERE member=r.oid)
	 FROM pg_catalog.pg_roles r WHERE r.rolname=$1`, role).Scan(&elevated)
	identifier := pgx.Identifier{role}.Sanitize()
	statement := "CREATE ROLE " + identifier + " LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB NOREPLICATION NOINHERIT PASSWORD " + quoteLiteral(verifier)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return "", errors.New("inspect native execution login failed (SQLSTATE " + sqlState(err) + ")")
	case elevated:
		return "", errors.New("native execution login " + role + " has elevated attributes or memberships; nothing was changed")
	default:
		statement = "ALTER ROLE " + identifier + " PASSWORD " + quoteLiteral(verifier)
	}
	if _, err := conn.Exec(ctx, statement); err != nil {
		return "", errors.New("prepare native execution login failed (SQLSTATE " + sqlState(err) + ")")
	}
	return (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(role, password),
		Host:     "127.0.0.1:" + strconv.FormatUint(uint64(pg.port), 10),
		Path:     "/" + embeddedDatabase,
		RawQuery: url.Values{"sslmode": {"verify-full"}, "sslrootcert": {pg.tls.root}}.Encode(),
	}).String(), nil
}
