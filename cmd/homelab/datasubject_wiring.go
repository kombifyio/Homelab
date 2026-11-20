package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kombifyio/homelab/internal/datasubject"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/monthlyruntime"
)

// registerDataSubjectRoute mounts the Cloud data-subject participant route. It
// answers 503 unless HOMELAB_DATASUBJECT_ENABLED, SERVICE_AUTH_SECRET and a
// valid HOMELAB_DATASUBJECT_SIGNING_KEY are all set; an enabled endpoint with
// a malformed key stays closed and logs why. The subject directory (migration
// 156) resolves opaque refs across tenants; while it is absent an enabled
// endpoint answers blocked receipts (homelab_subject_directory_unwired)
// instead of claiming that nothing is held.
func registerDataSubjectRoute(router *httpx.Router, deps routeDeps) {
	handler := datasubject.Unavailable()
	if envBoolDefault("HOMELAB_DATASUBJECT_ENABLED", false) {
		seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("HOMELAB_DATASUBJECT_SIGNING_KEY")))
		switch {
		case err != nil || len(seed) != ed25519.SeedSize:
			deps.log.Error("datasubject_disabled", "reason", "HOMELAB_DATASUBJECT_SIGNING_KEY must be a base64 32-byte Ed25519 seed")
		default:
			var retention time.Duration
			if raw := strings.TrimSpace(os.Getenv("HOMELAB_DATASUBJECT_EVIDENCE_RETENTION")); raw != "" {
				if d, perr := time.ParseDuration(raw); perr == nil {
					retention = d
				} else {
					deps.log.Error("datasubject_evidence_retention_invalid", "error", perr)
				}
			}
			handler = datasubject.NewHandler(datasubject.Config{
				Enabled:    true,
				Secret:     strings.TrimSpace(os.Getenv("SERVICE_AUTH_SECRET")),
				SecretNext: strings.TrimSpace(os.Getenv("SERVICE_AUTH_SECRET_NEXT")),
				SigningKey: ed25519.NewKeyFromSeed(seed),
				Executor:   dataSubjectExecutor(deps, retention),
			})
		}
	}
	router.POST(datasubject.Path, func(e *httpx.Event) error {
		handler.ServeHTTP(e.Response, e.Request)
		return nil
	})
}

func dataSubjectExecutor(deps routeDeps, retention time.Duration) datasubject.Executor {
	exec := &datasubject.StoreExecutor{EvidenceRetention: retention}
	if deps.governedDecommission != nil {
		// Erasure decommissions the subject's managed leases before it erases.
		exec.Decommissioner = deps.governedDecommission
	}
	if deps.v2 != nil && deps.v2.db != nil && deps.v2.db.DB != nil {
		exec.Store = &datasubject.SQLStore{DB: deps.v2.db.DB}
		// Migration 156 owns the cross-tenant subject directory. Until it is
		// applied the lookup fails and requests answer blocked
		// (homelab_subject_directory_unwired), never "nothing held".
		exec.Directory = &datasubject.SQLDirectory{DB: deps.v2.db.DB}
	}
	return exec
}

// dataSubjectGovernedActor names the account-erasure cascade in the lease
// journal: the Gateway-delivered data-subject request is its authority.
const dataSubjectGovernedActor = "service:gateway/datasubject"

// governedDecommissionBinding hands the monthly-runtime service to the
// data-subject participant, which is registered before the service exists.
// Until it is bound, erasure stays blocked on live resources.
type governedDecommissionBinding struct {
	mu  sync.RWMutex
	svc *monthlyruntime.Service
}

func (b *governedDecommissionBinding) bind(svc *monthlyruntime.Service) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.svc = svc
}

// DecommissionErasedSubject admits the governed decommission of the erased
// subject's managed leases in one tenant.
func (b *governedDecommissionBinding) DecommissionErasedSubject(ctx context.Context, p datasubject.Principal, requestID string) ([]string, error) {
	var svc *monthlyruntime.Service
	if b != nil {
		b.mu.RLock()
		svc = b.svc
		b.mu.RUnlock()
	}
	if svc == nil {
		return nil, errors.New("governed decommission is not wired")
	}
	return svc.DecommissionErasedSubjectLeases(ctx, p.TenantID, p.Subject, dataSubjectGovernedActor, requestID)
}
