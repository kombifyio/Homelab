package jobs

import (
	"context"
	"strings"

	"github.com/kombifyio/techstack/internal/gocommon/edgeauth"
	"github.com/kombifyio/techstack/pkg/identity"
	"github.com/kombifyio/techstack/pkg/middleware"
)

type requestAuthoritySnapshot struct {
	tenantID     string
	ownerID      string
	authorityCtx context.Context
	entitlements []string
}

type requestAuthorityContext struct {
	context.Context
	authority context.Context
}

func (c requestAuthorityContext) Value(key any) any {
	if value := c.authority.Value(key); value != nil {
		return value
	}
	return c.Context.Value(key)
}

// CopyEdgeFlagsFromContext retains the exact verified decisions only for this
// in-process job. Persisted payloads cannot recreate signed flag authority.
func CopyEdgeFlagsFromContext(ctx context.Context, job *Job) {
	if job == nil {
		return
	}
	flags, present := edgeauth.FlagsFromContext(ctx)
	job.mu.Lock()
	defer job.mu.Unlock()
	job.flagDecisions = nil
	if present {
		job.flagDecisions = &flags
	}
}

// CaptureRequestAuthority stores an in-process-only authority context containing
// only the already verified v2 commercial entitlements and signed budget
// decision. Re-attaching the exact verified FlagSet preserves the unexported
// cryptographic provenance that edgeauth intentionally refuses to reconstruct
// from application data, while context.Background detaches HTTP cancellation
// and every unrelated request value. The snapshot is deliberately not
// serialized: after a restart, fresh cost-bearing work without a newly verified
// request must fail closed. Exact NativeAdmission replay remains available from
// its durable database records.
func CaptureRequestAuthority(ctx context.Context, job *Job, tenantID, ownerID string) {
	captureRequestAuthority(ctx, job, tenantID, ownerID, false)
}

// CaptureRequestOperateAuthority retains the existing Inventory operate grant
// for non-spend runtime control. Cost-bearing capture still requires budgets.
func CaptureRequestOperateAuthority(ctx context.Context, job *Job, tenantID, ownerID string) {
	captureRequestAuthority(ctx, job, tenantID, ownerID, true)
}

func captureRequestAuthority(ctx context.Context, job *Job, tenantID, ownerID string, operate bool) {
	if ctx == nil || job == nil {
		return
	}
	tenantID = strings.TrimSpace(tenantID)
	ownerID = strings.TrimSpace(ownerID)
	principal := identity.FromContext(ctx)
	entitlements, source, hasEntitlements := middleware.AuthorizedEntitlementsFromContext(ctx)
	decisions, hasDecisions := edgeauth.FlagsFromContext(ctx)
	if tenantID == "" || ownerID == "" || principal == nil || principal.UserID != ownerID ||
		(strings.TrimSpace(principal.OrgID) != "" && strings.TrimSpace(principal.OrgID) != tenantID) ||
		!hasEntitlements {
		return
	}
	if operate {
		if !entitlements.Has("techstack.inventory.operate") && !entitlements.Has("techstack.inventory.admin") {
			return
		}
	} else if source != middleware.EntitlementSourceSignedEdge || !hasDecisions || len(decisions.Budgets) == 0 {
		return
	}
	snapshotEntitlements := entitlements.Values()
	authorityCtx := context.Background()
	if hasDecisions {
		authorityCtx = edgeauth.FlagsToContext(authorityCtx, decisions)
	}
	if source == middleware.EntitlementSourceSignedEdge {
		authorityCtx = middleware.WithSignedEntitlements(authorityCtx, snapshotEntitlements...)
	} else {
		authorityCtx = middleware.WithMembershipEntitlements(authorityCtx, snapshotEntitlements...)
	}
	snapshot := &requestAuthoritySnapshot{
		tenantID: tenantID, ownerID: ownerID,
		authorityCtx: authorityCtx,
		entitlements: snapshotEntitlements,
	}
	if len(snapshot.entitlements) == 0 {
		return
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	if firstJobString(job.Payload, job.Result, "tenant_id") != tenantID ||
		firstJobString(job.Payload, job.Result, "owner_id") != ownerID {
		return
	}
	job.requestAuthority = snapshot
}

func contextWithJobEdgeFlags(ctx context.Context, job *Job) context.Context {
	if job == nil {
		return ctx
	}
	job.mu.RLock()
	defer job.mu.RUnlock()
	if job.flagDecisions == nil {
		return ctx
	}
	return edgeauth.FlagsToContext(ctx, *job.flagDecisions)
}

func contextWithJobRequestAuthority(ctx context.Context, job *Job) context.Context {
	if job == nil {
		return ctx
	}
	job.mu.RLock()
	snapshot := cloneRequestAuthoritySnapshot(job.requestAuthority)
	tenantID := firstJobString(job.Payload, job.Result, "tenant_id")
	ownerID := firstJobString(job.Payload, job.Result, "owner_id")
	job.mu.RUnlock()
	if snapshot == nil || snapshot.tenantID != tenantID || snapshot.ownerID != ownerID {
		return ctx
	}
	if snapshot.authorityCtx == nil {
		return ctx
	}
	return requestAuthorityContext{Context: ctx, authority: snapshot.authorityCtx}
}

func cloneRequestAuthoritySnapshot(in *requestAuthoritySnapshot) *requestAuthoritySnapshot {
	if in == nil {
		return nil
	}
	return &requestAuthoritySnapshot{
		tenantID: in.tenantID, ownerID: in.ownerID,
		authorityCtx: in.authorityCtx,
		entitlements: append([]string(nil), in.entitlements...),
	}
}
