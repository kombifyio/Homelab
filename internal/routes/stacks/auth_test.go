package stacks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kombifyio/homelab/internal/gocommon/identity"
	"github.com/kombifyio/homelab/pkg/config"
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/middleware"
	"github.com/kombifyio/homelab/pkg/monthlyruntime"
	"github.com/kombifyio/homelab/pkg/specv2"
)

func TestRequireStackAuthAcceptsSignedEdgeIdentity(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/v1/stacks", nil)
	req = req.WithContext(identity.NewContext(context.Background(), &identity.Identity{UserID: "api-key:homelab"}))
	e := &httpx.Event{
		Request:  req,
		Response: httptest.NewRecorder(),
	}

	got, err := requireStackAuth(e)
	if err != nil {
		t.Fatalf("requireStackAuth returned error: %v", err)
	}
	if got != "api-key:homelab" {
		t.Fatalf("requireStackAuth returned %q, want edge identity user ID", got)
	}
}

func TestRequireStackAuthRejectsMissingIdentity(t *testing.T) {
	rec := httptest.NewRecorder()
	e := &httpx.Event{
		Request:  httptest.NewRequest("GET", "/api/v1/stacks", nil),
		Response: rec,
	}

	got, err := requireStackAuth(e)
	if err == nil {
		t.Fatal("requireStackAuth must return an error once it has written the refusal")
	}
	if got != "" {
		t.Fatalf("requireStackAuth returned %q, want empty user ID", got)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("requireStackAuth status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestJobStatsRequiresAuthentication(t *testing.T) {
	rec := httptest.NewRecorder()
	e := &httpx.Event{
		Request:  httptest.NewRequest(http.MethodGet, "/api/v1/jobs/stats", nil),
		Response: rec,
	}

	if err := (crudRouteHandlers{}).jobStats(e); err == nil {
		t.Fatal("jobStats returned nil after rejecting an unauthenticated request")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("jobStats status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	authedRequest := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/stats", nil)
	authedRequest = authedRequest.WithContext(identity.NewContext(authedRequest.Context(), &identity.Identity{UserID: "owner-1"}))
	authedRecorder := httptest.NewRecorder()
	if err := (crudRouteHandlers{}).jobStats(&httpx.Event{Request: authedRequest, Response: authedRecorder}); err != nil {
		t.Fatalf("authenticated jobStats: %v", err)
	}
	if authedRecorder.Code != http.StatusOK {
		t.Fatalf("authenticated jobStats status = %d, want %d", authedRecorder.Code, http.StatusOK)
	}
}

// Regression (kombify-Techstack-7kf0): an operator who signed in directly
// through Auth0 has no projected kombify Cloud profile yet. The automatic cloud
// owner bootstrap must accept the email the Gateway signed as verified, and
// must still fail closed when the signed identity does not assert it.
func TestAutomaticCloudOwnerBootstrapUsesEdgeVerifiedEmail(t *testing.T) {
	autoCloudRequest := func() normalizedCreateStackRequest {
		return normalizedCreateStackRequest{
			Name:       "Direct Login Owner",
			Mode:       "easy",
			UserConfig: map[string]interface{}{"name": "direct-login-owner"},
			Options: map[string]interface{}{
				"owner_bootstrap_mode": ownerBootstrapModeAuto,
				"owner_source":         ownerSourceCloud,
			},
		}
	}
	eventWith := func(id *identity.Identity) *httpx.Event {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/stacks", nil)
		req = req.WithContext(identity.NewContext(context.Background(), id))
		return &httpx.Event{Request: req, Response: httptest.NewRecorder()}
	}

	verified := ownerBootstrapContextFromRequest(eventWith(&identity.Identity{
		UserID: "auth0|direct", Email: "Direct.Owner@example.com", EmailVerified: true,
	}))
	resolved, denial := resolveCreateOwnerBootstrap(autoCloudRequest(), verified)
	if denial != nil {
		t.Fatalf("verified edge email denied: %s (%s)", denial.Message, denial.ReasonCode)
	}
	bootstrap, ok := ownerBootstrapFromRequest(resolved)
	if !ok || bootstrap.Source != ownerSourceCloud || bootstrap.Email != "Direct.Owner@example.com" || bootstrap.Username != "direct-owner" {
		t.Fatalf("unexpected resolved bootstrap: %+v", bootstrap)
	}

	unverified := ownerBootstrapContextFromRequest(eventWith(&identity.Identity{
		UserID: "auth0|direct", Email: "Direct.Owner@example.com",
	}))
	if unverified.EdgeVerifiedEmail != "" {
		t.Fatalf("unverified identity produced edge verified email %q", unverified.EdgeVerifiedEmail)
	}
	if _, denial := resolveCreateOwnerBootstrap(autoCloudRequest(), unverified); denial == nil || denial.ReasonCode != reasonCloudProfileEmailMissing {
		t.Fatalf("unverified edge email: denial = %+v, want %s", denial, reasonCloudProfileEmailMissing)
	}

	// A projected Cloud profile keeps precedence over the edge identity.
	verified.CurrentProfile = &cloudLinkIdentity{Email: "profile@example.com", EmailVerified: true, DisplayName: "Profile"}
	resolved, denial = resolveCreateOwnerBootstrap(autoCloudRequest(), verified)
	if denial != nil {
		t.Fatalf("verified profile denied: %s", denial.Message)
	}
	if bootstrap, _ := ownerBootstrapFromRequest(resolved); bootstrap.Email != "profile@example.com" {
		t.Fatalf("profile email not preferred: %+v", bootstrap)
	}
}

type wizardRunFeatureMap map[string]bool

func (m wizardRunFeatureMap) IsEnabled(_ context.Context, key, _ string) (bool, error) {
	return m[key], nil
}

// Regression (kombify-StackKits-8l2z): resuming a managed wizard run must
// enforce offering-tier entitlement and server hardware fit against the
// persisted stack even when Managed is omitted or attempts to downgrade tier.
func TestWizardRunResumeEnforcesPersistedManagedEntitlement(t *testing.T) {
	t.Setenv("HOMELAB_ENV", "production")
	t.Setenv("HOMELAB_ALLOW_LOCAL_SIMULATION_GATE", "")
	t.Setenv("HOMELAB_ALLOW_LOCAL_MANAGED_RUNTIME_E2E", "")

	cloudSeed := func() map[string]any {
		seed := wizardRunTestSeed(specv2.KitSlugCloud)
		seed["routes"] = map[string]any{
			"photos": map[string]any{"exposure": "private", "serviceRef": "photos"},
		}
		return seed
	}

	newSaaSHandlers := func(store *controlplane.MemoryStore, features managedRuntimeFeatureChecker) wizardRunHandlers {
		h := newWizardRunTestHandlers(store, &wizardRunFakeValidator{})
		h.crud.deploymentMode = config.ModeSaaS
		h.cfg.DeploymentMode = config.ModeSaaS
		h.crud.runtimeFeatures = features
		h.cfg.Features = features
		h.cfg.Seeds = wizardRunFakeSeeds{seeds: map[string]map[string]any{
			specv2.KitSlugCloud: cloudSeed(),
		}}
		return h
	}

	baseIntent := func() specv2.WizardIntent {
		return specv2.WizardIntent{
			Schema:  specv2.WizardIntentSchema,
			RunKind: specv2.RunKindFirstRun,
			Name:    "my-homelab",
			Goals:   []string{"photos"},
			Server:  specv2.ServerIntent{Transport: specv2.TransportKombifyCloud, Roles: []string{"controller", "worker"}},
			KitAssignment: specv2.KitAssignment{
				Mode:    specv2.KitAssignmentFound,
				KitSlug: specv2.KitSlugCloud,
			},
		}
	}

	t.Run("omitted managed on persisted premium stack without AYN returns 403", func(t *testing.T) {
		store := controlplane.NewMemoryStore()
		features := wizardRunFeatureMap{
			monthlyruntime.FeatureHomelabManagedRuntime:         true,
			monthlyruntime.FeatureHomelabManagedRuntimeCloudKit: true,
			monthlyruntime.FeatureHomelabManagedRuntimeIONOS:    true,
			"server_hardware_fit":                               true,
		}
		h := newSaaSHandlers(store, features)
		key := "resume-premium-no-managed"
		stackID := deterministicStackID("tenant-1", "auth0|user-1", key)
		wizardRunSeedV2Stack(t, store, stackID, "auth0|user-1", map[string]any{
			"options": map[string]any{
				"provider_id":         "ionos",
				"runtime_offering_id": "monthly-runtime-premium",
			},
			"server_provisioning_mode": specv2.TransportKombifyCloud,
		})

		event, rec := wizardRunTestEvent(t, wizardRunRequest{Intent: baseIntent()}, key)
		_ = h.createWizardRun(event)
		_, details := decodeWizardRunError(t, rec)
		if rec.Code != http.StatusForbidden || details["reason_code"] != monthlyruntime.EntitlementReasonOfferingTierRequired {
			t.Fatalf("status=%d details=%v, want 403 %s", rec.Code, details, monthlyruntime.EntitlementReasonOfferingTierRequired)
		}
	})

	t.Run("conflicting managed offering or provider on resume returns 409", func(t *testing.T) {
		store := controlplane.NewMemoryStore()
		features := wizardRunFeatureMap{
			monthlyruntime.FeatureHomelabManagedRuntime:         true,
			monthlyruntime.FeatureHomelabManagedRuntimeCloudKit: true,
			monthlyruntime.FeatureHomelabManagedRuntimeIONOS:    true,
			"server_hardware_fit":                               true,
		}
		h := newSaaSHandlers(store, features)
		key := "resume-premium-downgrade"
		stackID := deterministicStackID("tenant-1", "auth0|user-1", key)
		wizardRunSeedV2Stack(t, store, stackID, "auth0|user-1", map[string]any{
			"provider_id":              "ionos",
			"runtime_offering_id":      "monthly-runtime-premium",
			"server_provisioning_mode": specv2.TransportKombifyCloud,
		})

		for _, managed := range []*wizardRunManagedParams{
			{ProviderID: "ionos", RuntimeOfferingID: "monthly-runtime-standard"},
			{ProviderID: "centron", RuntimeOfferingID: "monthly-runtime-premium"},
		} {
			event, rec := wizardRunTestEvent(t, wizardRunRequest{Intent: baseIntent(), Managed: managed}, key)
			event.Request = event.Request.WithContext(middleware.WithSignedEntitlements(
				event.Request.Context(),
				monthlyruntime.EntitlementKeyAllYouNeedCredits,
			))
			_ = h.createWizardRun(event)
			_, details := decodeWizardRunError(t, rec)
			if rec.Code != http.StatusConflict || details["reason_code"] != "wizard_resume_managed_conflict" {
				t.Fatalf("managed=%+v: status=%d details=%v, want 409 wizard_resume_managed_conflict", managed, rec.Code, details)
			}
		}
	})

	t.Run("omitted managed on persisted standard stack enforces EvaluateServerFit", func(t *testing.T) {
		store := controlplane.NewMemoryStore()
		features := wizardRunFeatureMap{
			monthlyruntime.FeatureHomelabManagedRuntime:         true,
			monthlyruntime.FeatureHomelabManagedRuntimeCloudKit: true,
			monthlyruntime.FeatureHomelabManagedRuntimeIONOS:    true,
			"server_hardware_fit":                               true,
			"ionos_standard_photo_memory":                       false,
		}
		h := newSaaSHandlers(store, features)
		key := "resume-standard-fit-check"
		stackID := deterministicStackID("tenant-1", "auth0|user-1", key)
		wizardRunSeedV2Stack(t, store, stackID, "auth0|user-1", map[string]any{
			"options": map[string]any{
				"provider_id":         "ionos",
				"runtime_offering_id": "monthly-runtime-standard",
			},
			"server_provisioning_mode": specv2.TransportKombifyCloud,
		})

		intent := baseIntent()
		intent.Goals = []string{"files", "photos", "media"}
		event, rec := wizardRunTestEvent(t, wizardRunRequest{Intent: intent}, key)
		_ = h.createWizardRun(event)
		_, details := decodeWizardRunError(t, rec)
		if rec.Code != http.StatusConflict || details["reason_code"] != "server_hardware_incompatible" {
			t.Fatalf("status=%d details=%v, want 409 server_hardware_incompatible", rec.Code, details)
		}
	})

	t.Run("disabled IONOS base managed runtime entitlement on resume returns 403", func(t *testing.T) {
		store := controlplane.NewMemoryStore()
		features := wizardRunFeatureMap{
			monthlyruntime.FeatureHomelabManagedRuntime:         true,
			monthlyruntime.FeatureHomelabManagedRuntimeCloudKit: true,
			monthlyruntime.FeatureHomelabManagedRuntimeIONOS:    false,
			"server_hardware_fit":                               true,
		}
		h := newSaaSHandlers(store, features)
		key := "resume-ionos-entitlement-revoked"
		stackID := deterministicStackID("tenant-1", "auth0|user-1", key)
		wizardRunSeedV2Stack(t, store, stackID, "auth0|user-1", map[string]any{
			"provider_id":              "ionos",
			"runtime_offering_id":      "monthly-runtime-standard",
			"server_provisioning_mode": specv2.TransportKombifyCloud,
		})

		event, rec := wizardRunTestEvent(t, wizardRunRequest{Intent: baseIntent()}, key)
		_ = h.createWizardRun(event)
		_, details := decodeWizardRunError(t, rec)
		if rec.Code != http.StatusForbidden || details["reason_code"] != monthlyruntime.EntitlementReasonFeatureDisabled {
			t.Fatalf("status=%d details=%v, want 403 %s", rec.Code, details, monthlyruntime.EntitlementReasonFeatureDisabled)
		}
		if details["refusal_class"] != monthlyruntime.EntitlementRefusalClassEntitlement {
			t.Fatalf("refusal_class=%v, want %s", details["refusal_class"], monthlyruntime.EntitlementRefusalClassEntitlement)
		}
	})

	t.Run("omitted managed on persisted premium stack with AYN passes tier and fit gates", func(t *testing.T) {
		store := controlplane.NewMemoryStore()
		features := wizardRunFeatureMap{
			monthlyruntime.FeatureHomelabManagedRuntime:         true,
			monthlyruntime.FeatureHomelabManagedRuntimeCloudKit: true,
			monthlyruntime.FeatureHomelabManagedRuntimeIONOS:    true,
			"server_hardware_fit":                               true,
		}
		h := newSaaSHandlers(store, features)
		key := "resume-premium-entitled"
		stackID := deterministicStackID("tenant-1", "auth0|user-1", key)
		wizardRunSeedV2Stack(t, store, stackID, "auth0|user-1", map[string]any{
			"provider_id":              "ionos",
			"runtime_offering_id":      "monthly-runtime-premium",
			"server_provisioning_mode": specv2.TransportKombifyCloud,
		})

		event, rec := wizardRunTestEvent(t, wizardRunRequest{Intent: baseIntent()}, key)
		event.Request = event.Request.WithContext(middleware.WithSignedEntitlements(
			event.Request.Context(),
			monthlyruntime.EntitlementKeyAllYouNeedCredits,
		))
		_ = h.createWizardRun(event)
		// Passes 400/403/409 entitlement & fit checks and proceeds to persistence/managed admission (503 since managedLeases is nil in test fixture).
		if rec.Code == http.StatusBadRequest || rec.Code == http.StatusForbidden || rec.Code == http.StatusConflict {
			t.Fatalf("entitled resume was unexpectedly refused: status=%d body=%s", rec.Code, rec.Body.String())
		}
	})
}
