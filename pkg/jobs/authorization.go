package jobs

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/kombifyio/homelab/internal/gocommon/edgeauth"
	"github.com/kombifyio/homelab/internal/gocommon/identity"
	"github.com/kombifyio/homelab/pkg/middleware"
)

const jobAuthorizationPayloadKey = "_job_authorization"

// jobAuthorization is authored at queue admission, never from a client payload.
// Only trusted control-plane recovery may restore this server-owned field;
// HTTP inputs are typed commands, never serialized Jobs or recovery payloads.
// It grants only inventory reads and operation of this job's owned resources.
// Provisioning/spend uses the separately admitted managed lease request; this
// payload carries neither flags nor request budgets nor a wildcard grant.
type jobAuthorization struct {
	JobID        string   `json:"job_id"`
	Type         JobType  `json:"type"`
	TargetID     string   `json:"target_id"`
	TenantID     string   `json:"tenant_id"`
	OwnerID      string   `json:"owner_id"`
	Entitlements []string `json:"entitlements"`
}

// CaptureJobAuthorization converts current authenticated inventory grants into
// explicit decisions in the server-owned job payload. Request context values
// and signature envelopes never cross into the worker.
func CaptureJobAuthorization(ctx context.Context, job *Job, tenantID, ownerID string) {
	if job == nil {
		return
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	delete(job.Payload, jobAuthorizationPayloadKey)
	principal := identity.FromContext(ctx)
	grants, source, ok := middleware.AuthorizedEntitlementsFromContext(ctx)
	tenantID, ownerID = strings.TrimSpace(tenantID), strings.TrimSpace(ownerID)
	if !ok || principal == nil || tenantID == "" || ownerID == "" || principal.UserID != ownerID ||
		(strings.TrimSpace(principal.OrgID) != "" && principal.OrgID != tenantID) ||
		firstJobString(job.Payload, job.Result, "tenant_id") != tenantID ||
		firstJobString(job.Payload, job.Result, "owner_id") != ownerID {
		return
	}
	if source == middleware.EntitlementSourceSignedEdge {
		decisions, present := edgeauth.FlagsFromContext(ctx)
		if !present {
			return
		}
		binding, verified := decisions.VerifiedDecisionBinding()
		if !verified || binding.SubjectID != ownerID || binding.TenantID != tenantID ||
			binding.Audience != "homelab" || binding.PublicPrefix != "/v1/homelab" {
			return
		}
	}
	authorized := []string{}
	for _, key := range []string{"homelab.inventory.read", "homelab.inventory.operate"} {
		if grants.Has(key) || grants.Has("homelab.inventory.admin") {
			authorized = append(authorized, key)
		}
	}
	if len(authorized) == 0 || job.ID == "" || job.Type == "" || job.TargetID == "" {
		return
	}
	if job.Payload == nil {
		job.Payload = make(map[string]interface{})
	}
	job.Payload[jobAuthorizationPayloadKey] = jobAuthorization{JobID: job.ID, Type: job.Type, TargetID: job.TargetID,
		TenantID: tenantID, OwnerID: ownerID, Entitlements: authorized}
}

func contextWithJobAuthorization(ctx context.Context, job *Job) context.Context {
	if job == nil {
		return ctx
	}
	job.mu.RLock()
	defer job.mu.RUnlock()
	encoded, err := json.Marshal(job.Payload[jobAuthorizationPayloadKey])
	if err != nil {
		return ctx
	}
	var grant jobAuthorization
	if json.Unmarshal(encoded, &grant) != nil || grant.JobID == "" || grant.TenantID == "" || grant.OwnerID == "" ||
		grant.JobID != job.ID || grant.Type != job.Type || grant.TargetID != job.TargetID ||
		grant.TenantID != firstJobString(job.Payload, job.Result, "tenant_id") ||
		grant.OwnerID != firstJobString(job.Payload, job.Result, "owner_id") {
		return ctx
	}
	for _, key := range grant.Entitlements {
		if key != "homelab.inventory.read" && key != "homelab.inventory.operate" {
			return ctx
		}
	}
	return middleware.WithJobEntitlements(ctx, grant.Entitlements...)
}
