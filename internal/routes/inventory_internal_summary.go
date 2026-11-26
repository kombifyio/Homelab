package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kombifyio/homelab/internal/gocommon/denial"
	"github.com/kombifyio/homelab/internal/gocommon/servicecall"
	"github.com/kombifyio/homelab/internal/stackkitrelease"
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/jobs"
	"github.com/kombifyio/homelab/pkg/serviceauth"
	"github.com/kombifyio/homelab/pkg/specv2"
)

const (
	runtimeSummaryService     = "homelab"
	runtimeSummaryCallerCloud = "cloud"
	runtimeSummaryErrorPrefix = "homelab.runtime_summary."
	runtimeSummaryPageSize    = 50
)

// runtimeSummary is the private Cloud read model behind the dashboard
// Overview: the caller's servers and services in one round trip. It is a pure
// projection of the canonical inventory read model with the same sanitized
// shapes. Authorization differs on purpose: a verified active membership
// reads the signed owner's own rows (ownerInventoryReadPolicy), without the
// agent inventory entitlement and FGA surface relation that /api/v1/inventory
// requires; it never reaches another owner's rows.
type runtimeSummary struct {
	Version     string `json:"version"`
	GeneratedAt string `json:"generated_at"`
	// "ok" means authorization succeeded, including an actually empty inventory.
	// Missing authorization is not evidence of an empty collection.
	ScopeState string               `json:"scope_state"`
	Servers    inventoryServerList  `json:"servers"`
	Services   inventoryServiceList `json:"services"`
	// Nil means the desired configuration could not be read, not that it is empty.
	ConfiguredUseCases []configuredUseCase `json:"configured_use_cases"`
}

type configuredUseCase struct {
	Goal        string         `json:"goal"`
	Status      string         `json:"status"`
	Settings    map[string]any `json:"settings,omitempty"`
	PlannedApps []plannedApp   `json:"planned_apps,omitempty"`
	// Failure is present only for a failed or blocked use case whose cause
	// Homelab recorded. It carries closed reason vocabulary and identifiers,
	// never backend prose, hosts, paths or log lines.
	Failure *useCaseFailure `json:"failure,omitempty"`
}

// useCaseFailure is the machine-readable why behind a failed or blocked
// configured use case. Code is the job's recorded reason_code (the run's
// error_reason when no job was dispatched; "capability_blocked" or
// "goal_unmapped" for a blocked goal). DetailCode is the pinned StackKits
// reason code when one was reported. Step is the failed rollout step (or the
// blocking capability). OperationID is the rollout job id, or the run id when
// no job exists. The affected application is not recorded per use case, so
// there is no app field; Cloud maps the goal to its planned apps.
type useCaseFailure struct {
	Code        string `json:"code"`
	DetailCode  string `json:"detail_code,omitempty"`
	Step        string `json:"step,omitempty"`
	OccurredAt  string `json:"occurred_at,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
	Retryable   bool   `json:"retryable"`
}

// failureCodePattern keeps only closed identifiers such as "provision_failed"
// or "use-case-access"; anything else is dropped.
var failureCodePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

func failureToken(value any) string {
	text, _ := value.(string)
	text = strings.TrimSpace(text)
	if failureCodePattern.MatchString(text) {
		return text
	}
	return ""
}

func failureTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

// jobFailure projects the failed job's recorded outcome (job.Step and the
// Result keys reason_code, retryable and support_context.{failed_step,
// stackkit_reason_code} written by pkg/jobs/job_outcome.go) into the wire shape.
func jobFailure(job *controlplane.Job) *useCaseFailure {
	failure := &useCaseFailure{
		Code:        failureToken(job.Result["reason_code"]),
		Step:        failureToken(job.Step),
		OperationID: job.ID,
	}
	if job.CompletedAt != nil {
		failure.OccurredAt = failureTime(*job.CompletedAt)
	}
	if failure.OccurredAt == "" {
		failure.OccurredAt = failureTime(job.UpdatedAt)
	}
	failure.Retryable, _ = job.Result["retryable"].(bool)
	support, _ := job.Result["support_context"].(map[string]any)
	failure.DetailCode = failureToken(support["stackkit_reason_code"])
	if failure.Step == "" {
		failure.Step = failureToken(support["failed_step"])
	}
	if failure.Code == "" {
		failure.Code = "job_" + failureToken(job.State)
	}
	return failure
}

type plannedApp struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type configuredWizardIntent struct {
	Goals               []string                  `json:"goals"`
	UnmappedGoals       []string                  `json:"unmapped_goals"`
	UseCaseSettings     map[string]map[string]any `json:"use_case_settings"`
	UseCaseSetupAnswers json.RawMessage           `json:"use_case_setup_answers"`
	LastRunID           string                    `json:"last_run_id"`
	BlockedCapabilities []struct {
		UseCase    string `json:"use_case"`
		Capability string `json:"capability"`
	} `json:"blocked_capabilities"`
}

func (h inventoryHandlers) configuredUseCases(e *httpx.Event, ctx context.Context, scope inventoryScope) []configuredUseCase {
	if h.app.homelabs == nil {
		httpx.RecordDependencyFailure(e, "runtime_summary.homelab", errors.New("homelab authority unavailable"))
		return nil
	}
	homelab, err := h.app.homelabs.GetHomelabByOwner(ctx, scope.tenantID, scope.ownerID)
	if errors.Is(err, controlplane.ErrNotFound) {
		return []configuredUseCase{}
	}
	if err != nil || homelab == nil {
		if err == nil {
			err = errors.New("homelab authority returned no result")
		}
		httpx.RecordDependencyFailure(e, "runtime_summary.homelab", err)
		return nil
	}
	// The in-memory store retains typed slices and maps while Postgres returns
	// JSON values. Normalize both so the projection has identical semantics.
	var wizard configuredWizardIntent
	raw, err := json.Marshal(homelab.Intent["wizard"])
	if err != nil || json.Unmarshal(raw, &wizard) != nil {
		httpx.RecordDependencyFailure(e, "runtime_summary.homelab", errors.New("invalid wizard intent"))
		return nil
	}
	var setupAnswers map[string]map[string][]string
	// Malformed legacy answers must not hide otherwise valid goals and status.
	_ = json.Unmarshal(wizard.UseCaseSetupAnswers, &setupAnswers)
	goals := make([]configuredUseCase, 0, len(wizard.Goals))
	if len(wizard.Goals) == 0 {
		return goals
	}
	blocked := map[string]bool{}
	blockedFailure := map[string]*useCaseFailure{}
	blockedAt := failureTime(homelab.UpdatedAt)
	for _, goal := range wizard.UnmappedGoals {
		blocked[goal] = true
		blockedFailure[goal] = &useCaseFailure{Code: "goal_unmapped", Step: "intent_projection", OccurredAt: blockedAt}
	}
	for _, capability := range wizard.BlockedCapabilities {
		blocked[capability.UseCase] = true
		blockedFailure[capability.UseCase] = &useCaseFailure{Code: "capability_blocked", Step: failureToken(capability.Capability), OccurredAt: blockedAt}
	}
	runGoals, runStatus, runFailure := h.latestConfiguredRun(e, ctx, scope, homelab.ID, wizard.LastRunID)
	catalog, err := stackkitrelease.ResolveUseCaseCatalog()
	if err != nil {
		httpx.RecordDependencyFailure(e, "runtime_summary.use_case_catalog", err)
	}
	workloads, specsKnown := h.ownerDesiredWorkloads(e, ctx, scope, homelab.ID)
	for _, goal := range wizard.Goals {
		if strings.TrimSpace(goal) == "" {
			httpx.RecordDependencyFailure(e, "runtime_summary.homelab", errors.New("invalid wizard goal"))
			return nil
		}
		item := configuredUseCase{Goal: goal, Status: "unknown"}
		if blocked[goal] {
			item.Status = "blocked"
			item.Failure = blockedFailure[goal]
		} else if runGoals[goal] {
			item.Status = runStatus
			if runStatus == "failed" {
				item.Failure = runFailure
			}
		}
		// Saved Wizard goals are historical. Only discard one when the current
		// catalog and every owned spec establish that none of its native
		// workloads is desired. Preserve unresolved and in-flight decisions.
		if item.Status == "unknown" && !runGoals[goal] && specsKnown && err == nil {
			if useCase, ok := catalog.FindUseCase(goal); ok {
				refs := []string{useCase.WorkloadRef}
				for _, addon := range useCase.AddOns {
					refs = append(refs, addon.ID)
				}
				known, present := false, false
				for _, ref := range refs {
					if ref != "" {
						known = true
						_, found := workloads[ref]
						present = present || found
					}
				}
				if known && !present {
					continue
				}
			}
		}
		if err == nil {
			if useCase, ok := catalog.FindUseCase(goal); ok {
				item.Settings = configuredSettings(useCase, wizard.UseCaseSettings[goal], setupAnswers[goal])
				item.PlannedApps = configuredPlannedApps(useCase, workloads)
			}
		}
		goals = append(goals, item)
	}
	return goals
}

func (h inventoryHandlers) ownerDesiredWorkloads(e *httpx.Event, ctx context.Context, scope inventoryScope, homelabID string) (map[string]map[string]bool, bool) {
	if h.ownerStackSpecs == nil {
		return nil, false
	}
	stacks, err := h.ownerStackSpecs.ListOwnerHomelabSpecs(ctx, scope.tenantID, scope.ownerID, homelabID)
	if err != nil {
		httpx.RecordDependencyFailure(e, "runtime_summary.desired_specs", err)
		return nil, false
	}
	// A missing deployment is not evidence that earlier intent was removed.
	if len(stacks) == 0 {
		return nil, false
	}
	workloads := map[string]map[string]bool{}
	complete := true
	for _, stack := range stacks {
		if stack.HomelabID != homelabID {
			complete = false
			continue
		}
		spec, ok := stack.Spec.(map[string]any)
		if !ok || specv2.RequireCanonicalV2(spec) != nil {
			complete = false
			continue
		}
		selected, ok := spec["workloads"].(map[string]any)
		if !ok {
			complete = false
			continue
		}
		for ref, raw := range selected {
			if workloads[ref] == nil {
				workloads[ref] = map[string]bool{}
			}
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if enabled, exists := entry["enabled"]; exists && enabled != true {
				continue
			}
			if alternative, ok := entry["alternative"].(string); ok && alternative != "" {
				workloads[ref][alternative] = true
			}
		}
	}
	return workloads, complete
}

// Only the guide's extras groups carry positive toggle IDs. An absent ID
// is not a negative answer, and explicit validated scalar values win.
var configuredExtrasGroup = regexp.MustCompile(`^extras(?:-[0-9]+)?$`)

func configuredSettings(useCase stackkitrelease.UseCase, saved map[string]any, answers map[string][]string) map[string]any {
	settings := map[string]any{}
	for id, value := range saved {
		setting, ok := useCase.FindSetting(id)
		if ok && (setting.Kind == "choice" || setting.Kind == "toggle") && setting.AcceptsSettingValue(value) {
			settings[id] = value
		}
	}
	for id, values := range answers {
		if setting, ok := useCase.FindSetting(id); ok && setting.Kind == "choice" && len(values) == 1 && setting.AcceptsSettingValue(values[0]) {
			if _, exists := settings[id]; !exists {
				settings[id] = values[0]
			}
		}
		if !configuredExtrasGroup.MatchString(id) {
			continue
		}
		for _, value := range values {
			setting, ok := useCase.FindSetting(value)
			if !ok || setting.Kind != "toggle" {
				continue
			}
			if _, exists := settings[value]; !exists {
				settings[value] = true
			}
		}
	}
	return settings
}

func configuredPlannedApps(useCase stackkitrelease.UseCase, workloads map[string]map[string]bool) []plannedApp {
	seen := map[string]plannedApp{}
	candidates := []struct {
		ref          string
		alternatives []stackkitrelease.UseCaseAlternative
	}{{useCase.WorkloadRef, useCase.Alternatives}}
	for _, addon := range useCase.AddOns {
		candidates = append(candidates, struct {
			ref          string
			alternatives []stackkitrelease.UseCaseAlternative
		}{addon.ID, addon.Alternatives})
	}
	for _, candidate := range candidates {
		if candidate.ref == "" {
			continue
		}
		for _, alternative := range candidate.alternatives {
			if alternative.ID == "" || !workloads[candidate.ref][alternative.ID] {
				continue
			}
			name := alternative.ID
			for _, component := range useCase.Components {
				if component.ID == alternative.ID && strings.TrimSpace(component.Name) != "" {
					name = component.Name
					break
				}
			}
			seen[alternative.ID] = plannedApp{ID: alternative.ID, Name: name}
		}
	}
	result := make([]plannedApp, 0, len(seen))
	for _, app := range seen {
		result = append(result, app)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// Attribute a job only to the exact latest saved run and its explicit goals.
// Multiple-job changes and missing or uncertain replacement jobs stay unknown.
func (h inventoryHandlers) latestConfiguredRun(e *httpx.Event, ctx context.Context, scope inventoryScope, homelabID, lastID string) (map[string]bool, string, *useCaseFailure) {
	goals := map[string]bool{}
	if h.wizardRuns == nil || lastID == "" {
		return goals, "unknown", nil
	}
	run, err := h.wizardRuns.GetLatestWizardRunByOwner(ctx, scope.tenantID, scope.ownerID)
	if err != nil {
		if !errors.Is(err, controlplane.ErrNotFound) {
			httpx.RecordDependencyFailure(e, "runtime_summary.wizard_run", err)
		}
		return goals, "unknown", nil
	}
	if run == nil || run.ID != lastID || run.HomelabID != homelabID || run.StackID == "" {
		return goals, "unknown", nil
	}
	intent, _ := run.Intent["intent"].(map[string]any)
	values, _ := intent["goals"].([]any)
	for _, value := range values {
		if goal, ok := value.(string); ok {
			goals[goal] = true
		}
	}
	if len(goals) == 0 {
		return goals, "unknown", nil
	}
	if run.JobID == "" {
		if run.ErrorReason == "projection_pending" || run.ErrorReason == controlplane.WizardIntentProjectionPending {
			return goals, "pending", nil
		}
		if run.Status == "failed" {
			failure := &useCaseFailure{Code: failureToken(run.ErrorReason), OperationID: run.ID, OccurredAt: failureTime(run.UpdatedAt)}
			if failure.Code == "" {
				failure.Code = "run_failed"
			}
			return goals, "failed", failure
		}
		return goals, "unknown", nil
	}
	if h.jobs == nil {
		return goals, "unknown", nil
	}
	if raw := run.Result["job_ids"]; raw != nil {
		encoded, err := json.Marshal(raw)
		var ids []string
		if err != nil || json.Unmarshal(encoded, &ids) != nil || len(ids) > 1 || len(ids) == 1 && ids[0] != run.JobID {
			return goals, "unknown", nil
		}
	}
	id := run.JobID
	for hop := 0; hop < 5; hop++ {
		job, err := h.jobs.GetJob(ctx, scope.tenantID, id)
		if err != nil || job == nil || job.StackID != run.StackID {
			if err != nil {
				httpx.RecordDependencyFailure(e, "runtime_summary.wizard_job", err)
			}
			return goals, "unknown", nil
		}
		if next := jobs.SupersededByJobID(job.ID, job.Result); next != "" {
			id = next
			continue
		}
		switch job.State {
		case "pending", "waiting", "running":
			return goals, "pending", nil
		case "failed", "canceled", "cancelled":
			return goals, "failed", jobFailure(job)
		default:
			return goals, "unknown", nil
		}
	}
	return goals, "unknown", nil
}

// cloudReadSurface names one private Cloud servicecall read model and the
// InternalErrorEnvelope (homelab-internal-v1.yaml) its denials carry.
type cloudReadSurface struct {
	errorPrefix       string
	unavailableReason string
	title             string
	body              string
	// queryKeys are the only query parameters the surface accepts, each once.
	// They carry a read option (a range, a limit), never scope: a principal
	// field is rejected like any other unknown key. Empty means no query.
	queryKeys []string
}

// queryAllowed reports whether the request carries no query at all or only the
// surface's own read options, each exactly once.
func (s cloudReadSurface) queryAllowed(request *http.Request) bool {
	if request.URL.RawQuery == "" {
		return true
	}
	values, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		return false
	}
	for key, found := range values {
		if len(found) != 1 || !slices.Contains(s.queryKeys, key) {
			return false
		}
	}
	return true
}

var runtimeSummarySurface = cloudReadSurface{
	errorPrefix: runtimeSummaryErrorPrefix, unavailableReason: "runtime_summary_unavailable",
	title: "Runtime summary unavailable", body: "Homelab Runtime could not resolve this runtime summary safely.",
}

type internalErrorEnvelope struct {
	ErrorCode        string              `json:"error_code"`
	ReasonCode       string              `json:"reason_code"`
	RequiredFeatures []string            `json:"required_features"`
	MissingFeatures  []string            `json:"missing_features"`
	Retryable        bool                `json:"retryable"`
	UserGuidance     denial.UserGuidance `json:"user_guidance"`
}

func (s cloudReadSurface) deny(e *httpx.Event, status int, reasonCode string, retryable bool, nextStep string) error {
	return e.JSON(status, internalErrorEnvelope{
		ErrorCode: s.errorPrefix + reasonCode, ReasonCode: reasonCode,
		RequiredFeatures: []string{}, MissingFeatures: []string{}, Retryable: retryable,
		UserGuidance: denial.UserGuidance{Title: s.title, Body: s.body, NextSteps: []string{nextStep}},
	})
}

// serveCloudRead is the Cloud servicecall boundary shared by the private read
// models. The service token is verified first; owner and tenant come only
// from the signed on_behalf_of identity, never from the request, and the
// stored membership authorization is resolved before serve runs. Inside the
// servicecall middleware the router never sees a returned error, so every
// branch writes its own response.
func (h inventoryHandlers) serveCloudRead(
	e *httpx.Event,
	surface cloudReadSurface,
	serve func(e *httpx.Event, ctx context.Context, scope inventoryScope),
) error {
	if !serviceauth.CanVerify(h.serviceAuthSecret) {
		return surface.deny(e, http.StatusServiceUnavailable, surface.unavailableReason, true,
			"Retry after Homelab Runtime service authentication is configured.")
	}
	authCfg, err := serviceauth.Apply(servicecall.Config{
		ServiceName:    runtimeSummaryService,
		Secret:         h.serviceAuthSecret,
		SecretNext:     h.serviceAuthNext,
		AllowedCallers: []string{runtimeSummaryCallerCloud},
		Enabled:        true,
	})
	if err != nil {
		return surface.deny(e, http.StatusServiceUnavailable, surface.unavailableReason, true,
			"Retry after Homelab Runtime service authentication is configured.")
	}
	auth := servicecall.RequireServiceAuth(authCfg)
	auth(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		e.Request = request
		caller := servicecall.FromContext(request.Context())
		if caller == nil || caller.Service != runtimeSummaryCallerCloud || caller.OnBehalfOf == nil ||
			strings.TrimSpace(caller.OnBehalfOf.Sub) == "" || strings.TrimSpace(caller.OnBehalfOf.OrgID) == "" {
			_ = surface.deny(e, http.StatusForbidden, "tenant_context_required", false,
				"Open Homelab Runtime from the authenticated Kombify Cloud organization and retry.")
			return
		}
		if !surface.queryAllowed(request) || request.ContentLength != 0 {
			_ = surface.deny(e, http.StatusForbidden, "principal_injection_denied", false,
				"Do not send principal or scope fields; Homelab Runtime derives them from the signed Cloud service identity.")
			return
		}
		scope := inventoryScope{
			tenantID: strings.TrimSpace(caller.OnBehalfOf.OrgID),
			ownerID:  strings.TrimSpace(caller.OnBehalfOf.Sub),
		}
		ctx := request.Context()
		if h.runtimeSummaryContext != nil {
			resolved, tenantID, err := h.runtimeSummaryContext(ctx, scope.tenantID, scope.ownerID)
			if err != nil || resolved == nil || strings.TrimSpace(tenantID) == "" {
				status, reason, retryable := http.StatusServiceUnavailable, "authorization_unavailable", true
				if errors.Is(err, ErrInventoryAccessDenied) {
					status, reason, retryable = http.StatusForbidden, "inventory_access_denied", false
				}
				_ = surface.deny(e, status, reason, retryable, "Open Homelab Runtime in the same account and organization to refresh its access context.")
				return
			}
			ctx = resolved
			scope.tenantID = strings.TrimSpace(tenantID)
		}
		e.Response.Header().Set("Cache-Control", "private, no-cache")
		e.Response.Header().Set("Vary", "Authorization, X-Kombify-Service-Auth")
		serve(e, ctx, scope)
	})).ServeHTTP(e.Response, e.Request)
	return nil
}

func (h inventoryHandlers) httpInternalRuntimeSummary(e *httpx.Event) error {
	return h.serveCloudRead(e, runtimeSummarySurface, h.serveRuntimeSummary)
}

func (h inventoryHandlers) serveRuntimeSummary(e *httpx.Event, ctx context.Context, scope inventoryScope) {
	page := inventoryPageOptions{Limit: runtimeSummaryPageSize}
	now := h.app.now().UTC()

	// The Overview is the owner looking at their own homelab, an account
	// feature in every plan. Once the signed subject's active membership in
	// the tenant is verified, the read is that owner's own rows only: the
	// agent/MCP inventory entitlement (which no plan grants by itself) and its
	// FGA surface relation do not apply here.
	app := h.ownerReadApplication()

	servers, err := app.listServers(ctx, scope, page)
	if err != nil {
		h.writeRuntimeSummaryError(e, err)
		return
	}
	services, err := app.listServices(ctx, scope, "", page)
	if err != nil {
		h.writeRuntimeSummaryError(e, err)
		return
	}

	_ = e.JSON(http.StatusOK, runtimeSummary{
		Version:            "1",
		GeneratedAt:        now.Format(time.RFC3339Nano),
		ScopeState:         "ok",
		Servers:            servers,
		Services:           services,
		ConfiguredUseCases: h.configuredUseCases(e, ctx, scope),
	})
}

// ownerInventoryReadPolicy admits only collection reads, scoped to the
// requesting owner's own rows in the verified tenant. Anything else (an
// object read, operate, write, provision, admin) is denied.
//
// The private monitoring reads (history, events, ports, backups; see
// inventory_internal_monitoring.go) run under this same policy without
// widening it: each is a read of the owner's own server collection, so it
// needs no new target, action or capability, and nothing writable or
// object-level becomes reachable through them.
type ownerInventoryReadPolicy struct{}

func (ownerInventoryReadPolicy) AuthorizeInventory(_ context.Context, authorization InventoryAuthorization) (InventoryDecision, error) {
	if authorization.Action != InventoryActionRead {
		return InventoryDecision{}, ErrInventoryAccessDenied
	}
	switch authorization.ResourceType {
	case controlplane.InventoryReadTargetServerCollection, controlplane.InventoryReadTargetServiceCollection:
	default:
		return InventoryDecision{}, ErrInventoryAccessDenied
	}
	readScope, err := controlplane.NewOwnerInventoryReadScope(authorization.TenantID, authorization.SubjectID)
	if err != nil {
		return InventoryDecision{}, ErrInventoryAccessDenied
	}
	return InventoryDecision{ReadScope: readScope}, nil
}

// writeRuntimeSummaryError always writes a response. Inside the servicecall
// middleware the router never sees a returned error, so the inventory denials
// that are normally rendered by the router (tenant guard, access denial) must
// be written here explicitly, in this surface's envelope. A denied collection
// must never be reported as a successful empty inventory.
func (h inventoryHandlers) writeRuntimeSummaryError(e *httpx.Event, err error) {
	h.writeCloudReadError(e, runtimeSummarySurface, err)
}

// writeCloudReadError is writeRuntimeSummaryError for any private Cloud read
// surface: the denials are written in that surface's envelope.
func (h inventoryHandlers) writeCloudReadError(e *httpx.Event, surface cloudReadSurface, err error) {
	var inventoryErr *inventoryError
	if errors.As(err, &inventoryErr) && inventoryErr.reasonCode == "inventory_access_denied" {
		_ = surface.deny(e, http.StatusForbidden, "inventory_access_denied", false, "Refresh Homelab Runtime access for this account and organization before retrying.")
		return
	}
	if errors.As(err, &inventoryErr) && inventoryErr.reasonCode == "tenant_context_missing" {
		_ = surface.deny(e, http.StatusForbidden, "tenant_context_required", false,
			"Open Homelab Runtime from the authenticated Kombify Cloud organization and retry.")
		return
	}
	// Every remaining branch of writeInventoryHTTPError writes through
	// httpx.Error itself.
	_ = writeInventoryHTTPError(e, err)
}
