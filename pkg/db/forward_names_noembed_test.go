//go:build !embeddedpg

package db

import "testing"

// forwardNamesEmbeddedDSN is empty without the embeddedpg tag; the migration
// 199 proof then needs HOMELAB_TEST_POSTGRES_URL or skips.
func forwardNamesEmbeddedDSN(t *testing.T) string {
	t.Helper()
	return ""
}
