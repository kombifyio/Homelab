package routes

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kombifyio/techstack/internal/routes/stacks"
	"github.com/kombifyio/techstack/pkg/controlplane"
	"github.com/kombifyio/techstack/pkg/orchestrator"
)

// NewWizardExistingServers connects first installation, and a join of an
// enrolled host as a new node, to the same canonical CAS assignment used by
// the operations route. It never enrolls a new host.
func NewWizardExistingServers(stores StackOperationsRouteStores) stacks.WizardExistingServers {
	return wizardExistingServers{stackOperationsRouteHandlers{
		stackStore: stores.Stacks, serverStore: stores.Servers, workerStore: stores.Workers,
	}}
}

type wizardExistingServers struct{ operations stackOperationsRouteHandlers }

func (a wizardExistingServers) Resolve(ctx context.Context, req stacks.WizardExistingServerRequest) error {
	_, err := a.resolve(ctx, req)
	return err
}

func (a wizardExistingServers) resolve(ctx context.Context, req stacks.WizardExistingServerRequest) (*controlplane.Worker, error) {
	h := a.operations
	if h.serverStore == nil || h.workerStore == nil || h.stackStore == nil {
		return nil, fmt.Errorf("canonical assignment stores are unavailable")
	}
	if req.TenantID == "" || req.OwnerID == "" || req.StackID == "" || req.ServerID == "" {
		return nil, controlplane.ErrConflict
	}
	server, err := h.serverStore.GetServerRuntime(ctx, req.TenantID, req.ServerID)
	if err != nil {
		return nil, err
	}
	if server.TenantID != req.TenantID || server.OwnerSubjectID != req.OwnerID {
		return nil, controlplane.ErrNotFound
	}
	// A join takes a bare server: never one that already serves this or
	// another deployment, except the node this same join already bound.
	if req.SpecNodeID != "" && strings.TrimSpace(server.StackID) != "" {
		if bound, err := runtimeBoundToNode(*server, req.StackID, req.SpecNodeID); err != nil || !bound ||
			stringFromAnyMap(server.Metadata, "spec_node_id") != req.SpecNodeID {
			return nil, controlplane.ErrConflict
		}
	}
	// Managed leased machines retain provider custody; this lane binds an
	// existing self-owned server, never reassigns a provider lease.
	if server.LeaseID != "" || strings.TrimSpace(server.WorkerID) == "" ||
		stringFromAnyMap(server.Metadata, "server_node_role") == "substrate" {
		return nil, controlplane.ErrConflict
	}
	worker, err := h.workerStore.GetWorker(ctx, req.TenantID, server.WorkerID)
	if err != nil {
		return nil, err
	}
	if worker.Type == "substrate" || !worker.Approved || !workerAssignmentStatusAllowed(worker.Status) ||
		(worker.StackID != "" && worker.StackID != req.StackID) ||
		!orchestrator.GuardRuntimeDeployEligible(*server, time.Now().UTC()) {
		return nil, controlplane.ErrConflict
	}
	// The worker row is a projection of the same binding: a join refuses a
	// worker some deployment already claims, unless this join bound it.
	if req.SpecNodeID != "" && strings.TrimSpace(worker.StackID) != "" && strings.TrimSpace(server.StackID) != req.StackID {
		return nil, controlplane.ErrConflict
	}
	if err := validateWorkerRuntimeAssignment(*server, *worker, req.TenantID, req.OwnerID, req.StackID); err != nil {
		return nil, err
	}
	canonicalID, err := assignmentServerID(*worker)
	if err != nil || canonicalID != req.ServerID {
		return nil, controlplane.ErrConflict
	}
	return worker, nil
}

func (a wizardExistingServers) Assign(ctx context.Context, req stacks.WizardExistingServerRequest) error {
	worker, err := a.resolve(ctx, req)
	if err != nil {
		return err
	}
	stack, err := a.operations.stackStore.GetStack(ctx, req.TenantID, req.StackID)
	if err != nil {
		return err
	}
	if stack.OwnerSubjectID != req.OwnerID || stack.TenantID != req.TenantID {
		return controlplane.ErrNotFound
	}
	_, err = a.operations.assignCanonicalWorker(ctx, req.TenantID, req.OwnerID, req.StackID, req.SpecNodeID, *worker)
	return err
}
