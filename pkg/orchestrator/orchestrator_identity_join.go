package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kombifyio/techstack/pkg/controlplane"
	"github.com/kombifyio/techstack/pkg/jobs"
)

// HomelabIdentityJoinInput joins a further server to the homelab's Authority
// (homelab identity target §4). The route resolves the stacks from the
// homelab's authority pointer (ADR-036) and the homelab's own deployments;
// the Authority server is the pointer's server.
type HomelabIdentityJoinInput struct {
	HomelabID         string   `json:"homelab_id"`
	AuthorityStackID  string   `json:"authority_stack_id"`
	AuthorityServerID string   `json:"authority_server_id"`
	JoiningStackID    string   `json:"joining_stack_id"`
	OtherStackIDs     []string `json:"other_stack_ids,omitempty"`
	AuthorityRoot     string   `json:"authority_root"`
	Hostnames         []string `json:"hostnames,omitempty"`
	OwnerApproved     bool     `json:"owner_approved"`
}

// identityJoinNode binds one server of the join: the stack's single identity
// node (on the Authority, the pointer's server) and its rollout paths.
func (o *Orchestrator) identityJoinNode(ctx context.Context, tenant, owner, stackID, serverID string) (jobs.HomelabIdentityJoinNode, error) {
	var node jobs.HomelabIdentityJoinNode
	stack, err := o.findStackForJob(ctx, stackID, ProvisionStackOptions{RequestContext: ctx, TenantID: tenant, OwnerID: owner})
	if err != nil {
		return node, err
	}
	if stack.tenantID != tenant || stack.ownerID != owner || stack.stackKitInstanceID == "" {
		return node, controlplane.ErrNotFound
	}
	binding, err := o.identityRuntimeAgentBinding(ctx, stack)
	if err != nil {
		return node, err
	}
	if serverID != "" && binding.ServerID != serverID {
		return node, fmt.Errorf("%w: the homelab's Authority pointer names another server than the stack's identity node", ErrStackKitLifecycleUnavailable)
	}
	rollout := stackKitRolloutBinding(stack)
	normalized := jobs.ApplyStackKitRolloutDefaults(jobs.StackKitLifecycleRequest{StackID: stackID, TenantID: tenant, OwnerID: owner, AgentID: binding.AgentID}, rollout, stackKitCatalogRef(stack))
	return jobs.HomelabIdentityJoinNode{
		StackID: stackID, StackKitInstanceID: stack.stackKitInstanceID, AgentID: binding.AgentID, NodeID: binding.NodeID, NodeRef: binding.NodeRef,
		StackKit: normalized.StackKit, StackName: normalized.StackName, WorkingDirectory: normalized.WorkingDirectory, SpecPath: normalized.SpecPath,
	}, nil
}

// EnqueueHomelabIdentityJoin queues the join of one server. A key names one
// join: repeating it returns the same job and never dispatches the Owner's
// approvals again.
func (o *Orchestrator) EnqueueHomelabIdentityJoin(ctx context.Context, tenant, owner, key string, input HomelabIdentityJoinInput) (string, error) {
	if o == nil || o.jobStore == nil || o.StackKitCommander() == nil {
		return "", ErrStackKitLifecycleUnavailable
	}
	if len(key) < 8 || len(key) > 128 || strings.TrimSpace(key) != key || strings.ContainsAny(key, "\r\n\x00") {
		return "", fmt.Errorf("one bounded Idempotency-Key is required")
	}
	if input.AuthorityStackID == "" || input.JoiningStackID == "" || input.AuthorityStackID == input.JoiningStackID {
		return "", errors.New("the Authority and the joining server are two different deployments of the homelab")
	}
	intent, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	intentDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(intent))
	id := fmt.Sprintf("identity-join-%x", sha256.Sum256([]byte(tenant+"\x00"+owner+"\x00"+key)))
	if existing, err := o.jobStore.GetJob(ctx, tenant, id); err == nil {
		if existing.Type != string(jobs.JobTypeHomelabIdentityJoin) || existing.StackID != input.JoiningStackID || existing.Result["identity_join_request_sha256"] != intentDigest || existing.Payload["owner_id"] != owner {
			return "", controlplane.ErrConflict
		}
		return id, nil
	} else if !errors.Is(err, controlplane.ErrNotFound) {
		return "", err
	}
	authority, err := o.identityJoinNode(ctx, tenant, owner, input.AuthorityStackID, input.AuthorityServerID)
	if err != nil {
		return "", err
	}
	joining, err := o.identityJoinNode(ctx, tenant, owner, input.JoiningStackID, "")
	if err != nil {
		return "", err
	}
	others := []jobs.HomelabIdentityJoinNode{}
	for _, stackID := range input.OtherStackIDs {
		if stackID == input.AuthorityStackID || stackID == input.JoiningStackID {
			continue
		}
		// A server without an approved identity node is left out of the zone.
		if node, nodeErr := o.identityJoinNode(ctx, tenant, owner, stackID, ""); nodeErr == nil {
			others = append(others, node)
		}
	}
	req, err := jobs.NormalizeHomelabIdentityJoinRequest(jobs.HomelabIdentityJoinRequest{
		TenantID: tenant, OwnerID: owner, HomelabID: input.HomelabID, Authority: authority, Joining: joining, Others: others,
		AuthorityRoot: input.AuthorityRoot, Hostnames: input.Hostnames, OwnerApproved: input.OwnerApproved,
	})
	if err != nil {
		return "", err
	}
	payload, err := jobs.HomelabIdentityJoinPayload(req)
	if err != nil {
		return "", err
	}
	payload["tenant_id"], payload["owner_id"], payload["agent_id"] = tenant, owner, joining.AgentID
	targets := []controlplane.ExecutionTarget{{StackID: authority.StackID, AgentID: authority.AgentID}, {StackID: joining.StackID, AgentID: joining.AgentID}}
	for _, other := range others {
		targets = append(targets, controlplane.ExecutionTarget{StackID: other.StackID, AgentID: other.AgentID})
	}
	payload["execution_targets"] = targets
	job := &jobs.Job{ID: id, Type: jobs.JobTypeHomelabIdentityJoin, TargetType: targetTypeStack, TargetID: joining.StackID, TargetName: joining.StackName,
		Payload: payload, Result: map[string]any{"identity_join_request_sha256": intentDigest}, MaxAttempts: 1}
	jobs.CaptureJobAuthorization(ctx, job, tenant, owner)
	_, err = o.jobStore.CreateJob(ctx, controlplane.UpsertJobRequest{ID: id, TenantID: tenant, StackID: joining.StackID, Type: string(job.Type), State: persistentStatePending, Payload: job.Payload, Result: job.Result, ScheduledFor: time.Now().UTC()})
	if err != nil {
		if existing, readErr := o.jobStore.GetJob(ctx, tenant, id); readErr == nil && existing.Type == string(jobs.JobTypeHomelabIdentityJoin) && existing.Result["identity_join_request_sha256"] == intentDigest && existing.Payload["owner_id"] == owner {
			return id, nil
		}
		return "", err
	}
	if err := o.enqueueWithSync(job, tenant); err != nil {
		return "", err
	}
	return id, nil
}
