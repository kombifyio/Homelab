package unifier

import (
	"sort"
	"strings"
	"time"

	"github.com/kombifyio/homelab/pkg/core"
	"github.com/kombifyio/homelab/pkg/monthlyruntime"
)

const (
	wizardRecommendationDeploymentSaaS       = "saas"
	wizardRecommendationDeploymentSelfHosted = "self-hosted"
	wizardRecommendationSurfaceEasy          = "easy"
	wizardRecommendationSurfaceTechie        = "techie"
	wizardRecommendationInventoryFresh       = "fresh"
	wizardRecommendationInventoryStale       = "stale"
	wizardRecommendationInventoryUnavailable = "unavailable"
)

// WizardRecommendationInventory is the server-owned environment evidence
// used by the recommendation authority. The route populates it from the
// canonical tenant worker registry after applying the authenticated owner
// scope; callers cannot provide this structure through the public JSON API.
type WizardRecommendationInventory struct {
	Workers        []core.Worker
	StaleWorkerIDs map[string]bool
	State          string
	Source         string
	SnapshotID     string
	ObservedAt     time.Time
	StaleInputs    []string
}

// WizardRecommendationEvaluation is the complete server-side input bundle
// for one Wizard recommendation. Tenant and owner are included here so the
// resulting DecisionContext is bound to the authenticated scope.
type WizardRecommendationEvaluation struct {
	Request   core.WizardRecommendationRequest
	TenantID  string
	OwnerID   string
	Inventory WizardRecommendationInventory
	// SelfDisclosure is the authenticated subject's persisted voluntary
	// self-disclosure, loaded by the route from the tenant store. When it
	// answers the experience question it decides the operator profile.
	SelfDisclosure          *core.OperatorSelfDisclosure
	SelfDisclosureUpdatedAt time.Time
	Now                     time.Time
}

// StackKitCatalog is the existing StackKit catalog boundary. Engine satisfies
// this interface, so recommendations reuse the configured/pinned StackKits
// catalog and StackKitResolver instead of introducing another CUE authority.
type StackKitCatalog interface {
	ListStackKits() []string
}

// WizardRecommendationAuthority evaluates Wizard answers against the shared
// StackKitResolver and catalog. It produces explainable ranked choices even
// when the environment evidence is stale, while making that state explicit.
type WizardRecommendationAuthority struct {
	resolver      *StackKitResolver
	catalogSource string
}

// NewWizardRecommendationAuthority creates the recommendation authority from
// the existing StackKit catalog. A nil catalog uses pinned release metadata.
// An explicit empty catalog remains empty.
func NewWizardRecommendationAuthority(catalog StackKitCatalog) *WizardRecommendationAuthority {
	available := []string(nil)
	source := "pinned-stackkits-metadata"
	if catalog != nil {
		available = append([]string{}, catalog.ListStackKits()...)
		source = "stackkits-catalog"
	}
	return &WizardRecommendationAuthority{
		resolver:      NewStackKitResolver(available),
		catalogSource: source,
	}
}

// Recommend evaluates one server-scoped Wizard request. The response is
// intentionally usable for incomplete or stale states: its status and input
// arrays tell the caller whether it is safe to act on the ranked result.
func (a *WizardRecommendationAuthority) Recommend(input WizardRecommendationEvaluation) core.WizardRecommendationResponse {
	now := input.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	req := normalizeWizardRecommendationRequest(input.Request)
	response := core.WizardRecommendationResponse{
		Status:          core.WizardRecommendationReady,
		GeneratedAt:     now,
		CatalogSource:   a.catalogSource,
		MissingInputs:   []string{},
		StaleInputs:     append([]string{}, input.Inventory.StaleInputs...),
		Recommendations: []core.WizardRecommendation{},
	}
	if response.CatalogSource == "" {
		response.CatalogSource = "embedded-known-stackkits"
	}

	if req.ServerFit != nil {
		response.ServerFit = EvaluateServerFit(req, input.Inventory)
	}
	starterReq := req
	starterLANReachable := false
	if starterReq.ServerFit == nil {
		if starterReq.DeploymentLane == wizardRecommendationDeploymentSaaS && starterReq.ProviderID != "" {
			offeringID := strings.TrimSpace(starterReq.RuntimeOfferingID)
			if offeringID == "" {
				offeringID = string(monthlyruntime.DefaultOfferingID())
			}
			starterReq.ServerFit = &core.WizardServerFitRequest{
				ProviderID: starterReq.ProviderID,
				OfferingID: offeringID,
				Source:     "managed",
			}
		} else if worker, ok := bestOnlineObservedWorker(input.Inventory); ok {
			starterReq.ServerFit = &core.WizardServerFitRequest{
				ServerID: firstNonEmptyRecommendation(worker.ServerID, worker.ID),
				Source:   "guard",
			}
			starterLANReachable = hasOwnedLANReachable(worker)
		}
	} else {
		source := strings.ToLower(strings.TrimSpace(starterReq.ServerFit.Source))
		isManagedTarget := strings.TrimSpace(starterReq.ServerFit.ProviderID) != "" ||
			strings.TrimSpace(starterReq.ServerFit.OfferingID) != "" ||
			source == "managed" ||
			source == "managed-runtime"
		if !isManagedTarget {
			if worker, found := findOwnedWorker(starterReq.ServerFit, input.Inventory.Workers); found {
				starterLANReachable = hasOwnedLANReachable(worker)
			}
		}
	}
	response.ProStarters = EvaluateProStarters(starterReq, input.Inventory, starterLANReachable)
	response.Selection = GuideWizardSelection(req, input)
	response.Capacity = AssessWizardCapacity(req)
	spec := wizardRecommendationSpec(req)
	decisionContext := buildWizardRecommendationDecisionContext(req, input, spec, starterLANReachable)
	response.DecisionContext = decisionContext
	response.DecisionContextHash = HashDecisionContext(decisionContext)

	if len(req.Goals) == 0 && len(req.Services) == 0 {
		response.Status = core.WizardRecommendationIncomplete
		response.MissingInputs = []string{"goals", "services"}
		return response
	}

	resolver := a.resolver
	if resolver == nil {
		response.Status = core.WizardRecommendationDegraded
		response.StaleInputs = appendUniqueString(response.StaleInputs, "stackkit_catalog")
		return response
	}
	resolved := resolver.Resolve(spec)
	trace := BuildDecisionTrace(spec, decisionContext, resolved, nil)
	response.Recommendations = wizardRecommendations(req, resolved, trace, response.CatalogSource)
	capacityReasons, unknownCapacity := catalogCapacityReasons(req, input.Inventory)
	for i := range response.Recommendations {
		response.Recommendations[i].Reasons = append(response.Recommendations[i].Reasons, capacityReasons...)
	}

	switch {
	case unknownCapacity:
		response.Status = core.WizardRecommendationDegraded
		response.StaleInputs = appendUniqueString(response.StaleInputs, "module_capacity")
	case strings.EqualFold(strings.TrimSpace(input.Inventory.State), wizardRecommendationInventoryUnavailable):
		response.Status = core.WizardRecommendationDegraded
		response.StaleInputs = appendUniqueString(response.StaleInputs, "worker_inventory")
	case wizardRecommendationInventoryIsStale(input.Inventory):
		response.Status = core.WizardRecommendationStale
		response.StaleInputs = appendUniqueString(response.StaleInputs, "worker_inventory")
	case resolved == nil || !resolved.Valid:
		response.Status = core.WizardRecommendationDegraded
	}
	return response
}

func normalizeWizardRecommendationRequest(request core.WizardRecommendationRequest) core.WizardRecommendationRequest {
	request.Goals = normalizeWizardRecommendationValues(request.Goals)
	request.Services = normalizeWizardRecommendationValues(request.Services)
	request.DeploymentLane = strings.ToLower(strings.TrimSpace(request.DeploymentLane))
	if request.DeploymentLane != wizardRecommendationDeploymentSaaS && request.DeploymentLane != wizardRecommendationDeploymentSelfHosted {
		request.DeploymentLane = wizardRecommendationDeploymentSelfHosted
	}
	request.ProviderID = strings.TrimSpace(request.ProviderID)
	if request.DeploymentLane != wizardRecommendationDeploymentSaaS ||
		(request.ProviderID != providerCentron && request.ProviderID != providerIONOS) {
		request.ProviderID = ""
	}
	request.AvailableGoals = normalizeWizardRecommendationValues(request.AvailableGoals)
	request.SelectionIntent = strings.ToLower(strings.TrimSpace(request.SelectionIntent))
	request.ComputeProfile = strings.TrimSpace(request.ComputeProfile)
	request.Surface = strings.ToLower(strings.TrimSpace(request.Surface))
	if request.Surface != wizardRecommendationSurfaceTechie {
		request.Surface = wizardRecommendationSurfaceEasy
	}
	return request
}

func normalizeWizardRecommendationValues(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	return result
}

func wizardRecommendationSpec(request core.WizardRecommendationRequest) *core.KombinationSpec {
	provider := request.ProviderID
	if provider == "" {
		if request.DeploymentLane == wizardRecommendationDeploymentSaaS {
			provider = "centron"
		} else {
			provider = "local"
		}
	}
	metadata := map[string]string{
		"created_by":      "wizard",
		"wizard_type":     request.Surface,
		"deployment_lane": request.DeploymentLane,
		"provider_id":     request.ProviderID,
		"use_cases":       strings.Join(request.Goals, ","),
	}
	if request.ComputeProfile != "" {
		metadata["compute_tier"] = request.ComputeProfile
	}
	services := make([]core.ServiceSpec, 0, len(request.Services))
	for _, service := range request.Services {
		services = append(services, core.ServiceSpec{Name: service, Type: service})
	}
	return &core.KombinationSpec{
		Name:     "wizard-recommendation",
		Nodes:    []core.NodeSpec{{Name: "foundation", Type: "main", Provider: provider}},
		Services: services,
		Metadata: metadata,
	}
}

func buildWizardRecommendationDecisionContext(request core.WizardRecommendationRequest, input WizardRecommendationEvaluation, spec *core.KombinationSpec, lanReachable bool) *core.DecisionContext {
	observedAt := input.Inventory.ObservedAt.UTC()
	context := &core.DecisionContext{
		Channel: "wizard:" + request.Surface,
		Source:  "wizard-recommendation",
		Constraints: []core.DecisionConstraint{
			{Key: "deployment_lane", Value: request.DeploymentLane, Source: "wizard", Required: true},
			{Key: "surface", Value: request.Surface, Source: "wizard", Required: true},
		},
		Environment: &core.EnvironmentInventory{
			SnapshotID: input.Inventory.SnapshotID,
			ObservedAt: observedAt,
			Source:     firstNonEmptyRecommendation(input.Inventory.Source, "worker-registry"),
		},
	}
	if context.Environment.SnapshotID == "" {
		context.Environment.SnapshotID = "worker-registry"
	}
	if request.ProviderID != "" {
		context.Constraints = append(context.Constraints, core.DecisionConstraint{Key: "provider_id", Value: request.ProviderID, Source: "wizard"})
	}
	if strings.TrimSpace(request.RuntimeOfferingID) != "" {
		context.Constraints = append(context.Constraints, core.DecisionConstraint{Key: "runtime_offering_id", Value: strings.TrimSpace(request.RuntimeOfferingID), Source: "wizard"})
	}
	if lanReachable {
		context.Constraints = append(context.Constraints, core.DecisionConstraint{Key: "lan_reachable", Value: "true", Source: "worker-registry"})
	} else {
		context.Constraints = append(context.Constraints, core.DecisionConstraint{Key: "lan_reachable", Value: "false", Source: "worker-registry"})
	}
	if request.IONOSMemoryEnabled {
		context.Constraints = append(context.Constraints, core.DecisionConstraint{Key: "ionos_memory_enabled", Value: "true", Source: "feature"})
	}
	if request.OfferingEntitlementChecked || len(request.AllowedOfferings) > 0 {
		offerings := append([]string(nil), request.AllowedOfferings...)
		sort.Strings(offerings)
		context.Constraints = append(context.Constraints, core.DecisionConstraint{Key: "allowed_offerings", Value: strings.Join(offerings, ","), Source: "entitlement", Required: true})
	}
	if request.ProviderAdmissionChecked || len(request.AdmittedProviders) > 0 {
		providers := append([]string(nil), request.AdmittedProviders...)
		sort.Strings(providers)
		context.Constraints = append(context.Constraints, core.DecisionConstraint{Key: "admitted_providers", Value: strings.Join(providers, ","), Source: "entitlement", Required: true})
	}
	if request.ServerFit != nil {
		fitTarget := strings.Join([]string{
			strings.TrimSpace(request.ServerFit.Source),
			strings.TrimSpace(request.ServerFit.ProviderID),
			strings.TrimSpace(request.ServerFit.OfferingID),
			strings.TrimSpace(request.ServerFit.ServerID),
			strings.TrimSpace(request.ServerFit.Host),
		}, ":")
		context.Constraints = append(context.Constraints, core.DecisionConstraint{Key: "server_fit_target", Value: fitTarget, Source: "wizard"})
	}
	context.Operator = OperatorCapabilityFromSelfDisclosure(input.SelfDisclosure, input.SelfDisclosureUpdatedAt)
	context.Constraints = append(context.Constraints, selfDisclosureConstraints(input.SelfDisclosure)...)
	for _, goal := range request.Goals {
		context.Constraints = append(context.Constraints, core.DecisionConstraint{Key: "goal", Value: goal, Source: "wizard", Required: true})
	}
	for _, service := range request.Services {
		context.Constraints = append(context.Constraints, core.DecisionConstraint{Key: "service", Value: service, Source: "wizard", Required: true})
	}
	if strings.TrimSpace(input.TenantID) != "" {
		context.Constraints = append(context.Constraints, core.DecisionConstraint{Key: "tenant_scope", Value: strings.TrimSpace(input.TenantID), Source: "tenantguard", Required: true})
	}
	if strings.TrimSpace(input.OwnerID) != "" {
		context.Constraints = append(context.Constraints, core.DecisionConstraint{Key: "owner_scope", Value: strings.TrimSpace(input.OwnerID), Source: "authenticated-subject", Required: true})
	}
	context.Environment.NetworkSignals = append(context.Environment.NetworkSignals, core.EnvironmentSignal{
		Key: "inventory_state", Value: firstNonEmptyRecommendation(input.Inventory.State, wizardRecommendationInventoryStale), Source: "worker-registry",
	})
	workers := append([]core.Worker(nil), input.Inventory.Workers...)
	sort.SliceStable(workers, func(i, j int) bool { return workers[i].ID < workers[j].ID })
	for _, worker := range workers {
		status := strings.TrimSpace(worker.Status)
		if status == "" {
			status = "unknown"
		}
		context.Environment.ComputeNodes = append(context.Environment.ComputeNodes, core.EnvironmentComputeNode{
			ID: worker.ID, Name: worker.Name, Role: worker.Type, Provider: worker.Provider,
			Status: status, CPU: worker.Capabilities.CPU, RAMMB: worker.Capabilities.RAM,
			DiskGB: worker.Capabilities.Disk, Volumes: append([]core.StorageVolume(nil), worker.Capabilities.Volumes...),
			Arch: worker.Capabilities.Arch, OS: worker.Capabilities.OS,
			Source: "worker-registry", Tags: cloneRecommendationTags(worker.Tags),
			GPU: worker.Capabilities.GPU, AMD64MicroarchitectureLevel: worker.Capabilities.AMD64MicroarchitectureLevel,
		})
	}
	return BuildDecisionContext(spec, context)
}

func wizardRecommendations(request core.WizardRecommendationRequest, resolved *ResolveResult, trace *core.DecisionTrace, catalogSource string) []core.WizardRecommendation {
	if resolved == nil || strings.TrimSpace(resolved.StackKit) == "" {
		return []core.WizardRecommendation{}
	}
	selected := CanonicalStackKitName(resolved.StackKit)
	selectedScore := resolved.Confidence * 100
	if selectedScore < 0 {
		selectedScore = 0
	}
	if selectedScore > 100 {
		selectedScore = 100
	}
	reasons := make([]core.WizardRecommendationReason, 0)
	if trace != nil {
		for _, reason := range trace.Reasons {
			reasons = appendUniqueRecommendationReason(reasons, core.WizardRecommendationReason(reason))
		}
	}
	if len(request.Goals) > 0 {
		reasons = appendUniqueRecommendationReason(reasons, core.WizardRecommendationReason{Code: "goal_alignment", Message: "The recommendation evaluates the selected goals against the StackKit decision rules.", Source: "wizard"})
	}
	if len(request.Services) > 0 {
		reasons = appendUniqueRecommendationReason(reasons, core.WizardRecommendationReason{Code: "service_alignment", Message: "The recommendation evaluates the selected services against the StackKit decision rules.", Source: "wizard"})
	}
	if request.ProviderID != "" {
		reasons = appendUniqueRecommendationReason(reasons, core.WizardRecommendationReason{Code: "provider_alignment", Message: "The selected provider is included in the StackKit evaluation.", Source: "wizard"})
	}
	if request.DeploymentLane == wizardRecommendationDeploymentSaaS && request.ProviderID == "" {
		reasons = appendUniqueRecommendationReason(reasons, core.WizardRecommendationReason{
			Code: "safe_standard", Message: "The supported managed-cloud standard is used until a provider preference is available.", Source: "standard-bundle",
		})
	}
	alternatives := make([]string, 0, len(resolved.AlternativeKits))
	for _, kit := range resolved.AlternativeKits {
		kit = CanonicalStackKitName(kit)
		if kit != "" && kit != selected && !containsString(alternatives, kit) {
			alternatives = append(alternatives, kit)
		}
	}
	recommendations := []core.WizardRecommendation{{
		ID: "stackkit:" + selected, StackKit: selected, Rank: 1, Score: selectedScore, Recommended: true,
		Reasons: reasons, Alternatives: alternatives,
		DeepLink: core.WizardRecommendationDeepLink{Step: "node", Section: "stackkit"},
	}}
	for index, kit := range alternatives {
		score := selectedScore - float64(index+1)*10
		if score < 0 {
			score = 0
		}
		recommendations = append(recommendations, core.WizardRecommendation{
			ID: "stackkit:" + kit, StackKit: kit, Rank: index + 2, Score: score,
			Recommended:  false,
			Reasons:      []core.WizardRecommendationReason{{Code: "resolver_alternative", Message: "The shared StackKit resolver identified this as a compatible alternative.", Source: catalogSource}},
			Alternatives: []string{selected},
			DeepLink:     core.WizardRecommendationDeepLink{Step: "node", Section: "stackkit"},
		})
	}
	return recommendations
}

func wizardRecommendationInventoryIsStale(inventory WizardRecommendationInventory) bool {
	state := strings.ToLower(strings.TrimSpace(inventory.State))
	if state == wizardRecommendationInventoryStale || state == wizardRecommendationInventoryUnavailable {
		return true
	}
	if len(inventory.StaleInputs) > 0 {
		return true
	}
	if state == wizardRecommendationInventoryFresh {
		return false
	}
	return len(inventory.Workers) == 0
}

func appendUniqueRecommendationReason(reasons []core.WizardRecommendationReason, candidate core.WizardRecommendationReason) []core.WizardRecommendationReason {
	for _, reason := range reasons {
		if reason.Code == candidate.Code {
			return reasons
		}
	}
	return append(reasons, candidate)
}

func appendUniqueString(values []string, candidate string) []string {
	for _, value := range values {
		if value == candidate {
			return values
		}
	}
	return append(values, candidate)
}

func containsString(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func firstNonEmptyRecommendation(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func isOwnedNonManagedWorker(worker core.Worker) bool {
	return !strings.EqualFold(strings.TrimSpace(worker.Tags["source"]), "managed-runtime") &&
		strings.TrimSpace(worker.Tags["lease_id"]) == ""
}

func isOwnedUnboundWorker(worker core.Worker) bool {
	return isOwnedNonManagedWorker(worker) &&
		strings.TrimSpace(worker.Tags["stack_id"]) == "" &&
		strings.TrimSpace(worker.StackID) == ""
}

func bestOnlineObservedWorker(inventory WizardRecommendationInventory) (core.Worker, bool) {
	if strings.EqualFold(strings.TrimSpace(inventory.State), wizardRecommendationInventoryUnavailable) {
		return core.Worker{}, false
	}
	var (
		best  core.Worker
		found bool
	)
	for _, worker := range inventory.Workers {
		if inventory.StaleWorkerIDs[worker.ID] || (worker.ServerID != "" && inventory.StaleWorkerIDs[worker.ServerID]) {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(worker.Status), "online") || !worker.UsageObserved || worker.Capabilities.RAM <= 0 {
			continue
		}
		if !isOwnedUnboundWorker(worker) {
			continue
		}
		if !found || worker.Capabilities.RAM > best.Capabilities.RAM ||
			(worker.Capabilities.RAM == best.Capabilities.RAM && firstNonEmptyRecommendation(worker.ServerID, worker.ID) < firstNonEmptyRecommendation(best.ServerID, best.ID)) {
			best, found = worker, true
		}
	}
	return best, found
}

func cloneRecommendationTags(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}
