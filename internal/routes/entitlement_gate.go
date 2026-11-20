package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/kombifyio/homelab/internal/gocommon/denial"
	ksapi "github.com/kombifyio/homelab/pkg/api"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/middleware"
)

// EntitlementGate decides whether the request's verified principal holds a
// commercial entitlement. The composition root picks the deployment's gate;
// routes never infer it from request data.
type EntitlementGate interface {
	// SignedGrant accepts only an Edge-signed (v2) grant. Mutations that change
	// a customer's host use it: the server-owned membership fallback is not a
	// per-request Gateway decision.
	SignedGrant(ctx context.Context, entitlement string) bool
	// AuthorizedGrant accepts a signed grant or the membership fallback that a
	// verified Homelab session persisted (see contextWithMembershipAuthorization).
	AuthorizedGrant(ctx context.Context, entitlement string) bool
	// FreshMultiFactor reports a multi-factor sign-in no older than maxAge,
	// read only from the verified edge envelope.
	FreshMultiFactor(ctx context.Context, now time.Time, maxAge time.Duration) bool
}

type hostedEntitlementGate struct{}

func (hostedEntitlementGate) SignedGrant(ctx context.Context, entitlement string) bool {
	grants, ok := middleware.SignedEntitlementsFromContext(ctx)
	return ok && grants.Has(entitlement)
}

func (hostedEntitlementGate) AuthorizedGrant(ctx context.Context, entitlement string) bool {
	grants, _, ok := middleware.AuthorizedEntitlementsFromContext(ctx)
	return ok && grants.Has(entitlement)
}

func (hostedEntitlementGate) FreshMultiFactor(ctx context.Context, now time.Time, maxAge time.Duration) bool {
	claims, ok := middleware.VerifiedStepUpFromContext(ctx)
	return ok && claims.FreshMultiFactor(now, maxAge)
}

type selfHostedEntitlementGate struct{}

func (selfHostedEntitlementGate) SignedGrant(context.Context, string) bool     { return true }
func (selfHostedEntitlementGate) AuthorizedGrant(context.Context, string) bool { return true }
func (selfHostedEntitlementGate) FreshMultiFactor(context.Context, time.Time, time.Duration) bool {
	return true
}

// EntitlementGateForDeployment returns the fail-closed hosted gate for SaaS and
// the owner-operated gate for a self-hosted control plane, which has no
// commercial entitlement authority; there the owner scope and inventory
// policy remain the boundary, as in NewSelfHostedInventoryPolicy.
func EntitlementGateForDeployment(saas bool) EntitlementGate {
	if saas {
		return hostedEntitlementGate{}
	}
	return selfHostedEntitlementGate{}
}

// freshPersonOrActingAgent admits an owner action that needs no person by
// workspace AI-CAPABILITY-PROVISIONING-TARGET.md section 5e: an agent the
// Gateway attested as acting for the person passes without a step-up page,
// and the person's own session still presents a fresh second factor. It
// returns the attested agent for the audit record, empty for the person. The
// per-route decisions are in docs/GUARD-PRIVILEGES.md, "Person and agent
// authority on owner routes".
func freshPersonOrActingAgent(ctx context.Context, gate EntitlementGate, now time.Time, maxAge time.Duration) (string, bool) {
	if claims, verified := middleware.VerifiedStepUpFromContext(ctx); verified {
		if agentID, acting := claims.ActingAgent(); acting {
			return agentID, true
		}
	}
	return "", entitlementGateOrHosted(gate).FreshMultiFactor(ctx, now, maxAge)
}

// stepUpRefusal is the refusal of freshPersonOrActingAgent. A person is asked
// to confirm a second factor; a signed principal that is no person and no attested
// agent (demo, machine, service, an agent without a signed identity) cannot
// sign in, so it is told what is missing instead of a retry.
func stepUpRefusal(ctx context.Context, message, reason string) (string, map[string]any) {
	if claims, verified := middleware.VerifiedStepUpFromContext(ctx); verified && claims.PrincipalType != middleware.PrincipalTypeUser {
		return "This action needs the owner or an agent acting for the owner", map[string]any{
			"reason": "acting_agent_attestation_required", "retryable": false,
			"guidance": "Call as an agent the kombify Gateway attests for the owner, or let the owner perform the action.",
		}
	}
	return message, map[string]any{
		"reason": reason, "retryable": true,
		"guidance": "Confirm with your second factor, then repeat the request within five minutes.",
	}
}

// entitlementGateOrHosted keeps an unset gate fail-closed.
func entitlementGateOrHosted(gate EntitlementGate) EntitlementGate {
	if gate == nil {
		return hostedEntitlementGate{}
	}
	return gate
}

// writeEntitlementDenial writes the 403 client-error-envelope/v1 for a missing
// commercial entitlement and returns httpx.ErrResponseWritten.
func writeEntitlementDenial(e *httpx.Event, capability, required string) error {
	env := denial.Envelope{
		ErrorCode: "entitlement_required", ReasonCode: "missing_entitlement", Capability: capability,
		RequiredFeatures: []string{required}, MissingFeatures: []string{required},
		UserGuidance: denial.UserGuidance{
			Title:     "Your plan does not include this action",
			Body:      "This action needs an entitlement your account does not hold.",
			NextSteps: []string{"Upgrade to a plan that includes it, or ask your organization owner for access."},
		},
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return httpx.Reject(e, http.StatusInternalServerError, ksapi.ErrCodeInternal, "Entitlement check unavailable", nil)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return httpx.Reject(e, http.StatusInternalServerError, ksapi.ErrCodeInternal, "Entitlement check unavailable", nil)
	}
	payload["error"] = map[string]any{"code": ksapi.ErrCodeForbidden, "message": env.UserGuidance.Title, "details": map[string]any{"reason": env.ReasonCode}}
	if err := e.JSON(http.StatusForbidden, payload); err != nil {
		return err
	}
	return httpx.ErrResponseWritten
}
