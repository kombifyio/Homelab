package unifier

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/kombifyio/techstack/internal/stackkitrelease"
	"github.com/kombifyio/techstack/pkg/core"
	"github.com/kombifyio/techstack/pkg/logger"
	"github.com/kombifyio/techstack/pkg/monthlyruntime"
)

// The existing worker policy starts at 2048 MiB (analyze.go). Reserve that
// envelope for core + OS before adding applications. This is planning policy,
// not a measurement or a separate sum of container limits. Kit floors stay.
const capacityCoreOSMiB = 2048

// ErrInitialCapacityCore identifies a proven core floor shortfall, not unavailable advice.
var ErrInitialCapacityCore = errors.New("managed core memory floor exceeds provider package")

const CapacityModel = "resident-plus-concurrent-increments/v1"

type goalMemory struct {
	goal                       string
	resident, increment, floor float64
	peakKnown, concurrent      bool
}

// AssessWizardCapacity is soft advice from the released catalog. It neither
// attests a host nor changes any selection, provider size, entitlement or pin.
func AssessWizardCapacity(request core.WizardRecommendationRequest) *core.WizardCapacityAssessment {
	result := &core.WizardCapacityAssessment{Status: "unknown", Advisory: true, Model: CapacityModel,
		ConcurrentWorkloads: request.ConcurrentWorkloads, OmitGoals: []string{}, Reasons: []core.WizardRecommendationReason{}, PerUseCase: []core.WizardCapacityUseCase{}}
	if result.ConcurrentWorkloads == 0 {
		result.ConcurrentWorkloads = 2
	}
	if result.ConcurrentWorkloads < 1 || result.ConcurrentWorkloads > 2 {
		return result
	}
	host, err := wizardCapacityHost(request)
	if err != nil {
		logger.Default().Warn("wizard_capacity_host_unknown", "error", err)
		result.Reasons = append(result.Reasons, core.WizardRecommendationReason{Code: "capacity_host_unknown", Message: "The chosen server size could not be resolved", Source: "monthly-runtime-provider-package"})
		return result
	}
	result.AvailableRAMMB = host
	catalog, err := stackkitrelease.ResolveUseCaseCatalog()
	if err != nil {
		logger.Default().Warn("wizard_capacity_catalog_unknown", "error", err)
		result.Reasons = append(result.Reasons, core.WizardRecommendationReason{Code: "capacity_catalog_unknown", Message: "Released memory estimates are unavailable", Source: "stackkits-release-footprints"})
		return result
	}
	result.Source = "stackkits-release-footprints:" + catalog.Release.Tag + ";core-os-policy:2048MiB;historical-estimates"
	goals, err := selectedGoalMemory(catalog, request)
	if err != nil {
		logger.Default().Warn("wizard_capacity_profile_unknown", "error", err)
		result.Reasons = append(result.Reasons, core.WizardRecommendationReason{Code: "capacity_profile_unknown", Message: "The selected profile has no complete released memory estimate", Source: result.Source})
		return result
	}
	kitFloor, err := capacityKitFloor(request)
	if err != nil {
		logger.Default().Warn("wizard_capacity_kit_unknown", "error", err)
		result.Reasons = append(result.Reasons, core.WizardRecommendationReason{Code: "capacity_kit_unknown", Message: "The kit core memory policy could not be identified", Source: result.Source})
		return result
	}
	required, resident, increment, known := memoryNeed(goals, result.ConcurrentWorkloads, kitFloor)
	result.RequiredRAMMB, result.ResidentRAMMB = required, resident
	if known {
		result.WorkingIncrementRAMMB = &increment
	}
	result.SuggestedRAMMB = int(math.Ceil(float64(required)/1024)) * 1024
	for _, goal := range goals {
		need, _, _, _ := memoryNeed([]goalMemory{goal}, result.ConcurrentWorkloads, kitFloor)
		result.PerUseCase = append(result.PerUseCase, core.WizardCapacityUseCase{Goal: goal.goal, ResidentRAMMB: mib(goal.resident), WorkingIncrementRAMMB: knownMemoryIncrement(goal.increment, goal.peakKnown), RequiredRAMMB: need})
	}
	switch {
	case host <= 0:
		result.Reasons = append(result.Reasons, core.WizardRecommendationReason{Code: "capacity_host_unknown", Message: "Choose a server size to assess memory", Source: result.Source})
	case required > host:
		result.Status = "warning"
		result.Reasons = append(result.Reasons, core.WizardRecommendationReason{Code: "capacity_memory", Message: "Selected workloads exceed the chosen server's memory estimate", Source: result.Source})
		if known {
			result.OmitGoals = memoryOmissions(catalog, request, host)
		}
	case known:
		result.Status = "fits"
	}
	if known && host > 0 && required > host && request.DeploymentLane == "saas" && request.ServerCapacity == nil && request.ProviderID != "" && request.RuntimeOfferingID != "" {
		provider := request.ProviderID
		smallest := 0
		for _, offering := range monthlyruntime.Catalog() {
			pkg, err := monthlyruntime.PackageForProvider(provider, string(offering.ID))
			if err == nil && pkg.MemoryMB >= required && (smallest == 0 || pkg.MemoryMB < smallest) {
				smallest = pkg.MemoryMB
				result.SuggestedOfferingID = string(offering.ID)
				result.SuggestedOfferingRAMMB = pkg.MemoryMB
			}
		}
	}
	if !known {
		result.SuggestedRAMMB = 0
		result.SuggestedOfferingID = ""
		result.SuggestedOfferingRAMMB = 0
		result.OmitGoals = []string{}
		result.Reasons = append(result.Reasons, core.WizardRecommendationReason{Code: "capacity_peak_unknown", Message: "At least one released module has no working-memory estimate; this is a lower bound", Source: result.Source})
	}
	return result
}

func wizardCapacityHost(request core.WizardRecommendationRequest) (int, error) {
	if request.ServerCapacity != nil {
		return request.ServerCapacity.RAMMB, nil
	}
	if request.DeploymentLane != "saas" || request.RuntimeOfferingID == "" || request.ProviderID == "" {
		return 0, nil
	}
	offering := request.RuntimeOfferingID
	provider := request.ProviderID
	pkg, err := monthlyruntime.PackageForProvider(provider, offering)
	return pkg.MemoryMB, err
}

func selectedGoalMemory(catalog stackkitrelease.UseCaseCatalog, request core.WizardRecommendationRequest) ([]goalMemory, error) {
	footprints, err := selectedFootprints(catalog, request.Goals, request.ComputeProfile, request.UseCaseSettings, request.UseCaseCapabilities)
	if err != nil {
		return nil, err
	}
	// Goal ownership follows the released capability/add-on graph used by layout.
	demands := footprintDemands(catalog, footprints)
	owners := map[string]string{}
	for _, demand := range demands {
		for _, workload := range demand.Workloads {
			owners[workload] = demand.Goal
		}
	}
	byGoal := map[string]*goalMemory{}
	seen := map[string]bool{}
	for _, footprint := range footprints {
		goal := owners[footprint.Workload]
		if goal == "" {
			return nil, fmt.Errorf("released workload %s has no goal owner", footprint.Workload)
		}
		item := byGoal[goal]
		if item == nil {
			item = &goalMemory{goal: goal, peakKnown: true, concurrent: true}
			byGoal[goal] = item
		}
		key := footprint.Module + ":" + footprint.ComputeProfile
		if seen[key] {
			continue
		}
		seen[key] = true
		item.resident += footprint.Reservation.RAMGB
		item.floor = math.Max(item.floor, footprint.HostFloor.MinRAMGB)
		if footprint.Recommended == nil || footprint.Recommended.RAMGB <= 0 {
			item.peakKnown = false
		} else {
			item.increment += math.Max(0, footprint.Recommended.RAMGB-footprint.Reservation.RAMGB)
		}
		useCase, _ := catalog.FindUseCase(goal)
		profile := request.ComputeProfile
		if profile == "" {
			profile = "standard"
		}
		load := useCase.ComputeTiers[profile].Load
		// Background/ingest/always-active loads are not constrained by open tabs.
		// Count their full increment alongside the two interactive tools.
		if load == nil || load.Baseline == "active-resident" || load.Burst != "interactive" {
			item.concurrent = false
		}
	}
	ids := make([]string, 0, len(byGoal))
	for id := range byGoal {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]goalMemory, 0, len(ids))
	for _, id := range ids {
		result = append(result, *byGoal[id])
	}
	return result, nil
}

func mib(gib float64) int { return int(math.Ceil(gib * 1024)) }
func memoryNeed(goals []goalMemory, concurrency int, kitFloor float64) (required, resident, increment int, known bool) {
	baseline, floor, working := float64(capacityCoreOSMiB)/1024, kitFloor, 0.0
	known = true
	var interactive []float64
	for _, goal := range goals {
		baseline += goal.resident
		floor = math.Max(floor, goal.floor)
		known = known && goal.peakKnown
		if goal.concurrent {
			interactive = append(interactive, goal.increment)
		} else {
			working += goal.increment
		}
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(interactive)))
	for i, value := range interactive {
		if i < concurrency {
			working += value
		}
	}
	return mib(math.Max(floor, baseline+working)), mib(baseline), mib(working), known
}

// Greedy removal is deterministic advice, not a minimum-cardinality claim.
// Resolve the released selection afresh so shared modules survive their first
// owner's omission and settings/capabilities follow the same selection path.
func memoryOmissions(catalog stackkitrelease.UseCaseCatalog, request core.WizardRecommendationRequest, host int) []string {
	kitFloor, err := capacityKitFloor(request)
	if err != nil {
		return nil
	}
	// Hard-memory advice uses the same retained set as initial rollout. Extra
	// peak-only suggestions remain optional, since rollout does not gate peaks.
	remaining, failures, err := initialCapacitySelection(catalog, request, host, kitFloor)
	if err != nil {
		return nil
	}
	omitted := []string{}
	for _, failure := range failures {
		omitted = append(omitted, strings.TrimPrefix(failure["component"].(string), "use_case:"))
	}
	concurrency := request.ConcurrentWorkloads
	if concurrency == 0 {
		concurrency = 2
	}
	for len(remaining) > 0 {
		current := request
		current.Goals = remaining
		goals, err := selectedGoalMemory(catalog, current)
		if err != nil {
			return nil
		}
		need, _, _, known := memoryNeed(goals, concurrency, kitFloor)
		if !known {
			return nil
		}
		if need <= host {
			break
		}
		best, bestNeed := -1, int(^uint(0)>>1)
		for i := range remaining {
			candidate := request
			candidate.Goals = append([]string(nil), remaining[:i]...)
			candidate.Goals = append(candidate.Goals, remaining[i+1:]...)
			facts, err := selectedGoalMemory(catalog, candidate)
			if err != nil {
				continue
			}
			next, _, _, known := memoryNeed(facts, concurrency, kitFloor)
			if known && next < bestNeed {
				best, bestNeed = i, next
			}
		}
		if best < 0 {
			return nil
		}
		omitted = append(omitted, remaining[best])
		remaining = append(remaining[:best], remaining[best+1:]...)
	}
	return omitted
}

// InitialCapacitySelection partitions only a fresh managed installation. It
// checks declared host floors and resident budgets, never advisory peaks.
// Installed stacks must use their pinned lifecycle rather than this function.
func InitialCapacitySelection(request core.WizardRecommendationRequest) ([]string, []map[string]any, error) {
	host, err := wizardCapacityHost(request)
	if err != nil || host <= 0 {
		return nil, nil, fmt.Errorf("managed server capacity unavailable: %v", err)
	}
	catalog, err := stackkitrelease.ResolveUseCaseCatalog()
	if err != nil {
		return nil, nil, err
	}
	kitFloor, err := capacityKitFloor(request)
	if err != nil {
		return nil, nil, err
	}
	return initialCapacitySelection(catalog, request, host, kitFloor)
}

func initialCapacitySelection(catalog stackkitrelease.UseCaseCatalog, request core.WizardRecommendationRequest, host int, kitFloor float64) ([]string, []map[string]any, error) {
	if host < mib(kitFloor) {
		return nil, nil, fmt.Errorf("%w: need %d MiB, have %d MiB", ErrInitialCapacityCore, mib(kitFloor), host)
	}
	// Stable catalog priority keeps everyday services ahead of heavier optional
	// selections without changing the owner's saved goal order.
	ranked := normalizeWizardRecommendationValues(request.Goals)
	sort.SliceStable(ranked, func(i, j int) bool {
		rank := func(goal string) int {
			for n, id := range selectionPriority {
				if id == goal {
					return n
				}
			}
			return len(selectionPriority)
		}
		return rank(ranked[i]) < rank(ranked[j])
	})
	kept := []string{}
	failures := []map[string]any{}
	for _, goal := range ranked {
		candidate := request
		candidate.Goals = append(append([]string(nil), kept...), goal)
		facts, err := selectedGoalMemory(catalog, candidate)
		if err != nil {
			return nil, nil, err
		}
		baseline, floor := float64(capacityCoreOSMiB)/1024, kitFloor
		for _, item := range facts {
			baseline += item.resident
			floor = math.Max(floor, item.floor)
		}
		need := mib(math.Max(baseline, floor))
		if need > host {
			failures = append(failures, map[string]any{"component": "use_case:" + goal, "reason_code": "capacity_memory", "stage": "stackkit_rollout", "status": "failed", "retryable": false, "detail": fmt.Sprintf("Released host floor/resident budget needs %d MiB; provider package has %d MiB", need, host), "params": map[string]any{"need_gb": float64(need) / 1024, "have_gb": float64(host) / 1024}})
		} else {
			kept = append(kept, goal)
		}
	}
	return kept, failures, nil
}

// Floors are kit policy, independent of whether the person owns the host.
// A self-hosted request must name its kit; otherwise its core graph is unknown.
func capacityKitFloor(request core.WizardRecommendationRequest) (float64, error) {
	kit := request.KitSlug
	if kit == "" && request.DeploymentLane == "saas" {
		kit = "cloud-kit"
	}
	switch kit {
	case "cloud-kit":
		return 4, nil
	case "basement-kit":
		if request.ComputeProfile == "low" {
			return 2, nil
		}
		return 4, nil
	default:
		return 0, fmt.Errorf("released kit/core profile is not identified for capacity advice")
	}
}

func knownMemoryIncrement(gib float64, known bool) *int {
	if !known {
		return nil
	}
	value := mib(gib)
	return &value
}
