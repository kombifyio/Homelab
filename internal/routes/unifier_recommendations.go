package routes

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/kombifyio/homelab/internal/routes/tenantguard"
	"github.com/kombifyio/homelab/internal/stackkitrelease"
	ksapi "github.com/kombifyio/homelab/pkg/api"
	kscore "github.com/kombifyio/homelab/pkg/core"
	"github.com/kombifyio/homelab/pkg/features"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/jobs"
	"github.com/kombifyio/homelab/pkg/monthlyruntime"
	"github.com/kombifyio/homelab/pkg/specv2"
	"github.com/kombifyio/homelab/pkg/unifier"
)

func (api *UnifierAPI) handleRecommendations(e *httpx.Event) error {
	ownerID, err := requireUnifierAuth(e)
	if err != nil {
		return err
	}
	tenantID, tenantErr := tenantguard.TenantScope(requestExplicitTenantID(e), ownerID, "homelab.unifier.recommendations")
	if tenantErr != nil {
		return tenantErr
	}
	body, tooLarge, readErr := readRequestBodyLimited(e.Request.Body, maxUnifierRequestBodyBytes)
	if readErr != nil {
		return httpx.BadRequest(e, "failed to read recommendation request")
	}
	if tooLarge {
		return httpx.Error(e, http.StatusRequestEntityTooLarge, ksapi.ErrCodeBadRequest, "request body exceeds size limit", nil)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return httpx.BadRequest(e, "recommendation request cannot be empty")
	}

	var envelope struct {
		kscore.WizardRecommendationRequest
		SmartHomeContext  specv2.SmartHomeContext `json:"smart_home_context,omitempty"`
		SmartHomeSettings map[string]any          `json:"smart_home_settings,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decodeErr := decoder.Decode(&envelope); decodeErr != nil {
		return httpx.BadRequest(e, "invalid recommendation request: "+decodeErr.Error())
	}
	var trailing any
	if decodeErr := decoder.Decode(&trailing); decodeErr != io.EOF {
		if decodeErr == nil {
			return httpx.BadRequest(e, "recommendation request must contain one JSON object")
		}
		return httpx.BadRequest(e, "invalid recommendation request: "+decodeErr.Error())
	}
	request := envelope.WizardRecommendationRequest
	// A recommendation never preselects a use case this person cannot
	// receive in full (jobs.UnofferedUseCases); the catalog does not offer it.
	// An unreadable catalog still withholds the Wiki itself.
	offerCatalog, _ := stackkitrelease.ResolveUseCaseCatalog()
	unoffered := jobs.UnofferedUseCases(offerCatalog, features.ManagedWikiInstallEnabled(e.Request.Context(), ownerID, tenantID))
	request.AvailableGoals = slices.DeleteFunc(slices.Clone(request.AvailableGoals), func(goal string) bool {
		return slices.Contains(unoffered, strings.ToLower(strings.TrimSpace(goal)))
	})
	if api.features != nil {
		if memoryEnabled, flagErr := api.features.IsEnabled(e.Request.Context(), "ionos_standard_photo_memory", ownerID); flagErr == nil {
			request.IONOSMemoryEnabled = memoryEnabled
		}
	}
	if request.ServerFit != nil {
		fitEnabled := false
		if api.features != nil {
			var err error
			fitEnabled, err = api.features.IsEnabled(e.Request.Context(), "server_hardware_fit", ownerID)
			if err != nil {
				fitEnabled = false
			}
		}
		if !fitEnabled && !request.IONOSMemoryEnabled {
			request.ServerFit = nil
		}
	}
	if validationErr := validateWizardRecommendationRequest(request); validationErr != "" {
		return httpx.BadRequest(e, validationErr)
	}

	inventory, inventoryErr := api.fetchRecommendationInventory(e.Request.Context(), tenantID, ownerID)
	if inventoryErr != nil {
		// The recommendation remains a useful answer when registry evidence is
		// unavailable, but the authority must expose the degraded state instead
		// of silently treating an empty list as a healthy inventory.
		inventory.State = "unavailable"
	}
	authority := api.recommendations
	if authority == nil {
		var catalog unifier.StackKitCatalog
		if api.engine != nil {
			catalog = api.engine
		}
		authority = unifier.NewWizardRecommendationAuthority(catalog)
	}
	disclosure, disclosureUpdatedAt := api.loadSelfDisclosure(e.Request.Context(), tenantID, ownerID)
	response := authority.Recommend(unifier.WizardRecommendationEvaluation{
		Request:                 request,
		TenantID:                tenantID,
		OwnerID:                 ownerID,
		Inventory:               inventory,
		SelfDisclosure:          disclosure,
		SelfDisclosureUpdatedAt: disclosureUpdatedAt,
	})
	if response.ServerFit != nil && response.ServerFit.Template == "Memory Cube S" &&
		(api.memoryAvailability == nil || api.memoryAvailability.IONOSStandardMemoryAvailable(e.Request.Context()) != nil) {
		response.ServerFit.Status = unifier.FitStatusPending
		response.ServerFit.PendingReason = "provider_package_unavailable"
		response.ServerFit.RefusalClass = unifier.RefusalClassUnavailableProvider
		response.ServerFit.Template, response.ServerFit.SKU, response.ServerFit.Location = "", "", ""
		response.ServerFit.Specs = kscore.WizardServerFitSpecs{}
		response.ServerFit.MemoryPeak = kscore.WizardMemoryPeak{Status: "unknown"}
		response.ServerFit.Deficits = nil
		response.ServerFit.Unknowns = append(response.ServerFit.Unknowns, kscore.WizardServerFitFact{Code: "provider_package_unavailable"})
		response.ServerFit.Recovery = &kscore.WizardServerFitRecovery{Action: "review_selection", ReasonCode: "provider_package_unavailable"}
	}
	if response.ServerFit != nil && response.ServerFit.ProviderID != "" && api.features != nil {
		decision := monthlyruntime.EvaluateManagedRuntimeEntitlement(e.Request.Context(), api.features, ownerID, response.ServerFit.ProviderID)
		if decision.Denied {
			if response.ServerFit.RefusalClass == "" {
				response.ServerFit.RefusalClass = unifier.RefusalClassEntitlement
			}
			response.ServerFit.Advisories = append(response.ServerFit.Advisories, kscore.WizardServerFitFact{Code: "entitlement_required"})
		}
	}
	var smartHome *specv2.SmartHomeRecommendation
	for _, goal := range request.Goals {
		if goal == "smart-home" {
			value := specv2.RecommendSmartHome(envelope.SmartHomeContext, envelope.SmartHomeSettings)
			smartHome = &value
			break
		}
	}
	return httpx.Success(e, http.StatusOK, struct {
		kscore.WizardRecommendationResponse
		SmartHome *specv2.SmartHomeRecommendation `json:"smart_home_recommendation,omitempty"`
	}{response, smartHome})
}

func validateWizardRecommendationRequest(request kscore.WizardRecommendationRequest) string {
	if request.ConcurrentWorkloads < 0 || request.ConcurrentWorkloads > 2 {
		return "concurrent_workloads must be 1 or 2 when specified"
	}
	if len(request.Goals) > 32 {
		return "at most 32 goals may be assessed"
	}
	if host := request.ServerCapacity; host != nil && (host.RAMMB <= 0 || host.RAMMB > 1048576 || host.CPUCores < 0 || host.CPUCores > 1024 || host.DiskGB < 0 || host.DiskGB > 1048576) {
		return "server_capacity must contain a positive bounded ram_mb and non-negative cpu_cores and disk_gb"
	}
	if fit := request.ServerFit; fit != nil {
		provider, offering, server := strings.TrimSpace(fit.ProviderID), strings.TrimSpace(fit.OfferingID), strings.TrimSpace(fit.ServerID)
		source := strings.ToLower(strings.TrimSpace(fit.Source))
		managed := provider != "" || offering != ""
		ownedSource := source == "guard" || source == "ssh" || source == "substrate" || source == "install-command" || source == "existing"
		if managed {
			if provider == "" || offering == "" || server != "" || fit.ObservedCapacity != nil {
				return "server_fit requires either provider_id and offering_id or an owned server target"
			}
		} else if server == "" && !ownedSource {
			return "server_fit requires either provider_id and offering_id or server_id"
		}
		if obs := fit.ObservedCapacity; obs != nil {
			if obs.CPUCores < 0 || obs.CPUCores > 1024 || obs.MemoryMB < 0 || obs.MemoryMB > 1048576 || obs.DiskGB < 0 || obs.DiskGB > 1048576 || obs.FreeDiskGB < 0 || obs.FreeDiskGB > 1048576 {
				return "server_fit observed_capacity must contain non-negative bounded cpu_cores, memory_mb, disk_gb and free_disk_gb"
			}
		}
	}
	if lane := strings.ToLower(strings.TrimSpace(request.DeploymentLane)); lane != "saas" && lane != "self-hosted" {
		return "deployment_lane must be saas or self-hosted"
	}
	if surface := strings.ToLower(strings.TrimSpace(request.Surface)); surface != "easy" && surface != "techie" {
		return "surface must be easy or techie"
	}
	return ""
}
