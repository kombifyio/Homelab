package unifier

import (
	"net/netip"
	"slices"
	"strings"

	"github.com/kombifyio/homelab/internal/stackkitrelease"
	"github.com/kombifyio/homelab/pkg/core"
	"github.com/kombifyio/homelab/pkg/monthlyruntime"
)

// DocumentsMediaWorkloadEnvelope documents the supported Pro Standard VPS
// operating envelope for full Documents & Media (Cloudreve + full Immich with
// smart search + Jellyfin) with zero surcharge.
type DocumentsMediaWorkloadEnvelope = core.DocumentsMediaWorkloadEnvelope

// DefaultDocumentsMediaProEnvelope returns the documented Pro Standard VPS
// envelope for full Documents & Media (kombify-StackKits-8l2z.2).
func DefaultDocumentsMediaProEnvelope() DocumentsMediaWorkloadEnvelope {
	return DocumentsMediaWorkloadEnvelope{
		SupportedPlaybackModes: []string{"direct-play", "remux"},
		MaxConcurrentWorkloads: 2,
		MinRootDiskGB:          80,
		BackupWorkingSpaceGB:   10,
		RequiresFullImmich:     true,
	}
}

// ProStarterID names a Creation Wizard Pro starter preset.
type ProStarterID = string

const (
	ProStarterDocumentsMedia ProStarterID = "documents-media"
	ProStarterFamily         ProStarterID = "family"
	ProStarterSelfEmployed   ProStarterID = "self-employed"
)

// ProStarterAlternative describes a concrete, explicit alternative when the
// full starter preset does not fit the target host.
type ProStarterAlternative = core.WizardProStarterAlternative

// ProStarterEvaluation describes what starts now, what is deferred for later,
// whether the full choice already fits, and concrete reversible alternatives.
type ProStarterEvaluation = core.WizardProStarter

// resolveManagedPackageForRequest is the single source of truth for resolving
// the admitted managed provider package (e.g. IONOS Memory Cube S for full
// Immich when IONOSMemoryEnabled is active, Basic Cube S for Immich Lite / Ente
// Photos / non-Immich selections, Centron Standard 8 GiB, etc.).
// Both EvaluateServerFit and AssessWizardCapacity / InitialCapacitySelection
// share this helper so preview, capacity advice, and rollout never disagree.
func resolveManagedPackageForRequest(
	catalog stackkitrelease.UseCaseCatalog,
	catalogErr error,
	request core.WizardRecommendationRequest,
	providerID, offeringID string,
) (monthlyruntime.ProviderPackage, error) {
	providerID = strings.TrimSpace(providerID)
	offeringID = strings.TrimSpace(offeringID)
	sku := ""
	if providerID == monthlyruntime.ProviderIONOS && offeringID == "monthly-runtime-standard" {
		if catalogErr == nil && request.IONOSMemoryEnabled {
			// Standard managed rollouts always evaluate at standard tier.
			computeProfile := request.ComputeProfile
			if computeProfile != "" {
				computeProfile = ""
			}
			if footprints, err := selectedFootprints(catalog, request.Goals, computeProfile, request.UseCaseSettings, request.UseCaseCapabilities); err == nil {
				for _, footprint := range footprints {
					if footprint.Workload == "photos" && footprint.Module == "stackkits-immich-runtime" {
						sku = "Memory Cube S"
						break
					}
				}
			}
		}
	}
	return monthlyruntime.PackageForRecordedSKU(providerID, offeringID, sku)
}

// EvaluateProStarters evaluates all three Pro starter presets for the
// recommendation response.
func EvaluateProStarters(
	baseRequest core.WizardRecommendationRequest,
	inventory WizardRecommendationInventory,
	lanReachable bool,
) map[string]ProStarterEvaluation {
	return map[string]ProStarterEvaluation{
		ProStarterDocumentsMedia: EvaluateProStarter(ProStarterDocumentsMedia, baseRequest, inventory, lanReachable),
		ProStarterFamily:         EvaluateProStarter(ProStarterFamily, baseRequest, inventory, lanReachable),
		ProStarterSelfEmployed:   EvaluateProStarter(ProStarterSelfEmployed, baseRequest, inventory, lanReachable),
	}
}

// EvaluateProStarter evaluates a Pro starter preset ("documents-media",
// "family", or "self-employed") against the pinned StackKits catalog and target
// server. When the full choice already fits (and satisfies placement reach),
// it recommends the full choice first; otherwise it computes the complete
// fitting starter set, preserves deferred goals in intent, and lists concrete
// alternatives with explicit confirmation flags.
func EvaluateProStarter(
	id ProStarterID,
	baseRequest core.WizardRecommendationRequest,
	inventory WizardRecommendationInventory,
	lanReachable bool,
) ProStarterEvaluation {
	catalog, catalogErr := stackkitrelease.ResolveUseCaseCatalog()
	switch id {
	case ProStarterDocumentsMedia:
		return evaluateDocumentsMediaStarter(catalog, catalogErr, baseRequest, inventory)
	case ProStarterFamily:
		return evaluateFamilyStarter(catalog, catalogErr, baseRequest, inventory, lanReachable)
	case ProStarterSelfEmployed:
		return evaluateSelfEmployedStarter(catalog, catalogErr, baseRequest, inventory)
	default:
		return ProStarterEvaluation{ID: id}
	}
}

func filterOfferedStarterGoals(goals, availableGoals []string) []string {
	if len(availableGoals) == 0 {
		return slices.Clone(goals)
	}
	allowed := make(map[string]struct{}, len(availableGoals))
	for _, goal := range availableGoals {
		if normalized := strings.ToLower(strings.TrimSpace(goal)); normalized != "" {
			allowed[normalized] = struct{}{}
		}
	}
	filtered := make([]string, 0, len(goals))
	for _, goal := range goals {
		if _, ok := allowed[strings.ToLower(strings.TrimSpace(goal))]; ok {
			filtered = append(filtered, goal)
		}
	}
	return filtered
}

func evaluateDocumentsMediaStarter(
	catalog stackkitrelease.UseCaseCatalog,
	catalogErr error,
	baseRequest core.WizardRecommendationRequest,
	inventory WizardRecommendationInventory,
) ProStarterEvaluation {
	env := DefaultDocumentsMediaProEnvelope()
	goals := filterOfferedStarterGoals([]string{"files", "photos", "media"}, baseRequest.AvailableGoals)
	settings := cloneUseCaseSettings(baseRequest.UseCaseSettings)
	// Preserve full default Immich (with smart search); never silently downgrade.
	if slices.Contains(goals, "photos") {
		if settings["photos"] == nil {
			settings["photos"] = map[string]any{}
		}
		if _, explicitlySet := settings["photos"]["backend"]; !explicitlySet {
			settings["photos"]["backend"] = "immich"
		}
	}
	if len(goals) == 0 {
		return ProStarterEvaluation{
			ID:               ProStarterDocumentsMedia,
			StartsWith:       []string{},
			DefaultSettings:  settings,
			WorkloadEnvelope: &env,
		}
	}

	req := baseRequest
	req.Goals = goals
	req.UseCaseSettings = settings
	fit := EvaluateServerFit(req, inventory)

	eval := ProStarterEvaluation{
		ID:               ProStarterDocumentsMedia,
		StartsWith:       goals,
		DefaultSettings:  settings,
		Fit:              fit,
		WorkloadEnvelope: &env,
	}
	if fit != nil && fit.Status == FitStatusFits {
		eval.RecommendedFull = true
		return eval
	}
	if fit != nil && (fit.Status == FitStatusDoesNotFit || fit.Status == FitStatusTight) {
		if slices.Contains(goals, "photos") {
			eval.Alternatives = photosLighterAlternatives(catalog, catalogErr, fit)
		}
		eval.RequiresConfirmation = true
		eval.ConfirmationReasons = []string{"documents_media_host_insufficient"}
	}
	return eval
}

func evaluateFamilyStarter(
	catalog stackkitrelease.UseCaseCatalog,
	catalogErr error,
	baseRequest core.WizardRecommendationRequest,
	inventory WizardRecommendationInventory,
	lanReachable bool,
) ProStarterEvaluation {
	fullGoals := filterOfferedStarterGoals([]string{"photos", "files", "media", "vault", "smart-home"}, baseRequest.AvailableGoals)
	cloudGoals := filterOfferedStarterGoals([]string{"photos", "files", "media", "vault"}, baseRequest.AvailableGoals)
	settings := cloneUseCaseSettings(baseRequest.UseCaseSettings)
	if len(fullGoals) == 0 {
		return ProStarterEvaluation{
			ID:              ProStarterFamily,
			StartsWith:      []string{},
			DefaultSettings: settings,
		}
	}
	hasSmartHome := slices.Contains(fullGoals, "smart-home")

	// 1. Where the full choice (including Smart Home on verified LAN reach)
	// already fits, recommend it first.
	var fullFit *core.WizardServerFit
	if !hasSmartHome || lanReachable {
		req := baseRequest
		req.Goals = fullGoals
		req.UseCaseSettings = settings
		fullFit = EvaluateServerFit(req, inventory)
		if fullFit != nil && fullFit.Status == FitStatusFits {
			return ProStarterEvaluation{
				ID:              ProStarterFamily,
				RecommendedFull: true,
				StartsWith:      fullGoals,
				DefaultSettings: settings,
				Fit:             fullFit,
			}
		}
	}

	// 2. Without verified LAN reach (e.g. managed Pro VPS) or when the full
	// 5-goal set exceeds host capacity, start with photos + files + playback +
	// vault and preserve smart-home as a deferred expansion.
	var fit *core.WizardServerFit
	if len(cloudGoals) > 0 {
		req := baseRequest
		req.Goals = cloudGoals
		req.UseCaseSettings = settings
		fit = EvaluateServerFit(req, inventory)
	}

	deferred := []string{}
	if hasSmartHome {
		deferred = append(deferred, "smart-home")
	}
	eval := ProStarterEvaluation{
		ID:              ProStarterFamily,
		RecommendedFull: false,
		StartsWith:      cloudGoals,
		DeferredGoals:   deferred,
		DefaultSettings: settings,
		Fit:             fit,
	}
	if hasSmartHome && !lanReachable {
		eval.RequiresConfirmation = true
		eval.ConfirmationReasons = append(eval.ConfirmationReasons, "smart_home_deferred_lan_reach")
	} else if hasSmartHome && lanReachable && fullFit != nil && (fullFit.Status == FitStatusDoesNotFit || fullFit.Status == FitStatusTight) {
		eval.RequiresConfirmation = true
		eval.ConfirmationReasons = append(eval.ConfirmationReasons, "smart_home_deferred_host_capacity")
	}
	if fit != nil && fit.Status != FitStatusFits {
		eval.RequiresConfirmation = true
		if slices.Contains(cloudGoals, "photos") {
			eval.Alternatives = append(eval.Alternatives, photosLighterAlternatives(catalog, catalogErr, fit)...)
		}
	}
	if hasSmartHome {
		excludeServerID := ""
		if baseRequest.ServerFit != nil {
			excludeServerID = strings.TrimSpace(baseRequest.ServerFit.ServerID)
		}
		lanInventory := ownedLANReachableInventory(inventory)
		if worker, ok := bestCapableOwnedWorker(reqWithGoals(baseRequest, fullGoals, settings), excludeServerID, lanInventory); ok {
			workerID := firstNonEmptyRecommendation(worker.ServerID, worker.ID)
			alreadyOffered := false
			for _, alt := range eval.Alternatives {
				if alt.Kind == "use_owned_server" && alt.ServerID == workerID {
					alreadyOffered = true
					break
				}
			}
			if !alreadyOffered {
				eval.Alternatives = append(eval.Alternatives, ProStarterAlternative{
					Kind:                 "use_owned_server",
					Goal:                 "smart-home",
					ServerID:             workerID,
					ServerName:           firstNonEmptyRecommendation(worker.Name, worker.ServerID, worker.ID),
					RequiresConfirmation: true,
				})
			}
		}
	}
	return eval
}

func reqWithGoals(base core.WizardRecommendationRequest, goals []string, settings map[string]map[string]any) core.WizardRecommendationRequest {
	out := base
	out.Goals = goals
	out.UseCaseSettings = settings
	return out
}

func ownedLANReachableInventory(inventory WizardRecommendationInventory) WizardRecommendationInventory {
	filtered := inventory
	filtered.Workers = make([]core.Worker, 0, len(inventory.Workers))
	for _, worker := range inventory.Workers {
		if hasOwnedLANReachable(worker) {
			filtered.Workers = append(filtered.Workers, worker)
		}
	}
	return filtered
}

func hasOwnedLANReachable(worker core.Worker) bool {
	if !isOwnedNonManagedWorker(worker) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(worker.Tags["lan_reachable"])) {
	case "false":
		return false
	case "true":
		return true
	}
	ip := strings.TrimSpace(worker.Tags["ip"])
	if ip == "" {
		return false
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	addr = addr.Unmap().WithZone("")
	return addr.IsPrivate()
}

func evaluateSelfEmployedStarter(
	catalog stackkitrelease.UseCaseCatalog,
	catalogErr error,
	baseRequest core.WizardRecommendationRequest,
	inventory WizardRecommendationInventory,
) ProStarterEvaluation {
	settings := cloneUseCaseSettings(baseRequest.UseCaseSettings)
	fullGoals := filterOfferedStarterGoals([]string{"files", "vault", "mail", "automation", "ai"}, baseRequest.AvailableGoals)
	if slices.Contains(fullGoals, "mail") {
		if settings["mail"] == nil {
			settings["mail"] = map[string]any{}
		}
		if _, set := settings["mail"]["hosting"]; !set {
			settings["mail"]["hosting"] = "existing"
		}
	}
	if len(fullGoals) == 0 {
		return ProStarterEvaluation{
			ID:              ProStarterSelfEmployed,
			StartsWith:      []string{},
			DefaultSettings: settings,
		}
	}

	// 1. Check full choice first (files + vault + mail + automation + local AI).
	fullReq := baseRequest
	fullReq.Goals = fullGoals
	fullReq.UseCaseSettings = settings
	fullFit := EvaluateServerFit(fullReq, inventory)
	if fullFit != nil && fullFit.Status == FitStatusFits {
		return ProStarterEvaluation{
			ID:              ProStarterSelfEmployed,
			RecommendedFull: true,
			StartsWith:      fullGoals,
			DefaultSettings: settings,
			Fit:             fullFit,
		}
	}

	// 2. Include automation only if the pinned catalog AND fit prove it fits.
	starts := filterOfferedStarterGoals([]string{"files", "vault", "mail"}, baseRequest.AvailableGoals)
	deferred := []string{}
	hasAutomation := slices.Contains(fullGoals, "automation")
	hasAI := slices.Contains(fullGoals, "ai")
	automationAdmitted := false
	if hasAutomation && catalogErr == nil {
		if _, ok := catalog.FindUseCase("automation"); ok {
			if _, fpErr := catalog.WorkloadFootprints("automation", "", "standard", ""); fpErr == nil {
				autoReq := baseRequest
				autoReq.Goals = append(slices.Clone(starts), "automation")
				autoReq.UseCaseSettings = settings
				autoFit := EvaluateServerFit(autoReq, inventory)
				if autoFit != nil && autoFit.Status == FitStatusFits {
					automationAdmitted = true
					starts = autoReq.Goals
				}
			}
		}
	}
	if hasAutomation && !automationAdmitted {
		deferred = append(deferred, "automation")
	}
	// Retain local AI for a suitable host (>=12 GiB RAM / 4 vCPU); never
	// silently swap to provider inference.
	if hasAI {
		deferred = append(deferred, "ai")
	}

	var starterFit *core.WizardServerFit
	if len(starts) > 0 {
		starterReq := baseRequest
		starterReq.Goals = starts
		starterReq.UseCaseSettings = settings
		starterFit = EvaluateServerFit(starterReq, inventory)
	}

	eval := ProStarterEvaluation{
		ID:                   ProStarterSelfEmployed,
		RecommendedFull:      false,
		StartsWith:           starts,
		DeferredGoals:        deferred,
		DefaultSettings:      settings,
		RequiresConfirmation: hasAI || (starterFit != nil && starterFit.Status != FitStatusFits),
		Fit:                  starterFit,
	}
	if hasAI {
		eval.ConfirmationReasons = []string{"local_ai_requires_suitable_host"}
	}
	excludeServerID := ""
	if baseRequest.ServerFit != nil {
		excludeServerID = strings.TrimSpace(baseRequest.ServerFit.ServerID)
	}
	if worker, ok := bestCapableOwnedWorker(fullReq, excludeServerID, inventory); ok {
		eval.Alternatives = append(eval.Alternatives, ProStarterAlternative{
			Kind:                 "use_owned_server",
			Goal:                 "ai",
			ServerID:             firstNonEmptyRecommendation(worker.ServerID, worker.ID),
			ServerName:           firstNonEmptyRecommendation(worker.Name, worker.ServerID, worker.ID),
			RequiresConfirmation: true,
		})
	}
	if fullFit != nil && fullFit.Recovery != nil && fullFit.Recovery.Action == "switch_offering" && fullFit.RefusalClass != RefusalClassEntitlement {
		eval.Alternatives = append(eval.Alternatives, ProStarterAlternative{
			Kind:                 "switch_offering",
			OfferingID:           fullFit.Recovery.OfferingID,
			OfferingSKU:          fullFit.Recovery.OfferingSKU,
			RequiresConfirmation: true,
		})
	}
	return eval
}

func photosLighterAlternatives(
	catalog stackkitrelease.UseCaseCatalog,
	catalogErr error,
	fit *core.WizardServerFit,
) []ProStarterAlternative {
	var alts []ProStarterAlternative
	if fit != nil && fit.Recovery != nil {
		switch fit.Recovery.Action {
		case "switch_provider":
			if fit.Recovery.ProviderID != "" {
				alts = append(alts, ProStarterAlternative{
					Kind:                 "switch_provider",
					ProviderID:           fit.Recovery.ProviderID,
					OfferingID:           fit.Recovery.OfferingID,
					OfferingSKU:          fit.Recovery.OfferingSKU,
					RequiresConfirmation: true,
				})
			}
		case "use_owned_server":
			if fit.Recovery.ServerID != "" {
				alts = append(alts, ProStarterAlternative{
					Kind:                 "use_owned_server",
					ServerID:             fit.Recovery.ServerID,
					ServerName:           firstNonEmptyRecommendation(fit.Recovery.ServerName, fit.Recovery.ServerID),
					RequiresConfirmation: true,
				})
			}
		case "switch_offering":
			if fit.Recovery.OfferingID != "" {
				alts = append(alts, ProStarterAlternative{
					Kind:                 "switch_offering",
					OfferingID:           fit.Recovery.OfferingID,
					OfferingSKU:          fit.Recovery.OfferingSKU,
					RequiresConfirmation: true,
				})
			}
		}
	}
	if catalogErr == nil {
		if photos, ok := catalog.FindUseCase("photos"); ok {
			if _, installable := photos.InstallingAlternative("ente-photos"); installable {
				alts = append(alts, ProStarterAlternative{
					Kind:                 "switch_profile",
					Goal:                 "photos",
					Profile:              "ente-photos",
					RequiresConfirmation: true,
					LostFunctions:        []string{"ai-search"},
					Settings:             map[string]any{"backend": "ente-photos"},
				})
			}
			if _, installable := photos.InstallingAlternative("immich-lite"); installable {
				alts = append(alts, ProStarterAlternative{
					Kind:                 "switch_profile",
					Goal:                 "photos",
					Profile:              "immich-lite",
					RequiresConfirmation: true,
					LostFunctions:        []string{"ai-search"},
					Settings:             map[string]any{"backend": "immich-lite"},
				})
			}
		}
	}
	return alts
}

func cloneUseCaseSettings(in map[string]map[string]any) map[string]map[string]any {
	out := make(map[string]map[string]any, len(in))
	for k, v := range in {
		cp := make(map[string]any, len(v))
		for sk, sv := range v {
			cp[sk] = sv
		}
		out[k] = cp
	}
	return out
}

// PreserveStarterSelection verifies a fit preserves the complete chosen starter
// function set and never treats an omitted workload as a full success.
func PreserveStarterSelection(requestedGoals, keptGoals []string) bool {
	if len(requestedGoals) == 0 {
		return false
	}
	for _, goal := range requestedGoals {
		if !slices.Contains(keptGoals, goal) {
			return false
		}
	}
	return true
}
