package unifier

import (
	"testing"
	"time"

	"github.com/kombifyio/homelab/pkg/core"
	"github.com/kombifyio/homelab/pkg/monthlyruntime"
)

func TestEvaluateServerFitWholeStackManagedSKUsIONOSAndCentron(t *testing.T) {
	// Multi-template selection (Documents & Media) on IONOS Standard Basic Cube S (4 GiB)
	// must fail whole-stack fit, name photos as affected, classify as hardware_fit,
	// and suggest switching to the fitting IONOS Premium Basic Cube M (8 GiB).
	ionosStandard := EvaluateServerFit(core.WizardRecommendationRequest{
		KitSlug:           "cloud-kit",
		DeploymentLane:    "saas",
		ProviderID:        monthlyruntime.ProviderIONOS,
		RuntimeOfferingID: "monthly-runtime-standard",
		Goals:             []string{"files", "media", "photos"},
		UseCaseSettings:   map[string]map[string]any{"photos": {"backend": "immich"}},
		ServerFit: &core.WizardServerFitRequest{
			ProviderID: monthlyruntime.ProviderIONOS,
			OfferingID: "monthly-runtime-standard",
		},
	}, WizardRecommendationInventory{})

	if ionosStandard == nil {
		t.Fatal("expected server fit result")
	}
	if ionosStandard.Scope != "whole_stack" {
		t.Fatalf("scope = %q, want whole_stack", ionosStandard.Scope)
	}
	if ionosStandard.CapacityOrigin != CapacityOriginCatalogSKU || ionosStandard.Evidence != "projected" {
		t.Fatalf("managed SKU must be modeled distinctly from observed inventory: origin=%q evidence=%q", ionosStandard.CapacityOrigin, ionosStandard.Evidence)
	}
	if ionosStandard.Template != "Basic Cube S" || ionosStandard.SKU != "Basic Cube S" || ionosStandard.Location != "de/fra/2" {
		t.Fatalf("unexpected IONOS SKU metadata: %+v", ionosStandard)
	}
	if ionosStandard.Status != FitStatusDoesNotFit || ionosStandard.RefusalClass != RefusalClassHardwareFit {
		t.Fatalf("status=%q refusal=%q, want does_not_fit / hardware_fit", ionosStandard.Status, ionosStandard.RefusalClass)
	}
	if ionosStandard.Budget == nil || ionosStandard.Budget.BaseRAMGiB < 2 || ionosStandard.Budget.TotalRAMGiB <= 4 {
		t.Fatalf("budget must include base stack overhead + workloads: %+v", ionosStandard.Budget)
	}
	foundPhotosDeficit := false
	for _, deficit := range ionosStandard.Deficits {
		if deficit.Goal == "photos" && deficit.Code == "memory" {
			foundPhotosDeficit = true
			break
		}
	}
	if !foundPhotosDeficit {
		t.Fatalf("expected deficit identifying affected use case photos: %+v", ionosStandard.Deficits)
	}
	if ionosStandard.Recovery == nil || ionosStandard.Recovery.Action != "switch_offering" || ionosStandard.Recovery.OfferingID != "monthly-runtime-premium" || ionosStandard.Recovery.OfferingSKU != "Basic Cube M" {
		t.Fatalf("expected actionable switch_offering recovery to Basic Cube M: %+v", ionosStandard.Recovery)
	}

	// Same selection with IONOS Memory Cube S enabled must fit whole-stack.
	ionosMemory := EvaluateServerFit(core.WizardRecommendationRequest{
		KitSlug:            "cloud-kit",
		DeploymentLane:     "saas",
		ProviderID:         monthlyruntime.ProviderIONOS,
		RuntimeOfferingID:  "monthly-runtime-standard",
		IONOSMemoryEnabled: true,
		Goals:              []string{"files", "media", "photos"},
		UseCaseSettings:    map[string]map[string]any{"photos": {"backend": "immich"}},
		ServerFit: &core.WizardServerFitRequest{
			ProviderID: monthlyruntime.ProviderIONOS,
			OfferingID: "monthly-runtime-standard",
		},
	}, WizardRecommendationInventory{})

	if ionosMemory.Template != "Memory Cube S" || ionosMemory.Specs.MemoryMB != 8192 || ionosMemory.Status != FitStatusFits {
		t.Fatalf("expected Memory Cube S to fit Documents & Media: %+v", ionosMemory)
	}

	// Same multi-template selection on Centron Standard (4 vCPU / 8 GiB / 80 GB)
	// must report exact Centron SKU & region and fit whole-stack.
	centronStandard := EvaluateServerFit(core.WizardRecommendationRequest{
		KitSlug:           "cloud-kit",
		DeploymentLane:    "saas",
		ProviderID:        monthlyruntime.ProviderCentron,
		RuntimeOfferingID: "monthly-runtime-standard",
		Goals:             []string{"files", "media", "photos"},
		UseCaseSettings:   map[string]map[string]any{"photos": {"backend": "immich"}},
		ServerFit: &core.WizardServerFitRequest{
			ProviderID: monthlyruntime.ProviderCentron,
			OfferingID: "monthly-runtime-standard",
		},
	}, WizardRecommendationInventory{})

	if centronStandard.Status != FitStatusFits || centronStandard.CapacityOrigin != CapacityOriginCatalogSKU {
		t.Fatalf("expected Centron Standard to fit: %+v", centronStandard)
	}
	if centronStandard.SKU == "" || centronStandard.Location != "de-fra" || centronStandard.Specs.MemoryMB != 8192 {
		t.Fatalf("expected exact Centron SKU and region: %+v", centronStandard)
	}

	// Unavailable provider/offering pair (Centron Large) must classify as unavailable_provider.
	centronLarge := EvaluateServerFit(core.WizardRecommendationRequest{
		DeploymentLane: "saas",
		Goals:          []string{"files"},
		ServerFit: &core.WizardServerFitRequest{
			ProviderID: monthlyruntime.ProviderCentron,
			OfferingID: "monthly-runtime-large",
		},
	}, WizardRecommendationInventory{})
	if centronLarge.RefusalClass != RefusalClassUnavailableProvider || centronLarge.Status != FitStatusDoesNotFit {
		t.Fatalf("expected unavailable_provider refusal for unsupported Centron Large: %+v", centronLarge)
	}
}

func TestEvaluateServerFitOwnedHostGuardSSHSubstrateAndPending(t *testing.T) {
	now := time.Date(2026, 11, 26, 21, 0, 0, 0, time.UTC)

	// 1. Unmeasured install-command host stays pending with clear reason.
	unmeasured := EvaluateServerFit(core.WizardRecommendationRequest{
		KitSlug:        "basement-kit",
		DeploymentLane: "self-hosted",
		Goals:          []string{"files"},
		ServerFit:      &core.WizardServerFitRequest{Source: "install-command"},
	}, WizardRecommendationInventory{})
	if unmeasured.Status != FitStatusPending || unmeasured.PendingReason != "host_unmeasured" || unmeasured.CapacityOrigin != CapacityOriginUnmeasured {
		t.Fatalf("unmeasured host must stay pending: %+v", unmeasured)
	}
	if unmeasured.Budget == nil || unmeasured.Budget.TotalRAMGiB <= 0 {
		t.Fatalf("pending host should still expose whole-stack budget: %+v", unmeasured.Budget)
	}

	// 2. Stale Guard worker emits explicit pending with inventory_stale reason.
	worker := core.Worker{
		ID:            "wrk-1",
		ServerID:      "srv-1",
		Name:          "homelab.local",
		Status:        "online",
		UsageObserved: true,
		Capabilities: core.WorkerCapabilities{
			CPU: 4, RAM: 8192, Disk: 120, Arch: "amd64", AMD64MicroarchitectureLevel: 2,
		},
		Tags: map[string]string{"ip": "192.168.1.50"},
	}
	staleFit := EvaluateServerFit(core.WizardRecommendationRequest{
		KitSlug:        "basement-kit",
		DeploymentLane: "self-hosted",
		Goals:          []string{"files", "vault"},
		ServerFit:      &core.WizardServerFitRequest{ServerID: "srv-1", Source: "guard"},
	}, WizardRecommendationInventory{
		State:          "stale",
		SnapshotID:     "snap-1",
		ObservedAt:     now,
		Workers:        []core.Worker{worker},
		StaleWorkerIDs: map[string]bool{"wrk-1": true},
	})
	if staleFit.Status != FitStatusPending || staleFit.PendingReason != "inventory_stale" || staleFit.CapacityOrigin != CapacityOriginObservedInventory {
		t.Fatalf("stale Guard host must emit pending/inventory_stale: %+v", staleFit)
	}

	// 3. Fresh Guard worker becomes checkable and fits (and matches by SSH host IP too).
	freshFit := EvaluateServerFit(core.WizardRecommendationRequest{
		KitSlug:        "basement-kit",
		DeploymentLane: "self-hosted",
		Goals:          []string{"files", "vault"},
		ServerFit:      &core.WizardServerFitRequest{Source: "ssh", Host: "192.168.1.50"},
	}, WizardRecommendationInventory{
		State:      "fresh",
		SnapshotID: "snap-2",
		ObservedAt: now,
		Workers:    []core.Worker{worker},
	})
	if freshFit.Status != FitStatusFits || freshFit.CapacityOrigin != CapacityOriginObservedInventory || freshFit.InventorySource != "ssh" {
		t.Fatalf("fresh scoped SSH/Guard match should fit: %+v", freshFit)
	}

	// 4. Fresh Proxmox substrate observed capacity fits, and unmeasured substrate stays pending.
	substratePending := EvaluateServerFit(core.WizardRecommendationRequest{
		KitSlug:        "basement-kit",
		DeploymentLane: "self-hosted",
		Goals:          []string{"files"},
		ServerFit:      &core.WizardServerFitRequest{Source: "substrate", Host: "pve-node-1"},
	}, WizardRecommendationInventory{})
	if substratePending.Status != FitStatusPending || substratePending.PendingReason != "substrate_unmeasured" {
		t.Fatalf("unmeasured substrate must be pending: %+v", substratePending)
	}

	substrateFit := EvaluateServerFit(core.WizardRecommendationRequest{
		KitSlug:        "basement-kit",
		DeploymentLane: "self-hosted",
		Goals:          []string{"files", "vault"},
		ServerFit: &core.WizardServerFitRequest{
			Source: "substrate",
			Host:   "pve-node-1",
			ObservedCapacity: &core.WizardObservedHostCapacity{
				CPUCores:     4,
				MemoryMB:     8192,
				DiskGB:       80,
				Architecture: "amd64",
				AMD64Level:   2,
				Source:       "substrate",
				ObservedAt:   now.Format(time.RFC3339),
			},
		},
	}, WizardRecommendationInventory{})
	if substrateFit.Status != FitStatusFits || substrateFit.InventorySource != "substrate" || substrateFit.CapacityOrigin != CapacityOriginObservedInventory {
		t.Fatalf("fresh substrate capacity should fit: %+v", substrateFit)
	}

	// 5. External data volume offloads workload data when internal disk holds base OS.
	extWorker := core.Worker{
		ID:            "wrk-ext",
		ServerID:      "srv-ext",
		Status:        "online",
		UsageObserved: true,
		Capabilities: core.WorkerCapabilities{
			CPU: 4, RAM: 8192, Disk: 16, Arch: "amd64", AMD64MicroarchitectureLevel: 2,
			Volumes: []core.StorageVolume{{MountPoint: "/mnt/data", SizeGB: 500, FreeGB: 450, Filesystem: "ext4", External: true}},
		},
	}
	extFit := EvaluateServerFit(core.WizardRecommendationRequest{
		KitSlug:        "basement-kit",
		DeploymentLane: "self-hosted",
		Goals:          []string{"files"},
		ServerFit:      &core.WizardServerFitRequest{ServerID: "srv-ext", Source: "guard"},
	}, WizardRecommendationInventory{State: "fresh", Workers: []core.Worker{extWorker}})
	if extFit.Status != FitStatusFits || extFit.Budget == nil || extFit.Budget.ExternalDataMount != "/mnt/data" {
		t.Fatalf("external data volume should satisfy data footprint on 16 GB root host: %+v", extFit)
	}
}

func TestEvaluateServerFitCatalogFeasibilityConstraintsAndDrivers(t *testing.T) {
	// 1. Pinned StackKits feasibility: AI omitted on low compute profile -> unsupported_runtime + switch_profile recovery.
	aiLow := EvaluateServerFit(core.WizardRecommendationRequest{
		KitSlug:        "basement-kit",
		DeploymentLane: "self-hosted",
		ComputeProfile: "low",
		Goals:          []string{"ai"},
		ServerFit: &core.WizardServerFitRequest{
			Source: "substrate",
			ObservedCapacity: &core.WizardObservedHostCapacity{
				CPUCores: 8, MemoryMB: 16384, DiskGB: 100, Architecture: "amd64", AMD64Level: 3, Source: "substrate",
			},
		},
	}, WizardRecommendationInventory{})
	if aiLow.Status != FitStatusDoesNotFit || aiLow.RefusalClass != RefusalClassUnsupportedRuntime {
		t.Fatalf("ai on low profile must be unsupported_runtime: %+v", aiLow)
	}
	if aiLow.Recovery == nil || aiLow.Recovery.Action != "switch_profile" || aiLow.Recovery.Profile != "standard" {
		t.Fatalf("expected switch_profile recovery to standard: %+v", aiLow.Recovery)
	}

	// 2. Pinned StackKits feasibility: Wiki allowedArchitectures=['amd64'] on arm64 host -> unsupported_runtime.
	wikiArm := EvaluateServerFit(core.WizardRecommendationRequest{
		KitSlug:        "basement-kit",
		DeploymentLane: "self-hosted",
		Goals:          []string{"wiki"},
		ServerFit: &core.WizardServerFitRequest{
			Source: "substrate",
			ObservedCapacity: &core.WizardObservedHostCapacity{
				CPUCores: 4, MemoryMB: 8192, DiskGB: 80, Architecture: "arm64", Source: "substrate",
			},
		},
	}, WizardRecommendationInventory{})
	if wikiArm.Status != FitStatusDoesNotFit || wikiArm.RefusalClass != RefusalClassUnsupportedRuntime {
		t.Fatalf("wiki on arm64 must be refused as unsupported_runtime: %+v", wikiArm)
	}

	// 3. Goals Configure capacity drivers explain what changes capacity.
	advice := AssessWizardCapacity(core.WizardRecommendationRequest{
		Goals:          []string{"photos", "files", "media"},
		DeploymentLane: "saas",
		ProviderID:     monthlyruntime.ProviderIONOS,
		UseCaseSettings: map[string]map[string]any{
			"photos": {"backend": "immich", "machine-learning": true},
			"files":  {"paperless": true},
			"media":  {"hardware-transcoding": "on"},
		},
	})
	codes := map[string]bool{}
	for _, driver := range advice.Drivers {
		codes[driver.Code] = true
	}
	for _, want := range []string{"photos_smart_search", "files_paperless", "media_transcoding"} {
		if !codes[want] {
			t.Fatalf("missing capacity driver %q in %+v", want, advice.Drivers)
		}
	}
}
