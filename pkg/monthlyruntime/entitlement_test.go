package monthlyruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/kombifyio/homelab/internal/gocommon/identity"
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
