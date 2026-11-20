package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/jobs"
)

// HomeGraphLoxoneImport is the state of one Loxone import run as Cloud reads
// it. Graph is the merged kombify.home_graph.v1 proposal of a completed run.
type HomeGraphLoxoneImport struct {
	JobID        string
	Status       string // pending, running, completed or failed
	ErrorCode    string
	Revision     int64
	RoomsAdded   int
	DevicesAdded int
	Graph        json.RawMessage
}

// EnqueueHomeGraphLoxoneImport enqueues one application_home_graph_loxone_import
// run for the owner's stored space on the node of the home graph sync (the
// deterministic smart-home target service). The Miniserver credential stays
// in that node's workspace; nothing here carries it.
func (o *Orchestrator) EnqueueHomeGraphLoxoneImport(ctx context.Context, tenantID, ownerID, spaceID string) (string, error) {
	if o == nil || o.jobStore == nil {
		return "", ErrStackKitLifecycleUnavailable
	}
	store := o.homeGraphStore()
	if store == nil {
		return "", ErrStackKitLifecycleUnavailable
	}
	target, err := o.homeGraphTargetService(ctx, tenantID, ownerID)
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
	req, err := jobs.ApplicationHomeGraphLoxoneImportRequestForStack(ctx, original, head.Service, spaceID, state.GraphRevision, time.Now())
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
	if err = jobs.ApplicationHomeGraphLoxoneImportReleaseAvailable(ctx, tenantID, req.StackID); err != nil {
		return "", err
	}
	return o.EnqueueStackKitLifecycle(ctx, req)
}

// HomeGraphLoxoneImportStatus reads one import run of the owner's space.
// Another owner's, another space's or another kind of job is not found.
func (o *Orchestrator) HomeGraphLoxoneImportStatus(ctx context.Context, tenantID, ownerID, spaceID, jobID string) (*HomeGraphLoxoneImport, error) {
	if o == nil || o.jobStore == nil {
		return nil, ErrStackKitLifecycleUnavailable
	}
	job, err := o.jobStore.GetJob(ctx, tenantID, jobID)
	if err != nil {
		return nil, err
	}
	if job == nil || job.TenantID != tenantID {
		return nil, controlplane.ErrNotFound
	}
	if operation, _ := job.Payload["operation"].(string); operation != jobs.StackKitLifecycleApplicationHomeGraphLoxoneImport {
		return nil, controlplane.ErrNotFound
	}
	if owner, _ := job.Result[jobs.HomeGraphLoxoneOwnerResultField].(string); owner != "" && owner != ownerID {
		return nil, controlplane.ErrNotFound
	}
	if space, _ := job.Result[jobs.HomeGraphLoxoneSpaceResultField].(string); space != "" && space != spaceID {
		return nil, controlplane.ErrNotFound
	}
	out := &HomeGraphLoxoneImport{JobID: job.ID}
	switch job.State {
	case string(jobs.JobStateCompleted):
		raw, err := json.Marshal(job.Result[jobs.HomeGraphLoxoneProposalResultField])
		owner, _ := job.Result[jobs.HomeGraphLoxoneOwnerResultField].(string)
		space, _ := job.Result[jobs.HomeGraphLoxoneSpaceResultField].(string)
		if err != nil || owner != ownerID || space != spaceID || string(raw) == "null" {
			return nil, errors.New("orchestrator: Loxone import result is incomplete")
		}
		out.Status, out.Graph = "completed", raw
		out.Revision = int64(resultNumber(job.Result[jobs.HomeGraphLoxoneRevisionResultField]))
		out.RoomsAdded = int(resultNumber(job.Result[jobs.HomeGraphLoxoneRoomsResultField]))
		out.DevicesAdded = int(resultNumber(job.Result[jobs.HomeGraphLoxoneDevicesResultField]))
	case string(jobs.JobStateFailed), string(jobs.JobStateCancelled):
		out.Status = "failed"
		out.ErrorCode, _ = job.Result[jobs.HomeGraphLoxoneErrorCodeResultField].(string)
		if out.ErrorCode == "" {
			out.ErrorCode = "loxone_import_failed"
		}
	case string(jobs.JobStateRunning):
		out.Status = "running"
	default:
		out.Status = "pending"
	}
	return out, nil
}

// resultNumber reads a number of a job result that went through JSON (float64)
// or stayed in memory (int, int64).
func resultNumber(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}
