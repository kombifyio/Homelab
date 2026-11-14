package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kombifyio/techstack/pkg/controlplane"
	"github.com/kombifyio/techstack/pkg/jobs"
	"github.com/kombifyio/techstack/pkg/stackkitcommand"
	"time"
)

var ErrHomeGraphAdoptionStale = errors.New("adoption graph changed; inventory again")

type HomeGraphAdoption struct {
	ServiceID string
	JobID     string
	Status    string
	ErrorCode string
	Result    json.RawMessage
}

func (o *Orchestrator) EnqueueHomeGraphAdoption(ctx context.Context, tenantID, ownerID, spaceID, serviceID string, document json.RawMessage) (string, error) {
	if o == nil || o.jobStore == nil {
		return "", ErrStackKitLifecycleUnavailable
	}
	store := o.homeGraphStore()
	if store == nil {
		return "", ErrStackKitLifecycleUnavailable
	}
	request, header, err := stackkitcommand.DecodeHomeGraphAdoptionRequest(document)
	if err != nil || header.SpaceID != spaceID {
		return "", controlplane.ErrConflict
	}
	target, err := o.homeGraphAdoptionTarget(ctx, tenantID, ownerID, serviceID)
	if err != nil {
		return "", err
	}
	if target == nil {
		return "", ErrHomeGraphNoConnector
	}
	if target.MutationLock.Locked() {
		return "", fmt.Errorf("%w: %w", ErrHomeGraphFenced, controlplane.ErrConflict)
	}
	state, err := store.GetHomeGraph(ctx, tenantID, ownerID, spaceID)
	if err != nil {
		return "", err
	}
	if state.GraphRevision != header.Revision || !stackkitcommand.SameHomeGraphContent(state.Graph, request.Graph) {
		return "", ErrHomeGraphAdoptionStale
	}
	runtimeStore, ok := o.cfg.StackStore.(interface {
		controlplane.ServiceRuntimeStore
		controlplane.ServiceEventReader
	})
	serviceJobs, okJobs := o.jobStore.(controlplane.ServiceOperationJobStore)
	if !ok || !okJobs {
		return "", ErrStackKitLifecycleUnavailable
	}
	head, err := runtimeStore.ReadServiceEventHead(ctx, tenantID, target.ID)
	if err != nil {
		return "", err
	}
	originalID, _ := head.Service.Metadata["application_connector_job_id"].(string)
	original, err := serviceJobs.GetServiceOperationJob(ctx, tenantID, head.Service.StackID, target.ID, originalID)
	if err != nil {
		return "", err
	}
	req, err := jobs.ApplicationHomeGraphAdoptionRequestForStack(ctx, original, head.Service, spaceID, state.GraphRevision, document, time.Now())
	if err != nil {
		return "", err
	}
	if req.TenantID != tenantID || req.OwnerID != ownerID {
		return "", controlplane.ErrNotFound
	}
	if err = o.authorizeApplicationConnector(ctx, req); err != nil {
		return "", err
	}
	competing, err := serviceJobs.FindUnresolvedServiceJob(ctx, tenantID, req.StackID, target.ID, req.DurableJobID)
	if err != nil {
		return "", err
	}
	if competing != "" {
		return "", fmt.Errorf("%w: %w", ErrHomeGraphFenced, controlplane.ErrConflict)
	}
	if err = jobs.ApplicationHomeGraphAdoptionReleaseAvailable(ctx, tenantID, req.StackID); err != nil {
		return "", err
	}
	return o.EnqueueStackKitLifecycle(ctx, req)
}

func (o *Orchestrator) homeGraphAdoptionTarget(ctx context.Context, tenantID, ownerID, serviceID string) (*controlplane.ServiceRuntime, error) {
	if serviceID == "" {
		return o.homeGraphTargetService(ctx, tenantID, ownerID)
	}
	stacks, ok := o.cfg.StackStore.(controlplane.StackStore)
	services, okServices := o.cfg.StackStore.(controlplane.ServiceRuntimeStore)
	if !ok || !okServices {
		return nil, ErrStackKitLifecycleUnavailable
	}
	all, err := stacks.ListStacksByTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	for _, stack := range all {
		if stack.OwnerSubjectID != ownerID {
			continue
		}
		rows, err := services.ListServiceRuntimes(ctx, tenantID, stack.ID, "")
		if err != nil {
			return nil, err
		}
		for i := range rows {
			projection, _ := rows[i].Metadata["application_connector"].(map[string]any)
			if rows[i].ID == serviceID && rows[i].ServiceKey == "smart-home" && projection["status"] == "active" {
				return &rows[i], nil
			}
		}
	}
	return nil, controlplane.ErrNotFound
}
func (o *Orchestrator) HomeGraphAdoptionStatus(ctx context.Context, tenantID, ownerID, spaceID, jobID string) (*HomeGraphAdoption, error) {
	if o == nil || o.jobStore == nil {
		return nil, ErrStackKitLifecycleUnavailable
	}
	job, err := o.jobStore.GetJob(ctx, tenantID, jobID)
	if err != nil {
		return nil, err
	}
	if job == nil || job.TenantID != tenantID || job.Payload["operation"] != jobs.StackKitLifecycleApplicationHomeGraphAdopt {
		return nil, controlplane.ErrNotFound
	}
	// Bind pending, running and failed jobs too, before exposing any status.
	raw, err := json.Marshal(job.Payload["connector_binding"])
	var binding jobs.ApplicationConnectorBinding
	if err != nil || json.Unmarshal(raw, &binding) != nil || job.Payload["owner_id"] != ownerID || binding.HomeGraphSpaceID != spaceID {
		return nil, controlplane.ErrNotFound
	}
	serviceID, ok := job.Payload["service_id"].(string)
	if !ok || serviceID == "" {
		return nil, controlplane.ErrNotFound
	}
	out := &HomeGraphAdoption{ServiceID: serviceID, JobID: job.ID, Status: "pending"}
	switch job.State {
	case string(jobs.JobStateCompleted):
		out.Result, err = json.Marshal(job.Result[jobs.HomeGraphAdoptionResultField])
		if err != nil || string(out.Result) == "null" {
			return nil, fmt.Errorf("adoption result incomplete")
		}
		out.Status = "completed"
	case string(jobs.JobStateFailed), string(jobs.JobStateCancelled):
		out.Status = "failed"
		out.ErrorCode, _ = job.Result[jobs.HomeGraphAdoptionErrorCodeResultField].(string)
		if out.ErrorCode == "" {
			out.ErrorCode = "home_graph_adoption_failed"
		}
	case string(jobs.JobStateRunning):
		out.Status = "running"
	}
	return out, nil
}
