package routes

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/kombifyio/techstack/pkg/controlplane"
	"github.com/kombifyio/techstack/pkg/guardbinding"
	"github.com/kombifyio/techstack/pkg/serverregistry"
)

// Predecessor result recovery (Guard G04). After a server is bound to a new
// worker of the same tenant, its Guard can still hold the unacknowledged
// outcome of commands the earlier worker received. The control plane lets the
// current worker deliver exactly those outcomes, result-only, under a short-
// lived signed statement. It is issued and admitted only when:
//   - the current request proved the host key (a bearer-only worker gets none);
//   - the current worker has a control-plane server identity;
//   - each earlier worker belongs to the same tenant, served that same server
//     (or, after a detach, was the detached server's worker on the very host
//     key this request proved), and was revoked or rejected, so its own
//     credential can never deliver;
//   - each command is still awaiting an outcome under the binding contract.
//
// A command of another tenant is never covered: the tenant-scoped lookups
// cannot see it, and the Guard keeps it held for a receipted handoff.

type predecessorCommandSource interface {
	PredecessorCommandsForTenant(ctx context.Context, tenantID, agentID string, held []guardbinding.PredecessorCommand) ([]guardbinding.PredecessorCommand, error)
}

type predecessorRecoverySigner interface {
	SignPredecessorRecovery(*guardbinding.PredecessorRecovery) error
}

// predecessorRecoveryFor returns a signed recovery statement for the held prior
// commands this worker may deliver, or nil when none qualifies or the
// statement cannot be signed.
func (h workerRouteHandlers) predecessorRecoveryFor(ctx context.Context, authCtx runtimeAgentAuthContext, held []guardbinding.PredecessorCommand) *guardbinding.PredecessorRecovery {
	if len(held) == 0 || authCtx.IdentityBinding != controlplane.IdentityBindingHostKey || strings.TrimSpace(authCtx.ServerID) == "" {
		return nil
	}
	source, ok := h.typedControl.(predecessorCommandSource)
	if !ok {
		return nil
	}
	if len(held) > guardbinding.MaxPredecessorCommands {
		held = held[:guardbinding.MaxPredecessorCommands]
	}
	eligible := make([]guardbinding.PredecessorCommand, 0, len(held))
	retired := map[string]bool{}
	for _, c := range held {
		worker := strings.TrimSpace(c.WorkerID)
		decided, seen := retired[worker]
		if !seen {
			decided = h.retiredPredecessorOfServer(ctx, authCtx, worker)
			retired[worker] = decided
		}
		if decided {
			eligible = append(eligible, guardbinding.PredecessorCommand{CommandID: strings.TrimSpace(c.CommandID), WorkerID: worker})
		}
	}
	if len(eligible) == 0 {
		return nil
	}
	commands, err := source.PredecessorCommandsForTenant(ctx, authCtx.TenantID, authCtx.RuntimeAgentID, eligible)
	if err != nil {
		slog.Warn("guard_predecessor_recovery_unavailable", "tenant_id", authCtx.TenantID, "runtime_agent_id", authCtx.RuntimeAgentID, "error", err.Error())
		return nil
	}
	if len(commands) == 0 {
		return nil
	}
	signer, err := h.bindingSignerFor(ctx, authCtx.TenantID, authCtx.RuntimeAgentID)
	recoverySigner, ok := signer.(predecessorRecoverySigner)
	if err != nil || signer == nil || !ok {
		// Without a signer of this statement the evidence stays held: an
		// unsigned recovery is never issued.
		return nil
	}
	recovery := guardbinding.NewPredecessorRecovery(authCtx.TenantID, authCtx.RuntimeAgentID, authCtx.ServerID, commands, time.Now())
	if err := recoverySigner.SignPredecessorRecovery(&recovery); err != nil {
		slog.Warn("guard_predecessor_recovery_unsigned", "tenant_id", authCtx.TenantID, "runtime_agent_id", authCtx.RuntimeAgentID, "error", err.Error())
		return nil
	}
	slog.Info("guard_predecessor_recovery_issued", "tenant_id", authCtx.TenantID, "runtime_agent_id", authCtx.RuntimeAgentID,
		"server_id", authCtx.ServerID, "commands", len(commands))
	return &recovery
}

// admitPredecessorDelivery checks one predecessor delivery: the presented
// statement must verify under this worker's own pinned key and cover exactly
// this command and earlier worker, and the earlier worker must still be a
// retired worker of the same server.
func (h workerRouteHandlers) admitPredecessorDelivery(ctx context.Context, authCtx runtimeAgentAuthContext, commandID, predecessor string, recovery *guardbinding.PredecessorRecovery) *guardbinding.Denial {
	if recovery == nil || authCtx.IdentityBinding != controlplane.IdentityBindingHostKey {
		return guardbinding.Denyf(guardbinding.CodePredecessorScope, "a predecessor outcome needs a recovery statement and a host-key proof")
	}
	signer, err := h.bindingSignerFor(ctx, authCtx.TenantID, authCtx.RuntimeAgentID)
	if err != nil || signer == nil {
		return guardbinding.Denyf(guardbinding.CodePredecessorScope, "no signing key verifies a predecessor recovery for this worker")
	}
	expect := guardbinding.Expectation{TenantID: authCtx.TenantID, WorkerID: authCtx.RuntimeAgentID, ServerID: authCtx.ServerID}
	if denial := recovery.AdmitDelivery(signer.PublicKeys(), expect, time.Now(), commandID, predecessor); denial != nil {
		return denial
	}
	if !h.retiredPredecessorOfServer(ctx, authCtx, predecessor) {
		return guardbinding.Denyf(guardbinding.CodePredecessorScope, "the earlier worker is not a retired worker of this server")
	}
	return nil
}

// retiredPredecessorOfServer reports whether workerID is another worker of the
// authenticated tenant and owner that served the same server, or the detached
// server of the same host, and can no longer authenticate on its own.
func (h workerRouteHandlers) retiredPredecessorOfServer(ctx context.Context, authCtx runtimeAgentAuthContext, workerID string) bool {
	if h.wst == nil || workerID == "" || workerID == authCtx.RuntimeAgentID || strings.TrimSpace(authCtx.ServerID) == "" {
		return false
	}
	worker, err := h.wst.GetWorker(ctx, authCtx.TenantID, workerID)
	if err != nil || worker == nil || worker.TenantID != authCtx.TenantID {
		return false
	}
	if strings.TrimSpace(stringFromAny(worker.Capabilities[workerFieldServerID])) != authCtx.ServerID && !h.detachedPredecessorOfHost(ctx, authCtx, *worker) {
		return false
	}
	owner := authCtx.OwnerID
	if authCtx.Worker != nil {
		owner = authCtx.Worker.OwnerSubjectID
	}
	if worker.OwnerSubjectID != owner {
		return false
	}
	return strings.EqualFold(worker.Status, workerStatusRevoked) || strings.EqualFold(worker.Status, managedRuntimeStatusRejected)
}

// detachedPredecessorOfHost: a detach leaves an immutable tombstone, so pairing
// the same host again binds a new server of the stack. The earlier worker still
// qualifies when it was the tombstone's own worker in the same stack and its
// retained identity is the host key this request proved.
func (h workerRouteHandlers) detachedPredecessorOfHost(ctx context.Context, authCtx runtimeAgentAuthContext, worker controlplane.Worker) bool {
	if h.identityStore == nil || h.serverStore == nil || authCtx.ProofThumbprint == "" || authCtx.Worker == nil ||
		strings.TrimSpace(worker.StackID) == "" || worker.StackID != authCtx.Worker.StackID {
		return false
	}
	identity, err := h.identityStore.GetWorkerIdentity(ctx, authCtx.TenantID, worker.ID)
	if err != nil || identity == nil || identity.Thumbprint != authCtx.ProofThumbprint {
		return false
	}
	serverID := strings.TrimSpace(stringFromAny(worker.Capabilities[workerFieldServerID]))
	if serverID == "" {
		return false
	}
	server, err := h.serverStore.GetServerRuntime(ctx, authCtx.TenantID, serverID)
	return err == nil && server != nil && server.LifecycleState == string(serverregistry.LifecycleDecommissioned) &&
		strings.TrimSpace(server.WorkerID) == worker.ID
}
