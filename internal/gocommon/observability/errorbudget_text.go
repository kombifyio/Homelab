package observability

// Shared by the Sentry build (errorbudget.go) and the Full Local build
// (observability_full_local.go): no Sentry dependency.

import (
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrorBudgetOptions tunes ErrorBudget. Zero values take the defaults.
type ErrorBudgetOptions struct {
	// Window is how long a (fingerprint, subject) pair stays suppressed. Default 1h.
	Window time.Duration
	// MaxKeys bounds the in-memory table. Default 4096.
	MaxKeys int
	// PerFingerprintPerWindow caps distinct subjects per fingerprint per
	// window. Default 100.
	PerFingerprintPerWindow int
	// Now is the clock. Default time.Now.
	Now func() time.Time
}

var (
	fingerprintUUID   = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	fingerprintHex    = regexp.MustCompile(`(?i)[0-9a-f]{12,}`)
	fingerprintDigits = regexp.MustCompile(`[0-9]+`)
)

// NormalizeFingerprintText replaces UUIDs, hex runs of 12 or more characters
// and digit runs with "#", collapses whitespace and truncates to 200 bytes, so
// messages that differ only in identifiers share a fingerprint.
func NormalizeFingerprintText(s string) string {
	s = fingerprintUUID.ReplaceAllString(s, "#")
	s = fingerprintHex.ReplaceAllString(s, "#")
	s = fingerprintDigits.ReplaceAllString(s, "#")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}
