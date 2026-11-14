//go:build kombify_full_local

// Package observability is the Full Local build of the Sentry wiring (build
// tag kombify_full_local, DATA-ARCHITECTURE.md section 5, D-2026-10-21-12): the
// same API, but no Sentry client is created (ErrorBudget only names the SDK's
// event types so callers' own sentry.Init hooks compile), so the binary has no error
// reporting client, no DSN handling and nothing to send. Tracing helpers
// (tracing.go) and the logger (logging.go) are unchanged; they have no
// network exporter.
package observability

import (
	"context"
	"errors"
	"net/http"

	sentry "github.com/getsentry/sentry-go"
)

// MustInit does nothing and returns a no-op shutdown.
func MustInit(Config) func() { return func() {} }

// Middleware returns next unchanged.
func Middleware(next http.Handler) http.Handler { return next }

// CaptureErr returns err unchanged; nothing is reported.
func CaptureErr(_ context.Context, err error) error { return err }

// ErrObservabilityDisabled reports that error reporting is disabled.
var ErrObservabilityDisabled = errors.New("observability: compiled out of this Full Local build")

// IsEnabled is always false.
func IsEnabled() bool { return false }

// ErrorBudget keeps the signature of the Sentry build so services that call it
// from their own sentry.Init compile under this tag. It applies no budget: it
// calls next, or returns the event unchanged when next is nil. Only the
// sentry-go event types are referenced; no client is created here.
func ErrorBudget(_ ErrorBudgetOptions, next func(*sentry.Event, *sentry.EventHint) *sentry.Event) func(*sentry.Event, *sentry.EventHint) *sentry.Event {
	return func(event *sentry.Event, hint *sentry.EventHint) *sentry.Event {
		if next != nil {
			return next(event, hint)
		}
		return event
	}
}
