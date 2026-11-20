package unifier

import (
	"github.com/kombifyio/homelab/pkg/core"
	"testing"
)

func TestCapacityAdviceSizesTheManagedServer(t *testing.T) {
	missing := AssessWizardCapacity(core.WizardRecommendationRequest{Goals: []string{"files"}, DeploymentLane: "saas", ProviderID: "ionos"})
	if missing.Status != "unknown" || missing.AvailableRAMMB != 0 || missing.SuggestedOfferingID != "" {
		t.Fatalf("provider alone is not a selected host: %+v", missing)
	}
	missingProvider := AssessWizardCapacity(core.WizardRecommendationRequest{Goals: []string{"files"}, DeploymentLane: "saas", RuntimeOfferingID: "monthly-runtime-standard"})
	if missingProvider.Status != "unknown" || missingProvider.AvailableRAMMB != 0 {
		t.Fatalf("offering alone is not a provider package: %+v", missingProvider)
	}
	request := core.WizardRecommendationRequest{Goals: []string{"photos", "files", "vault"}, DeploymentLane: "saas", ProviderID: "ionos", RuntimeOfferingID: "monthly-runtime-standard", Surface: "easy"}
	result := AssessWizardCapacity(request)
	if result.AvailableRAMMB != 4096 {
		t.Fatalf("IONOS Standard advice used generic offering: %+v", result)
	}
	if result.Status != "warning" || result.RequiredRAMMB < 6144 {
		t.Fatalf("upstream Immich floor lost: %+v", result)
	}
	if result.SuggestedOfferingID != "monthly-runtime-premium" || result.SuggestedOfferingRAMMB != 8192 {
		t.Fatalf("advice must name the actual fitting IONOS package: %+v", result)
	}
}

func TestCapacityAdviceNamesTheUseCasesToLeaveOut(t *testing.T) {
	request := core.WizardRecommendationRequest{Goals: []string{"photos", "files", "vault"}, DeploymentLane: "saas", ProviderID: "ionos", RuntimeOfferingID: "monthly-runtime-standard"}
	result := AssessWizardCapacity(request)
	if len(result.OmitGoals) != 1 || result.OmitGoals[0] != "photos" {
		t.Fatalf("expected full Photos as the 4 GiB shortfall: %+v", result)
	}
	kept, failures, err := InitialCapacitySelection(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 2 || len(failures) != 1 || failures[0]["component"] != "use_case:photos" || failures[0]["reason_code"] != "capacity_memory" {
		t.Fatalf("independent initial selection = %v, %v", kept, failures)
	}
	if len(request.Goals) != 3 {
		t.Fatal("original desired selection mutated")
	}
}

func TestCapacityMemoryUsesIncrementsAndBackgroundLoads(t *testing.T) {
	goals := []goalMemory{{resident: .25, increment: 1, peakKnown: true, concurrent: true}, {resident: .25, increment: .75, peakKnown: true, concurrent: true}, {resident: .25, increment: .5, peakKnown: true, concurrent: true}, {resident: .25, increment: .5, peakKnown: true}}
	need, resident, working, known := memoryNeed(goals, 2, 2)
	if !known || resident != 3072 || working != 2304 || need != 5376 {
		t.Fatalf("resident + top two interactive increments + background = %d/%d/%d/%v", need, resident, working, known)
	}
	goals[0].peakKnown = false
	_, _, _, known = memoryNeed(goals, 2, 2)
	if known {
		t.Fatal("unknown working peak became a fit")
	}
}

func TestCapacityUnknownPeakDoesNotSuggestFittingSize(t *testing.T) {
	request := core.WizardRecommendationRequest{Goals: []string{"files"}, DeploymentLane: "saas", ProviderID: "ionos", RuntimeOfferingID: "monthly-runtime-standard", UseCaseSettings: map[string]map[string]any{"files": {"backend": "nextcloud"}}}
	result := AssessWizardCapacity(request)
	if result.Status != "unknown" || result.SuggestedRAMMB != 0 || result.SuggestedOfferingID != "" || len(result.OmitGoals) != 0 || result.WorkingIncrementRAMMB != nil {
		t.Fatalf("missing released peak must not prove a fitting size: %+v", result)
	}
	found := false
	for _, reason := range result.Reasons {
		found = found || reason.Code == "capacity_peak_unknown"
	}
	if !found {
		t.Fatalf("missing released peak must be explicit: %+v", result)
	}
}

func TestCapacityRepresentativeEverydayTrioOnIONOSFourGiB(t *testing.T) {
	request := core.WizardRecommendationRequest{Goals: []string{"photos", "media", "smart-home"}, DeploymentLane: "saas", ProviderID: "ionos", RuntimeOfferingID: "monthly-runtime-standard", ComputeProfile: "standard", Surface: "easy"}
	advice := AssessWizardCapacity(request)
	if advice.Status != "warning" || advice.AvailableRAMMB != 4096 || advice.RequiredRAMMB != 6784 || advice.ResidentRAMMB != 5248 || advice.WorkingIncrementRAMMB == nil || *advice.WorkingIncrementRAMMB != 1536 {
		t.Fatalf("representative everyday standard selection capacity = %+v", advice)
	}
	if advice.SuggestedOfferingID != "monthly-runtime-premium" || advice.SuggestedOfferingRAMMB != 8192 || len(advice.OmitGoals) != 1 || advice.OmitGoals[0] != "photos" {
		t.Fatalf("representative early guidance = %+v", advice)
	}
	kept, failures, err := InitialCapacitySelection(request)
	if err != nil || len(kept) != 2 || len(failures) != 1 || failures[0]["component"] != "use_case:photos" {
		t.Fatalf("4 GiB runtime selection kept=%v failures=%v error=%v", kept, failures, err)
	}
	request.Goals = kept
	fits := AssessWizardCapacity(request)
	if fits.Status != "fits" || fits.RequiredRAMMB != 4096 {
		t.Fatalf("retained Media and Smart Home must fit the 4 GiB kit floor: %+v", fits)
	}
}
