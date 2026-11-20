package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/jobs"
)

const (
	// HomeGraphDriftCheckEvery is how often a synced home graph is compared
	// with Home Assistant again (drift detection).
	HomeGraphDriftCheckEvery = 15 * time.Minute
	// HomeGraphRetryAfter is how long a failed run waits before the reconcile
	// loop retries it.
	HomeGraphRetryAfter = 5 * time.Minute
)

var (
	// ErrHomeGraphNoConnector means the owner has no homelab service with an
	// active Home Assistant connector, or the pinned release lacks the op.
	ErrHomeGraphNoConnector = errors.New("orchestrator: no active Home Assistant connector for the home graph")
	// ErrHomeGraphFenced means an unresolved job is fenced on the service.
	ErrHomeGraphFenced = errors.New("orchestrator: home graph sync waits for the fenced service operation")
)

func (o *Orchestrator) homeGraphStore() controlplane.HomeGraphStore {
	if o == nil {
		return nil
	}
	store, _ := o.cfg.StackStore.(controlplane.HomeGraphStore)
	return store
}

// HomeGraphStore returns the durable home graph state, nil without Postgres.
func (o *Orchestrator) HomeGraphStore() controlplane.HomeGraphStore { return o.homeGraphStore() }

func activeConnectorService(service controlplane.ServiceRuntime) bool {
	projection, _ := service.Metadata["application_connector"].(map[string]any)
	return projection["status"] == "active" && !service.MutationLock.Locked()
}

// homeGraphTargetService chooses the one service that runs the owner's home
// graph syncs: the smart-home service with an active connector that has the
// lowest service ID across the owner's stacks. The choice ignores the mutation
// lock, so it does not flip while a lock is held; a locked target is fenced.
// The POST path and the reconcile path both use it, so two nodes never race.
func (o *Orchestrator) homeGraphTargetService(ctx context.Context, tenantID, ownerID string) (*controlplane.ServiceRuntime, error) {
	stacks, ok := o.cfg.StackStore.(controlplane.StackStore)
	services, okServices := o.cfg.StackStore.(controlplane.ServiceRuntimeStore)
	if !ok || !okServices {
		return nil, ErrStackKitLifecycleUnavailable
	}
	all, err := stacks.ListStacksByTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	var target *controlplane.ServiceRuntime
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
			if rows[i].ServiceKey != "smart-home" || projection["status"] != "active" {
				continue
			}
			if target == nil || rows[i].ID < target.ID {
				target = &rows[i]
			}
		}
	}
	return target, nil
}

// EnqueueHomeGraphSync enqueues one application_home_graph_sync run for the
// owner's space on the deterministic target service. It reuses the original
// connector setup binding like the connector refresh, never runs while an
// unresolved job is fenced on the service and only when the pinned release
// catalog lists the operation.
func (o *Orchestrator) EnqueueHomeGraphSync(ctx context.Context, tenantID, ownerID, spaceID string) (string, error) {
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
	id, err := o.enqueueHomeGraphSyncForService(ctx, store, tenantID, ownerID, spaceID, target.ID, false)
	if err != nil && !errors.Is(err, ErrHomeGraphFenced) {
		// A connector exists but this call could not admit the run (for
		// example the release lacks the operation); the reconcile loop retries.
		o.log.Warn("home_graph_sync_not_admitted", "service_id", target.ID)
	}
	return id, err
}

// HasHomeGraphConnector reports whether the owner has a service with an active
// Home Assistant connector (the sync status is "none" without one).
func (o *Orchestrator) HasHomeGraphConnector(ctx context.Context, tenantID, ownerID string) bool {
	if o == nil {
		return false
	}
	target, err := o.homeGraphTargetService(ctx, tenantID, ownerID)
	return err == nil && target != nil
}

func (o *Orchestrator) enqueueHomeGraphSyncForService(ctx context.Context, store controlplane.HomeGraphStore, tenantID, ownerID, spaceID, serviceID string, cachedRelease bool) (string, error) {
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
	head, err := runtimeStore.ReadServiceEventHead(ctx, tenantID, serviceID)
	if err != nil {
		return "", err
	}
	originalID, _ := head.Service.Metadata["application_connector_job_id"].(string)
	original, err := serviceJobs.GetServiceOperationJob(ctx, tenantID, head.Service.StackID, serviceID, originalID)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(state.Restore)
	req, err := jobs.ApplicationHomeGraphSyncRequestForStack(ctx, original, head.Service, spaceID, state.GraphRevision, hex.EncodeToString(digest[:8]), time.Now())
	if err != nil {
		return "", err
	}
	if req.TenantID != tenantID || req.OwnerID != ownerID {
		return "", controlplane.ErrNotFound
	}
	if err = o.authorizeApplicationConnector(ctx, req); err != nil {
		return "", err
	}
	competing, err := serviceJobs.FindUnresolvedServiceJob(ctx, tenantID, req.StackID, serviceID, req.DurableJobID)
	if err != nil {
		return "", err
	}
	if competing != "" {
		return "", fmt.Errorf("%w: %w", ErrHomeGraphFenced, controlplane.ErrConflict)
	}
	if err = jobs.ApplicationHomeGraphReleaseAvailable(ctx, tenantID, req.StackID, cachedRelease); err != nil {
		return "", err
	}
	id, err := o.EnqueueStackKitLifecycle(ctx, req)
	if err != nil {
		return "", err
	}
	_ = store.MarkHomeGraphAttempt(ctx, tenantID, ownerID, spaceID)
	return id, nil
}

// reconcileHomeGraphs runs inside ReconcileApplicationConnectors for one
// service with an active connector: it enqueues every stored space of the
// owner that has an unsynced revision, a waiting restore, a failed run to
// retry, or a last sync older than the drift interval.
func (o *Orchestrator) reconcileHomeGraphs(ctx context.Context, tenantID, ownerID string, service controlplane.ServiceRuntime) {
	store := o.homeGraphStore()
	if store == nil || service.ServiceKey != "smart-home" || !activeConnectorService(service) {
		return
	}
	states, err := store.ListHomeGraphs(ctx, tenantID, ownerID)
	if err != nil || len(states) == 0 {
		return
	}
	// Only the deterministic target service runs the owner's syncs.
	target, err := o.homeGraphTargetService(ctx, tenantID, ownerID)
	if err != nil || target == nil || target.ID != service.ID {
		return
	}
	if err = jobs.ApplicationHomeGraphReleaseAvailable(ctx, tenantID, service.StackID, true); err != nil {
		return
	}
	now := time.Now()
	for _, state := range states {
		if !controlplane.HomeGraphRetryDue(state, now, HomeGraphRetryAfter, HomeGraphDriftCheckEvery) {
			continue
		}
		if _, err = o.enqueueHomeGraphSyncForService(ctx, store, tenantID, ownerID, state.SpaceID, service.ID, true); err != nil && !errors.Is(err, ErrHomeGraphFenced) {
			o.log.Warn("home_graph_sync_not_admitted", "service_id", service.ID, "space_id", strings.TrimSpace(state.SpaceID))
		}
	}
}
