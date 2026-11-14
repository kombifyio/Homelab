package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kombifyio/techstack/internal/stackkitrelease"
	"github.com/kombifyio/techstack/pkg/api/agentpb"
	"github.com/kombifyio/techstack/pkg/controlplane"
	"google.golang.org/protobuf/proto"
)

// NodeStackKitRelease is what a node's Guard reports about its StackKits
// runtime: the release it runs and the published one Techstack has not served
// it yet, which waits for the owner's approved governed upgrade.
type NodeStackKitRelease struct {
	Installed       *agentpb.StackKitReleasePin
	UpdateAvailable string
}

// NodeStackKitReleaseReader reads the node state of the agent's worker.
type NodeStackKitReleaseReader func(ctx context.Context, tenantID, agentID string) (NodeStackKitRelease, error)

var nodeStackKitReleaseReader atomic.Pointer[NodeStackKitReleaseReader]

// SetNodeStackKitReleaseReader installs the node state source; nil removes it.
func SetNodeStackKitReleaseReader(reader NodeStackKitReleaseReader) {
	if reader == nil {
		nodeStackKitReleaseReader.Store(nil)
		return
	}
	nodeStackKitReleaseReader.Store(&reader)
}

// ReadNodeStackKitRelease returns what the agent's node reports about its
// StackKits runtime through the installed node state source. ok is false when
// no source is installed, the read fails or the node withholds no update.
func ReadNodeStackKitRelease(ctx context.Context, tenantID, agentID string) (NodeStackKitRelease, bool) {
	reader := nodeStackKitReleaseReader.Load()
	if reader == nil || strings.TrimSpace(tenantID) == "" || strings.TrimSpace(agentID) == "" {
		return NodeStackKitRelease{}, false
	}
	node, err := (*reader)(ctx, tenantID, agentID)
	if err != nil || node.Installed == nil || strings.TrimSpace(node.UpdateAvailable) == "" {
		return NodeStackKitRelease{}, false
	}
	return node, true
}

// AddressNodeStackKitRelease points a command at the release its node runs
// while that node reports exactly the command's release as available.
// Techstack serves a deployed node a newer runtime only after the approved
// governed upgrade, so the node keeps the CLI that compiled its stored plan,
// and the upgrade itself runs on that CLI. A release the worker's recorded
// target serves is not reported as withheld and keeps the command's pin. Any
// other mismatch keeps the target pin, which the node refuses.
func AddressNodeStackKitRelease(ctx context.Context, tenantID, agentID string, command *agentpb.StackKitCommand) {
	reader := nodeStackKitReleaseReader.Load()
	if reader == nil || command == nil || command.Release == nil {
		return
	}
	node, err := (*reader)(ctx, tenantID, agentID)
	installed := node.Installed
	if err != nil || installed == nil || node.UpdateAvailable == "" {
		return
	}
	target := command.Release
	if stackKitReleaseTag(node.UpdateAvailable) != stackKitReleaseTag(target.Version) ||
		stackKitReleaseTag(installed.Version) == stackKitReleaseTag(target.Version) ||
		installed.PlatformOs != target.PlatformOs || installed.PlatformArch != target.PlatformArch {
		return
	}
	command.Release = proto.Clone(installed).(*agentpb.StackKitReleasePin)
}

func stackKitReleaseTag(version string) string {
	return "v" + strings.TrimPrefix(strings.TrimSpace(version), "v")
}

// FirstRolloutStackKitsRequest names the first rollout of a stack and the
// release that authored its INIT candidate.
type FirstRolloutStackKitsRequest struct {
	TenantID string
	OwnerID  string
	StackID  string
	AgentID  string
	JobID    string
	Release  string
}

// FirstRolloutStackKitsAdmission records, for a node whose stack never applied
// anything, the rollout's release as the target the runtime endpoint serves.
// It changes nothing for a node it cannot prove that for.
type FirstRolloutStackKitsAdmission func(ctx context.Context, request FirstRolloutStackKitsRequest) error

var firstRolloutStackKitsAdmission atomic.Pointer[FirstRolloutStackKitsAdmission]

// SetFirstRolloutStackKitsAdmission installs the admission; nil removes it.
func SetFirstRolloutStackKitsAdmission(admit FirstRolloutStackKitsAdmission) {
	if admit == nil {
		firstRolloutStackKitsAdmission.Store(nil)
		return
	}
	firstRolloutStackKitsAdmission.Store(&admit)
}

// admitFirstRolloutStackKitsRelease runs before the INIT of a rollout that has
// no current node intent. The control plane authors the INIT candidate with its
// pinned release, and an older CLI refuses a candidate of a newer schema (B3
// window m04-20261105-b3c: v0.55.19 `deviceEnrollment.mode: "person"` on the
// node's withheld v0.54.18). Withholding protects a plan an older CLI compiled;
// a stack that never applied anything has none, so its node is served the
// rollout's release and installs it before the INIT runs.
func admitFirstRolloutStackKitsRelease(ctx context.Context, jobID string, req StackKitLifecycleRequest, release stackkitrelease.Release) error {
	admit := firstRolloutStackKitsAdmission.Load()
	if admit == nil {
		return nil
	}
	return (*admit)(ctx, FirstRolloutStackKitsRequest{
		TenantID: req.TenantID, OwnerID: req.OwnerID, StackID: req.StackID, AgentID: req.AgentID,
		JobID: jobID, Release: stackKitReleaseTag(release.Receipt().Version),
	})
}

// StackKitsTargetResultField records, on an owner-approved upgrade job, the
// StackKits target Techstack recorded for the node and its outcome.
const StackKitsTargetResultField = "stackkits_target"

// recordStackKitsTarget records the owner-approved governed upgrade's outcome
// as the node's StackKits target. Only a succeeded upgrade moves the runtime
// the endpoint serves the node; a failed one leaves it on the prior release,
// which the upgrade transaction restored.
func recordStackKitsTarget(ctx context.Context, recorder controlplane.WorkerStackKitsTargetRecorder, jobID string, req StackKitLifecycleRequest, result *agentpb.StackKitResult) (map[string]interface{}, error) {
	version := upgradeResultVersion(result)
	if version == "" {
		version = strings.TrimSpace(req.TargetRelease)
	}
	outcome := controlplane.StackKitsTargetFailed
	if result != nil && result.Success {
		outcome = controlplane.StackKitsTargetSucceeded
	}
	record := map[string]interface{}{"version": version, "outcome": outcome, "recorded": false}
	if recorder == nil {
		return record, fmt.Errorf("the StackKits target store is not configured")
	}
	if !stackKitTargetVersionPattern.MatchString(version) {
		return record, fmt.Errorf("the StackKits upgrade names no exact release")
	}
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		if err = recorder.RecordWorkerStackKitsTarget(ctx, req.TenantID, req.AgentID, controlplane.WorkerStackKitsTarget{
			Version: version, Outcome: outcome, ApprovedBy: req.OwnerID, JobID: jobID,
		}); err == nil {
			record["recorded"] = true
			return record, nil
		}
		select {
		case <-ctx.Done():
			return record, ctx.Err()
		case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
		}
	}
	return record, err
}

// approvedStackKitsUpgradeTarget is the release an owner-approved upgrade
// moves the node to: only the published one, which the runtime endpoint then
// serves. "latest" and the stable channel name it; any other release would
// compile the plan with a CLI the node never receives.
func approvedStackKitsUpgradeTarget(requested string, release stackkitrelease.Release) (string, error) {
	published := stackKitReleaseTag(release.Receipt().Version)
	switch target := strings.TrimSpace(requested); {
	case target == "" || target == "latest" || target == "channel:stable":
		return published, nil
	case stackKitReleaseTag(target) == published:
		return published, nil
	default:
		return "", fmt.Errorf("a StackKits update moves a node only to the published release %s, not %s", published, target)
	}
}

var stackKitTargetVersionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// upgradeResultVersion is the release `stackkit upgrade --json` resolved.
func upgradeResultVersion(result *agentpb.StackKitResult) string {
	if result == nil {
		return ""
	}
	var envelope struct {
		Data struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	if json.Unmarshal(result.CommandResultJson, &envelope) != nil {
		return ""
	}
	return stackKitReleaseTagOrEmpty(envelope.Data.Version)
}

func stackKitReleaseTagOrEmpty(version string) string {
	if strings.TrimSpace(version) == "" {
		return ""
	}
	return stackKitReleaseTag(version)
}
