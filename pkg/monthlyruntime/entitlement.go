package monthlyruntime

import (
	"context"
	"os"
	"strings"

	"github.com/kombifyio/homelab/internal/providercatalog"
)

// ManagedRuntimeEntitlementDecision is the single account/provider
// availability decision shared by every Homelab managed-create surface.
type ManagedRuntimeEntitlementDecision struct {
	Denied           bool
	Message          string
	ProviderID       string
	ReasonCode       string
	RequiredFeatures []string
	MissingFeatures  []string
}

func (d ManagedRuntimeEntitlementDecision) Details() map[string]any {
	return ManagedRuntimeEntitlementDenialDetails(
		d.ProviderID,
		d.ReasonCode,
		d.RequiredFeatures,
		d.MissingFeatures,
	)
}

// EvaluateManagedRuntimeEntitlement fails closed before persistence, queueing,
// admission, or provider dispatch. The local E2E exception is product policy
// here, not a route-specific fork; no platform role skips the subscription.
func EvaluateManagedRuntimeEntitlement(
	ctx context.Context,
	checker FeatureChecker,
	userID string,
	providerID string,
) ManagedRuntimeEntitlementDecision {
	canonicalProviderID, err := providercatalog.CanonicalProviderID(providerID)
	if err != nil {
		return deniedManagedRuntimeEntitlement(providerID, EntitlementReasonProviderUnsupported, nil, nil)
	}
	if LocalManagedRuntimeE2EAllowed() {
		return ManagedRuntimeEntitlementDecision{ProviderID: canonicalProviderID}
	}
	requiredFeatures := RequiredFeatureKeysForProvider(canonicalProviderID)
	if checker == nil {
		return deniedManagedRuntimeEntitlement(canonicalProviderID, EntitlementReasonCheckerUnavailable, requiredFeatures, nil)
	}
	for _, featureKey := range requiredFeatures {
		enabled, checkErr := checker.IsEnabled(ctx, featureKey, userID)
		if checkErr != nil {
			return deniedManagedRuntimeEntitlement(canonicalProviderID, EntitlementReasonFeatureCheckFailed, requiredFeatures, []string{featureKey})
		}
		if !enabled {
			return deniedManagedRuntimeEntitlement(canonicalProviderID, EntitlementReasonFeatureDisabled, requiredFeatures, []string{featureKey})
		}
	}
	return ManagedRuntimeEntitlementDecision{ProviderID: canonicalProviderID, RequiredFeatures: requiredFeatures}
}

// EvaluateManagedRuntimeRenewal decides whether a managed lease of providerID
// may be renewed from a person's committed entitlement projection (Administration
// GET /api/v1/entitlements/current). Every key the provider requires must be
// granted. The caller passes only a projection it read successfully: an
// unreadable projection is unknown, never reaches this decision and therefore
// neither renews nor shortens anything. The local E2E exception does not apply;
// renewal always needs the committed projection.
func EvaluateManagedRuntimeRenewal(providerID string, granted []string) ManagedRuntimeEntitlementDecision {
	canonicalProviderID, err := providercatalog.CanonicalProviderID(providerID)
	if err != nil {
		return deniedManagedRuntimeEntitlement(providerID, EntitlementReasonProviderUnsupported, nil, nil)
	}
	requiredFeatures := RequiredFeatureKeysForProvider(canonicalProviderID)
	held := make(map[string]bool, len(granted))
	for _, key := range granted {
		held[strings.TrimSpace(key)] = true
	}
	var missing []string
	for _, featureKey := range requiredFeatures {
		if !held[featureKey] {
			missing = append(missing, featureKey)
		}
	}
	if len(missing) > 0 {
		return deniedManagedRuntimeEntitlement(canonicalProviderID, EntitlementReasonFeatureDisabled, requiredFeatures, missing)
	}
	return ManagedRuntimeEntitlementDecision{ProviderID: canonicalProviderID, RequiredFeatures: requiredFeatures}
}

func deniedManagedRuntimeEntitlement(providerID, reasonCode string, requiredFeatures, missingFeatures []string) ManagedRuntimeEntitlementDecision {
	return ManagedRuntimeEntitlementDecision{
		Denied: true, Message: ManagedRuntimeEntitlementDeniedMessage,
		ProviderID: providerID, ReasonCode: reasonCode,
		RequiredFeatures: requiredFeatures, MissingFeatures: missingFeatures,
	}
}

// LocalManagedRuntimeE2EAllowed is the one narrow self-host development
// exception. Both switches are required so one stale environment flag cannot
// authorize a cost-bearing managed provider path.
// LocalManagedRuntimeE2EAllowed opens managed monthly-runtime creation on a
// non-SaaS Core for test lanes only: HOMELAB_ENV development (the Docker
// deployment-target E2E) or local (the loopback Core of
// scripts/os-compat/managed-lab-lane.py --substrate provider), and both
// explicit test flags. Every other environment keeps the SaaS-only rule.
func LocalManagedRuntimeE2EAllowed() bool {
	environment := strings.ToLower(strings.TrimSpace(os.Getenv("HOMELAB_ENV")))
	return (environment == "development" || environment == "local") &&
		truthyManagedRuntimeEntitlementEnv("HOMELAB_ALLOW_LOCAL_SIMULATION_GATE") &&
		truthyManagedRuntimeEntitlementEnv("HOMELAB_ALLOW_LOCAL_MANAGED_RUNTIME_E2E")
}

func truthyManagedRuntimeEntitlementEnv(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// LocalRuntimeE2EBackupGrantTenant names the one tenant whose backup
// entitlement a managed-runtime test lane may hold without the signed chain
// (owner decision 2026-10-03). It applies only on a non-SaaS Core with
// HOMELAB_ENV=local and both runtime E2E test flags (the
// LocalManagedRuntimeE2EAllowed exception), and only to the tenant the lane
// provisions (HOMELAB_RUNTIME_E2E_TENANT_ID). It grants the entitlement
// only; every other admission check is unchanged.
func LocalRuntimeE2EBackupGrantTenant(saas bool) string {
	if saas || !strings.EqualFold(strings.TrimSpace(os.Getenv("HOMELAB_ENV")), "local") || !LocalManagedRuntimeE2EAllowed() {
		return ""
	}
	return strings.TrimSpace(os.Getenv("HOMELAB_RUNTIME_E2E_TENANT_ID"))
}
