package unifier

import (
	"errors"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/kombifyio/homelab/internal/stackkitrelease"
	"github.com/kombifyio/homelab/pkg/core"
	"github.com/kombifyio/homelab/pkg/monthlyruntime"
)

const (
	FitStatusFits       = "fits"
	FitStatusTight      = "tight"
	FitStatusDoesNotFit = "does_not_fit"
	FitStatusPending    = "pending"

	CapacityOriginCatalogSKU        = "catalog_sku"
	CapacityOriginObservedInventory = "observed_inventory"
	CapacityOriginUnmeasured        = "unmeasured"

	RefusalClassHardwareFit         = "hardware_fit"
	RefusalClassUnsupportedRuntime  = "unsupported_runtime"
	RefusalClassUnavailableProvider = "unavailable_provider"
	RefusalClassEntitlement         = "entitlement"
)

type wholeStackBasePolicy struct {
	kitSlug string
	base    stackkitrelease.UseCaseResources
	floor   stackkitrelease.UseCaseHostFloor
}

// wholeStackBase resolves the Advanced base/shared runtime overhead and kit
// host floor for the target StackKit. Cloud Kit and standard Basement Kit share
// the 2 GiB core+OS envelope (wizard_capacity.go) and 4 GiB / 2 vCPU / 20 GB
// kit floor; Basement Kit low profile uses the 2 GiB / 10 GB low compute floor.
func wholeStackBase(request core.WizardRecommendationRequest) wholeStackBasePolicy {
	kit := strings.TrimSpace(request.KitSlug)
	if kit == "" {
		if request.DeploymentLane == wizardRecommendationDeploymentSaaS ||
			(request.ServerFit != nil && strings.TrimSpace(request.ServerFit.ProviderID) != "") {
			kit = "cloud-kit"
		} else {
			kit = "basement-kit"
		}
	}
	if kit == "basement-kit" && strings.EqualFold(strings.TrimSpace(request.ComputeProfile), "low") {
		return wholeStackBasePolicy{
			kitSlug: kit,
			base:    stackkitrelease.UseCaseResources{CPUCores: 0.25, RAMGB: 1.0, StorageGB: 6.0},
			floor:   stackkitrelease.UseCaseHostFloor{MinCPUCores: 2, MinRAMGB: 2, MinStorageGB: 10},
		}
	}
	return wholeStackBasePolicy{
		kitSlug: kit,
		base:    stackkitrelease.UseCaseResources{CPUCores: 0.5, RAMGB: float64(capacityCoreOSMiB) / 1024, StorageGB: 8.0},
		floor:   stackkitrelease.UseCaseHostFloor{MinCPUCores: 2, MinRAMGB: 4, MinStorageGB: 20},
	}
}

// EvaluateServerFit evaluates the whole target stack (Advanced base/shared
// runtime overhead, per-module reservations, data volumes, and pinned
// StackKits feasibility constraints) against either a catalog-declared future
// managed SKU or observed owned-host inventory (Guard, SSH, or substrate).
// It never provisions.
func EvaluateServerFit(request core.WizardRecommendationRequest, inventory WizardRecommendationInventory) *core.WizardServerFit {
	return evaluateServerFit(request, inventory, true)
}

func evaluateServerFit(request core.WizardRecommendationRequest, inventory WizardRecommendationInventory, allowRecovery bool) *core.WizardServerFit {
	choice := request.ServerFit
	if choice == nil {
		return nil
	}
	fit := &core.WizardServerFit{
		MemoryPeak: core.WizardMemoryPeak{Status: "unknown"},
		Status:     FitStatusPending,
		Scope:      "whole_stack",
		Advisories: []core.WizardServerFitFact{},
		Deficits:   []core.WizardServerFitFact{},
		Unknowns:   []core.WizardServerFitFact{},
	}
	addUnknown := func(code, workload, module string) {
		fit.Unknowns = append(fit.Unknowns, core.WizardServerFitFact{Code: code, Workload: workload, Module: module})
	}
	addAdvisory := func(code, workload, module string) {
		fit.Advisories = append(fit.Advisories, core.WizardServerFitFact{Code: code, Workload: workload, Module: module})
	}

	catalog, catalogErr := stackkitrelease.ResolveUseCaseCatalog()
	if catalogErr == nil {
		fit.CatalogSource = "stackkits-release-footprints:" + catalog.Release.Tag
	}

	var (
		worker            core.Worker
		pendingReason     string
		hostMeasured      bool
		entitlementDenied bool
		isManagedTarget   = strings.TrimSpace(choice.ProviderID) != "" || strings.TrimSpace(choice.OfferingID) != ""
	)

	if !isManagedTarget {
		fit.Evidence = "observed"
		fit.ServerID = strings.TrimSpace(choice.ServerID)
		matched, found := findOwnedWorker(choice, inventory.Workers)
		switch {
		case found:
			worker = matched
			hostMeasured = true
			if fit.ServerID == "" {
				fit.ServerID = firstNonEmptyRecommendation(worker.ServerID, worker.ID)
			}
			fit.CapacityOrigin = CapacityOriginObservedInventory
			fit.InventorySource = ownedInventorySource(worker, choice.Source)
			fit.InventoryRevision = ownedInventoryRevision(inventory, worker)
			if strings.EqualFold(strings.TrimSpace(inventory.State), wizardRecommendationInventoryUnavailable) {
				pendingReason = "inventory_unavailable"
			} else if inventory.StaleWorkerIDs[worker.ID] || (worker.ServerID != "" && inventory.StaleWorkerIDs[worker.ServerID]) {
				pendingReason = "inventory_stale"
			} else if !strings.EqualFold(strings.TrimSpace(worker.Status), "online") {
				pendingReason = "host_offline"
			} else if !worker.UsageObserved {
				worker.Usage = core.WorkerUsage{}
				pendingReason = "usage_unobserved"
				addUnknown("usage", "", "")
			}
		case choice.ObservedCapacity != nil && (choice.ObservedCapacity.CPUCores > 0 || choice.ObservedCapacity.MemoryMB > 0 || choice.ObservedCapacity.DiskGB > 0):
			obs := choice.ObservedCapacity
			hostMeasured = true
			fit.CapacityOrigin = CapacityOriginObservedInventory
			fit.InventorySource = firstNonEmptyRecommendation(obs.Source, choice.Source, "substrate")
			fit.InventoryRevision = firstNonEmptyRecommendation(obs.ObservedAt, "scoped:"+fit.InventorySource)
			diskGB := obs.DiskGB
			if obs.FreeDiskGB > 0 && (diskGB <= 0 || obs.FreeDiskGB < diskGB) {
				diskGB = obs.FreeDiskGB
			}
			worker = core.Worker{
				ID:            firstNonEmptyRecommendation(strings.TrimSpace(choice.Host), fit.InventorySource),
				Status:        "online",
				UsageObserved: obs.UsageObserved || strings.EqualFold(fit.InventorySource, "substrate"),
				Capabilities: core.WorkerCapabilities{
					CPU:                         obs.CPUCores,
					RAM:                         obs.MemoryMB,
					Disk:                        diskGB,
					Arch:                        strings.TrimSpace(obs.Architecture),
					AMD64MicroarchitectureLevel: obs.AMD64Level,
				},
			}
			if obs.Stale {
				pendingReason = "inventory_stale"
			}
		default:
			fit.CapacityOrigin = CapacityOriginUnmeasured
			fit.InventorySource = firstNonEmptyRecommendation(strings.TrimSpace(choice.Source), "unknown")
			pendingReason = unmeasuredPendingReason(choice.Source, choice.ServerID)
			addUnknown(pendingReason, "", "")
		}
	} else {
		fit.Evidence = "projected"
		fit.CapacityOrigin = CapacityOriginCatalogSKU
		fit.InventorySource = CapacityOriginCatalogSKU
		fit.ProviderID = strings.TrimSpace(choice.ProviderID)
		fit.OfferingID = strings.TrimSpace(choice.OfferingID)
		// The wizard authors managed workloads at the standard tier; a client
		// preview hint cannot select a smaller package or relax admission.
		if fit.ProviderID == monthlyruntime.ProviderIONOS && fit.OfferingID == "monthly-runtime-standard" {
			request.ComputeProfile = ""
		}
		if fit.ProviderID == monthlyruntime.ProviderIONOS && fit.OfferingID == "monthly-runtime-standard" {
			if catalogErr != nil {
				addUnknown("catalog_footprint_unavailable", "", "")
				fit.PendingReason = "catalog_footprint_unavailable"
				fit.Recovery = &core.WizardServerFitRecovery{Action: "refresh_inventory", ReasonCode: "catalog_footprint_unavailable"}
				return fit
			}
			if checkUnsupportedPhotoBackend(catalog, request, fit) {
				return fit
			}
		}
		pkg, err := resolveManagedPackageForRequest(catalog, catalogErr, request, fit.ProviderID, fit.OfferingID)
		if err != nil {
			fit.Status = FitStatusDoesNotFit
			fit.RefusalClass = RefusalClassUnavailableProvider
			addUnknown("provider_package_unavailable", "", "")
			fit.Recovery = &core.WizardServerFitRecovery{Action: "review_selection", ReasonCode: "provider_package_unavailable"}
			return fit
		}
		if !offeringAllowedInRequest(request, fit.OfferingID) {
			entitlementDenied = true
			addAdvisory("entitlement_required", "", fit.OfferingID)
		}
		hostMeasured = true
		fit.Template = pkg.Template
		fit.SKU = pkg.DisplaySKU()
		fit.Location = pkg.Location
		fit.InventoryRevision = "sku:" + fit.ProviderID + ":" + fit.OfferingID + ":" + fit.SKU
		if catalogErr == nil && catalog.Release.Tag != "" {
			fit.InventoryRevision += "@" + catalog.Release.Tag
		}
		worker = core.Worker{
			ID:            "planned",
			Status:        "online",
			UsageObserved: true,
			Capabilities: core.WorkerCapabilities{
				CPU:                         pkg.VCPUs,
				RAM:                         pkg.MemoryMB,
				Disk:                        pkg.DiskGB,
				Arch:                        pkg.Architecture,
				AMD64MicroarchitectureLevel: pkg.AMD64Level,
			},
		}
	}

	cap := worker.Capabilities
	fit.Specs = core.WizardServerFitSpecs{
		CPUCores:     cap.CPU,
		MemoryMB:     cap.RAM,
		DiskGB:       cap.Disk,
		Architecture: cap.Arch,
		AMD64Level:   cap.AMD64MicroarchitectureLevel,
	}

	if len(request.Goals) == 0 {
		fit.Status = FitStatusPending
		fit.PendingReason = "no_goals"
		addUnknown("no_goals", "", "")
		return fit
	}
	if catalogErr != nil {
		fit.Status = FitStatusPending
		fit.PendingReason = "catalog_footprint_unavailable"
		addUnknown("catalog_footprint_unavailable", "", "")
		fit.Recovery = &core.WizardServerFitRecovery{Action: "refresh_inventory", ReasonCode: "catalog_footprint_unavailable"}
		return fit
	}
	// The entitlement refusal is a deficit only for an evaluation that can
	// leave pending, so it is recorded after the no_goals and catalog
	// early returns and never travels with a pending result.
	if entitlementDenied {
		fit.Deficits = append(fit.Deficits, core.WizardServerFitFact{
			Code:   "entitlement_required",
			Module: fit.OfferingID,
		})
	}
	if checkUnsupportedPhotoBackend(catalog, request, fit) {
		return fit
	}

	footprints, err := selectedFootprints(catalog, request.Goals, request.ComputeProfile, request.UseCaseSettings, request.UseCaseCapabilities)
	if err != nil {
		if errors.Is(err, stackkitrelease.ErrWorkloadOmittedOnProfile) {
			return failUnsupportedProfileFit(catalog, request, fit)
		}
		fit.Deficits = []core.WizardServerFitFact{}
		fit.Status = FitStatusPending
		fit.PendingReason = "catalog_footprint_unavailable"
		addUnknown("catalog_footprint_unavailable", "", "")
		fit.Recovery = &core.WizardServerFitRecovery{Action: "refresh_inventory", ReasonCode: "catalog_footprint_unavailable"}
		return fit
	}
	if len(footprints) == 0 {
		fit.Deficits = []core.WizardServerFitFact{}
		fit.Status = FitStatusPending
		fit.PendingReason = "catalog_footprint_unavailable"
		addUnknown("catalog_footprint_unavailable", "", "")
		return fit
	}
	if len(request.Services) > 0 {
		addAdvisory("selected_services", "", "")
	}

	basePolicy := wholeStackBase(request)
	workloadTotals := deduplicatedWorkloadResources(footprints)
	dataVolumeFloor := selectionStorageNeed(footprints)
	dataVolumeNeed := math.Max(workloadTotals.StorageGB, dataVolumeFloor)

	budget := &core.WizardServerFitBudget{
		KitSlug:             basePolicy.kitSlug,
		BaseCPUCores:        roundMetric(basePolicy.base.CPUCores),
		BaseRAMGiB:          roundMetric(basePolicy.base.RAMGB),
		BaseStorageGB:       roundMetric(basePolicy.base.StorageGB),
		WorkloadCPUCores:    roundMetric(workloadTotals.CPUCores),
		WorkloadRAMGiB:      roundMetric(workloadTotals.RAMGB),
		WorkloadStorageGB:   roundMetric(workloadTotals.StorageGB),
		TotalCPUCores:       roundMetric(basePolicy.base.CPUCores + workloadTotals.CPUCores),
		TotalRAMGiB:         roundMetric(basePolicy.base.RAMGB + workloadTotals.RAMGB),
		TotalStorageGB:      roundMetric(math.Max(basePolicy.floor.MinStorageGB, basePolicy.base.StorageGB+dataVolumeNeed)),
		DataVolumeStorageGB: roundMetric(dataVolumeNeed),
		KitFloorCPUCores:    basePolicy.floor.MinCPUCores,
		KitFloorRAMGiB:      basePolicy.floor.MinRAMGB,
		KitFloorStorageGB:   basePolicy.floor.MinStorageGB,
	}
	fit.Budget = budget

	if !hostMeasured {
		fit.Status = FitStatusPending
		fit.PendingReason = pendingReason
		fit.Recovery = &core.WizardServerFitRecovery{Action: "refresh_inventory", ReasonCode: pendingReason}
		return fit
	}

	cpuUsed, ramUsed, diskUsed := 0.0, 0.0, 0.0
	if worker.UsageObserved {
		cpuUsed, ramUsed, diskUsed = float64(worker.Usage.CPU), float64(worker.Usage.RAM)/1024, float64(worker.Usage.Disk)
	}
	// On a projected SKU or idle host, base stack overhead is added directly.
	// On an observed host with live OS usage, max(base, observed) avoids
	// double-counting the running OS while preserving the platform floor.
	baseAndUsedCPU := basePolicy.base.CPUCores
	baseAndUsedRAM := basePolicy.base.RAMGB
	if fit.CapacityOrigin == CapacityOriginObservedInventory && worker.UsageObserved {
		baseAndUsedCPU = math.Max(basePolicy.base.CPUCores, cpuUsed)
		baseAndUsedRAM = math.Max(basePolicy.base.RAMGB, ramUsed)
	}

	goalOwners := workloadGoalOwners(catalog, footprints, request.Goals)
	for _, footprint := range footprints {
		goal := goalOwners[footprint.Workload]
		short, missing := footprintFits(footprint, worker, stackkitrelease.UseCaseResources{}, dataVolumeFloor)
		for _, code := range short {
			if code == "accelerator" && cap.GPU == nil && fit.CapacityOrigin == CapacityOriginObservedInventory {
				addUnknown("accelerator", footprint.Workload, footprint.Module)
				continue
			}
			fact := core.WizardServerFitFact{Code: code, Workload: footprint.Workload, Goal: goal, Module: footprint.Module}
			switch code {
			case "cpu":
				fact.Required = roundMetric(math.Max(footprint.HostFloor.MinCPUCores, footprint.Reservation.CPUCores+baseAndUsedCPU))
				fact.Available = float64(cap.CPU)
				fact.Unit = "cores"
			case "memory":
				fact.Required = roundMetric(math.Max(footprint.HostFloor.MinRAMGB, footprint.Reservation.RAMGB+baseAndUsedRAM))
				fact.Available = roundMetric(float64(cap.RAM) / 1024)
				fact.Unit = "GiB"
			case "storage":
				fact.Required = roundMetric(math.Max(footprint.HostFloor.MinStorageGB, footprint.Reservation.StorageGB+diskUsed))
				fact.Available = float64(cap.Disk)
				fact.Unit = "GB"
			}
			fit.Deficits = append(fit.Deficits, fact)
		}
		for _, code := range missing {
			// When a module declares a host CPU floor but omits a per-container
			// CPU reservation, keep it as an advisory rather than blocking fit.
			if code == "cpu_reservation" && footprint.HostFloor.MinCPUCores > 0 {
				addAdvisory(code, footprint.Workload, footprint.Module)
				continue
			}
			addUnknown(code, footprint.Workload, footprint.Module)
		}
	}

	// Whole-stack CPU check (kit floor + base/shared runtime + workloads).
	requiredCPU := math.Max(basePolicy.floor.MinCPUCores, baseAndUsedCPU+workloadTotals.CPUCores)
	if cap.CPU > 0 && requiredCPU > float64(cap.CPU) {
		fit.Deficits = append(fit.Deficits, core.WizardServerFitFact{
			Code:      "selection_cpu",
			Required:  roundMetric(requiredCPU),
			Available: float64(cap.CPU),
			Unit:      "cores",
		})
	} else if cap.CPU <= 0 {
		addUnknown("cpu", "", "")
	}

	// Whole-stack memory check (kit floor + base/shared runtime + workloads).
	availableRAM := float64(cap.RAM) / 1024
	requiredRAM := math.Max(basePolicy.floor.MinRAMGB, baseAndUsedRAM+workloadTotals.RAMGB)
	if cap.RAM > 0 && requiredRAM > availableRAM {
		fit.Deficits = append(fit.Deficits, core.WizardServerFitFact{
			Code:      "selection_memory",
			Required:  roundMetric(requiredRAM),
			Available: roundMetric(availableRAM),
			Unit:      "GiB",
		})
	} else if cap.RAM <= 0 {
		addUnknown("memory", "", "")
	}

	// Whole-stack storage check: base OS/platform stays on root disk; data
	// volumes may sit on a qualifying external volume when root disk is smaller.
	rootDisk := float64(cap.Disk)
	rootNeed := basePolicy.base.StorageGB + diskUsed
	externalUsed := false
	if dataVolumeNeed > 0 && (rootDisk < basePolicy.base.StorageGB+dataVolumeNeed+diskUsed || rootDisk < basePolicy.floor.MinStorageGB) {
		if volume, ok := largestFittingVolume(cap.Volumes, dataVolumeNeed); ok {
			externalUsed = true
			budget.ExternalDataVolumeGB = volume.SizeGB
			budget.ExternalDataMount = volume.MountPoint
			volUsed := externalVolumeUsedGB(volume)
			if dataVolumeNeed+volUsed > float64(volume.SizeGB) {
				fit.Deficits = append(fit.Deficits, core.WizardServerFitFact{
					Code:      "selection_storage",
					Required:  roundMetric(dataVolumeNeed + volUsed),
					Available: float64(volume.SizeGB),
					Unit:      "GB",
				})
			}
			if rootDisk > 0 && rootNeed > rootDisk {
				fit.Deficits = append(fit.Deficits, core.WizardServerFitFact{
					Code:      "selection_storage",
					Required:  roundMetric(rootNeed),
					Available: rootDisk,
					Unit:      "GB",
				})
			}
		}
	}
	if !externalUsed {
		combinedStorageNeed := math.Max(basePolicy.floor.MinStorageGB, basePolicy.base.StorageGB+dataVolumeNeed) + diskUsed
		if rootDisk > 0 && combinedStorageNeed > rootDisk {
			fit.Deficits = append(fit.Deficits, core.WizardServerFitFact{
				Code:      "selection_storage",
				Required:  roundMetric(combinedStorageNeed),
				Available: rootDisk,
				Unit:      "GB",
			})
		} else if rootDisk <= 0 {
			addUnknown("storage", "", "")
		}
	}

	fit.MemoryPeak = estimateWholeStackMemoryPeak(catalog, footprints, worker, baseAndUsedRAM, len(request.Services) == 0)

	if entitlementDenied {
		fit.Status = FitStatusDoesNotFit
		fit.RefusalClass = RefusalClassEntitlement
		if allowRecovery {
			fit.Recovery = buildFitRecovery(catalog, request, fit, basePolicy, footprints, inventory)
		}
		return fit
	}

	if len(fit.Deficits) > 0 {
		fit.Status = FitStatusDoesNotFit
		fit.RefusalClass = classifyFitRefusal(fit.Deficits)
		if allowRecovery {
			fit.Recovery = buildFitRecovery(catalog, request, fit, basePolicy, footprints, inventory)
		}
		return fit
	}

	if pendingReason != "" {
		fit.Status = FitStatusPending
		fit.PendingReason = pendingReason
		fit.MemoryPeak = core.WizardMemoryPeak{Status: "unknown"}
		fit.Recovery = &core.WizardServerFitRecovery{Action: "refresh_inventory", ReasonCode: pendingReason}
		return fit
	}

	if len(fit.Unknowns) > 0 {
		fit.Status = FitStatusPending
		fit.PendingReason = fit.Unknowns[0].Code
		fit.Recovery = &core.WizardServerFitRecovery{Action: "refresh_inventory", ReasonCode: fit.PendingReason}
		return fit
	}

	if fit.MemoryPeak.Warning {
		fit.Status = FitStatusTight
		if allowRecovery {
			fit.Recovery = buildFitRecovery(catalog, request, fit, basePolicy, footprints, inventory)
		}
		return fit
	}

	fit.Status = FitStatusFits
	return fit
}

func findOwnedWorker(choice *core.WizardServerFitRequest, workers []core.Worker) (core.Worker, bool) {
	serverID := strings.TrimSpace(choice.ServerID)
	host := strings.ToLower(strings.TrimSpace(choice.Host))
	for _, candidate := range workers {
		if serverID != "" {
			if candidate.ServerID == serverID || candidate.ID == serverID {
				return candidate, true
			}
		}
		if host != "" {
			if strings.EqualFold(strings.TrimSpace(candidate.Name), host) ||
				strings.EqualFold(strings.TrimSpace(candidate.Tags["ip"]), host) ||
				strings.EqualFold(strings.TrimSpace(candidate.Tags["host"]), host) {
				return candidate, true
			}
		}
	}
	return core.Worker{}, false
}

func ownedInventorySource(worker core.Worker, requestedSource string) string {
	switch strings.ToLower(strings.TrimSpace(requestedSource)) {
	case "ssh", "substrate", "guard":
		return strings.ToLower(strings.TrimSpace(requestedSource))
	}
	if src := strings.ToLower(strings.TrimSpace(worker.Tags["inventory_source"])); src != "" {
		return src
	}
	return "guard"
}

func ownedInventoryRevision(inventory WizardRecommendationInventory, worker core.Worker) string {
	parts := []string{}
	if id := strings.TrimSpace(inventory.SnapshotID); id != "" {
		parts = append(parts, id)
	}
	if worker.ServerID != "" {
		parts = append(parts, worker.ServerID)
	} else if worker.ID != "" {
		parts = append(parts, worker.ID)
	}
	if !inventory.ObservedAt.IsZero() {
		parts = append(parts, inventory.ObservedAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	return strings.Join(parts, ":")
}

func unmeasuredPendingReason(source, serverID string) string {
	if strings.TrimSpace(serverID) != "" {
		return "server_unavailable"
	}
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "substrate", "hypervisor":
		return "substrate_unmeasured"
	case "ssh", "connect-remote":
		return "ssh_unmeasured"
	default:
		return "host_unmeasured"
	}
}

func checkUnsupportedPhotoBackend(catalog stackkitrelease.UseCaseCatalog, request core.WizardRecommendationRequest, fit *core.WizardServerFit) bool {
	backend, _ := request.UseCaseSettings["photos"]["backend"].(string)
	if backend == "" || !slices.Contains(request.Goals, "photos") {
		return false
	}
	photos, ok := catalog.FindUseCase("photos")
	if !ok || backend == photos.DefaultAlternative {
		return false
	}
	if _, installable := photos.InstallingAlternative(backend); !installable {
		fit.Status = FitStatusDoesNotFit
		fit.RefusalClass = RefusalClassUnsupportedRuntime
		fit.Deficits = append(fit.Deficits, core.WizardServerFitFact{
			Code:     "photo_backend_unavailable",
			Workload: "photos",
			Goal:     "photos",
		})
		fit.Recovery = &core.WizardServerFitRecovery{
			Action:     "review_selection",
			Goal:       "photos",
			ReasonCode: "photo_backend_unavailable",
		}
		return true
	}
	return false
}

func failUnsupportedProfileFit(catalog stackkitrelease.UseCaseCatalog, request core.WizardRecommendationRequest, fit *core.WizardServerFit) *core.WizardServerFit {
	profile := strings.TrimSpace(request.ComputeProfile)
	if profile == "" {
		profile = "standard"
	}
	firstOmitted := ""
	for _, goal := range request.Goals {
		useCase, found := catalog.FindUseCase(goal)
		if !found {
			continue
		}
		if verdict, declared := useCase.ComputeTiers[profile]; declared && !verdict.Included {
			if firstOmitted == "" {
				firstOmitted = goal
			}
			fit.Deficits = append(fit.Deficits, core.WizardServerFitFact{
				Code:     "profile_unsupported",
				Workload: goal,
				Goal:     goal,
			})
		}
	}
	fit.Status = FitStatusDoesNotFit
	fit.RefusalClass = RefusalClassUnsupportedRuntime
	fit.Recovery = &core.WizardServerFitRecovery{
		Action:     "switch_profile",
		Profile:    "standard",
		Goal:       firstOmitted,
		ReasonCode: "profile_unsupported",
	}
	return fit
}

func deduplicatedWorkloadResources(footprints []stackkitrelease.WorkloadFootprint) stackkitrelease.UseCaseResources {
	var total stackkitrelease.UseCaseResources
	seen := map[string]bool{}
	for _, footprint := range footprints {
		key := footprint.Module + ":" + footprint.ComputeProfile
		if seen[key] {
			continue
		}
		seen[key] = true
		total.CPUCores += footprint.Reservation.CPUCores
		total.RAMGB += footprint.Reservation.RAMGB
		total.StorageGB += footprint.Reservation.StorageGB
	}
	return total
}

func workloadGoalOwners(catalog stackkitrelease.UseCaseCatalog, footprints []stackkitrelease.WorkloadFootprint, goals []string) map[string]string {
	owners := map[string]string{}
	for _, demand := range footprintDemands(catalog, footprints) {
		for _, workload := range demand.Workloads {
			owners[workload] = demand.Goal
		}
	}
	for _, goal := range goals {
		if _, exists := owners[goal]; !exists {
			owners[goal] = goal
		}
	}
	return owners
}

func classifyFitRefusal(deficits []core.WizardServerFitFact) string {
	for _, fact := range deficits {
		switch fact.Code {
		case "photo_backend_unavailable", "profile_unsupported", "cpu_architecture", "cpu_microarchitecture":
			return RefusalClassUnsupportedRuntime
		}
	}
	return RefusalClassHardwareFit
}

func buildFitRecovery(
	catalog stackkitrelease.UseCaseCatalog,
	request core.WizardRecommendationRequest,
	fit *core.WizardServerFit,
	basePolicy wholeStackBasePolicy,
	footprints []stackkitrelease.WorkloadFootprint,
	inventory WizardRecommendationInventory,
) *core.WizardServerFitRecovery {
	primaryReason := "hardware_fit"
	deficitGoal := ""
	for _, fact := range fit.Deficits {
		if primaryReason == "hardware_fit" && fact.Code != "" {
			primaryReason = fact.Code
		}
		if deficitGoal == "" && fact.Goal != "" {
			deficitGoal = fact.Goal
		}
	}
	if fit.Status == FitStatusTight {
		primaryReason = "memory_peak"
	}
	if fit.RefusalClass == RefusalClassEntitlement {
		primaryReason = monthlyruntime.EntitlementReasonOfferingTierRequired
	}

	canResolveCapacityOrTier := fit.RefusalClass == "" || fit.RefusalClass == RefusalClassHardwareFit || fit.RefusalClass == RefusalClassEntitlement

	// 1. Check an allowed same-tier managed option across admitted providers
	// first when it retains all chosen functions without a tier upgrade.
	if fit.CapacityOrigin == CapacityOriginCatalogSKU && fit.ProviderID != "" && canResolveCapacityOrTier {
		if offering, pkg, ok := sameTierFittingManagedPackage(catalog, fit.ProviderID, fit.OfferingID, request, basePolicy, footprints); ok {
			action := "switch_offering"
			if pkg.ProviderID != fit.ProviderID {
				action = "switch_provider"
			}
			return &core.WizardServerFitRecovery{
				Action:           action,
				ReasonCode:       primaryReason,
				ProviderID:       pkg.ProviderID,
				OfferingID:       string(offering.ID),
				OfferingName:     offering.Name,
				OfferingSKU:      pkg.DisplaySKU(),
				OfferingMemoryMB: pkg.MemoryMB,
				Goal:             deficitGoal,
			}
		}
	}

	// 2. Offer a viable capable owned server in inventory without forcing a
	// commerce upgrade.
	if canResolveCapacityOrTier {
		if worker, ok := bestCapableOwnedWorker(request, fit.ServerID, inventory); ok {
			return &core.WizardServerFitRecovery{
				Action:     "use_owned_server",
				ReasonCode: primaryReason,
				ServerID:   firstNonEmptyRecommendation(worker.ServerID, worker.ID),
				ServerName: firstNonEmptyRecommendation(worker.Name, worker.ServerID, worker.ID),
				Goal:       deficitGoal,
			}
		}
	}

	// 3. For managed catalog SKUs with a hardware fit / tight / entitlement
	// verdict, suggest the smallest admitted and entitled larger (or standard)
	// package from the same provider that fits.
	if fit.CapacityOrigin == CapacityOriginCatalogSKU && fit.ProviderID != "" && canResolveCapacityOrTier {
		if !request.ProviderAdmissionChecked || slices.Contains(request.AdmittedProviders, fit.ProviderID) {
			if offering, pkg, ok := smallestFittingManagedPackage(catalog, fit.ProviderID, fit.OfferingID, request, basePolicy, footprints); ok {
				return &core.WizardServerFitRecovery{
					Action:           "switch_offering",
					ReasonCode:       primaryReason,
					ProviderID:       pkg.ProviderID,
					OfferingID:       string(offering.ID),
					OfferingName:     offering.Name,
					OfferingSKU:      pkg.DisplaySKU(),
					OfferingMemoryMB: pkg.MemoryMB,
					Goal:             deficitGoal,
				}
			}
		}
	}

	// 3b. When refused for offering-tier entitlement, ALWAYS surface an entitled
	// offering recovery (monthly-runtime-standard) before falling back to
	// profile or goal edits that would still 403 on the unentitled tier.
	if fit.RefusalClass == RefusalClassEntitlement && fit.CapacityOrigin == CapacityOriginCatalogSKU && fit.ProviderID != "" {
		if recovery, ok := entitledStandardOfferingRecovery(catalog, request, fit.ProviderID, fit.OfferingID, primaryReason, deficitGoal); ok {
			return recovery
		}
	}

	// 4. If full Photos (Immich) exceeds memory and a lighter catalog-admitted
	// Photos alternative (Immich Lite or Ente Photos) is available, offer
	// switching Photos to that lighter profile.
	if fit.RefusalClass != RefusalClassEntitlement && slices.Contains(request.Goals, "photos") {
		backend, _ := request.UseCaseSettings["photos"]["backend"].(string)
		if backend != "immich-lite" && backend != "ente-photos" {
			if photos, ok := catalog.FindUseCase("photos"); ok {
				if deficitGoal == "photos" || primaryReason == "selection_memory" || primaryReason == "memory" || primaryReason == "memory_peak" {
					for _, candidate := range []string{"ente-photos", "immich-lite"} {
						if _, installable := photos.InstallingAlternative(candidate); installable {
							return &core.WizardServerFitRecovery{
								Action:     "switch_profile",
								Profile:    candidate,
								Goal:       "photos",
								ReasonCode: primaryReason,
							}
						}
					}
				}
			}
		}
	}

	// 5. When multiple use cases are selected and one specific goal drives the
	// deficit, offer leaving out that single use case.
	if fit.RefusalClass != RefusalClassEntitlement && len(request.Goals) > 1 {
		targetGoal := deficitGoal
		if targetGoal == "" {
			targetGoal = heaviestGoal(catalog, footprints, request.Goals)
		}
		if targetGoal != "" {
			return &core.WizardServerFitRecovery{
				Action:     "omit_use_case",
				Goal:       targetGoal,
				ReasonCode: primaryReason,
			}
		}
	}

	// 6. Otherwise keep selection intact and guide the operator to review it.
	return &core.WizardServerFitRecovery{
		Action:     "review_selection",
		Goal:       deficitGoal,
		ReasonCode: primaryReason,
	}
}

func entitledStandardOfferingRecovery(
	catalog stackkitrelease.UseCaseCatalog,
	request core.WizardRecommendationRequest,
	currentProviderID, currentOfferingID, primaryReason, deficitGoal string,
) (*core.WizardServerFitRecovery, bool) {
	standardID := string(monthlyruntime.DefaultOfferingID())
	if currentOfferingID == standardID || !offeringAllowedInRequest(request, standardID) {
		return nil, false
	}
	offering, ok := monthlyruntime.OfferingByID(monthlyruntime.DefaultOfferingID())
	if !ok {
		return nil, false
	}
	providers := []string{currentProviderID}
	if request.ProviderAdmissionChecked {
		providers = []string{}
		if slices.Contains(request.AdmittedProviders, currentProviderID) {
			providers = append(providers, currentProviderID)
		}
		for _, pid := range request.AdmittedProviders {
			if pid != currentProviderID {
				providers = append(providers, pid)
			}
		}
	}
	for _, providerID := range providers {
		if strings.TrimSpace(providerID) == "" {
			continue
		}
		pkg, err := resolveManagedPackageForRequest(catalog, nil, request, providerID, standardID)
		if err != nil {
			continue
		}
		action := "switch_offering"
		if pkg.ProviderID != currentProviderID {
			action = "switch_provider"
		}
		return &core.WizardServerFitRecovery{
			Action:           action,
			ReasonCode:       primaryReason,
			ProviderID:       pkg.ProviderID,
			OfferingID:       string(offering.ID),
			OfferingName:     offering.Name,
			OfferingSKU:      pkg.DisplaySKU(),
			OfferingMemoryMB: pkg.MemoryMB,
			Goal:             deficitGoal,
		}, true
	}
	return nil, false
}

func offeringAllowedInRequest(request core.WizardRecommendationRequest, offeringID string) bool {
	trimmed := strings.TrimSpace(offeringID)
	if !request.OfferingEntitlementChecked && len(request.AllowedOfferings) == 0 {
		switch strings.ToLower(strings.TrimSpace(request.DeploymentLane)) {
		case wizardRecommendationDeploymentSaaS:
			return trimmed == string(monthlyruntime.DefaultOfferingID())
		case wizardRecommendationDeploymentSelfHosted:
			return true
		default:
			return false
		}
	}
	return slices.Contains(request.AllowedOfferings, trimmed)
}

func sameTierFittingManagedPackage(
	catalog stackkitrelease.UseCaseCatalog,
	currentProviderID, currentOfferingID string,
	request core.WizardRecommendationRequest,
	basePolicy wholeStackBasePolicy,
	footprints []stackkitrelease.WorkloadFootprint,
) (monthlyruntime.Offering, monthlyruntime.ProviderPackage, bool) {
	standardID := string(monthlyruntime.DefaultOfferingID())
	if currentOfferingID != "" && currentOfferingID != standardID {
		return monthlyruntime.Offering{}, monthlyruntime.ProviderPackage{}, false
	}
	if !offeringAllowedInRequest(request, standardID) {
		return monthlyruntime.Offering{}, monthlyruntime.ProviderPackage{}, false
	}
	offering, ok := monthlyruntime.OfferingByID(monthlyruntime.DefaultOfferingID())
	if !ok {
		return monthlyruntime.Offering{}, monthlyruntime.ProviderPackage{}, false
	}
	providers := []string{currentProviderID}
	if request.ProviderAdmissionChecked {
		providers = request.AdmittedProviders
	}
	for _, providerID := range providers {
		if providerID == "" {
			continue
		}
		pkg, err := resolveManagedPackageForRequest(catalog, nil, request, providerID, standardID)
		if err != nil {
			continue
		}
		effectiveCurrentOffering := currentOfferingID
		if effectiveCurrentOffering == "" {
			effectiveCurrentOffering = standardID
		}
		if pkg.ProviderID == currentProviderID && string(offering.ID) == effectiveCurrentOffering {
			continue
		}
		if packageFitsWholeStack(pkg, basePolicy, footprints) {
			return offering, pkg, true
		}
	}
	return monthlyruntime.Offering{}, monthlyruntime.ProviderPackage{}, false
}

func bestCapableOwnedWorker(
	request core.WizardRecommendationRequest,
	excludeServerID string,
	inventory WizardRecommendationInventory,
) (core.Worker, bool) {
	if len(inventory.Workers) == 0 || strings.EqualFold(strings.TrimSpace(inventory.State), wizardRecommendationInventoryUnavailable) {
		return core.Worker{}, false
	}
	var (
		best  core.Worker
		found bool
	)
	for _, worker := range inventory.Workers {
		candidateID := firstNonEmptyRecommendation(worker.ServerID, worker.ID)
		if candidateID == "" || (excludeServerID != "" && candidateID == excludeServerID) {
			continue
		}
		if inventory.StaleWorkerIDs[worker.ID] || (worker.ServerID != "" && inventory.StaleWorkerIDs[worker.ServerID]) {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(worker.Status), "online") || !worker.UsageObserved {
			continue
		}
		if !isOwnedUnboundWorker(worker) {
			continue
		}
		ownedReq := request
		ownedReq.ProviderID = ""
		ownedReq.RuntimeOfferingID = ""
		if ownedReq.KitSlug == "" || ownedReq.KitSlug == "cloud-kit" {
			ownedReq.KitSlug = "basement-kit"
		}
		ownedReq.ServerFit = &core.WizardServerFitRequest{ServerID: candidateID, Source: "guard"}
		fit := evaluateServerFit(ownedReq, inventory, false)
		if fit == nil || fit.Status != FitStatusFits {
			continue
		}
		if !found || worker.Capabilities.RAM < best.Capabilities.RAM ||
			(worker.Capabilities.RAM == best.Capabilities.RAM && candidateID < firstNonEmptyRecommendation(best.ServerID, best.ID)) {
			best, found = worker, true
		}
	}
	return best, found
}

func packageFitsWholeStack(
	pkg monthlyruntime.ProviderPackage,
	basePolicy wholeStackBasePolicy,
	footprints []stackkitrelease.WorkloadFootprint,
) bool {
	workloadTotals := deduplicatedWorkloadResources(footprints)
	dataVolumeFloor := selectionStorageNeed(footprints)
	dataVolumeNeed := math.Max(workloadTotals.StorageGB, dataVolumeFloor)
	needCPU := math.Max(basePolicy.floor.MinCPUCores, basePolicy.base.CPUCores+workloadTotals.CPUCores)
	needRAM := math.Max(basePolicy.floor.MinRAMGB, basePolicy.base.RAMGB+workloadTotals.RAMGB)
	needStorage := math.Max(basePolicy.floor.MinStorageGB, basePolicy.base.StorageGB+dataVolumeNeed)
	for _, footprint := range footprints {
		needCPU = math.Max(needCPU, footprint.HostFloor.MinCPUCores)
		needRAM = math.Max(needRAM, footprint.HostFloor.MinRAMGB)
		needStorage = math.Max(needStorage, footprint.HostFloor.MinStorageGB)
		if footprint.Accelerator != nil {
			return false
		}
		if footprint.HostFloor.MinAMD64MicroarchitectureLevel > 0 && pkg.AMD64Level > 0 && pkg.AMD64Level < footprint.HostFloor.MinAMD64MicroarchitectureLevel {
			return false
		}
		if len(footprint.HostFloor.AllowedArchitectures) > 0 && pkg.Architecture != "" && !slices.Contains(footprint.HostFloor.AllowedArchitectures, pkg.Architecture) {
			return false
		}
	}
	return float64(pkg.VCPUs) >= needCPU && float64(pkg.MemoryMB)/1024 >= needRAM && float64(pkg.DiskGB) >= needStorage
}

func smallestFittingManagedPackage(
	catalog stackkitrelease.UseCaseCatalog,
	providerID, currentOfferingID string,
	request core.WizardRecommendationRequest,
	basePolicy wholeStackBasePolicy,
	footprints []stackkitrelease.WorkloadFootprint,
) (monthlyruntime.Offering, monthlyruntime.ProviderPackage, bool) {
	var (
		bestOffering monthlyruntime.Offering
		bestPkg      monthlyruntime.ProviderPackage
		found        bool
	)
	for _, offering := range monthlyruntime.Catalog() {
		if string(offering.ID) == currentOfferingID {
			continue
		}
		if !offeringAllowedInRequest(request, string(offering.ID)) {
			continue
		}
		pkg, err := resolveManagedPackageForRequest(catalog, nil, request, providerID, string(offering.ID))
		if err != nil {
			continue
		}
		if !packageFitsWholeStack(pkg, basePolicy, footprints) {
			continue
		}
		if !found || pkg.MemoryMB < bestPkg.MemoryMB || (pkg.MemoryMB == bestPkg.MemoryMB && pkg.VCPUs < bestPkg.VCPUs) {
			bestOffering, bestPkg, found = offering, pkg, true
		}
	}
	return bestOffering, bestPkg, found
}

func heaviestGoal(catalog stackkitrelease.UseCaseCatalog, footprints []stackkitrelease.WorkloadFootprint, goals []string) string {
	owners := workloadGoalOwners(catalog, footprints, goals)
	byGoal := map[string]float64{}
	for _, fp := range footprints {
		goal := owners[fp.Workload]
		if goal == "" {
			continue
		}
		byGoal[goal] += math.Max(fp.Reservation.RAMGB, fp.HostFloor.MinRAMGB)
	}
	type pair struct {
		goal string
		ram  float64
	}
	ranked := make([]pair, 0, len(byGoal))
	for goal, ram := range byGoal {
		ranked = append(ranked, pair{goal: goal, ram: ram})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].ram == ranked[j].ram {
			return ranked[i].goal < ranked[j].goal
		}
		return ranked[i].ram > ranked[j].ram
	})
	if len(ranked) == 0 {
		return ""
	}
	return ranked[0].goal
}

// estimateWholeStackMemoryPeak sums base/shared runtime overhead (or observed
// usage on an active host) with source-bound module peak evidence.
func estimateWholeStackMemoryPeak(
	catalog stackkitrelease.UseCaseCatalog,
	footprints []stackkitrelease.WorkloadFootprint,
	worker core.Worker,
	baseAndUsedRAM float64,
	completeSelection bool,
) core.WizardMemoryPeak {
	unknown := core.WizardMemoryPeak{Status: "unknown"}
	if len(footprints) == 0 || !completeSelection || !worker.UsageObserved || worker.Capabilities.RAM <= 0 {
		return unknown
	}
	peak := baseAndUsedRAM
	seen := map[string]bool{}
	for _, footprint := range footprints {
		key := footprint.Module + ":" + footprint.ComputeProfile
		if seen[key] {
			continue
		}
		seen[key] = true
		mib, ok := stackkitrelease.MeasuredMemoryPeak(catalog, footprint)
		if !ok || footprint.Accelerator != nil {
			return unknown
		}
		peak += mib / 1024
	}
	available := float64(worker.Capabilities.RAM) / 1024
	return memoryPeakResult(peak, available)
}

func memoryPeakResult(peak, available float64) core.WizardMemoryPeak {
	return core.WizardMemoryPeak{
		Status:       "available",
		EstimatedGiB: math.Ceil(peak*100) / 100,
		AvailableGiB: roundMetric(available),
		Warning:      peak > available*1.25,
	}
}

func roundMetric(value float64) float64 {
	return math.Round(value*100) / 100
}
