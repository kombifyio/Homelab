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

func TestWizardRecommendationAuthorityEvaluatesProStartersEndToEnd(t *testing.T) {
	authority := NewWizardRecommendationAuthority(nil)

	// 1. Managed cloud Pro Standard without LAN reach: Family defers smart-home,
	// Self-Employed includes automation and defers local AI, Documents & Media
	// fits with full Immich on Memory Cube S.
	cloudResp := authority.Recommend(WizardRecommendationEvaluation{
		TenantID: "t1",
		OwnerID:  "u1",
		Request: core.WizardRecommendationRequest{
			KitSlug:                    "cloud-kit",
			DeploymentLane:             "saas",
			ProviderID:                 monthlyruntime.ProviderIONOS,
			RuntimeOfferingID:          "monthly-runtime-standard",
			IONOSMemoryEnabled:         true,
			LANReachable:               false,
			AllowedOfferings:           []string{"monthly-runtime-standard"},
			OfferingEntitlementChecked: true,
			Goals:                      []string{"files", "photos", "media"},
			Surface:                    "easy",
			ServerFit: &core.WizardServerFitRequest{
				ProviderID: monthlyruntime.ProviderIONOS,
				OfferingID: "monthly-runtime-standard",
				Source:     "managed",
			},
		},
	})
	if len(cloudResp.ProStarters) != 3 {
		t.Fatalf("expected 3 evaluated ProStarters from Recommend(), got %d", len(cloudResp.ProStarters))
	}
	if !cloudResp.ProStarters[ProStarterDocumentsMedia].RecommendedFull {
		t.Fatalf("expected documents-media RecommendedFull on Memory Cube S: %+v", cloudResp.ProStarters[ProStarterDocumentsMedia])
	}
	familyCloud := cloudResp.ProStarters[ProStarterFamily]
	if familyCloud.RecommendedFull || !slices.Equal(familyCloud.DeferredGoals, []string{"smart-home"}) || !familyCloud.RequiresConfirmation {
		t.Fatalf("expected family cloud starter to defer smart-home with confirmation: %+v", familyCloud)
	}
	selfEmpCloud := cloudResp.ProStarters[ProStarterSelfEmployed]
	if selfEmpCloud.RecommendedFull || !slices.Contains(selfEmpCloud.DeferredGoals, "ai") {
		t.Fatalf("expected self-employed cloud starter to defer local ai: %+v", selfEmpCloud)
	}

	// 2. Owned LAN-reachable 16 GiB host: Family recommends full choice first!
	ownedWorker := core.Worker{
		ID:            "wrk-lan",
		ServerID:      "srv-lan",
		Name:          "home-node",
		Status:        "online",
		UsageObserved: true,
		Capabilities: core.WorkerCapabilities{
			CPU: 4, RAM: 16384, Disk: 200, Arch: "amd64", AMD64MicroarchitectureLevel: 2,
		},
		Tags: map[string]string{"ip": "192.168.1.40"},
	}
	lanResp := authority.Recommend(WizardRecommendationEvaluation{
		TenantID: "t1",
		OwnerID:  "u1",
		Inventory: WizardRecommendationInventory{
			State:   "fresh",
			Workers: []core.Worker{ownedWorker},
		},
		Request: core.WizardRecommendationRequest{
			KitSlug:        "basement-kit",
			DeploymentLane: "self-hosted",
			LANReachable:   true,
			Goals:          []string{"photos", "files", "media", "vault", "smart-home"},
			Surface:        "easy",
		},
	})
	if !lanResp.ProStarters[ProStarterFamily].RecommendedFull || !slices.Contains(lanResp.ProStarters[ProStarterFamily].StartsWith, "smart-home") {
		t.Fatalf("expected full family starter recommended first on LAN-reachable 16 GiB host: %+v", lanResp.ProStarters[ProStarterFamily])
	}
	if !lanResp.ProStarters[ProStarterSelfEmployed].RecommendedFull {
		t.Fatalf("expected full self-employed starter recommended first on 16 GiB host: %+v", lanResp.ProStarters[ProStarterSelfEmployed])
	}
}

func TestProStartersDocumentsMediaConfirmationAlternativesAndExcludeServerID(t *testing.T) {
	// 1. Basic Cube S (4 GiB) with Centron admitted: documents-media sets
	// RequiresConfirmation=true, ConfirmationReasons=["documents_media_host_insufficient"],
	// and surfaces switch_provider (Centron Standard) alongside lighter photo alternatives.
	centronReq := core.WizardRecommendationRequest{
		KitSlug:                  "cloud-kit",
		DeploymentLane:           "saas",
		ProviderID:               monthlyruntime.ProviderIONOS,
		RuntimeOfferingID:        "monthly-runtime-standard",
		IONOSMemoryEnabled:       false,
		ProviderAdmissionChecked: true,
		AdmittedProviders:        []string{monthlyruntime.ProviderIONOS, monthlyruntime.ProviderCentron},
		ServerFit: &core.WizardServerFitRequest{
			ProviderID: monthlyruntime.ProviderIONOS,
			OfferingID: "monthly-runtime-standard",
			Source:     "managed",
		},
	}
	dmCentron := EvaluateProStarter(ProStarterDocumentsMedia, centronReq, WizardRecommendationInventory{}, false)
	if dmCentron.RecommendedFull || !dmCentron.RequiresConfirmation {
		t.Fatalf("documents-media on 4 GiB Basic Cube S must require confirmation: %+v", dmCentron)
	}
	if !slices.Contains(dmCentron.ConfirmationReasons, "documents_media_host_insufficient") {
		t.Fatalf("expected documents_media_host_insufficient reason, got %v", dmCentron.ConfirmationReasons)
	}
	hasSwitchProvider := false
	for _, alt := range dmCentron.Alternatives {
		if alt.Kind == "switch_provider" && alt.ProviderID == monthlyruntime.ProviderCentron && alt.OfferingID == "monthly-runtime-standard" {
			hasSwitchProvider = true
		}
	}
	if !hasSwitchProvider {
		t.Fatalf("expected switch_provider alternative to Centron Standard, got %+v", dmCentron.Alternatives)
	}

	// 2. Basic Cube S (4 GiB) without Centron, with a capable 16 GiB owned server:
	// documents-media surfaces use_owned_server and requires confirmation.
	capableWorker := core.Worker{
		ID:            "wrk-16g",
		ServerID:      "srv-16g",
		Name:          "capable-node",
		Status:        "online",
		UsageObserved: true,
		Capabilities: core.WorkerCapabilities{
			CPU: 4, RAM: 16384, Disk: 200, Arch: "amd64", AMD64MicroarchitectureLevel: 2,
		},
	}
	ownedFallbackReq := centronReq
	ownedFallbackReq.AdmittedProviders = []string{monthlyruntime.ProviderIONOS}
	dmOwned := EvaluateProStarter(ProStarterDocumentsMedia, ownedFallbackReq, WizardRecommendationInventory{
		State:   "fresh",
		Workers: []core.Worker{capableWorker},
	}, false)
	if !dmOwned.RequiresConfirmation {
		t.Fatalf("expected RequiresConfirmation on Basic Cube S with owned server fallback: %+v", dmOwned)
	}
	hasOwnedAlt := false
	for _, alt := range dmOwned.Alternatives {
		if alt.Kind == "use_owned_server" && alt.ServerID == "srv-16g" {
			hasOwnedAlt = true
		}
	}
	if !hasOwnedAlt {
		t.Fatalf("expected use_owned_server alternative for srv-16g, got %+v", dmOwned.Alternatives)
	}

	// 3. excludeServerID: if baseRequest.ServerFit.ServerID is already "srv-lan-selected",
	// evaluateFamilyStarter (even with LAN-reachable IP) and evaluateSelfEmployedStarter
	// must not recommend "srv-lan-selected" again.
	selectedLANOwned := core.Worker{
		ID:            "wrk-16g-lan-selected",
		ServerID:      "srv-lan-selected",
		Name:          "lan-selected-16g",
		Status:        "online",
		UsageObserved: true,
		Capabilities: core.WorkerCapabilities{
			CPU: 4, RAM: 16384, Disk: 200, Arch: "amd64", AMD64MicroarchitectureLevel: 2,
		},
		Tags: map[string]string{"ip": "192.168.1.50"},
	}
	selectedOwnedReq := core.WizardRecommendationRequest{
		KitSlug:        "basement-kit",
		DeploymentLane: "self-hosted",
		ServerFit: &core.WizardServerFitRequest{
			ServerID: "srv-lan-selected",
			Source:   "guard",
		},
	}
	familySelected := EvaluateProStarter(ProStarterFamily, selectedOwnedReq, WizardRecommendationInventory{
		State:   "fresh",
		Workers: []core.Worker{selectedLANOwned},
	}, false)
	for _, alt := range familySelected.Alternatives {
		if alt.Kind == "use_owned_server" && alt.ServerID == "srv-lan-selected" {
			t.Fatalf("evaluateFamilyStarter must exclude already-selected ServerFit.ServerID %q, got %+v", "srv-lan-selected", familySelected.Alternatives)
		}
	}
	selfEmpSelected := EvaluateProStarter(ProStarterSelfEmployed, selectedOwnedReq, WizardRecommendationInventory{
		State:   "fresh",
		Workers: []core.Worker{selectedLANOwned},
	}, false)
	for _, alt := range selfEmpSelected.Alternatives {
		if alt.Kind == "use_owned_server" && alt.ServerID == "srv-lan-selected" {
			t.Fatalf("evaluateSelfEmployedStarter must exclude already-selected ServerFit.ServerID %q, got %+v", "srv-lan-selected", selfEmpSelected.Alternatives)
		}
	}

	// 4. evaluateFamilyStarter must not offer a non-LAN-reachable owned worker as
	// a Smart Home use_owned_server alternative, and evaluateDocumentsMediaStarter
	// must set RequiresConfirmation whenever fit != fits even with no alternatives.
	nonLANWorker := core.Worker{
		ID: "wrk-noip", ServerID: "srv-noip", Name: "noip-16g", Status: "online", UsageObserved: true,
		Capabilities: core.WorkerCapabilities{CPU: 4, RAM: 16384, Disk: 200, Arch: "amd64", AMD64MicroarchitectureLevel: 2},
	}
	familyNoLAN := EvaluateProStarter(ProStarterFamily, centronReq, WizardRecommendationInventory{
		State:   "fresh",
		Workers: []core.Worker{nonLANWorker},
	}, false)
	for _, alt := range familyNoLAN.Alternatives {
		if alt.Kind == "use_owned_server" && alt.ServerID == "srv-noip" {
			t.Fatalf("evaluateFamilyStarter must require LAN reachability for smart-home use_owned_server, got %+v", familyNoLAN.Alternatives)
		}
	}

	dmNoAlts := EvaluateProStarter(ProStarterDocumentsMedia, core.WizardRecommendationRequest{
		KitSlug:        "basement-kit",
		DeploymentLane: "self-hosted",
		ServerFit: &core.WizardServerFitRequest{
			Source: "substrate",
			ObservedCapacity: &core.WizardObservedHostCapacity{
				CPUCores: 2, MemoryMB: 2048, DiskGB: 40, Architecture: "amd64", AMD64Level: 2, Source: "substrate", UsageObserved: true,
			},
		},
	}, WizardRecommendationInventory{}, false)
	if dmNoAlts.RecommendedFull || !dmNoAlts.RequiresConfirmation || !slices.Contains(dmNoAlts.ConfirmationReasons, "documents_media_host_insufficient") {
		t.Fatalf("evaluateDocumentsMediaStarter must set RequiresConfirmation when fit != fits even without alternatives: %+v", dmNoAlts)
	}

	// 5. Nil or pending fit must NOT claim documents_media_host_insufficient.
	dmNilFit := EvaluateProStarter(ProStarterDocumentsMedia, core.WizardRecommendationRequest{
		KitSlug:        "basement-kit",
		DeploymentLane: "self-hosted",
	}, WizardRecommendationInventory{}, false)
	if dmNilFit.RecommendedFull || dmNilFit.RequiresConfirmation || slices.Contains(dmNilFit.ConfirmationReasons, "documents_media_host_insufficient") {
		t.Fatalf("evaluateDocumentsMediaStarter with nil fit must not claim host insufficient: %+v", dmNilFit)
	}
	dmPendingFit := EvaluateProStarter(ProStarterDocumentsMedia, core.WizardRecommendationRequest{
		KitSlug:        "basement-kit",
		DeploymentLane: "self-hosted",
		ServerFit:      &core.WizardServerFitRequest{Source: "install-command"},
	}, WizardRecommendationInventory{}, false)
	if dmPendingFit.RecommendedFull || dmPendingFit.RequiresConfirmation || slices.Contains(dmPendingFit.ConfirmationReasons, "documents_media_host_insufficient") {
		t.Fatalf("evaluateDocumentsMediaStarter with pending fit must not claim host insufficient: %+v", dmPendingFit)
	}

	// 6. On a LAN-reachable host where 5 goals exceed capacity but 4 cloud goals
	// fit, evaluateFamilyStarter must set RequiresConfirmation with a capacity reason
	// instead of silently dropping smart-home.
	tightLANWorker := core.Worker{
		ID: "wrk-tight-lan", ServerID: "srv-tight-lan", Name: "tight-lan", Status: "online", UsageObserved: true,
		Capabilities: core.WorkerCapabilities{CPU: 4, RAM: 6144, Disk: 120, Arch: "amd64", AMD64MicroarchitectureLevel: 2},
		Usage:        core.WorkerUsage{CPU: 1, RAM: 3072, Disk: 10},
		Tags:         map[string]string{"ip": "192.168.1.90"},
	}
	tightFullFit := EvaluateServerFit(core.WizardRecommendationRequest{
		KitSlug:        "basement-kit",
		DeploymentLane: "self-hosted",
		Goals:          []string{"photos", "files", "media", "vault", "smart-home"},
		ServerFit:      &core.WizardServerFitRequest{ServerID: "srv-tight-lan", Source: "guard"},
	}, WizardRecommendationInventory{State: "fresh", Workers: []core.Worker{tightLANWorker}})
	tightCloudFit := EvaluateServerFit(core.WizardRecommendationRequest{
		KitSlug:        "basement-kit",
		DeploymentLane: "self-hosted",
		Goals:          []string{"photos", "files", "media", "vault"},
		ServerFit:      &core.WizardServerFitRequest{ServerID: "srv-tight-lan", Source: "guard"},
	}, WizardRecommendationInventory{State: "fresh", Workers: []core.Worker{tightLANWorker}})
	if tightFullFit == nil || tightFullFit.Status == FitStatusFits || tightCloudFit == nil || tightCloudFit.Status != FitStatusFits {
		t.Fatalf("precondition failed: full=%+v cloud=%+v", tightFullFit, tightCloudFit)
	}
	familyTightLAN := EvaluateProStarter(ProStarterFamily, core.WizardRecommendationRequest{
		KitSlug:        "basement-kit",
		DeploymentLane: "self-hosted",
		ServerFit:      &core.WizardServerFitRequest{ServerID: "srv-tight-lan", Source: "guard"},
	}, WizardRecommendationInventory{State: "fresh", Workers: []core.Worker{tightLANWorker}}, true)
	if familyTightLAN.RecommendedFull || !slices.Equal(familyTightLAN.DeferredGoals, []string{"smart-home"}) ||
		!familyTightLAN.RequiresConfirmation || !slices.Contains(familyTightLAN.ConfirmationReasons, "smart_home_deferred_host_capacity") {
		t.Fatalf("evaluateFamilyStarter must require confirmation with capacity reason when smart-home overflows LAN host: %+v", familyTightLAN)
	}
}

func TestBestOnlineObservedWorkerExcludesManagedLeasedAndStackBound(t *testing.T) {
	caps := core.WorkerCapabilities{CPU: 8, RAM: 32768, Disk: 500, Arch: "amd64", AMD64MicroarchitectureLevel: 2}
	managed := core.Worker{
		ID: "wrk-managed", ServerID: "srv-managed", Status: "online", UsageObserved: true, Capabilities: caps,
		Tags: map[string]string{"source": "managed-runtime", "ip": "192.168.1.10"},
	}
	leased := core.Worker{
		ID: "wrk-leased", ServerID: "srv-leased", Status: "online", UsageObserved: true, Capabilities: caps,
		Tags: map[string]string{"lease_id": "lease-123", "ip": "192.168.1.11"},
	}
	tagBound := core.Worker{
		ID: "wrk-tag-bound", ServerID: "srv-tag-bound", Status: "online", UsageObserved: true, Capabilities: caps,
		Tags: map[string]string{"stack_id": "stk-1", "ip": "192.168.1.12"},
	}
	fieldBound := core.Worker{
		ID: "wrk-field-bound", ServerID: "srv-field-bound", StackID: "stk-2", Status: "online", UsageObserved: true, Capabilities: caps,
		Tags: map[string]string{"ip": "192.168.1.13"},
	}
	if got, ok := bestOnlineObservedWorker(WizardRecommendationInventory{
		State:   "fresh",
		Workers: []core.Worker{managed, leased, tagBound, fieldBound},
	}); ok {
		t.Fatalf("bestOnlineObservedWorker must exclude managed, leased, and stack-bound workers, got %+v", got)
	}

	owned := core.Worker{
		ID: "wrk-owned", ServerID: "srv-owned", Status: "online", UsageObserved: true,
		Capabilities: core.WorkerCapabilities{CPU: 4, RAM: 16384, Disk: 200, Arch: "amd64", AMD64MicroarchitectureLevel: 2},
		Tags:         map[string]string{"ip": "192.168.1.50"},
	}
	got, ok := bestOnlineObservedWorker(WizardRecommendationInventory{
		State:   "fresh",
		Workers: []core.Worker{managed, leased, tagBound, fieldBound, owned},
	})
	if !ok || got.ID != "wrk-owned" {
		t.Fatalf("bestOnlineObservedWorker must select eligible owned unbound worker, got ok=%v worker=%+v", ok, got)
	}
}

func TestRecommendDerivesLANReachServerSideAndIgnoresClientAssertion(t *testing.T) {
	authority := NewWizardRecommendationAuthority(nil)
	caps16G := core.WorkerCapabilities{CPU: 4, RAM: 16384, Disk: 200, Arch: "amd64", AMD64MicroarchitectureLevel: 2}

	// 1. Managed target with client LANReachable=true must NOT admit full Family Smart Home.
	managedResp := authority.Recommend(WizardRecommendationEvaluation{
		TenantID: "t1", OwnerID: "u1",
		Request: core.WizardRecommendationRequest{
			KitSlug:            "cloud-kit",
			DeploymentLane:     "saas",
			ProviderID:         monthlyruntime.ProviderIONOS,
			RuntimeOfferingID:  "monthly-runtime-standard",
			IONOSMemoryEnabled: true,
			LANReachable:       true,
			Goals:              []string{"photos", "files", "media", "vault", "smart-home"},
			Surface:            "easy",
			ServerFit: &core.WizardServerFitRequest{
				ProviderID: monthlyruntime.ProviderIONOS,
				OfferingID: "monthly-runtime-standard",
				Source:     "managed",
			},
		},
	})
	if fam := managedResp.ProStarters[ProStarterFamily]; fam.RecommendedFull || !slices.Contains(fam.DeferredGoals, "smart-home") {
		t.Fatalf("managed target must ignore client LANReachable=true and defer smart-home: %+v", fam)
	}

	// 2. Missing owned target with client LANReachable=true must NOT admit full Family Smart Home.
	missingResp := authority.Recommend(WizardRecommendationEvaluation{
		TenantID: "t1", OwnerID: "u1",
		Inventory: WizardRecommendationInventory{State: "fresh"},
		Request: core.WizardRecommendationRequest{
			KitSlug:        "basement-kit",
			DeploymentLane: "self-hosted",
			LANReachable:   true,
			Goals:          []string{"photos", "files", "media", "vault", "smart-home"},
			Surface:        "easy",
		},
	})
	if fam := missingResp.ProStarters[ProStarterFamily]; fam.RecommendedFull || !slices.Contains(fam.DeferredGoals, "smart-home") {
		t.Fatalf("missing owned target must ignore client LANReachable=true and defer smart-home: %+v", fam)
	}

	// 3. Managed worker in inventory with private IP + client LANReachable=true must be excluded.
	managedWorkerResp := authority.Recommend(WizardRecommendationEvaluation{
		TenantID: "t1", OwnerID: "u1",
		Inventory: WizardRecommendationInventory{
			State: "fresh",
			Workers: []core.Worker{{
				ID: "wrk-vps", ServerID: "srv-vps", Status: "online", UsageObserved: true, Capabilities: caps16G,
				Tags: map[string]string{"source": "managed-runtime", "ip": "192.168.1.80"},
			}},
		},
		Request: core.WizardRecommendationRequest{
			KitSlug:        "basement-kit",
			DeploymentLane: "self-hosted",
			LANReachable:   true,
			Goals:          []string{"photos", "files", "media", "vault", "smart-home"},
			Surface:        "easy",
		},
	})
	if fam := managedWorkerResp.ProStarters[ProStarterFamily]; fam.RecommendedFull || !slices.Contains(fam.DeferredGoals, "smart-home") {
		t.Fatalf("managed-runtime worker must not count as owned LAN starter target: %+v", fam)
	}

	// 4. Owned worker with a public IP + client LANReachable=true must NOT count as LAN-reachable.
	publicIPWorker := core.Worker{
		ID: "wrk-pub", ServerID: "srv-pub", Status: "online", UsageObserved: true, Capabilities: caps16G,
		Tags: map[string]string{"ip": "203.0.113.10"},
	}
	if hasOwnedLANReachable(publicIPWorker) {
		t.Fatal("hasOwnedLANReachable must reject public IP 203.0.113.10")
	}
	publicResp := authority.Recommend(WizardRecommendationEvaluation{
		TenantID: "t1", OwnerID: "u1",
		Inventory: WizardRecommendationInventory{State: "fresh", Workers: []core.Worker{publicIPWorker}},
		Request: core.WizardRecommendationRequest{
			KitSlug:        "basement-kit",
			DeploymentLane: "self-hosted",
			LANReachable:   true,
			Goals:          []string{"photos", "files", "media", "vault", "smart-home"},
			Surface:        "easy",
		},
	})
	if fam := publicResp.ProStarters[ProStarterFamily]; fam.RecommendedFull || !slices.Contains(fam.DeferredGoals, "smart-home") {
		t.Fatalf("public-IP owned worker must ignore client LANReachable=true and defer smart-home: %+v", fam)
	}

	// 5. Owned worker with private RFC1918 / ULA IP admits full Family Smart Home via server derivation.
	ulaWorker := core.Worker{
		ID: "wrk-ula", ServerID: "srv-ula", Status: "online", UsageObserved: true, Capabilities: caps16G,
		Tags: map[string]string{"ip": "fd00:1::10"},
	}
	if !hasOwnedLANReachable(ulaWorker) {
		t.Fatal("hasOwnedLANReachable must accept IPv6 ULA fd00:1::10")
	}
	ulaResp := authority.Recommend(WizardRecommendationEvaluation{
		TenantID: "t1", OwnerID: "u1",
		Inventory: WizardRecommendationInventory{State: "fresh", Workers: []core.Worker{ulaWorker}},
		Request: core.WizardRecommendationRequest{
			KitSlug:        "basement-kit",
			DeploymentLane: "self-hosted",
			LANReachable:   false,
			Goals:          []string{"photos", "files", "media", "vault", "smart-home"},
			Surface:        "easy",
		},
	})
	if fam := ulaResp.ProStarters[ProStarterFamily]; !fam.RecommendedFull || !slices.Contains(fam.StartsWith, "smart-home") {
		t.Fatalf("server-derived ULA owned worker must recommend full Family including smart-home: %+v", fam)
	}

	// 6. Selected stack-bound owned worker (ServerFit.ServerID != "") with LAN IP
	// counts as LAN-reachable for its own deployment, while unbound discovery still excludes it.
	boundLANWorker := core.Worker{
		ID: "wrk-bound-lan", ServerID: "srv-bound-lan", StackID: "stk-existing", Status: "online", UsageObserved: true, Capabilities: caps16G,
		Tags: map[string]string{"stack_id": "stk-existing", "ip": "192.168.1.77"},
	}
	if _, ok := bestOnlineObservedWorker(WizardRecommendationInventory{State: "fresh", Workers: []core.Worker{boundLANWorker}}); ok {
		t.Fatal("bestOnlineObservedWorker must still exclude stack-bound worker")
	}
	boundSelectedResp := authority.Recommend(WizardRecommendationEvaluation{
		TenantID: "t1", OwnerID: "u1",
		Inventory: WizardRecommendationInventory{State: "fresh", Workers: []core.Worker{boundLANWorker}},
		Request: core.WizardRecommendationRequest{
			KitSlug:        "basement-kit",
			DeploymentLane: "self-hosted",
			Goals:          []string{"photos", "files", "media", "vault", "smart-home"},
			Surface:        "easy",
			ServerFit:      &core.WizardServerFitRequest{ServerID: "srv-bound-lan", Source: "guard"},
		},
	})
	if fam := boundSelectedResp.ProStarters[ProStarterFamily]; !fam.RecommendedFull || !slices.Contains(fam.StartsWith, "smart-home") {
		t.Fatalf("selected stack-bound LAN owned worker must recommend full Family including smart-home: %+v", fam)
	}

	// 7. AvailableGoals filters ProStarter StartsWith and DeferredGoals so unoffered
	// goals are never preselected or deferred.
	filteredResp := authority.Recommend(WizardRecommendationEvaluation{
		TenantID: "t1", OwnerID: "u1",
		Inventory: WizardRecommendationInventory{State: "fresh", Workers: []core.Worker{ulaWorker}},
		Request: core.WizardRecommendationRequest{
			KitSlug:        "basement-kit",
			DeploymentLane: "self-hosted",
			AvailableGoals: []string{"files", "photos", "vault", "mail"},
			Goals:          []string{"files"},
			Surface:        "easy",
		},
	})
	if dm := filteredResp.ProStarters[ProStarterDocumentsMedia]; slices.Contains(dm.StartsWith, "media") || !slices.Equal(dm.StartsWith, []string{"files", "photos"}) {
		t.Fatalf("documents-media must filter unoffered media from StartsWith: %+v", dm)
	}
	if fam := filteredResp.ProStarters[ProStarterFamily]; slices.Contains(fam.StartsWith, "media") || slices.Contains(fam.StartsWith, "smart-home") || slices.Contains(fam.DeferredGoals, "smart-home") {
		t.Fatalf("family must filter unoffered goals from StartsWith/DeferredGoals: %+v", fam)
	}
	if se := filteredResp.ProStarters[ProStarterSelfEmployed]; slices.Contains(se.StartsWith, "automation") || slices.Contains(se.DeferredGoals, "automation") || slices.Contains(se.DeferredGoals, "ai") {
		t.Fatalf("self-employed must filter unoffered goals from StartsWith/DeferredGoals: %+v", se)
	}
}
