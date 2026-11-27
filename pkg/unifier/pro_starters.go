package unifier

import (
	"slices"
	"strings"

	"github.com/kombifyio/homelab/internal/stackkitrelease"
	"github.com/kombifyio/homelab/pkg/core"
	"github.com/kombifyio/homelab/pkg/monthlyruntime"
)

// DocumentsMediaWorkloadEnvelope documents the supported Pro Standard VPS
// operating envelope for full Documents & Media (Cloudreve + full Immich with
// smart search + Jellyfin) with zero surcharge.
type DocumentsMediaWorkloadEnvelope struct {
	SupportedPlaybackModes []string `json:"supported_playback_modes"`
	MaxConcurrentWorkloads int      `json:"max_concurrent_workloads"`
	MinRootDiskGB          int      `json:"min_root_disk_gb"`
	BackupWorkingSpaceGB   int      `json:"backup_working_space_gb"`
	RequiresFullImmich     bool     `json:"requires_full_immich"`
}

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
type ProStarterID string

const (
	ProStarterDocumentsMedia ProStarterID = "documents-media"
	ProStarterFamily         ProStarterID = "family"
	ProStarterSelfEmployed   ProStarterID = "self-employed"
)

// ProStarterAlternative describes a concrete, explicit alternative when the
// full starter preset does not fit the target host.
type ProStarterAlternative struct {
	Kind                 string         `json:"kind"` // "switch_profile" | "switch_offering" | "defer_goal"
	Goal                 string         `json:"goal,omitempty"`
	Profile              string         `json:"profile,omitempty"`
	OfferingID           string         `json:"offering_id,omitempty"`
	OfferingSKU          string         `json:"offering_sku,omitempty"`
	RequiresConfirmation bool           `json:"requires_confirmation"`
	LostFunctions        []string       `json:"lost_functions,omitempty"`
	Settings             map[string]any `json:"settings,omitempty"`
}

// ProStarterEvaluation describes what starts now, what is deferred for later,
// whether the full choice already fits, and concrete reversible alternatives.
type ProStarterEvaluation struct {
	ID                       ProStarterID            `json:"id"`
	RecommendedFull          bool                    `json:"recommended_full"`
	StartsWith               []string                `json:"starts_with"`
	DeferredGoals            []string                `json:"deferred_goals,omitempty"`
	DefaultSettings          map[string]map[string]any `json:"default_settings,omitempty"`
	RequiresConfirmation     bool                    `json:"requires_confirmation,omitempty"`
	ConfirmationReasons      []string                `json:"confirmation_reasons,omitempty"`
	Fit                      *core.WizardServerFit   `json:"fit,omitempty"`
	Alternatives             []ProStarterAlternative `json:"alternatives,omitempty"`
	WorkloadEnvelope         *DocumentsMediaWorkloadEnvelope `json:"workload_envelope,omitempty"`
}

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

func evaluateDocumentsMediaStarter(
	catalog stackkitrelease.UseCaseCatalog,
	catalogErr error,
	baseRequest core.WizardRecommendationRequest,
	inventory WizardRecommendationInventory,
) ProStarterEvaluation {
	env := DefaultDocumentsMediaProEnvelope()
	goals := []string{"files", "photos", "media"}
	settings := cloneUseCaseSettings(baseRequest.UseCaseSettings)
	// Preserve full default Immich (with smart search); never silently downgrade.
	if settings["photos"] == nil {
		settings["photos"] = map[string]any{}
	}
	if _, explicitlySet := settings["photos"]["backend"]; !explicitlySet {
		settings["photos"]["backend"] = "immich"
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
	eval.Alternatives = photosLighterAlternatives(catalog, catalogErr, fit)
	return eval
}

func evaluateFamilyStarter(
	catalog stackkitrelease.UseCaseCatalog,
	catalogErr error,
	baseRequest core.WizardRecommendationRequest,
	inventory WizardRecommendationInventory,
	lanReachable bool,
) ProStarterEvaluation {
	fullGoals := []string{"photos", "files", "media", "vault", "smart-home"}
	cloudGoals := []string{"photos", "files", "media", "vault"}
	settings := cloneUseCaseSettings(baseRequest.UseCaseSettings)

	// 1. Where the full choice (including Smart Home on verified LAN reach)
	// already fits, recommend it first.
	if lanReachable {
		req := baseRequest
		req.Goals = fullGoals
		req.UseCaseSettings = settings
		fit := EvaluateServerFit(req, inventory)
		if fit != nil && fit.Status == FitStatusFits {
			return ProStarterEvaluation{
				ID:              ProStarterFamily,
				RecommendedFull: true,
				StartsWith:      fullGoals,
				DefaultSettings: settings,
				Fit:             fit,
			}
		}
	}

	// 2. Without verified LAN reach (e.g. managed Pro VPS), start with photos +
	// files + playback + vault and preserve smart-home as a deferred expansion.
	req := baseRequest
	req.Goals = cloudGoals
	req.UseCaseSettings = settings
	fit := EvaluateServerFit(req, inventory)

	eval := ProStarterEvaluation{
		ID:              ProStarterFamily,
		RecommendedFull: false,
		StartsWith:      cloudGoals,
		DeferredGoals:   []string{"smart-home"},
		DefaultSettings: settings,
		Fit:             fit,
	}
	if !lanReachable {
		eval.ConfirmationReasons = append(eval.ConfirmationReasons, "smart_home_deferred_lan_reach")
	}
	if fit != nil && fit.Status != FitStatusFits {
		eval.Alternatives = photosLighterAlternatives(catalog, catalogErr, fit)
	}
	return eval
}

func evaluateSelfEmployedStarter(
	catalog stackkitrelease.UseCaseCatalog,
	catalogErr error,
	baseRequest core.WizardRecommendationRequest,
	inventory WizardRecommendationInventory,
) ProStarterEvaluation {
	settings := cloneUseCaseSettings(baseRequest.UseCaseSettings)
	if settings["mail"] == nil {
		settings["mail"] = map[string]any{}
	}
	if _, set := settings["mail"]["hosting"]; !set {
		settings["mail"]["hosting"] = "existing"
	}

	// 1. Check full choice first (files + vault + mail + automation + local AI).
	fullGoals := []string{"files", "vault", "mail", "automation", "ai"}
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
	starts := []string{"files", "vault", "mail"}
	deferred := []string{}
	automationAdmitted := false
	if catalogErr == nil {
		if _, ok := catalog.FindUseCase("automation"); ok {
			if _, fpErr := catalog.WorkloadFootprints("automation", "", "standard", ""); fpErr == nil {
				autoReq := baseRequest
				autoReq.Goals = []string{"files", "vault", "mail", "automation"}
				autoReq.UseCaseSettings = settings
				autoFit := EvaluateServerFit(autoReq, inventory)
				if autoFit != nil && autoFit.Status == FitStatusFits {
					automationAdmitted = true
					starts = autoReq.Goals
				}
			}
		}
	}
	if !automationAdmitted {
		deferred = append(deferred, "automation")
	}
	// Retain local AI for a suitable host (>=12 GiB RAM / 4 vCPU); never
	// silently swap to provider inference.
	deferred = append(deferred, "ai")

	starterReq := baseRequest
	starterReq.Goals = starts
	starterReq.UseCaseSettings = settings
	starterFit := EvaluateServerFit(starterReq, inventory)

	eval := ProStarterEvaluation{
		ID:                  ProStarterSelfEmployed,
		RecommendedFull:     false,
		StartsWith:          starts,
		DeferredGoals:       deferred,
		DefaultSettings:     settings,
		Fit:                 starterFit,
		ConfirmationReasons: []string{"local_ai_requires_suitable_host"},
	}
	if fullFit != nil && fullFit.Recovery != nil && fullFit.Recovery.Action == "switch_offering" {
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
	if fit != nil && fit.Recovery != nil && fit.Recovery.Action == "switch_offering" {
		alts = append(alts, ProStarterAlternative{
			Kind:                 "switch_offering",
			OfferingID:           fit.Recovery.OfferingID,
			OfferingSKU:          fit.Recovery.OfferingSKU,
			RequiresConfirmation: true,
		})
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
