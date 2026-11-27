package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	ksapi "github.com/kombifyio/homelab/pkg/api"
	"github.com/kombifyio/homelab/pkg/api/agentpb"
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/jobs"
	"github.com/kombifyio/homelab/pkg/stackkitcommand"
)

// "Stack it" for any unmanaged single container: the producer rebuilds it as
// an own application (homelab.own-application/v4, profile
// own-application-adopted/v1). The Home Assistant in-place adoption in
// service_adoption.go stays the profile for Home Assistant.
const (
	adoptionModeInPlace         = "adopt_in_place"
	adoptionModeStackIt         = "stack_it"
	adoptionModeRestoreOriginal = "restore_original"
	adoptionModeReleaseOriginal = "release_original"
	ownApplicationProjectPrefix = "stackkit-own-"
)

func stackItUnavailable(e *httpx.Event, err error) error {
	details := map[string]any{"reason_code": "application_stack_unavailable", "guidance": "Activate a published StackKits release with Stack it (" + stackkitcommand.MinApplicationStackRelease + " or newer) on this Guard."}
	if err != nil {
		details["detail"] = err.Error()
	}
	return httpx.Reject(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Stack it requires a published capable StackKits release and connected Guard", details)
}

func (h *serviceRuntimeHandlers) stackItRelease(ctx context.Context, tenant, stack string, read bool) (*agentpb.StackKitReleasePin, error) {
	if h.stackItAdmission != nil {
		return h.stackItAdmission(ctx, tenant, stack, read)
	}
	return jobs.ApplicationStackReleaseForStack(ctx, tenant, stack, read)
}

// inPlaceProfileUnsupported refuses the in-place profile for a container it
// does not own; such a container is stacked as an own application instead.
func inPlaceProfileUnsupported(e *httpx.Event) error {
	return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "The in-place profile adopts only Home Assistant; stack this container as an own application", map[string]any{"reason_code": "application_adoption_profile_unsupported"})
}

// inPlaceAdoptionProfile reports a service the Home Assistant in-place
// profile owns; every other container is stacked as an own application.
func inPlaceAdoptionProfile(service controlplane.ServiceRuntime) bool {
	entry := serviceCatalogEntry(service)
	return entry != nil && entry.ID == "home-assistant"
}

// ownApplicationNameOf is the project suffix (<name>-<node>) of a rebuilt
// container; ownApplicationNameFor resolves the name itself from the Stack it
// records. StackKits runs it as Compose project stackkit-own-<name>-<node> whose
// single-container service is "app" (a project names its own services); the
// container is named by Compose from that project, never chosen by a user.
// The node reference is not on the server aggregate, so the project prefix,
// the Compose service and the Compose-derived container name are the evidence.
func ownApplicationNameOf(service controlplane.ServiceRuntime) string {
	identity := stringMapFromAny(service.Metadata["runtime_identity"])
	project, composeService := identity["project"], identity["service"]
	if identity["kind"] != "docker_compose_service" || !strings.HasPrefix(project, ownApplicationProjectPrefix) || len(project) <= len(ownApplicationProjectPrefix) || composeService == "" {
		return ""
	}
	if name := identity["container_name"]; name != "" && !strings.HasPrefix(name, project+"-"+composeService) {
		return ""
	}
	return strings.TrimPrefix(project, ownApplicationProjectPrefix)
}

func stackItContainerName(service *controlplane.ServiceRuntime) string {
	identity := stringMapFromAny(service.Metadata["runtime_identity"])
	return firstNonEmptyString(identity["container_name"], service.Name)
}

type stackItAvailability struct {
	Available                bool       `json:"available"`
	ReasonCode               string     `json:"reason_code,omitempty"`
	Name                     string     `json:"name,omitempty"`
	State                    string     `json:"state,omitempty"`
	RetainUntil              *time.Time `json:"retain_until,omitempty"`
	RestoreAvailable         bool       `json:"restore_available"`
	ReleaseOriginalAvailable bool       `json:"release_original_available"`
}

func (h *serviceRuntimeHandlers) stackItAvailabilityFor(ctx context.Context, s controlplane.ServiceRuntime) *stackItAvailability {
	if s.ManagementState != "observed" || ownApplicationNameOf(s) != "" || inPlaceAdoptionProfile(s) {
		return nil
	}
	a := &stackItAvailability{ReasonCode: "application_stack_unavailable", Name: stackkitcommand.OwnApplicationName(stackItContainerName(&s))}
	record, recorded := jobs.ReadOwnApplicationAdoption(&s)
	if recorded {
		a.Name, a.State, a.RetainUntil = record.Name, record.State, record.RetainUntil
	}
	if !applicationContainerPattern.MatchString(jobs.ApplicationContainerIdentity(&s)) {
		a.ReasonCode = "application_source_identity_unavailable"
		return a
	}
	if a.Name == "" {
		a.ReasonCode = "application_stack_name_unavailable"
		return a
	}
	if _, err := h.stackItRelease(ctx, s.TenantID, s.StackID, true); err != nil {
		return a
	}
	a.ReasonCode = ""
	switch {
	case recorded && record.State == "stacked":
		a.ReasonCode = "application_stacked"
		locked := s.MutationLock.Locked()
		a.RestoreAvailable = !locked && record.RetainUntil != nil && h.now().Before(*record.RetainUntil)
		a.ReleaseOriginalAvailable = !locked
	case recorded && record.State == "released":
		a.ReasonCode = "application_stacked"
	default:
		a.Available = !s.MutationLock.Locked()
	}
	return a
}

func (h *serviceRuntimeHandlers) stackItBinding(ctx context.Context, t adoptionTarget, read bool) (*jobs.ApplicationLifecycleBinding, error) {
	pin, err := h.stackItRelease(ctx, t.tenant, t.stack.ID, read)
	if err != nil {
		return nil, err
	}
	binding := jobs.RetainedApplicationBinding(t.head)
	binding.ExpectedAdoptionJobID, _ = t.head.Service.Metadata[jobs.OwnApplicationAdoptionJobIDKey].(string)
	binding.ReleasePin = pin
	binding.Profile = stackkitcommand.OwnApplicationAdoptedProfile
	binding.OwnApplicationName = stackkitcommand.OwnApplicationName(stackItContainerName(t.service))
	if record, ok := jobs.ReadOwnApplicationAdoption(t.service); ok {
		binding.OwnApplicationName = record.Name
	}
	return binding, nil
}

func applyStackItOverrides(e *httpx.Event, r adoptionRequest, binding *jobs.ApplicationLifecycleBinding) error {
	if rawName := strings.TrimSpace(r.OwnApplicationName); rawName != "" {
		if !stackkitcommand.ValidOwnApplicationName(rawName) {
			return httpx.BadRequest(e, "own_application_name must be lowercase letters, digits and hyphens (1..40 chars, starting with a letter, not kombify* or stackkit*)", map[string]any{"field": "own_application_name"})
		}
		binding.OwnApplicationName = rawName
	}
	if r.HTTPPort != nil {
		if *r.HTTPPort < stackkitcommand.StackItHTTPPortMin || *r.HTTPPort > stackkitcommand.StackItHTTPPortMax {
			return httpx.BadRequest(e, "http_port must be between 1 and 65535", map[string]any{"field": "http_port"})
		}
		binding.HTTPPort = *r.HTTPPort
	}
	binding.NoOutbound = r.NoOutbound
	if r.MemoryMiB != nil {
		if *r.MemoryMiB < stackkitcommand.StackItMemoryMiBMin || *r.MemoryMiB > stackkitcommand.StackItMemoryMiBMax {
			return httpx.BadRequest(e, "memory_mib must be between 64 and 8192", map[string]any{"field": "memory_mib"})
		}
		binding.MemoryMiB = *r.MemoryMiB
	}
	if r.CPUs != nil {
		formatted, err := stackkitcommand.FormatStackItCPUs(*r.CPUs)
		if err != nil {
			return httpx.BadRequest(e, err.Error(), map[string]any{"field": "cpus"})
		}
		binding.CPUs = formatted
	}
	binding.AdoptComposeProject = r.AdoptComposeProject
	if rawService := strings.TrimSpace(r.HTTPService); rawService != "" {
		if !r.AdoptComposeProject {
			return httpx.BadRequest(e, "http_service requires adopt_compose_project", map[string]any{"field": "http_service"})
		}
		if err := stackkitcommand.ValidateStackItHTTPService(rawService); err != nil {
			return httpx.BadRequest(e, err.Error(), map[string]any{"field": "http_service"})
		}
		binding.HTTPService = rawService
	}
	return nil
}

func (h *serviceRuntimeHandlers) prepareStackIt(e *httpx.Event, t adoptionTarget, r adoptionRequest, key string) error {
	if r.OwnerApproved || r.PlanDigest != "" {
		return httpx.BadRequest(e, "A Stack it plan is read-only", nil)
	}
	if t.service.ManagementState != "observed" || inPlaceAdoptionProfile(*t.service) || ownApplicationNameOf(*t.service) != "" {
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "Service cannot be stacked as an own application", nil)
	}
	if record, ok := jobs.ReadOwnApplicationAdoption(t.service); ok && record.State != "restored" {
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "Service is already stacked", map[string]any{"reason_code": "application_stacked"})
	}
	if err := h.validateAdoptionSource(e, t, r); err != nil {
		return err
	}
	binding, err := h.stackItBinding(e.Request.Context(), t, false)
	if err != nil {
		return stackItUnavailable(e, err)
	}
	if err := applyStackItOverrides(e, r, binding); err != nil {
		return err
	}
	if binding.OwnApplicationName == "" {
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "No valid own application name derives from this container name", map[string]any{"reason_code": "application_stack_name_unavailable"})
	}
	return h.enqueueAdoption(e, t, r, key, jobs.StackKitLifecycleApplicationStackPlan, binding)
}

func (h *serviceRuntimeHandlers) executeStackIt(e *httpx.Event, t adoptionTarget, r adoptionRequest, key string, plan *controlplane.Job) error {
	if !r.OwnerApproved || r.PlanDigest == "" || r.hasStackItOverrides() {
		return httpx.BadRequest(e, "Stack it requires the reviewed plan digest and Owner approval", nil)
	}
	if plan.State != "completed" {
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "Stack it plan is not complete", nil)
	}
	var binding jobs.ApplicationLifecycleBinding
	encoded, _ := json.Marshal(plan.Result["application_binding"])
	if json.Unmarshal(encoded, &binding) != nil || binding.Profile != stackkitcommand.OwnApplicationAdoptedProfile {
		return stackItUnavailable(e, nil)
	}
	var reviewed stackkitcommand.ApplicationStackResult
	encoded, _ = json.Marshal(plan.Result["application_stack"])
	if json.Unmarshal(encoded, &reviewed) != nil || reviewed.Outcome != "planned" || !reviewed.Admissible || reviewed.PlanDigest != r.PlanDigest {
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "Reviewed Stack it digest differs from the retained plan", nil)
	}
	binding.PlanJobID = plan.ID
	binding.ReviewedStack = &reviewed
	request := adoptionLifecycleRequest(t, r, key, jobs.StackKitLifecycleApplicationStack, &binding)
	if replayed, replayErr := h.replayServiceAction(e, t.tenant, t.service, jobs.StackKitLifecycleApplicationStack, request); replayed || replayErr != nil {
		return replayErr
	}
	if binding.ContainerID != jobs.ApplicationContainerIdentity(t.service) || binding.ServerID != t.service.ServerID || t.service.ManagementState != "observed" {
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "Retained application source changed; prepare a new plan", nil)
	}
	if err := h.validateAdoptionSource(e, t, r); err != nil {
		return err
	}
	return h.enqueueAdoption(e, t, r, key, jobs.StackKitLifecycleApplicationStack, &binding)
}

// originalStackIt restores or removes the original container a Stack it kept.
func (h *serviceRuntimeHandlers) originalStackIt(e *httpx.Event) error {
	t, err := h.adoptionTarget(e, true)
	if err != nil {
		return err
	}
	if err := authorizeOwnerServerOperate(e.Request.Context(), h.policy, h.servers, inventoryScope{tenantID: t.tenant, ownerID: t.owner}, t.server.ID); err != nil {
		return writeInventoryHTTPError(e, err)
	}
	r, key, err := decodeAdoptionRequest(e)
	if err != nil {
		return err
	}
	op := map[string]string{adoptionModeRestoreOriginal: jobs.StackKitLifecycleApplicationStackRestoreOriginal, adoptionModeReleaseOriginal: jobs.StackKitLifecycleApplicationStackReleaseOriginal}[r.Mode]
	if op == "" || !r.OwnerApproved || r.PlanDigest != "" || r.hasStackItOverrides() {
		return httpx.BadRequest(e, "Restore or release of the original requires its mode and Owner approval", nil)
	}
	binding, err := h.stackItBinding(e.Request.Context(), t, false)
	if err != nil {
		return stackItUnavailable(e, err)
	}
	if replayed, replayErr := h.replayServiceAction(e, t.tenant, t.service, op, adoptionLifecycleRequest(t, r, key, op, binding)); replayed || replayErr != nil {
		return replayErr
	}
	record, ok := jobs.ReadOwnApplicationAdoption(t.service)
	if !ok || record.State != "stacked" {
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "Service has no stacked original", nil)
	}
	if op == jobs.StackKitLifecycleApplicationStackRestoreOriginal && (record.RetainUntil == nil || !h.now().Before(*record.RetainUntil)) {
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "The restore window of this original has ended", map[string]any{"reason_code": "application_stack_retention_ended"})
	}
	if err = h.validateAdoptionSource(e, t, r); err != nil {
		return err
	}
	return h.enqueueAdoption(e, t, r, key, op, binding)
}
