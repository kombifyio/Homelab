package monthlyruntime

import (
	"errors"
	"strings"
	"time"
)

// Operational (non-entitlement) managed-runtime failure identifiers. These ride
// in the SAME structured error envelope as ManagedRuntimeEntitlementDenialDetails
// (service.go) so every surface — 403 entitlement denial, 502 provision failure,
// stalled dashboard note, 409 decommission-blocked — parses exactly one shape.
// Unlike the entitlement denial, these are retryable and carry no feature lists.
const (
	// DecommissionBlockedUnreachableErrorCode marks a decommission blocked
	// because the runtime is unreachable (force offered).
	DecommissionBlockedUnreachableErrorCode = "decommission_blocked_unreachable"
	// DecommissionBlockedProtectedErrorCode marks a decommission blocked
	// because the lease is a protected demo anchor server.
	DecommissionBlockedProtectedErrorCode = "decommission_blocked_protected"

	// ReasonRuntimeUnreachable is the reason_code for a decommission blocked on
	// an unreachable runtime.
	ReasonRuntimeUnreachable = "runtime_unreachable"
	// ReasonLeaseProtected is the reason_code for a decommission blocked on a
	// protected demo anchor lease.
	ReasonLeaseProtected = "lease_protected"
	// DecommissionBlockedProviderRefusedErrorCode marks a decommission whose
	// provider cleanup attempts are spent without proving provider absence.
	DecommissionBlockedProviderRefusedErrorCode = "decommission_blocked_provider_refused"
	// ReasonProviderCleanupRefused is the reason_code for that refusal.
	ReasonProviderCleanupRefused = "provider_cleanup_refused"
)

type failureEnvelope struct {
	phase, phaseLabel, errorCode, reasonCode string
	capability, providerID                   string
	requiredFeatures, missingFeatures        []string
	retryable                                bool
	guidance                                 map[string]any
}

func structuredFailureDetails(spec failureEnvelope, extra map[string]any) map[string]any {
	details := map[string]any{
		"phase":             spec.phase,
		"phase_label":       spec.phaseLabel,
		"error_code":        spec.errorCode,
		"reason_code":       spec.reasonCode,
		"capability":        spec.capability,
		"required_features": compactStringSlice(spec.requiredFeatures),
		"missing_features":  compactStringSlice(spec.missingFeatures),
		"retryable":         spec.retryable,
		"user_guidance":     spec.guidance,
		"support_context": map[string]any{
			"feature_source": "Stripe/FGA/Flagship entitlement chain",
			"cost_bearing":   true,
		},
	}
	if spec.providerID != "" {
		details["provider_id"] = spec.providerID
	}
	for key, value := range extra {
		if _, canonical := details[key]; !canonical {
			details[key] = value
		}
	}
	return details
}

func managedRuntimeProviderID(providerID string) string {
	providerID = strings.ToLower(strings.TrimSpace(providerID))
	if providerID == "" {
		providerID = ProviderCentron
	}
	return providerID
}

// decommissionProviderID names the provider of the lease a decommission
// refusal is about (DecommissionBlockedProviderID). Unlike a new managed
// runtime, an existing lease has no default provider: an unknown one is
// omitted rather than guessed.
func decommissionProviderID(providerID string) string {
	return strings.ToLower(strings.TrimSpace(providerID))
}

// DecommissionProtectedDetails is the structured envelope returned (409) when a
// decommission is refused because the lease is one of the protected demo
// anchor servers. Not retryable and no force offer: the protection is
// deliberate operator configuration, not a transient condition.
func DecommissionProtectedDetails(providerID, leaseID string) map[string]any {
	extra := map[string]any{"force_offered": false}
	if id := strings.TrimSpace(leaseID); id != "" {
		extra["lease_id"] = id
	}
	return structuredFailureDetails(failureEnvelope{
		phase: "managed_runtime_decommission", phaseLabel: "Managed runtime decommission",
		errorCode: DecommissionBlockedProtectedErrorCode, reasonCode: ReasonLeaseProtected,
		capability: ManagedRuntimeCapability, providerID: decommissionProviderID(providerID),
		guidance: map[string]any{
			"title": "This server is part of the kombify live demo",
			"body":  "The demo's anchor servers are protected and cannot be decommissioned. If this account currently has managed-server capacity, use the add-server flow instead.",
			"next_steps": []string{
				"Try the add-server flow if managed-server capacity is available for this account.",
			},
		},
	}, extra)
}

// DecommissionRefusedDetails is the structured envelope returned (409) when the
// provider cleanup of an already accepted decommission stopped without proving
// that the server is gone. The provider reason decides the repair: a conflict
// means the provider holds resources kombify did not create next to this
// server, so deleting would destroy them; every other reason is a provider
// failure the next attempt re-reads.
func DecommissionRefusedDetails(providerID, leaseID string, err error) map[string]any {
	extra := map[string]any{"force_offered": false}
	if id := strings.TrimSpace(leaseID); id != "" {
		extra["lease_id"] = id
	}
	var refused *DecommissionRefusedError
	if !errors.As(err, &refused) {
		refused = nil
	}
	when := "later"
	if refused != nil {
		if code := strings.TrimSpace(refused.ProviderReasonCode); code != "" {
			extra["provider_reason_code"] = code
		}
		if !refused.RetryAfter.IsZero() {
			extra["retry_after"] = refused.RetryAfter.UTC().Format(time.RFC3339)
			when = "after " + refused.RetryAfter.UTC().Format(time.RFC3339)
		}
	}
	conflict := refused != nil && refused.ProviderConflict
	title := "The server could not be removed at the provider"
	body := "Every cleanup attempt for this server stopped without the provider confirming that it is gone. The server may still be running and billing at the provider."
	nextSteps := []string{"Retry the decommission " + when + "; each attempt reads the provider again before it deletes anything."}
	if conflict {
		title = "Other resources share this server's provider datacenter"
		body = "The provider datacenter that kombify created for this server also holds servers or networks kombify did not create. Removing the datacenter would delete them too, so kombify stopped before deleting anything."
		nextSteps = []string{
			"Move or delete the resources that were added to this server's provider datacenter.",
			"Kombify reads the provider again " + when + " and then removes the server; a decommission retry from then on starts it too.",
		}
	}
	return structuredFailureDetails(failureEnvelope{
		phase: "managed_runtime_decommission", phaseLabel: "Managed runtime decommission",
		errorCode: DecommissionBlockedProviderRefusedErrorCode, reasonCode: ReasonProviderCleanupRefused,
		capability: ManagedRuntimeCapability, providerID: decommissionProviderID(providerID), retryable: !conflict,
		guidance: map[string]any{
			"title":      title,
			"body":       body,
			"next_steps": nextSteps,
		},
	}, extra)
}

// DecommissionUnreachableDetails is the structured envelope returned (409) when
// a decommission is blocked because the runtime is unreachable. When
// forceOffered is true it points the caller at force=true, which cancels the
// lease and reconciles the provider resource in the background.
func DecommissionUnreachableDetails(providerID, leaseID string, forceOffered bool) map[string]any {
	extra := map[string]any{"force_offered": forceOffered}
	if id := strings.TrimSpace(leaseID); id != "" {
		extra["lease_id"] = id
	}
	nextSteps := []string{"Check whether the runtime is reachable and retry the decommission."}
	body := "The managed runtime did not respond to the decommission request. Forced decommission is unavailable until durable provider reconciliation is ready."
	if forceOffered {
		nextSteps = append(nextSteps, "Force decommission to cancel the lease; the provider resource is reconciled in the background so it does not keep billing.")
		body = "The managed runtime did not respond to the decommission request. Force decommission cancels the lease and reconciles the underlying provider resource in the background."
	}
	return structuredFailureDetails(failureEnvelope{
		phase: "managed_runtime_decommission", phaseLabel: "Managed runtime decommission",
		errorCode: DecommissionBlockedUnreachableErrorCode, reasonCode: ReasonRuntimeUnreachable,
		capability: ManagedRuntimeCapability, providerID: decommissionProviderID(providerID), retryable: true,
		guidance: map[string]any{
			"title":      "Cannot reach the server to decommission it",
			"body":       body,
			"next_steps": nextSteps,
		},
	}, extra)
}
