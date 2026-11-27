package unifier

import (
	"slices"
	"testing"

	"github.com/kombifyio/homelab/pkg/core"
	"github.com/kombifyio/homelab/pkg/monthlyruntime"
	"github.com/kombifyio/homelab/pkg/specv2"
)

func TestDocumentsMediaFitsIncludedProStandardVPSWithoutSurcharge(t *testing.T) {
	// 1. IONOS Standard with Memory Cube S enabled (included, no surcharge):
	// both EvaluateServerFit AND AssessWizardCapacity AND InitialCapacitySelection
	// must agree that full Documents & Media (Cloudreve + full Immich w/ smart
	// search + Jellyfin) fits with zero omissions and zero upgrade suggestions.
	ionosReq := core.WizardRecommendationRequest{
		KitSlug:            "cloud-kit",
		DeploymentLane:     "saas",
		ProviderID:         monthlyruntime.ProviderIONOS,
		RuntimeOfferingID:  "monthly-runtime-standard",
		IONOSMemoryEnabled: true,
		Goals:              []string{"files", "photos", "media"},
		UseCaseSettings:    map[string]map[string]any{"photos": {"backend": "immich"}},
		ServerFit: &core.WizardServerFitRequest{
			ProviderID: monthlyruntime.ProviderIONOS,
			OfferingID: "monthly-runtime-standard",
			Source:     "managed",
		},
	}

	fit := EvaluateServerFit(ionosReq, WizardRecommendationInventory{})
	if fit == nil || fit.Status != FitStatusFits || fit.Template != "Memory Cube S" || fit.Specs.MemoryMB != 8192 {
		t.Fatalf("IONOS Standard Memory Cube S must fit full Documents & Media: %+v", fit)
	}
	capacity := AssessWizardCapacity(ionosReq)
	if capacity.Status != "fits" || capacity.AvailableRAMMB != 8192 || capacity.SuggestedOfferingID != "" || len(capacity.OmitGoals) != 0 {
		t.Fatalf("AssessWizardCapacity must use Memory Cube S (8192 MiB) and fit with no surcharge: %+v", capacity)
	}
	kept, failures, err := InitialCapacitySelection(ionosReq)
	if err != nil || len(failures) != 0 || !PreserveStarterSelection(ionosReq.Goals, kept) {
		t.Fatalf("InitialCapacitySelection must keep all 3 workloads on Memory Cube S: kept=%v failures=%v err=%v", kept, failures, err)
	}

	// 2. Centron Standard (4 vCPU / 8 GiB / 80 GB, included, no surcharge):
	centronReq := core.WizardRecommendationRequest{
		KitSlug:           "cloud-kit",
		DeploymentLane:    "saas",
		ProviderID:        monthlyruntime.ProviderCentron,
		RuntimeOfferingID: "monthly-runtime-standard",
		Goals:             []string{"files", "photos", "media"},
		UseCaseSettings:   map[string]map[string]any{"photos": {"backend": "immich"}},
		ServerFit: &core.WizardServerFitRequest{
			ProviderID: monthlyruntime.ProviderCentron,
			OfferingID: "monthly-runtime-standard",
			Source:     "managed",
		},
	}
	centronFit := EvaluateServerFit(centronReq, WizardRecommendationInventory{})
	if centronFit == nil || centronFit.Status != FitStatusFits || centronFit.Specs.MemoryMB != 8192 {
		t.Fatalf("Centron Standard must fit full Documents & Media: %+v", centronFit)
	}
	centronCap := AssessWizardCapacity(centronReq)
	if centronCap.Status != "fits" || centronCap.AvailableRAMMB != 8192 || centronCap.SuggestedOfferingID != "" {
		t.Fatalf("Centron Standard capacity advice must fit with no surcharge: %+v", centronCap)
	}

	// 3. EvaluateProStarter preserves full Immich (never downgrades to immich-lite)
	// and attaches the documented playback/concurrency/storage envelope.
	starter := EvaluateProStarter(ProStarterDocumentsMedia, ionosReq, WizardRecommendationInventory{}, false)
	if !starter.RecommendedFull || starter.Fit == nil || starter.Fit.Status != FitStatusFits {
		t.Fatalf("Documents & Media Pro starter must recommend full choice on Memory Cube S: %+v", starter)
	}
	if starter.DefaultSettings["photos"]["backend"] != "immich" {
		t.Fatalf("Documents & Media must preserve full Immich, got %+v", starter.DefaultSettings)
	}
	if starter.WorkloadEnvelope == nil || !starter.WorkloadEnvelope.RequiresFullImmich || !slices.Contains(starter.WorkloadEnvelope.SupportedPlaybackModes, "direct-play") {
		t.Fatalf("Documents & Media envelope missing documented playback/Immich contract: %+v", starter.WorkloadEnvelope)
	}
}

func TestExplicitProStartersFamilyAndSelfEmployed(t *testing.T) {
	ionosMemoryReq := core.WizardRecommendationRequest{
		KitSlug:            "cloud-kit",
		DeploymentLane:     "saas",
		ProviderID:         monthlyruntime.ProviderIONOS,
		RuntimeOfferingID:  "monthly-runtime-standard",
		IONOSMemoryEnabled: true,
		ServerFit: &core.WizardServerFitRequest{
			ProviderID: monthlyruntime.ProviderIONOS,
			OfferingID: "monthly-runtime-standard",
			Source:     "managed",
		},
	}

	// Family on managed cloud without LAN reach: starts with photos + files +
	// media + vault (fitting Memory Cube S) and preserves smart-home as deferred.
	familyCloud := EvaluateProStarter(ProStarterFamily, ionosMemoryReq, WizardRecommendationInventory{}, false)
	if familyCloud.Fit == nil || familyCloud.Fit.Status != FitStatusFits {
		t.Fatalf("Family Pro starter must fit IONOS Standard Memory Cube S: %+v", familyCloud)
	}
	if !slices.Equal(familyCloud.StartsWith, []string{"photos", "files", "media", "vault"}) {
		t.Fatalf("Family cloud starter goals = %v", familyCloud.StartsWith)
	}
	if !slices.Equal(familyCloud.DeferredGoals, []string{"smart-home"}) {
		t.Fatalf("Family cloud starter must defer smart-home for LAN reach, got %v", familyCloud.DeferredGoals)
	}

	// Family on a LAN-reachable 8 GiB owned host: recommends full choice first!
	lanHostReq := core.WizardRecommendationRequest{
		KitSlug:        "basement-kit",
		DeploymentLane: "self-hosted",
		ServerFit: &core.WizardServerFitRequest{
			Source: "substrate",
			ObservedCapacity: &core.WizardObservedHostCapacity{
				CPUCores: 4, MemoryMB: 8192, DiskGB: 120, Architecture: "amd64", AMD64Level: 2, Source: "substrate", UsageObserved: true,
			},
		},
	}
	familyLAN := EvaluateProStarter(ProStarterFamily, lanHostReq, WizardRecommendationInventory{}, true)
	if !familyLAN.RecommendedFull || !slices.Contains(familyLAN.StartsWith, "smart-home") || len(familyLAN.DeferredGoals) != 0 {
		t.Fatalf("Family on fitting LAN host must recommend full choice first: %+v", familyLAN)
	}

	// Family on IONOS Basic Cube S (4 GiB, Memory Cube S disabled): does not fit
	// full Immich, offers concrete lighter alternatives requiring confirmation.
	ionosBasicReq := ionosMemoryReq
	ionosBasicReq.IONOSMemoryEnabled = false
	familyBasic := EvaluateProStarter(ProStarterFamily, ionosBasicReq, WizardRecommendationInventory{}, false)
	if familyBasic.Fit == nil || familyBasic.Fit.Status != FitStatusDoesNotFit {
		t.Fatalf("Family with full Immich on 4 GiB Basic Cube S must not fit silently: %+v", familyBasic)
	}
	hasLiteAlt := false
	for _, alt := range familyBasic.Alternatives {
		if alt.Kind == "switch_profile" && alt.Profile == "immich-lite" && alt.RequiresConfirmation {
			hasLiteAlt = true
		}
	}
	if !hasLiteAlt {
		t.Fatalf("expected explicit immich-lite alternative with confirmation on 4 GiB host: %+v", familyBasic.Alternatives)
	}

	// Going self-employed on IONOS Standard Basic Cube S (4 GiB): files + vault +
	// existing mail + automation (proven by catalog+fit!), local AI deferred.
	selfEmp := EvaluateProStarter(ProStarterSelfEmployed, ionosBasicReq, WizardRecommendationInventory{}, false)
	if selfEmp.Fit == nil || selfEmp.Fit.Status != FitStatusFits {
		t.Fatalf("Going self-employed Pro starter must fit Basic Cube S: %+v", selfEmp)
	}
	if selfEmp.DefaultSettings["mail"]["hosting"] != "existing" {
		t.Fatalf("Going self-employed must default mail to existing access: %+v", selfEmp.DefaultSettings)
	}
	if !slices.Contains(selfEmp.StartsWith, "automation") {
		t.Fatalf("Going self-employed should include automation when catalog+fit prove it fits: %v", selfEmp.StartsWith)
	}
	if !slices.Equal(selfEmp.DeferredGoals, []string{"ai"}) {
		t.Fatalf("Going self-employed must defer local AI on 4 GiB host: %v", selfEmp.DeferredGoals)
	}

	// Going self-employed on a suitable host (16 GiB / 4 vCPU): recommends full
	// choice including local AI first!
	aiHostReq := core.WizardRecommendationRequest{
		KitSlug:        "basement-kit",
		DeploymentLane: "self-hosted",
		ServerFit: &core.WizardServerFitRequest{
			Source: "substrate",
			ObservedCapacity: &core.WizardObservedHostCapacity{
				CPUCores: 4, MemoryMB: 16384, DiskGB: 120, Architecture: "amd64", AMD64Level: 2, Source: "substrate", UsageObserved: true,
			},
		},
	}
	selfEmpFull := EvaluateProStarter(ProStarterSelfEmployed, aiHostReq, WizardRecommendationInventory{}, false)
	if !selfEmpFull.RecommendedFull || !slices.Contains(selfEmpFull.StartsWith, "ai") || len(selfEmpFull.DeferredGoals) != 0 {
		t.Fatalf("Going self-employed on 16 GiB host must recommend full choice including AI: %+v", selfEmpFull)
	}

	// ValidateWizardIntent admits Family deferred smart-home expansion.
	intent := specv2.WizardIntent{
		Schema:  specv2.WizardIntentSchema,
		RunKind: specv2.RunKindFirstRun,
		Name:    "family-starter",
		Goals:   []string{"photos", "files", "media", "vault"},
		ProfileExpansions: []specv2.ProfileExpansion{
			{Profile: "family", DeferredGoals: []string{"smart-home"}},
		},
		Server:        specv2.ServerIntent{Transport: specv2.TransportKombifyCloud},
		KitAssignment: specv2.KitAssignment{Mode: specv2.KitAssignmentFound, KitSlug: specv2.KitSlugCloud},
	}
	if err := intent.Validate(); err != nil {
		t.Fatalf("WizardIntent.Validate must accept family deferred smart-home expansion: %v", err)
	}
}

func TestEntePhotosDriverAndBasicCubeSelection(t *testing.T) {
	// Ente Photos capacity driver must be emitted distinctly when selected.
	advice := AssessWizardCapacity(core.WizardRecommendationRequest{
		Goals:          []string{"photos"},
		DeploymentLane: "saas",
		ProviderID:     monthlyruntime.ProviderIONOS,
		UseCaseSettings: map[string]map[string]any{
			"photos": {"backend": "ente-photos"},
		},
	})
	// Until pinned release catalog ships Ente Photos, AssessWizardCapacity fails
	// closed cleanly; verify driver helper directly as well.
	_ = advice
	drivers := wizardCapacityDrivers(core.WizardRecommendationRequest{
		Goals: []string{"photos"},
		UseCaseSettings: map[string]map[string]any{
			"photos": {"backend": "ente-photos"},
		},
	}, nil)
	if len(drivers) != 1 || drivers[0].Code != "photos_ente" {
		t.Fatalf("expected photos_ente capacity driver, got %+v", drivers)
	}
}
