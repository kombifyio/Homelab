package routes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kombifyio/techstack/internal/routes/tenantguard"
	ksapi "github.com/kombifyio/techstack/pkg/api"
	"github.com/kombifyio/techstack/pkg/controlplane"
	"github.com/kombifyio/techstack/pkg/httpx"
	"github.com/kombifyio/techstack/pkg/jobs"
	"github.com/kombifyio/techstack/pkg/serverregistry"
	"github.com/kombifyio/techstack/pkg/stackkitcommand"
)

// Own-application lifecycle of an application StackKits runs as an own
// application and that "Stack it" adopted (StackKits own-application.*,
// docs/OWN_APPLICATIONS.md "Managed nodes"). Every mutation is two steps: a
// dry run returns the review digest, and the owner approves exactly that
// review; the job then issues the own-application.lifecycle capability for
// this application, action, operation and review digest. Status, logs and
// backup need no review.
const serviceActionOwnApplication = "own_application"

// ownApplicationReviewLifetime bounds how long a completed dry run may be
// approved: the capability it leads to lives at most as long.
const ownApplicationReviewLifetime = 15 * time.Minute

// Own-application actions a row advertises in own_application.actions.
const (
	ownAppActionStatus             = "status"
	ownAppActionLogs               = "logs"
	ownAppActionBackup             = "backup"
	ownAppActionStart              = "start"
	ownAppActionStop               = "stop"
	ownAppActionRestart            = "restart"
	ownAppActionUpdate             = "update"
	ownAppActionRollback           = "rollback"
	ownAppActionCheckpointRollback = "checkpoint_rollback"
	ownAppActionCheckpointDiscard  = "checkpoint_discard"
)

// serviceOwnApplication marks a container StackKits runs as an own
// application. Its Compose project is stackkit-own-<name>-<node>; the name is
// the one its Stack it record holds, never guessed from the project.
type serviceOwnApplication struct {
	ProjectRef string                     `json:"project_ref"`
	Name       string                     `json:"name,omitempty"`
	Actions    []string                   `json:"actions"`
	ReasonCode string                     `json:"reason_code,omitempty"`
	Status     *jobs.OwnApplicationStatus `json:"status,omitempty"`
}

type ownApplicationNamesKey struct{}

// withOwnApplicationNames carries the Stack it names of every stack in rows,
// so a read projection resolves an own application's name without guessing
// where its name ends and its node reference begins.
func withOwnApplicationNames(ctx context.Context, rows []controlplane.ServiceRuntime) context.Context {
	names := map[string][]string{}
	for i := range rows {
		if record, ok := jobs.ReadOwnApplicationAdoption(&rows[i]); ok && (record.State == "stacked" || record.State == "released") {
			if !slices.Contains(names[rows[i].StackID], record.Name) {
				names[rows[i].StackID] = append(names[rows[i].StackID], record.Name)
			}
		}
	}
	return context.WithValue(ctx, ownApplicationNamesKey{}, names)
}

func ownApplicationNamesFrom(ctx context.Context, stackID string) ([]string, bool) {
	names, ok := ctx.Value(ownApplicationNamesKey{}).(map[string][]string)
	if !ok {
		return nil, false
	}
	return names[stackID], true
}

// resolveOwnApplicationName picks the adopted name whose project the row runs
// (stackkit-own-<name>-<node>); the longest match wins, none resolves to "".
func resolveOwnApplicationName(projectSuffix string, names []string) string {
	resolved := ""
	for _, name := range names {
		if strings.HasPrefix(projectSuffix, name+"-") && len(name) > len(resolved) {
			resolved = name
		}
	}
	return resolved
}

// ownApplicationNameFor resolves an own application row's name from the Stack
// it records of its stack (read from ctx, or loaded when ctx carries none).
func (h *serviceRuntimeHandlers) ownApplicationNameFor(ctx context.Context, service controlplane.ServiceRuntime) string {
	suffix := ownApplicationNameOf(service)
	if suffix == "" {
		return ""
	}
	names, ok := ownApplicationNamesFrom(ctx, service.StackID)
	if !ok && h.store != nil {
		rows, err := h.store.ListServiceRuntimes(ctx, service.TenantID, service.StackID, "")
		if err != nil {
			return ""
		}
		names, _ = ownApplicationNamesFrom(withOwnApplicationNames(ctx, rows), service.StackID)
	}
	return resolveOwnApplicationName(suffix, names)
}

// ownApplicationProjection is the read model of an own application row and
// the actions it admits now. Enforcement repeats every condition.
func (h *serviceRuntimeHandlers) ownApplicationProjection(ctx context.Context, service controlplane.ServiceRuntime, server *controlplane.ServerRuntime, observedState string) *serviceOwnApplication {
	suffix := ownApplicationNameOf(service)
	if suffix == "" {
		return nil
	}
	out := &serviceOwnApplication{ProjectRef: ownApplicationProjectPrefix + suffix, Actions: []string{}}
	if status, ok := jobs.ReadOwnApplicationStatus(&service); ok {
		out.Status = &status
	}
	out.Name = h.ownApplicationNameFor(ctx, service)
	switch {
	case out.Name == "":
		out.ReasonCode = "own_application_not_stacked"
	case server == nil || (server.ConnectionState != string(serverregistry.ConnectionConnected) && server.ConnectionState != string(serverregistry.ConnectionDegraded)):
		out.ReasonCode = "own_application_guard_unavailable"
	case !h.catalogDispatches(ctx, service.TenantID, service.StackID, stackkitcommand.OwnApplicationOperations()...):
		out.ReasonCode = "own_application_release_unavailable"
	}
	if out.ReasonCode != "" {
		return out
	}
	out.Actions = append(out.Actions, ownAppActionStatus, ownAppActionLogs)
	if service.MutationLock.Locked() || len(serviceGroupLockSources(service)) != 0 {
		out.ReasonCode = "service_locked"
		return out
	}
	state := observedState
	if out.Status != nil && out.Status.State != "" {
		state = out.Status.State
	}
	switch state {
	case "running":
		out.Actions = append(out.Actions, ownAppActionStop, ownAppActionRestart, ownAppActionUpdate)
	case "stopped", "exited":
		out.Actions = append(out.Actions, ownAppActionStart)
	}
	out.Actions = append(out.Actions, ownAppActionRollback, ownAppActionBackup)
	if out.Status != nil && out.Status.Checkpoint != nil {
		switch out.Status.Checkpoint.State {
		case "retained", "kept":
			out.Actions = append(out.Actions, ownAppActionCheckpointRollback, ownAppActionCheckpointDiscard)
		case "restored":
			out.Actions = append(out.Actions, ownAppActionCheckpointDiscard)
		}
	}
	return out
}

// ownApplicationOperationRequest asks for one read, dry run or backup.
type ownApplicationOperationRequest struct {
	Operation                 string `json:"operation"`
	Action                    string `json:"action,omitempty"`
	ImageTag                  string `json:"image_tag,omitempty"`
	AllowMajor                bool   `json:"allow_major,omitempty"`
	Checkpoint                string `json:"checkpoint,omitempty"`
	OwnerApproved             bool   `json:"owner_approved,omitempty"`
	ExpectedInventoryRevision int64  `json:"expected_inventory_revision"`
}

// ownApplicationExecuteRequest approves exactly one reviewed dry run.
type ownApplicationExecuteRequest struct {
	ReviewDigest              string `json:"review_digest"`
	OwnerApproved             bool   `json:"owner_approved"`
	ExpectedInventoryRevision int64  `json:"expected_inventory_revision"`
}

type ownApplicationOperationResponse struct {
	JobID     string         `json:"job_id"`
	ServiceID string         `json:"service_id"`
	Operation string         `json:"operation"`
	Status    string         `json:"status"`
	Result    map[string]any `json:"result,omitempty"`
	Error     string         `json:"error,omitempty"`
}

type ownApplicationTarget struct {
	tenant, owner string
	service       *controlplane.ServiceRuntime
	stack         *controlplane.Stack
	name          string
}

func ownApplicationRefusal(e *httpx.Event, status int, message, reason string) error {
	code := ksapi.ErrCodeConflict
	if status == http.StatusServiceUnavailable {
		code = ksapi.ErrCodeUnavailable
	}
	return httpx.Error(e, status, code, message, map[string]any{"reason_code": reason})
}

func (h *serviceRuntimeHandlers) ownApplicationTarget(e *httpx.Event) (ownApplicationTarget, error) {
	var t ownApplicationTarget
	owner, isAdmin, ok := authenticatedUser(e)
	if !ok {
		return t, httpx.RejectUnauthorized(e, "Authentication required")
	}
	tenant, err := tenantguard.TenantScope(requestExplicitTenantID(e), owner, "techstack.services.own-application")
	if err != nil {
		return t, err
	}
	service, err := h.loadServiceRuntime(e.Request.Context(), tenant, e.Request.PathValue("serviceId"))
	if err != nil {
		return t, httpx.Reject(e, http.StatusNotFound, ksapi.ErrCodeNotFound, "Service not found", nil)
	}
	stack, err := h.stacks.GetStack(e.Request.Context(), tenant, service.StackID)
	if err != nil || (!isAdmin && stack.OwnerSubjectID != owner) {
		return t, httpx.Reject(e, http.StatusNotFound, ksapi.ErrCodeNotFound, "Service not found", nil)
	}
	name := h.ownApplicationNameFor(e.Request.Context(), *service)
	if name == "" {
		return t, ownApplicationRefusal(e, http.StatusConflict, "Service is not an own application adopted with Stack it", "own_application_not_stacked")
	}
	if h.orch == nil || h.jobs == nil {
		return t, ownApplicationRefusal(e, http.StatusServiceUnavailable, "Managed service control is unavailable", "own_application_unavailable")
	}
	return ownApplicationTarget{tenant: tenant, owner: owner, service: service, stack: stack, name: name}, nil
}

func decodeOwnApplicationBody(e *httpx.Event, into any) (string, error) {
	key := strings.TrimSpace(e.Request.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 128 {
		return key, httpx.Reject(e, http.StatusBadRequest, ksapi.ErrCodeBadRequest, "A bounded Idempotency-Key is required", nil)
	}
	decoder := json.NewDecoder(io.LimitReader(e.Request.Body, 4097))
	decoder.DisallowUnknownFields()
	if decoder.Decode(into) != nil {
		return key, httpx.Reject(e, http.StatusBadRequest, ksapi.ErrCodeBadRequest, "Invalid own application request", nil)
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return key, httpx.Reject(e, http.StatusBadRequest, ksapi.ErrCodeBadRequest, "One own application request document is required", nil)
	}
	return key, nil
}

// ownApplicationSteps maps the request operation to the catalog step.
var ownApplicationReadSteps = map[string]string{
	"status": stackkitcommand.OwnApplicationStatus, "logs": stackkitcommand.OwnApplicationLogs, "backup": stackkitcommand.OwnApplicationBackup,
	"review": stackkitcommand.OwnApplicationReview, "update_review": stackkitcommand.OwnApplicationUpdateReview,
	"checkpoint_rollback_review": stackkitcommand.OwnApplicationCheckpointRollbackReview, "checkpoint_discard_review": stackkitcommand.OwnApplicationCheckpointDiscardReview,
}

// startOwnApplicationOperation enqueues status, logs, a backup or the dry run
// of a mutation.
func (h *serviceRuntimeHandlers) startOwnApplicationOperation(e *httpx.Event) error {
	t, err := h.ownApplicationTarget(e)
	if err != nil {
		return err
	}
	var r ownApplicationOperationRequest
	key, err := decodeOwnApplicationBody(e, &r)
	if err != nil {
		return err
	}
	step, known := ownApplicationReadSteps[strings.TrimSpace(r.Operation)]
	r.Action, r.ImageTag, r.Checkpoint = strings.TrimSpace(r.Action), strings.TrimSpace(r.ImageTag), strings.TrimSpace(r.Checkpoint)
	switch {
	case !known:
		return httpx.BadRequest(e, "operation must be status, logs, backup, review, update_review, checkpoint_rollback_review or checkpoint_discard_review", nil)
	case (step == stackkitcommand.OwnApplicationReview) != (r.Action != "") || (r.Action != "" && !slices.Contains([]string{"start", "stop", "restart", "rollback"}, r.Action)):
		return httpx.BadRequest(e, "review names exactly one of start, stop, restart or rollback", nil)
	case (step == stackkitcommand.OwnApplicationUpdateReview) != (r.ImageTag != "") || (r.ImageTag != "" && !stackkitcommand.ValidOwnApplicationImageTag(r.ImageTag)) || (r.AllowMajor && step != stackkitcommand.OwnApplicationUpdateReview):
		return httpx.BadRequest(e, "update_review names exactly one version tag", nil)
	case (step == stackkitcommand.OwnApplicationCheckpointRollbackReview || step == stackkitcommand.OwnApplicationCheckpointDiscardReview) != (r.Checkpoint != "") || (r.Checkpoint != "" && !stackkitcommand.ValidOwnApplicationOperationID(r.Checkpoint)):
		return httpx.BadRequest(e, "checkpoint reviews name exactly the checkpoint operation ID", nil)
	case (step == stackkitcommand.OwnApplicationBackup) != r.OwnerApproved:
		return httpx.BadRequest(e, "a backup, and only a backup, carries Owner approval here", nil)
	}
	request := jobs.OwnApplicationRequest{Name: t.name, Step: step, ServerID: t.service.ServerID, Action: r.Action, ImageTag: r.ImageTag, AllowMajor: r.AllowMajor, Checkpoint: r.Checkpoint}
	fingerprint := []string{r.Operation, r.Action, r.ImageTag, strconv.FormatBool(r.AllowMajor), r.Checkpoint, strconv.FormatBool(r.OwnerApproved)}
	return h.enqueueOwnApplication(e, t, key, r.ExpectedInventoryRevision, r.OwnerApproved, request, fingerprint)
}

// executeOwnApplicationOperation approves one completed dry run: the step it
// reviewed runs with exactly its review digest and inputs.
func (h *serviceRuntimeHandlers) executeOwnApplicationOperation(e *httpx.Event) error {
	t, err := h.ownApplicationTarget(e)
	if err != nil {
		return err
	}
	var r ownApplicationExecuteRequest
	_, err = decodeOwnApplicationBody(e, &r)
	if err != nil {
		return err
	}
	if !r.OwnerApproved || r.ReviewDigest == "" {
		return httpx.BadRequest(e, "Execution requires the reviewed digest and Owner approval", nil)
	}
	review, answer, err := h.loadOwnApplicationJob(e, t, e.Request.PathValue("jobId"))
	if err != nil {
		return err
	}
	var reviewed jobs.OwnApplicationRequest
	encoded, _ := json.Marshal(review.Payload["own_application"])
	if json.Unmarshal(encoded, &reviewed) != nil || reviewed.Name != t.name {
		return ownApplicationRefusal(e, http.StatusConflict, "The review belongs to another application", "own_application_review_mismatch")
	}
	if review.State != "completed" || answer.Outcome != "reviewed" || answer.ReviewDigest == "" || answer.ReviewDigest != r.ReviewDigest {
		return ownApplicationRefusal(e, http.StatusConflict, "Reviewed digest differs from the retained review", "own_application_review_mismatch")
	}
	reviewedAt := review.UpdatedAt
	if review.CompletedAt != nil {
		reviewedAt = *review.CompletedAt
	}
	if h.now().Sub(reviewedAt) > ownApplicationReviewLifetime {
		return ownApplicationRefusal(e, http.StatusConflict, "The review has expired; review again", "own_application_review_expired")
	}
	step := ""
	request := jobs.OwnApplicationRequest{Name: t.name, ServerID: t.service.ServerID, ReviewDigest: answer.ReviewDigest, ReviewJobID: review.ID}
	switch reviewed.Step {
	case stackkitcommand.OwnApplicationReview:
		step = reviewed.Action
	case stackkitcommand.OwnApplicationUpdateReview:
		if answer.Target == nil || answer.Target.ResolvedImage == "" {
			return ownApplicationRefusal(e, http.StatusConflict, "The update review resolved no image", "own_application_review_mismatch")
		}
		step = stackkitcommand.OwnApplicationUpdate
		request.ImageTag, request.AllowMajor, request.ResolvedImage = reviewed.ImageTag, reviewed.AllowMajor || answer.AllowMajor, answer.Target.ResolvedImage
	case stackkitcommand.OwnApplicationCheckpointRollbackReview:
		step, request.Checkpoint = stackkitcommand.OwnApplicationCheckpointRollback, reviewed.Checkpoint
	case stackkitcommand.OwnApplicationCheckpointDiscardReview:
		step, request.Checkpoint = stackkitcommand.OwnApplicationCheckpointDiscard, reviewed.Checkpoint
	}
	if !stackkitcommand.OwnApplicationStepExecutes(step) {
		return ownApplicationRefusal(e, http.StatusConflict, "The job is not a reviewable dry run", "own_application_review_mismatch")
	}
	request.Step = step
	// A review is single-use: its execution's durable identity derives from
	// the review job, not the request key, so any later execute of the same
	// review replays that one execution and never runs a second.
	return h.enqueueOwnApplication(e, t, "own-application-execute:"+review.ID, r.ExpectedInventoryRevision, true, request, []string{"execute", review.ID, r.ReviewDigest})
}

func (h *serviceRuntimeHandlers) enqueueOwnApplication(e *httpx.Event, t ownApplicationTarget, key string, revision int64, approved bool, request jobs.OwnApplicationRequest, fingerprint []string) error {
	read := stackkitcommand.OwnApplicationStepRead(request.Step)
	digest := sha256.Sum256([]byte(strings.Join(append([]string{t.tenant, t.service.ID, serviceActionOwnApplication, strconv.FormatInt(revision, 10)}, fingerprint...), "\x00")))
	lifecycle := jobs.StackKitLifecycleRequest{
		StackID: t.stack.ID, StackKitInstanceID: t.stack.StackKitInstanceID, TenantID: t.tenant, OwnerID: t.owner,
		Operation: jobs.StackKitLifecycleOwnApplication, OwnerApproved: approved, ServiceID: t.service.ID,
		DurableJobID: jobs.StackKitServiceActionJobID(t.tenant, t.owner, key), ServiceActionDigest: hex.EncodeToString(digest[:]),
		OwnApplication: &request,
	}
	if replayed, replayErr := h.replayServiceAction(e, t.tenant, t.service, serviceActionOwnApplication, lifecycle); replayed || replayErr != nil {
		return replayErr
	}
	if !read {
		if err := authorizeOwnerServerOperate(e.Request.Context(), h.policy, h.servers, inventoryScope{tenantID: t.tenant, ownerID: t.owner}, t.service.ServerID); err != nil {
			return writeInventoryHTTPError(e, err)
		}
		if t.service.MutationLock.Locked() || len(serviceGroupLockSources(*t.service)) != 0 {
			return ownApplicationRefusal(e, http.StatusConflict, "Service is locked", "service_locked")
		}
	}
	target, err := h.validateServiceActionTarget(e, t.tenant, t.service, t.stack, serviceActionRequest{Action: serviceActionOwnApplication, ExpectedInventoryRevision: revision})
	if err != nil {
		return err
	}
	if err := refuseDuringServerMaintenance(e, h.servers, t.tenant, controlplane.ServerMaintenanceScope{ServerID: target.server.ID, AgentID: target.server.WorkerID}); err != nil {
		return err
	}
	if !h.catalogDispatches(e.Request.Context(), t.tenant, t.stack.ID, "own-application."+request.Step) {
		return ownApplicationRefusal(e, http.StatusServiceUnavailable, "The pinned StackKits release does not run this own application operation", "own_application_release_unavailable")
	}
	lifecycle.AgentID, lifecycle.StackKit = target.server.WorkerID, target.stackKit
	return h.enqueueServiceAction(e, t.tenant, t.stack.ID, t.service, serviceActionOwnApplication, lifecycle)
}

func (h *serviceRuntimeHandlers) loadOwnApplicationJob(e *httpx.Event, t ownApplicationTarget, jobID string) (*controlplane.Job, stackkitcommand.OwnApplicationResult, error) {
	var answer stackkitcommand.OwnApplicationResult
	job, err := h.jobs.GetJob(e.Request.Context(), t.tenant, strings.TrimSpace(jobID))
	if errors.Is(err, controlplane.ErrNotFound) || (err == nil && (job.StackID != t.stack.ID || job.Payload["owner_id"] != t.owner || job.Payload["service_id"] != t.service.ID || job.Payload["operation"] != jobs.StackKitLifecycleOwnApplication)) {
		return nil, answer, httpx.Reject(e, http.StatusNotFound, ksapi.ErrCodeNotFound, "Own application operation not found", nil)
	}
	if err != nil {
		return nil, answer, httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Failed to read the own application operation", nil)
	}
	encoded, _ := json.Marshal(job.Result[jobs.OwnApplicationResultField])
	_ = json.Unmarshal(encoded, &answer)
	return job, answer, nil
}

// readOwnApplicationOperation returns one own-application job and its
// projected node answer: outcome, reason code, review digest, checkpoint,
// target and, for logs, the redacted tail.
func (h *serviceRuntimeHandlers) readOwnApplicationOperation(e *httpx.Event) error {
	t, err := h.ownApplicationTarget(e)
	if err != nil {
		return err
	}
	job, _, err := h.loadOwnApplicationJob(e, t, e.Request.PathValue("jobId"))
	if err != nil {
		return err
	}
	result, _ := job.Result[jobs.OwnApplicationResultField].(map[string]any)
	// Only codes leave: the outcome and reason code of the node answer, never
	// the job's free-text error.
	errorCode := ""
	if strings.EqualFold(job.State, "failed") {
		errorCode = firstNonEmptyString(stringFromAnyMap(result, "reason_code"), stringFromAnyMap(result, "outcome"), "own_application_failed")
	}
	return httpx.Success(e, http.StatusOK, ownApplicationOperationResponse{
		JobID: job.ID, ServiceID: t.service.ID, Operation: stringFromAnyMap(result, "operation"), Status: serviceActionJobStatus(job.State), Result: result, Error: errorCode,
	})
}
