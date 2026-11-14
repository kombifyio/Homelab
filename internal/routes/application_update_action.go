package routes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/kombifyio/techstack/internal/stackkitrelease"
	ksapi "github.com/kombifyio/techstack/pkg/api"
	"github.com/kombifyio/techstack/pkg/appreleases"
	"github.com/kombifyio/techstack/pkg/controlplane"
	"github.com/kombifyio/techstack/pkg/httpx"
	"github.com/kombifyio/techstack/pkg/jobs"
	"github.com/kombifyio/techstack/pkg/serviceregistry"
	"github.com/kombifyio/techstack/pkg/stackkitcommand"
)

// Single-application update (owner decision D-2026-11-12-06, workspace
// GUARD-RELIABILITY-SECURITY-TARGET §4.5). The `update` action of a
// StackKits-managed application whose fact is `application_update` carries
// {version, digest, acknowledge_untested?, advisory_id?}. The server re-reads
// a fresh fact and re-checks the update setting, the acknowledgement and the
// evidence before it enqueues one owner-approved StackKits
// `use-cases upgrade --apply` on the node; only this service is held while it
// runs. Policy:
//
//   - a checked (tested) version is one click;
//   - an unchecked in-track version needs acknowledge_untested (the owner's
//     own responsibility);
//   - a security fix (advisory_id of a high advisory affecting the installed
//     version, fixed in-track) is admitted in every setting;
//   - with_stackkits_releases admits nothing else per application;
//   - a major version never moves per application.
//
// The update runs as an owner-approved lifecycle job, not as a governed RIL
// card: the StackKits CLI's `use-cases upgrade` declares no executor-bound
// primitive contract (follow-up). It is advertised and admitted only while the
// stack's pinned release catalog dispatches application-release.update; the
// job issues that capability for exactly the admitted release.
const (
	reasonApplicationUpdateUnverified      = "application_update_evidence_unverified"
	reasonApplicationVersionNotOffered     = "application_version_not_offered"
	reasonApplicationDigestMismatch        = "application_digest_mismatch"
	reasonApplicationMajorUpdate           = "application_major_update"
	reasonApplicationUpdateWithStackKits   = "application_update_with_stackkits_releases"
	reasonApplicationUntestedAcknowledge   = "untested_acknowledgement_required"
	reasonApplicationAdvisoryNotFixed      = "advisory_not_fixed_by_version"
	reasonApplicationStackKitUpdatePending = "stackkit_update_pending"
	reasonApplicationUseCaseUnresolved     = "application_use_case_unresolved"
	reasonApplicationUpdateReleaseTooOld   = "stackkits_release_too_old"

	// The pinned release catalog's operation the update dispatches.
	applicationUpdateCatalogOperation = stackkitcommand.ApplicationUpdateOperation
)

// applicationUpdateTarget is the admitted application update of one request.
type applicationUpdateTarget struct {
	useCase string
	offer   appreleases.Offer
}

// applicationUpdateAllowed advertises the per-application update: a verified
// managed application with a newer in-track version the setting offers, a
// resolvable catalog use case and a pinned StackKits release whose catalog
// dispatches the update. Enforcement repeats every condition on a fresh fact
// (admitApplicationUpdate).
func applicationUpdateAllowed(service controlplane.ServiceRuntime, version *serviceVersion, server *controlplane.ServerRuntime, dispatchable func() bool) bool {
	if version == nil || server == nil || version.EvidenceStatus != appreleases.EvidenceVerified || len(version.NewerVersions) == 0 ||
		version.UpdateMode == updateModeStackKitUpgrade && version.SecurityFix == nil {
		return false
	}
	if serviceregistry.CanonicalManagementState(service.ManagementState) != serviceregistry.ManagementManaged || adoptedApplication(&service) ||
		service.MutationLock.Locked() || len(serviceGroupLockSources(service)) != 0 {
		return false
	}
	if version.UpdateSetting == controlplane.StackKitsUpdateWithStackKitsReleases && version.SecurityFix == nil {
		return false
	}
	if applicationUpdateUseCase(stringFromAnyMap(service.Metadata, "image")) == "" {
		return false
	}
	return dispatchable()
}

// admitApplicationUpdate is the server-side policy over a fresh fact. It
// returns the admitted target or the refusal reason code.
func admitApplicationUpdate(version *serviceVersion, request serviceActionRequest, image string, dispatchable bool) (applicationUpdateTarget, string) {
	if version == nil || version.EvidenceStatus != appreleases.EvidenceVerified || version.Current == "" {
		return applicationUpdateTarget{}, reasonApplicationUpdateUnverified
	}
	if !dispatchable {
		return applicationUpdateTarget{}, reasonApplicationUpdateReleaseTooOld
	}
	var offer *appreleases.Offer
	for i := range version.NewerVersions {
		if version.NewerVersions[i].Version == request.Version {
			offer = &version.NewerVersions[i]
			break
		}
	}
	if offer == nil {
		return applicationUpdateTarget{}, reasonApplicationVersionNotOffered
	}
	if offer.Digest == "" || offer.Digest != request.Digest {
		return applicationUpdateTarget{}, reasonApplicationDigestMismatch
	}
	if !sameMajorVersion(version.Current, offer.Version) {
		return applicationUpdateTarget{}, reasonApplicationMajorUpdate
	}
	if request.AdvisoryID != "" {
		fix := version.SecurityFix
		if fix == nil || fix.Version != offer.Version || !containsString(fix.Advisories, request.AdvisoryID) {
			return applicationUpdateTarget{}, reasonApplicationAdvisoryNotFixed
		}
	} else {
		if version.UpdateMode == updateModeStackKitUpgrade {
			return applicationUpdateTarget{}, reasonApplicationStackKitUpdatePending
		}
		if version.UpdateSetting == controlplane.StackKitsUpdateWithStackKitsReleases {
			return applicationUpdateTarget{}, reasonApplicationUpdateWithStackKits
		}
	}
	if !offer.Tested && !request.AcknowledgeUntested {
		return applicationUpdateTarget{}, reasonApplicationUntestedAcknowledge
	}
	useCase := applicationUpdateUseCase(image)
	if useCase == "" || !stackkitcommand.ValidApplicationUpdateTarget(useCase, offer.Version, offer.Digest) {
		return applicationUpdateTarget{}, reasonApplicationUseCaseUnresolved
	}
	return applicationUpdateTarget{useCase: useCase, offer: *offer}, ""
}

// sameMajorVersion compares the leading numeric component of two versions; an
// unparsable version is never the same major.
func sameMajorVersion(current, target string) bool {
	major := func(value string) (int, bool) {
		value = strings.TrimPrefix(strings.TrimSpace(value), "v")
		head, _, _ := strings.Cut(value, ".")
		number, err := strconv.Atoi(head)
		return number, err == nil
	}
	a, okA := major(current)
	b, okB := major(target)
	return okA && okB && a == b
}

var applicationUseCases = struct {
	sync.Mutex
	loaded  bool
	slugs   map[string][]string
	catalog stackkitrelease.UseCaseCatalog
}{}

// applicationUpdateUseCase resolves the image's feed application to the one
// catalog use case whose alternatives build its module: the slug the CLI
// takes. A module in no or in several use cases resolves to "".
func applicationUpdateUseCase(image string) string {
	application, ok := applicationReleasesStatus().Application(image)
	if !ok {
		return ""
	}
	module := firstNonEmptyString(application.CatalogModule, application.Module)
	applicationUseCases.Lock()
	defer applicationUseCases.Unlock()
	if !applicationUseCases.loaded {
		// The catalog is the pinned release's (fixed for the process); a
		// failed read is not cached, so the next request tries again.
		catalog, err := stackkitrelease.ResolveUseCaseCatalog()
		if err != nil {
			return ""
		}
		applicationUseCases.catalog, applicationUseCases.slugs, applicationUseCases.loaded = catalog, map[string][]string{}, true
	}
	slugs, found := applicationUseCases.slugs[module]
	if !found {
		slugs = applicationUseCases.catalog.UseCasesForModule(module)
		applicationUseCases.slugs[module] = slugs
	}
	if len(slugs) != 1 {
		return ""
	}
	return slugs[0]
}

// applicationReleasesStatus is the current verified feed status.
func applicationReleasesStatus() appreleases.Status { return applicationReleases() }

// catalogDispatches reports whether the stack's pinned StackKits release
// catalog lists every operation as dispatchable. An unreadable release or
// catalog dispatches nothing.
func (h *serviceRuntimeHandlers) catalogDispatches(ctx context.Context, tenantID, stackID string, operations ...string) bool {
	resolve := h.releaseCatalog
	if resolve == nil {
		resolve = func(ctx context.Context, tenantID, stackID string) (stackkitrelease.AdvancedOperationsCatalog, error) {
			release, err := jobs.CachedTargetStackKitReleaseForStack(ctx, tenantID, stackID)
			if err != nil || release == nil {
				return stackkitrelease.AdvancedOperationsCatalog{}, fmt.Errorf("pinned StackKits release is unavailable: %v", err)
			}
			return release.AdvancedOperations()
		}
	}
	catalog, err := resolve(ctx, tenantID, stackID)
	if err != nil {
		return false
	}
	for _, operation := range operations {
		if catalog.Dispatchable(operation) != nil {
			return false
		}
	}
	return true
}

func applicationUpdateRefusal(e *httpx.Event, reason string) error {
	return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "Application update is not allowed", map[string]any{"reason_code": reason})
}

// applicationUpdate enqueues one admitted application update. It follows the
// managed job path of the service actions: replay from the durable receipt,
// the owner guardrails, a fresh target, maintenance, then the policy on a
// freshly read fact, and one active action per service.
func (h *serviceRuntimeHandlers) applicationUpdate(e *httpx.Event, tenantID, ownerID, idempotencyKey string, service *controlplane.ServiceRuntime, stack *controlplane.Stack, request serviceActionRequest) error {
	if serviceregistry.CanonicalManagementState(service.ManagementState) != serviceregistry.ManagementManaged || adoptedApplication(service) {
		return applicationUpdateRefusal(e, "service_update_unavailable")
	}
	acknowledged := "0"
	if request.AcknowledgeUntested {
		acknowledged = "1"
	}
	digestBytes := sha256.Sum256([]byte(strings.Join([]string{
		tenantID, service.ID, request.Action, strconv.FormatInt(request.ExpectedInventoryRevision, 10),
		request.Version, request.Digest, acknowledged, request.AdvisoryID,
	}, "\x00")))
	lifecycleRequest := jobs.StackKitLifecycleRequest{
		StackID: stack.ID, StackKitInstanceID: stack.StackKitInstanceID, TenantID: tenantID, OwnerID: ownerID,
		Operation: jobs.StackKitLifecycleApplicationUpdate, OwnerApproved: true, ServiceKey: serviceActionControlKey(service), ServiceID: service.ID,
		DurableJobID: jobs.StackKitServiceActionJobID(tenantID, ownerID, idempotencyKey), ServiceActionDigest: hex.EncodeToString(digestBytes[:]),
	}
	if replayed, replayErr := h.replayServiceAction(e, tenantID, service, request.Action, lifecycleRequest); replayed || replayErr != nil {
		return replayErr
	}
	if service.MutationLock.Locked() {
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "Service is locked", map[string]any{
			"reason": "service_locked", "mutation_lock": serviceMutationLockResponse(service.MutationLock), "unlock_required_action": serviceActionUnfreeze,
		})
	}
	if sources := serviceGroupLockSources(*service); len(sources) != 0 {
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "Application control is locked", map[string]any{
			"reason_code": "application_control_locked", "unlock_required_action": serviceActionUnfreeze, "unlock_service_ids": sources,
		})
	}
	target, targetErr := h.validateServiceActionTarget(e, tenantID, service, stack, request)
	if targetErr != nil {
		return targetErr
	}
	if maintenanceErr := refuseDuringServerMaintenance(e, h.servers, tenantID, controlplane.ServerMaintenanceScope{
		ServerID: target.server.ID, AgentID: target.server.WorkerID,
	}); maintenanceErr != nil {
		return maintenanceErr
	}
	// A fresh node read and the current feed: the request names exactly a
	// version this fact offers, with its digest.
	version := h.serviceVersionFor(e.Request.Context(), *service, target.server, true)
	if version != nil && version.UpdateSetting != "" {
		// Fail closed: an unreadable setting admits only a security fix, as
		// with_stackkits_releases would.
		version.UpdateSetting = controlplane.StackKitsUpdateWithStackKitsReleases
		if setting, ok := jobs.ReadNodeUpdateSetting(e.Request.Context(), tenantID, strings.TrimSpace(target.server.WorkerID)); ok {
			version.UpdateSetting = setting
		}
	}
	// The node regenerates and applies its plan for the update, so an owner
	// guardrail on any service of the stack refuses it, like a stack update.
	if lockedErr := h.refuseLockedStackServices(e, tenantID, stack.ID); lockedErr != nil {
		return lockedErr
	}
	admitted, reason := admitApplicationUpdate(version, request, stringFromAnyMap(service.Metadata, "image"), h.catalogDispatches(e.Request.Context(), tenantID, stack.ID, applicationUpdateCatalogOperation))
	if reason != "" {
		return applicationUpdateRefusal(e, reason)
	}
	lifecycleRequest.AgentID = target.server.WorkerID
	lifecycleRequest.StackKit = target.stackKit
	lifecycleRequest.UseCase, lifecycleRequest.ApplicationVersion, lifecycleRequest.ApplicationDigest = admitted.useCase, admitted.offer.Version, admitted.offer.Digest
	lifecycleRequest.AcceptUntested = !admitted.offer.Tested
	return h.enqueueServiceAction(e, tenantID, stack.ID, service, request.Action, lifecycleRequest)
}
