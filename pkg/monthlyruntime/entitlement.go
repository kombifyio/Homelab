package monthlyruntime

import (
	"context"
	"os"
	"slices"
	"strings"

	"github.com/kombifyio/homelab/internal/providercatalog"
	"github.com/kombifyio/homelab/internal/runtimeproduct/serverruntime"
	"github.com/kombifyio/homelab/pkg/config"
	"github.com/kombifyio/homelab/pkg/middleware"
)

type DeploymentLane string

const (
	DeploymentLaneSaaS       DeploymentLane = "saas"
	DeploymentLaneSelfHosted DeploymentLane = "self-hosted"
)

const (
	EntitlementKeyAllYouNeedCredits        = "cloud.runtime.credits.ayn"
	EntitlementReasonOfferingTierRequired  = "offering_tier_entitlement_required"
	EntitlementReasonOfferingUnsupported   = "managed_offering_unsupported"
	EntitlementReasonDeploymentModeUnknown = "deployment_mode_unknown"
	EntitlementRefusalClassEntitlement     = "entitlement"
	EntitlementRefusalClassProvider        = "unavailable_provider"
)

// DeploymentLaneFromMode resolves a DeploymentMode to its typed offering lane.
// Unset or unrecognized modes return an empty lane so callers fail closed.
func DeploymentLaneFromMode(mode config.DeploymentMode) DeploymentLane {
	switch {
	case mode.IsSaaS():
		return DeploymentLaneSaaS
	case mode.IsSelfHosted():
		return DeploymentLaneSelfHosted
	default:
		return ""
	}
}

// ManagedRuntimeEntitlementDecision is the single account/provider
// availability decision shared by every Homelab managed-create surface.
type ManagedRuntimeEntitlementDecision struct {
	Denied           bool
	Message          string
	ProviderID       string
	ReasonCode       string
	RefusalClass     string
	RequiredFeatures []string
	MissingFeatures  []string
}

func (d ManagedRuntimeEntitlementDecision) Details() map[string]any {
	details := ManagedRuntimeEntitlementDenialDetails(
		d.ProviderID,
		d.ReasonCode,
		d.RequiredFeatures,
		d.MissingFeatures,
	)
	if d.RefusalClass != "" {
		details["refusal_class"] = d.RefusalClass
	} else if d.Denied {
		details["refusal_class"] = EntitlementRefusalClassEntitlement
	}
	return details
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

// EvaluateManagedRuntimeOfferingEntitlement checks offering-tier entitlement
// for managed-runtime offerings. Standard relies on base/provider entitlement
// (EvaluateManagedRuntimeEntitlement); Premium and Large additionally require a
// verified signed Edge "cloud.runtime.credits.ayn" entitlement on SaaS lanes.
func EvaluateManagedRuntimeOfferingEntitlement(
	ctx context.Context,
	lane DeploymentLane,
	providerID string,
	offeringID string,
) ManagedRuntimeEntitlementDecision {
	canonicalProviderID, err := providercatalog.CanonicalProviderID(providerID)
	if err != nil {
		decision := deniedManagedRuntimeEntitlement(providerID, EntitlementReasonProviderUnsupported, nil, nil)
		decision.RefusalClass = EntitlementRefusalClassProvider
		return decision
	}
	offering, err := ResolveOffering(offeringID)
	if err != nil {
		decision := deniedManagedRuntimeEntitlement(canonicalProviderID, EntitlementReasonOfferingUnsupported, nil, nil)
		decision.RefusalClass = EntitlementRefusalClassProvider
		return decision
	}
	if _, pkgErr := PackageForProvider(canonicalProviderID, string(offering.ID)); pkgErr != nil {
		decision := deniedManagedRuntimeEntitlement(canonicalProviderID, EntitlementReasonOfferingUnsupported, nil, nil)
		decision.RefusalClass = EntitlementRefusalClassProvider
		return decision
	}
	normalizedLane := DeploymentLane(strings.ToLower(strings.TrimSpace(string(lane))))
	if normalizedLane != DeploymentLaneSaaS && normalizedLane != DeploymentLaneSelfHosted {
		return ManagedRuntimeEntitlementDecision{
			Denied:           true,
			Message:          "Deployment mode is unknown; managed runtime tier entitlement cannot be verified",
			ProviderID:       canonicalProviderID,
			ReasonCode:       EntitlementReasonDeploymentModeUnknown,
			RefusalClass:     EntitlementRefusalClassEntitlement,
			RequiredFeatures: slices.Clone(RequiredFeatureKeysForProvider(canonicalProviderID)),
		}
	}
	if normalizedLane == DeploymentLaneSelfHosted || LocalManagedRuntimeE2EAllowed() {
		return ManagedRuntimeEntitlementDecision{ProviderID: canonicalProviderID}
	}
	required := slices.Clone(RequiredFeatureKeysForProvider(canonicalProviderID))
	if offering.ID == serverruntime.RuntimeOfferingStandard {
		return ManagedRuntimeEntitlementDecision{ProviderID: canonicalProviderID, RequiredFeatures: required}
	}
	required = append(required, EntitlementKeyAllYouNeedCredits)
	grants, ok := middleware.SignedEntitlementsFromContext(ctx)
	if !ok || !grants.Has(EntitlementKeyAllYouNeedCredits) {
		return ManagedRuntimeEntitlementDecision{
			Denied:           true,
			Message:          "This managed runtime tier requires the All You Need plan",
			ProviderID:       canonicalProviderID,
			ReasonCode:       EntitlementReasonOfferingTierRequired,
			RefusalClass:     EntitlementRefusalClassEntitlement,
			RequiredFeatures: required,
			MissingFeatures:  []string{EntitlementKeyAllYouNeedCredits},
		}
	}
	return ManagedRuntimeEntitlementDecision{ProviderID: canonicalProviderID, RequiredFeatures: required}
}

// AllowedOfferingsForRequest returns the offering IDs eligible for the request
// lane, provider (if specified), and verified Edge offering-tier grants.
func AllowedOfferingsForRequest(ctx context.Context, lane DeploymentLane, providerID string) []string {
	normalizedLane := DeploymentLane(strings.ToLower(strings.TrimSpace(string(lane))))
	if normalizedLane != DeploymentLaneSaaS && normalizedLane != DeploymentLaneSelfHosted {
		return []string{}
	}
	canonicalProviderID := ""
	if strings.TrimSpace(providerID) != "" {
		var err error
		canonicalProviderID, err = providercatalog.CanonicalProviderID(providerID)
		if err != nil {
			return []string{}
		}
	}
	allowed := make([]string, 0, len(Catalog()))
	for _, offering := range Catalog() {
		if canonicalProviderID != "" {
			if decision := EvaluateManagedRuntimeOfferingEntitlement(ctx, normalizedLane, canonicalProviderID, string(offering.ID)); !decision.Denied {
				allowed = append(allowed, string(offering.ID))
			}
			continue
		}
		if normalizedLane == DeploymentLaneSelfHosted ||
			LocalManagedRuntimeE2EAllowed() ||
			offering.ID == serverruntime.RuntimeOfferingStandard {
			allowed = append(allowed, string(offering.ID))
			continue
		}
		if grants, ok := middleware.SignedEntitlementsFromContext(ctx); ok && grants.Has(EntitlementKeyAllYouNeedCredits) {
			allowed = append(allowed, string(offering.ID))
		}
	}
	return allowed
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
