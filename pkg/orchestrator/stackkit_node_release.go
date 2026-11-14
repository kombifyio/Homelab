package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kombifyio/techstack/pkg/api/agentpb"
	"github.com/kombifyio/techstack/pkg/controlplane"
	"github.com/kombifyio/techstack/pkg/jobs"
	"github.com/kombifyio/techstack/pkg/runtimeconvergence"
)

// nodeStackKitRelease reads the StackKits runtime a node's Guard last
// reported: the installed release pin and the update it proposes. An update
// the worker's recorded target already serves is not withheld, so no command
// is pointed back at the installed release.
func (o *Orchestrator) nodeStackKitRelease(ctx context.Context, tenantID, agentID string) (jobs.NodeStackKitRelease, error) {
	if o.workerStore == nil {
		return jobs.NodeStackKitRelease{}, nil
	}
	worker, err := o.workerStore.GetWorker(ctx, tenantID, agentID)
	if err != nil || worker == nil {
		return jobs.NodeStackKitRelease{}, err
	}
	return withheldNodeStackKitRelease(worker), nil
}

// nodeUpdateSetting reads the node's update setting (ask by default).
func (o *Orchestrator) nodeUpdateSetting(ctx context.Context, tenantID, agentID string) (string, error) {
	if o.workerStore == nil {
		return "", fmt.Errorf("orchestrator: worker store unavailable")
	}
	worker, err := o.workerStore.GetWorker(ctx, tenantID, agentID)
	if err != nil {
		return "", err
	}
	if worker == nil {
		return "", controlplane.ErrNotFound
	}
	return controlplane.WorkerStackKitsUpdateBehaviorOf(worker).Behavior, nil
}

// withheldNodeStackKitRelease is the installed release and the update the
// runtime endpoint withholds from the worker's node. An update the worker's
// recorded target already serves is not withheld, so no command is pointed
// back at the installed release.
func withheldNodeStackKitRelease(worker *controlplane.Worker) jobs.NodeStackKitRelease {
	var node jobs.NodeStackKitRelease
	raw, err := json.Marshal(worker.Resources["runtime_convergence"])
	if err != nil {
		return node
	}
	var snapshot runtimeconvergence.Snapshot
	if json.Unmarshal(raw, &snapshot) != nil {
		return node
	}
	snapshot, err = runtimeconvergence.Normalize(snapshot)
	if err != nil {
		return node
	}
	for _, component := range snapshot.Components {
		if component.Name != runtimeconvergence.StackKitsRuntimeComponent || component.Release == nil || component.UpdateAvailable == nil {
			continue
		}
		if controlplane.WorkerStackKitsTargetServes(worker, component.UpdateAvailable.Version) {
			continue
		}
		release := component.Release
		node.Installed = &agentpb.StackKitReleasePin{
			Version: release.Version, PlatformOs: release.PlatformOS, PlatformArch: release.PlatformArch,
			ArchiveSha256: release.ArchiveSHA256, ReleaseIndexSha256: release.ReleaseIndexSHA256,
		}
		node.UpdateAvailable = component.UpdateAvailable.Version
	}
	return node
}

// admitFirstRolloutStackKitsRelease records the rollout's release as the
// StackKits target of a node whose stack never applied anything, so the
// runtime endpoint serves it and the first INIT runs on the release that
// authored its candidate. It acts only when the rollout's own worker reports
// that release as withheld; the job history must then prove that no Apply ran
// for the stack (the destroy's no-Apply proof, the running rollout excepted).
// Without that proof nothing changes and the node keeps its installed release.
func (o *Orchestrator) admitFirstRolloutStackKitsRelease(ctx context.Context, request jobs.FirstRolloutStackKitsRequest) error {
	recorder, ok := o.workerStore.(controlplane.WorkerStackKitsTargetRecorder)
	if o.workerStore == nil || o.jobStore == nil || !ok {
		return nil
	}
	worker, err := o.workerStore.GetWorker(ctx, request.TenantID, request.AgentID)
	if err != nil {
		return fmt.Errorf("admit the first rollout's StackKits release: read the node: %w", err)
	}
	if worker == nil || worker.TenantID != request.TenantID || strings.TrimSpace(worker.StackID) != request.StackID {
		return nil
	}
	if node := withheldNodeStackKitRelease(worker); node.Installed == nil || stackKitsReleaseTag(node.UpdateAvailable) != stackKitsReleaseTag(request.Release) {
		return nil
	}
	history, err := o.jobStore.ListJobsByStack(ctx, request.TenantID, request.StackID, jobs.NoApplyJobScanLimit)
	if err != nil {
		return fmt.Errorf("admit the first rollout's StackKits release: read the jobs of stack %s: %w", request.StackID, err)
	}
	if proofErr := jobs.ProveNoApplyBeforeRollout(history, request.JobID); proofErr != nil {
		o.log.Warn("stackkits_first_rollout_release_withheld", "stack_id", request.StackID, "worker_id", request.AgentID,
			"release", request.Release, "reason", proofErr.Error())
		return nil
	}
	if err := recorder.RecordWorkerStackKitsTarget(ctx, request.TenantID, request.AgentID, controlplane.WorkerStackKitsTarget{
		Version: request.Release, Outcome: controlplane.StackKitsTargetSucceeded, Source: controlplane.StackKitsTargetSourceFirstRollout,
		ApprovedBy: request.OwnerID, JobID: request.JobID,
	}); err != nil {
		return fmt.Errorf("admit the first rollout's StackKits release: record the node's target: %w", err)
	}
	return nil
}

func stackKitsReleaseTag(version string) string {
	return "v" + strings.TrimPrefix(strings.TrimSpace(version), "v")
}
