package monthlyruntime

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/kombifyio/homelab/internal/gocommon/identity"
	"github.com/kombifyio/homelab/internal/runtimeproduct/serverruntime"
	"github.com/kombifyio/homelab/pkg/config"
	"github.com/kombifyio/homelab/pkg/features"
	"github.com/kombifyio/homelab/pkg/middleware"
)

type managedRuntimeEntitlementChecker struct {
	enabled bool
	err     error
	calls   int
}

func (c *managedRuntimeEntitlementChecker) IsEnabled(context.Context, string, string) (bool, error) {
	c.calls++
	return c.enabled, c.err
}

// Billing is a registered sensitive boundary: every cost-bearing managed
// provider entry point must share these fail-closed outcomes.
func TestEvaluateManagedRuntimeEntitlement(t *testing.T) {
	t.Setenv("HOMELAB_ENV", "production")
	t.Setenv("HOMELAB_ALLOW_LOCAL_SIMULATION_GATE", "")
	t.Setenv("HOMELAB_ALLOW_LOCAL_MANAGED_RUNTIME_E2E", "")

	checkErr := errors.New("feature service unavailable")
	adminContext := identity.NewContext(context.Background(), &identity.Identity{
		UserID: "admin-1", Roles: []string{"global_admin"},
	})
	tests := []struct {
		name       string
		ctx        context.Context
		providerID string
		checker    *managedRuntimeEntitlementChecker
		wantDenied bool
		wantReason string
		wantNoCall bool
	}{
		{name: "allowed", ctx: context.Background(), providerID: ProviderIONOS, checker: &managedRuntimeEntitlementChecker{enabled: true}},
		{name: "missing checker", ctx: context.Background(), providerID: ProviderCentron, wantDenied: true, wantReason: EntitlementReasonCheckerUnavailable},
		{name: "checker error", ctx: context.Background(), providerID: ProviderCentron, checker: &managedRuntimeEntitlementChecker{err: checkErr}, wantDenied: true, wantReason: EntitlementReasonFeatureCheckFailed},
		{name: "disabled", ctx: context.Background(), providerID: ProviderCentron, checker: &managedRuntimeEntitlementChecker{}, wantDenied: true, wantReason: EntitlementReasonFeatureDisabled},
		{name: "unknown provider", ctx: context.Background(), providerID: "personal-ionos", checker: &managedRuntimeEntitlementChecker{enabled: true}, wantDenied: true, wantReason: EntitlementReasonProviderUnsupported, wantNoCall: true},
		{name: "noncanonical provider", ctx: context.Background(), providerID: "IONOS", checker: &managedRuntimeEntitlementChecker{enabled: true}, wantDenied: true, wantReason: EntitlementReasonProviderUnsupported, wantNoCall: true},
		{name: "platform role without subscription", ctx: adminContext, providerID: ProviderCentron, checker: &managedRuntimeEntitlementChecker{}, wantDenied: true, wantReason: EntitlementReasonFeatureDisabled},
		{name: "platform role with subscription", ctx: adminContext, providerID: ProviderIONOS, checker: &managedRuntimeEntitlementChecker{enabled: true}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var checker FeatureChecker
			if test.checker != nil {
				checker = test.checker
			}
			decision := EvaluateManagedRuntimeEntitlement(test.ctx, checker, "owner-1", test.providerID)
			if decision.Denied != test.wantDenied || decision.ReasonCode != test.wantReason {
				t.Fatalf("decision = %+v", decision)
			}
			if test.wantNoCall && test.checker != nil && test.checker.calls != 0 {
				t.Fatalf("feature checker was called before the policy decision: %d", test.checker.calls)
			}
			if test.providerID == "personal-ionos" {
				details := decision.Details()
				requiredFeatures, _ := details["required_features"].([]string)
				if details["provider_id"] != "personal-ionos" {
					t.Fatalf("unsupported-provider details = %+v", details)
				}
				for _, featureKey := range requiredFeatures {
					t.Errorf("unsupported provider advertised entitlement %q", featureKey)
				}
			}
		})
	}

	t.Setenv("HOMELAB_ENV", "development")
	t.Setenv("HOMELAB_ALLOW_LOCAL_SIMULATION_GATE", "true")
	t.Setenv("HOMELAB_ALLOW_LOCAL_MANAGED_RUNTIME_E2E", "true")
	if decision := EvaluateManagedRuntimeEntitlement(context.Background(), nil, "owner-1", ProviderCentron); decision.Denied {
		t.Fatalf("explicit local E2E path denied: %+v", decision)
	}
}

// A managed lease renews only on a committed projection that grants every key
// its provider requires (platform-2xnsxf); anything less never renews.
func TestEvaluateManagedRuntimeRenewal(t *testing.T) {
	t.Setenv("HOMELAB_ENV", "local")
	t.Setenv("HOMELAB_ALLOW_LOCAL_SIMULATION_GATE", "true")
	t.Setenv("HOMELAB_ALLOW_LOCAL_MANAGED_RUNTIME_E2E", "true")

	ionos := []string{"homelab.ril.read", FeatureHomelabManagedRuntime, FeatureHomelabManagedRuntimeCloudKit, FeatureHomelabManagedRuntimeIONOS}
	tests := []struct {
		name        string
		providerID  string
		granted     []string
		wantDenied  bool
		wantReason  string
		wantMissing []string
	}{
		{name: "ionos entitled", providerID: ProviderIONOS, granted: ionos},
		{name: "centron entitled", providerID: ProviderCentron, granted: []string{FeatureHomelabManagedRuntime, FeatureHomelabManagedRuntimeCloudKit, FeatureHomelabManagedRuntimeCentron}},
		{name: "other provider key only", providerID: ProviderCentron, granted: ionos, wantDenied: true, wantReason: EntitlementReasonFeatureDisabled, wantMissing: []string{FeatureHomelabManagedRuntimeCentron}},
		{name: "base key missing", providerID: ProviderIONOS, granted: []string{FeatureHomelabManagedRuntimeCloudKit, FeatureHomelabManagedRuntimeIONOS}, wantDenied: true, wantReason: EntitlementReasonFeatureDisabled, wantMissing: []string{FeatureHomelabManagedRuntime}},
		{name: "empty projection even under the local E2E exception", providerID: ProviderIONOS, wantDenied: true, wantReason: EntitlementReasonFeatureDisabled, wantMissing: []string{FeatureHomelabManagedRuntime, FeatureHomelabManagedRuntimeCloudKit, FeatureHomelabManagedRuntimeIONOS}},
		{name: "unknown provider", providerID: "personal-ionos", granted: ionos, wantDenied: true, wantReason: EntitlementReasonProviderUnsupported},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := EvaluateManagedRuntimeRenewal(test.providerID, test.granted)
			if decision.Denied != test.wantDenied || decision.ReasonCode != test.wantReason {
				t.Fatalf("decision = %+v", decision)
			}
			if len(decision.MissingFeatures) != len(test.wantMissing) {
				t.Fatalf("missing = %v, want %v", decision.MissingFeatures, test.wantMissing)
			}
			for i, key := range test.wantMissing {
				if decision.MissingFeatures[i] != key {
					t.Fatalf("missing = %v, want %v", decision.MissingFeatures, test.wantMissing)
				}
			}
		})
	}
}

func TestEvaluateManagedRuntimeOfferingEntitlement(t *testing.T) {
	t.Setenv("HOMELAB_ENV", "production")
	t.Setenv("HOMELAB_ALLOW_LOCAL_SIMULATION_GATE", "")
	t.Setenv("HOMELAB_ALLOW_LOCAL_MANAGED_RUNTIME_E2E", "")

	proCtx := middleware.WithSignedEntitlements(
		context.Background(),
		FeatureHomelabManagedRuntime,
		FeatureHomelabManagedRuntimeCloudKit,
		FeatureHomelabManagedRuntimeIONOS,
		"cloud.runtime.credits.pro",
	)
	aynCtx := middleware.WithSignedEntitlements(
		context.Background(),
		FeatureHomelabManagedRuntime,
		FeatureHomelabManagedRuntimeCloudKit,
		FeatureHomelabManagedRuntimeIONOS,
		EntitlementKeyAllYouNeedCredits,
	)

	// 1. SaaS without signed AYN credit denies premium/large fail-closed, allows standard.
	if d := EvaluateManagedRuntimeOfferingEntitlement(proCtx, "saas", ProviderIONOS, "monthly-runtime-standard"); d.Denied {
		t.Fatalf("pro standard offering denied unexpectedly: %+v", d)
	}
	for _, offering := range []string{"monthly-runtime-premium", "monthly-runtime-large"} {
		d := EvaluateManagedRuntimeOfferingEntitlement(proCtx, "saas", ProviderIONOS, offering)
		if !d.Denied || d.ReasonCode != EntitlementReasonOfferingTierRequired || d.RefusalClass != EntitlementRefusalClassEntitlement {
			t.Fatalf("pro user must fail closed on %s: %+v", offering, d)
		}
		if d.Details()["refusal_class"] != EntitlementRefusalClassEntitlement {
			t.Fatalf("decision.Details() must include refusal_class=%q: %+v", EntitlementRefusalClassEntitlement, d.Details())
		}
		if d := EvaluateManagedRuntimeOfferingEntitlement(context.Background(), "saas", ProviderIONOS, offering); !d.Denied || d.ReasonCode != EntitlementReasonOfferingTierRequired {
			t.Fatalf("unverified context must fail closed on %s: %+v", offering, d)
		}
		if d := EvaluateManagedRuntimeOfferingEntitlement(aynCtx, "saas", ProviderIONOS, offering); d.Denied {
			t.Fatalf("ayn context must allow %s: %+v", offering, d)
		}
	}

	// 2. Centron Large is unsupported by catalog regardless of AYN.
	if d := EvaluateManagedRuntimeOfferingEntitlement(aynCtx, "saas", ProviderCentron, "monthly-runtime-large"); !d.Denied || d.ReasonCode != EntitlementReasonOfferingUnsupported || d.RefusalClass != EntitlementRefusalClassProvider {
		t.Fatalf("centron large must fail closed as unsupported: %+v", d)
	}

	// 3. AllowedOfferingsForRequest filters to standard for Pro and all provider-supported offerings for AYN.
	if got := AllowedOfferingsForRequest(proCtx, "saas", ProviderIONOS); !slices.Equal(got, []string{"monthly-runtime-standard"}) {
		t.Fatalf("pro allowed offerings = %v, want [monthly-runtime-standard]", got)
	}
	if got := AllowedOfferingsForRequest(aynCtx, "saas", ProviderIONOS); !slices.Equal(got, []string{"monthly-runtime-standard", "monthly-runtime-premium", "monthly-runtime-large"}) {
		t.Fatalf("ayn allowed ionos offerings = %v", got)
	}
	if got := AllowedOfferingsForRequest(aynCtx, "saas", ProviderCentron); !slices.Equal(got, []string{"monthly-runtime-standard", "monthly-runtime-premium"}) {
		t.Fatalf("ayn allowed centron offerings = %v", got)
	}

	// 4. Empty or unknown deployment lane MUST fail closed even with signed AYN
	// or Standard offering, and DeploymentLaneFromMode maps zero-value mode to empty.
	if got := DeploymentLaneFromMode(config.DeploymentMode("")); got != "" {
		t.Fatalf("empty DeploymentMode must yield empty lane, got %q", got)
	}
	if got := DeploymentLaneFromMode(config.DeploymentMode("staging")); got != "" {
		t.Fatalf("unknown DeploymentMode must yield empty lane, got %q", got)
	}
	if got := DeploymentLaneFromMode(config.ModeSaaS); got != DeploymentLaneSaaS {
		t.Fatalf("ModeSaaS must yield saas lane, got %q", got)
	}
	if got := DeploymentLaneFromMode(config.ModeSelfHosted); got != DeploymentLaneSelfHosted {
		t.Fatalf("ModeSelfHosted must yield self-hosted lane, got %q", got)
	}
	for _, badLane := range []DeploymentLane{"", "unknown", DeploymentLaneFromMode(config.DeploymentMode(""))} {
		for _, offering := range []string{"monthly-runtime-standard", "monthly-runtime-premium", "monthly-runtime-large"} {
			d := EvaluateManagedRuntimeOfferingEntitlement(aynCtx, badLane, ProviderIONOS, offering)
			if !d.Denied || d.ReasonCode != EntitlementReasonDeploymentModeUnknown || d.RefusalClass != EntitlementRefusalClassEntitlement {
				t.Fatalf("lane %q must fail closed on %s: %+v", badLane, offering, d)
			}
		}
		if got := AllowedOfferingsForRequest(aynCtx, badLane, ProviderIONOS); len(got) != 0 {
			t.Fatalf("lane %q with provider must yield empty allowed offerings, got %v", badLane, got)
		}
		if got := AllowedOfferingsForRequest(aynCtx, badLane, ""); len(got) != 0 {
			t.Fatalf("lane %q without provider must yield empty allowed offerings, got %v", badLane, got)
		}
	}

	// 5. self-hosted lane and LocalManagedRuntimeE2EAllowed bypass tier restriction.
	if d := EvaluateManagedRuntimeOfferingEntitlement(context.Background(), "self-hosted", ProviderIONOS, "monthly-runtime-large"); d.Denied {
		t.Fatalf("self-hosted must remain open: %+v", d)
	}
	t.Setenv("HOMELAB_ENV", "local")
	t.Setenv("HOMELAB_ALLOW_LOCAL_SIMULATION_GATE", "true")
	t.Setenv("HOMELAB_ALLOW_LOCAL_MANAGED_RUNTIME_E2E", "true")
	if d := EvaluateManagedRuntimeOfferingEntitlement(context.Background(), "saas", ProviderIONOS, "monthly-runtime-large"); d.Denied {
		t.Fatalf("local E2E must remain open: %+v", d)
	}
	if d := EvaluateManagedRuntimeOfferingEntitlement(aynCtx, "", ProviderIONOS, "monthly-runtime-standard"); !d.Denied || d.ReasonCode != EntitlementReasonDeploymentModeUnknown {
		t.Fatalf("empty lane must still fail closed even when local E2E env is set: %+v", d)
	}
	unknownDecision := EvaluateManagedRuntimeOfferingEntitlement(aynCtx, "", ProviderIONOS, "monthly-runtime-standard")
	if len(unknownDecision.RequiredFeatures) == 0 {
		t.Fatalf("expected RequiredFeatures on deployment_mode_unknown decision, got %+v", unknownDecision)
	}
	originalFirst := RequiredFeatureKeysForProvider(ProviderIONOS)[0]
	unknownDecision.RequiredFeatures[0] = "mutated"
	if RequiredFeatureKeysForProvider(ProviderIONOS)[0] != originalFirst {
		t.Fatal("deployment_mode_unknown decision must clone RequiredFeatures instead of aliasing provider keys")
	}
}

func TestIONOSStandardPhotoMemoryFlagRemainsClosedByDefault(t *testing.T) {
	t.Setenv("HOMELAB_ENV", "production")
	decision := EvaluateManagedRuntimeEntitlement(context.Background(), &managedRuntimeEntitlementChecker{enabled: true}, "owner-1", ProviderIONOS)
	if decision.Denied {
		t.Fatalf("standard entitlement denied unexpectedly: %+v", decision)
	}
	svc, err := features.NewService(nil, features.ServiceConfig{})
	if err != nil {
		t.Fatalf("features.NewService: %v", err)
	}
	if flag, ok := svc.GetDefinition("ionos_standard_photo_memory"); !ok || flag.DefaultValue {
		t.Fatalf("ionos_standard_photo_memory flag = %+v (ok=%v), want DefaultValue=false", flag, ok)
	}
	pkg, err := PackageForProvider(ProviderIONOS, string(serverruntime.RuntimeOfferingStandard))
	if err != nil {
		t.Fatalf("PackageForProvider(ionos, standard): %v", err)
	}
	if pkg.SKU != "Basic Cube S" {
		t.Fatalf("unexpected standard SKU %q, want Basic Cube S", pkg.SKU)
	}
}
