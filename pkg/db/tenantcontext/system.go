package tenantcontext

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/kombifyio/homelab/internal/gocommon/tenantdb"
)

// ErrNoDatabase is returned when a scoped transaction has no pool to begin on.
var ErrNoDatabase = errors.New("tenantcontext: database handle is nil")

type systemRouteKey struct{}

type systemRoute struct{ application, system *sql.DB }

// WithSystemDatabase marks ctx as Homelab system work: background loops,
// Guard ingress and service-to-service calls that carry no Edge-signed
// database context. Every transaction that would begin on the hosted
// application pool begins on the dedicated system login instead, so the
// application login never reads or writes tenant rows through a runtime-chosen
// app.tenant_id alone. Any other pool (provider control, workers) is unchanged.
//
// The system login is never a context-token audience, so a request token in
// ctx is masked here. Only boot wiring holds the system pool; a caller cannot
// derive it from a request. A nil system pool (native Local) leaves ctx
// unchanged apart from the masked token: its single serving login keeps the
// product-native authority.
func WithSystemDatabase(ctx context.Context, application, system *sql.DB) context.Context {
	if ctx == nil {
		return nil
	}
	ctx = tenantdb.NewContextWithToken(ctx, "")
	if application == nil || system == nil || application == system {
		return ctx
	}
	return context.WithValue(ctx, systemRouteKey{}, systemRoute{application: application, system: system})
}

// IsSystem reports whether ctx carries the hosted system route.
func IsSystem(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	_, ok := ctx.Value(systemRouteKey{}).(systemRoute)
	return ok
}

// Database returns the pool a statement in ctx must use: the system login for
// system work on the application pool, otherwise pool itself.
func Database(ctx context.Context, pool *sql.DB) *sql.DB {
	if ctx != nil {
		if route, ok := ctx.Value(systemRouteKey{}).(systemRoute); ok && pool == route.application {
			return route.system
		}
	}
	return pool
}

// Begin starts the one tenant-scoped transaction shape: the routed pool, then
// ApplyTenant before any payload SQL. User requests bind the Edge-verified
// context; system work binds its own login. Neither can choose the other.
func Begin(ctx context.Context, pool *sql.DB, opts *sql.TxOptions, tenantID string) (*sql.Tx, error) {
	if ctx == nil || strings.TrimSpace(tenantID) == "" {
		return nil, tenantdb.ErrNoIdentity
	}
	database := Database(ctx, pool)
	if database == nil {
		return nil, ErrNoDatabase
	}
	tx, err := database.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	if err := ApplyTenant(ctx, tx, tenantID); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}
