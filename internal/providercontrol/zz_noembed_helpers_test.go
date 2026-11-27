//go:build !embeddedpg

package providercontrol

import "testing"

func embeddedProviderControlDSN(t *testing.T) string {
	t.Helper()
	return ""
}
