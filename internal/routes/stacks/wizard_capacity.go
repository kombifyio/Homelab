package stacks

import (
	"github.com/kombifyio/techstack/internal/runtimeproduct/serverruntime"
	"github.com/kombifyio/techstack/pkg/core"
	"github.com/kombifyio/techstack/pkg/specv2"
	"github.com/kombifyio/techstack/pkg/unifier"
)

// Only a fresh managed Cloud Kit has a known provider package and no running
// workload to preserve. Join, extend, own-server and multi-node plans keep their
// existing pinned admission. Desired intent stays on run.request and is saved
// unchanged; the release author receives only this runtime subset.
func initialManagedCapacityIntent(run *wizardRunState) (specv2.WizardIntent, error) {
	intent := run.request.Intent
	if run.request.Managed == nil || wizardRunTransport(run.request) != specv2.TransportKombifyCloud || intent.KitAssignment.KitSlug != specv2.KitSlugCloud || intent.Server.ExistingServerID != "" {
		return intent, nil
	}
	managed := run.request.Managed
	offering := managed.RuntimeOfferingID
	if offering == "" {
		offering = string(serverruntime.RuntimeOfferingStandard)
	}
	request := core.WizardRecommendationRequest{Goals: intent.Goals, KitSlug: intent.KitAssignment.KitSlug, DeploymentLane: "saas", ProviderID: managed.ProviderID, RuntimeOfferingID: offering, ComputeProfile: "standard", UseCaseSettings: intent.UseCaseSettings, UseCaseCapabilities: intent.UseCaseCapabilities}
	kept, failures, err := unifier.InitialCapacitySelection(request)
	if err != nil {
		return intent, err
	}
	run.capacityFailures = failures
	if len(failures) == 0 {
		return intent, nil
	}
	intent.Goals = kept
	intent.UseCaseSettings = capacitySelectedValues(intent.UseCaseSettings, kept)
	intent.UseCaseCapabilities = capacitySelectedValues(intent.UseCaseCapabilities, kept)
	intent.UseCaseSetupAnswers = capacitySelectedValues(intent.UseCaseSetupAnswers, kept)
	intent.UseCaseAccess = capacitySelectedValues(intent.UseCaseAccess, kept)
	return intent, nil
}

func capacitySelectedValues[T any](values map[string]T, goals []string) map[string]T {
	if values == nil {
		return nil
	}
	selected := map[string]T{}
	for _, goal := range goals {
		if value, ok := values[goal]; ok {
			selected[goal] = value
		}
	}
	return selected
}
