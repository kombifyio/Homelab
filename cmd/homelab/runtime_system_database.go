package main

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/kombifyio/homelab/internal/gocommon/tenantdb"
	"github.com/kombifyio/homelab/pkg/config"
	"github.com/kombifyio/homelab/pkg/db"
	"github.com/kombifyio/homelab/pkg/db/tenantcontext"
	"github.com/kombifyio/homelab/pkg/httpx"
)

// bootRuntimeSystemDatabase opens the hosted system login beside the
// application pool (platform-3r69a). The stable hosted runtime requires it:
// the application login reads the inventory only with an Edge-verified
// context, so loops, Guard ingress and Cloud service calls cannot run without
// it. Native Local and Guard keep their single product-native login; they
// refuse the custody, and their token-less work stays on the serving login.
func bootRuntimeSystemDatabase(ctx context.Context, cfg *config.Config, boot *v2Boot) error {
	if !hostedRuntime(cfg) || config.SecondaryChannel() {
		if strings.TrimSpace(os.Getenv(db.EnvRuntimeSystemDatabaseURL)) != "" {
			return errors.New("this runtime refuses hosted system database custody")
		}
		return nil
	}
	if boot == nil || boot.db == nil || boot.db.DB == nil {
		return errors.New("runtime system login requires the hosted application pool")
	}
	application, err := db.ConfigFromEnv()
	if err != nil {
		return err
	}
	systemConfig, err := db.RuntimeSystemConfig(application)
	if err != nil {
		return err
	}
	system, err := db.OpenContext(ctx, systemConfig)
	if err != nil {
		return errors.New("runtime system database authentication failed")
	}
	if err := db.VerifyRuntimeSystemLogin(ctx, system); err != nil {
		_ = system.Close()
		return err
	}
	boot.systemDB = system
	return nil
}

// runtimeSystemRoutePrefixes are the service-to-service HTTP classes: every
// route under them authenticates a Cloud or Gateway service call (signed,
// bound or shared-secret) or a host-key proof, never an owner. Guard and agent
// ingress is marked inside the worker routes after its credential check. Each
// handler keeps its own authentication; this only selects the login its
// transactions use.
var runtimeSystemRoutePrefixes = []string{"/api/v1/internal/", "/internal/v1/", "/api/internal/"}

// runtimeSystemRouteAdmission runs after Edge identity. A request on a service
// route without an Edge-signed context becomes system work. Every other
// token-less request (browser session, unauthenticated, owner routes) stays on
// the application login and reads no inventory row.
func runtimeSystemRouteAdmission(boot *v2Boot) func(*httpx.Event) error {
	return func(e *httpx.Event) error {
		if e == nil || e.Request == nil || tenantdb.ContextTokenFromContext(e.Request.Context()) != "" {
			return e.Next()
		}
		for _, prefix := range runtimeSystemRoutePrefixes {
			if strings.HasPrefix(e.Request.URL.Path, prefix) {
				e.Request = e.Request.WithContext(boot.systemContext(e.Request.Context()))
				break
			}
		}
		return e.Next()
	}
}

// systemContext marks ctx as Homelab system work. It masks any request
// token; on hosted runtimes, transactions on the application pool begin on the
// system login instead.
func (boot *v2Boot) systemContext(ctx context.Context) context.Context {
	if boot == nil || boot.db == nil || boot.systemDB == nil {
		return tenantcontext.WithSystemDatabase(ctx, nil, nil)
	}
	return tenantcontext.WithSystemDatabase(ctx, boot.db.DB, boot.systemDB.DB)
}
