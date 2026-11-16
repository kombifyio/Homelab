package unifier

import (
	"math"
	"slices"
	"strings"

	"github.com/kombifyio/techstack/internal/stackkitrelease"
	"github.com/kombifyio/techstack/pkg/core"
	"github.com/kombifyio/techstack/pkg/monthlyruntime"
)

// EvaluateServerFit uses exactly the provider package or scoped registry worker
// and the release catalog used by the wizard's projection. It never provisions.
func EvaluateServerFit(request core.WizardRecommendationRequest, inventory WizardRecommendationInventory) *core.WizardServerFit {
	choice := request.ServerFit
	if choice == nil {
		return nil
	}
	fit := &core.WizardServerFit{Status: "unknown", Scope: "selected_workloads", Advisories: []core.WizardServerFitFact{{Code: "base_stack_capacity"}}, Deficits: []core.WizardServerFitFact{}, Unknowns: []core.WizardServerFitFact{}}
	addUnknown := func(code, workload, module string) {
		fit.Unknowns = append(fit.Unknowns, core.WizardServerFitFact{Code: code, Workload: workload, Module: module})
	}
	var worker core.Worker
	if choice.ServerID != "" {
		fit.Evidence, fit.ServerID = "observed", strings.TrimSpace(choice.ServerID)
		found := false
		for _, candidate := range inventory.Workers {
			if candidate.ServerID == fit.ServerID || (candidate.ServerID == "" && candidate.ID == fit.ServerID) {
				worker, found = candidate, true
				break
			}
		}
		if !found {
			addUnknown("server_unavailable", "", "")
			return fit
		}
		if !worker.UsageObserved {
			worker.Usage = core.WorkerUsage{}
			addUnknown("usage", "", "")
		}
	} else {
		fit.Evidence, fit.ProviderID, fit.OfferingID = "projected", strings.TrimSpace(choice.ProviderID), strings.TrimSpace(choice.OfferingID)
		// The wizard authors managed workloads at the standard tier; a client
		// preview hint cannot select a smaller package or relax admission.
		if fit.ProviderID == monthlyruntime.ProviderIONOS && fit.OfferingID == "monthly-runtime-standard" {
			request.ComputeProfile = ""
		}
		sku := ""
		if fit.ProviderID == monthlyruntime.ProviderIONOS && fit.OfferingID == "monthly-runtime-standard" {
			catalog, catalogErr := stackkitrelease.ResolveUseCaseCatalog()
			if catalogErr != nil {
				addUnknown("catalog_footprint_unavailable", "", "")
				return fit
			}
			if backend, _ := request.UseCaseSettings["photos"]["backend"].(string); backend == "ente-photos" && slices.Contains(request.Goals, "photos") {
				photos, ok := catalog.FindUseCase("photos")
				_, installable := photos.InstallingAlternative(backend)
				if !ok || !installable {
					addUnknown("photo_backend_unavailable", "photos", "")
					return fit
				}
			}
			footprints, footprintErr := selectedFootprints(catalog, request.Goals, request.ComputeProfile, request.UseCaseSettings, request.UseCaseCapabilities)
			if footprintErr != nil {
				addUnknown("catalog_footprint_unavailable", "", "")
				return fit
			}
			for _, footprint := range footprints {
				if request.IONOSMemoryEnabled && footprint.Workload == "photos" && footprint.Module == "stackkits-immich-runtime" {
					sku = "Memory Cube S"
				}
			}
		}
		pkg, err := monthlyruntime.PackageForRecordedSKU(fit.ProviderID, fit.OfferingID, sku)
		if err != nil {
			addUnknown("provider_package_unavailable", "", "")
			return fit
		}
		fit.Template, fit.Location = pkg.Template, pkg.Location
		worker = core.Worker{ID: "planned", Status: "online", UsageObserved: true, Capabilities: core.WorkerCapabilities{CPU: pkg.VCPUs, RAM: pkg.MemoryMB, Disk: pkg.DiskGB}}
	}
	cap := worker.Capabilities
	fit.Specs = core.WizardServerFitSpecs{CPUCores: cap.CPU, MemoryMB: cap.RAM, DiskGB: cap.Disk, Architecture: cap.Arch, AMD64Level: cap.AMD64MicroarchitectureLevel}
	if len(request.Goals) == 0 {
		addUnknown("no_goals", "", "")
		return fit
	}
	catalog, err := stackkitrelease.ResolveUseCaseCatalog()
	if err != nil {
		addUnknown("catalog_footprint_unavailable", "", "")
		return fit
	}
	fit.CatalogSource = "stackkits-release-footprints:" + catalog.Release.Tag
	footprints, err := selectedFootprints(catalog, request.Goals, request.ComputeProfile, request.UseCaseSettings, request.UseCaseCapabilities)
	if err != nil {
		addUnknown("catalog_footprint_unavailable", "", "")
		return fit
	}
	if len(footprints) == 0 {
		addUnknown("catalog_footprint_unavailable", "", "")
	}
	if len(request.Services) > 0 {
		addUnknown("selected_services", "", "")
	}
	// The catalog footprints cover selected application workloads, not the
	// complete foundation and platform. Do not call the whole stack a fit.

	var total stackkitrelease.UseCaseResources
	cpuUsed, ramUsed, diskUsed := 0.0, 0.0, 0.0
	if worker.UsageObserved {
		cpuUsed, ramUsed, diskUsed = float64(worker.Usage.CPU), float64(worker.Usage.RAM)/1024, float64(worker.Usage.Disk)
	}
	selectionNeed := selectionStorageNeed(footprints)
	for _, footprint := range footprints {
		short, missing := footprintFits(footprint, worker, stackkitrelease.UseCaseResources{}, selectionNeed)
		for _, code := range short {
			if code == "accelerator" && cap.GPU == nil {
				addUnknown("accelerator", footprint.Workload, footprint.Module)
				continue
			}
			fact := core.WizardServerFitFact{Code: code, Workload: footprint.Workload, Module: footprint.Module}
			for _, goal := range request.Goals {
				if goal == footprint.Workload {
					fact.Goal = goal
					break
				}
			}
			switch code {
			case "cpu":
				fact.Required, fact.Available, fact.Unit = math.Max(footprint.HostFloor.MinCPUCores, footprint.Reservation.CPUCores+cpuUsed), float64(cap.CPU), "cores"
			case "memory":
				fact.Required, fact.Available, fact.Unit = math.Max(footprint.HostFloor.MinRAMGB, footprint.Reservation.RAMGB+ramUsed), float64(cap.RAM)/1024, "GiB"
			}
			fit.Deficits = append(fit.Deficits, fact)
		}
		for _, code := range missing {
			addUnknown(code, footprint.Workload, footprint.Module)
		}
		total.CPUCores += footprint.Reservation.CPUCores
		total.RAMGB += footprint.Reservation.RAMGB
		total.StorageGB += footprint.Reservation.StorageGB
	}
	// Without observed use only total-host impossibility can be proved. This
	// still catches known incompatible selections rather than assuming idle.
	if cap.CPU > 0 && total.CPUCores+cpuUsed > float64(cap.CPU) {
		fit.Deficits = append(fit.Deficits, core.WizardServerFitFact{Code: "selection_cpu", Required: total.CPUCores + cpuUsed, Available: float64(cap.CPU), Unit: "cores"})
	}
	if cap.RAM > 0 && total.RAMGB+ramUsed > float64(cap.RAM)/1024 {
		fit.Deficits = append(fit.Deficits, core.WizardServerFitFact{Code: "selection_memory", Required: total.RAMGB + ramUsed, Available: float64(cap.RAM) / 1024, Unit: "GiB"})
	}
	// Use the same selected data volume policy as the workload capacity check.
	storage, storageUsed := float64(cap.Disk), diskUsed
	if selectionNeed > 0 && storage < selectionNeed {
		if volume, ok := largestFittingVolume(cap.Volumes, selectionNeed); ok {
			storage, storageUsed = float64(volume.SizeGB), externalVolumeUsedGB(volume)
		}
	}
	if storage > 0 && total.StorageGB+storageUsed > storage {
		fit.Deficits = append(fit.Deficits, core.WizardServerFitFact{Code: "selection_storage", Required: total.StorageGB + storageUsed, Available: storage, Unit: "GB"})
	}
	if fit.Evidence == "observed" && (inventory.State == "unavailable" || inventory.StaleWorkerIDs[worker.ID] || !strings.EqualFold(worker.Status, "online")) {
		fit.Status = "stale"
		return fit
	}
	if len(fit.Deficits) > 0 {
		fit.Status = "does_not_fit"
	} else if len(fit.Unknowns) == 0 {
		fit.Status = "fits"
	}
	return fit
}
