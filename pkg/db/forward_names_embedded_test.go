//go:build embeddedpg

package db

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
)

// forwardNamesEmbeddedDSN starts a private superuser PostgreSQL 17 for the
// migration 199 proof on a host without a throwaway server (the rack's fixed
// check environment): go test -tags embeddedpg -run '^TestIntegrationHomelabForwardNames' ./pkg/db
func forwardNamesEmbeddedDSN(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	base := t.TempDir()
	password := strings.ReplaceAll(uuid.NewString(), "-", "")
	cfg := embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V17).
		Encoding("UTF8").
		Locale("C").
		Port(uint32(port)).
		Username("postgres").
		Password(password).
		Database("postgres").
		RuntimePath(filepath.Join(base, "runtime")).
		DataPath(filepath.Join(base, "data")).
		CachePath(filepath.Join(os.TempDir(), "homelab-embedded-postgres-cache")).
		StartTimeout(3 * time.Minute).
		Logger(io.Discard)
	server := embeddedpostgres.NewDatabase(cfg)
	if err := server.Start(); err != nil {
		t.Fatalf("start the embedded PostgreSQL 17: %v", err)
	}
	t.Cleanup(func() { _ = server.Stop() })
	return fmt.Sprintf("postgres://postgres:%s@127.0.0.1:%d/postgres?sslmode=disable", password, port)
}
