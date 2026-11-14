package actions

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kombifyio/techstack/pkg/ril/actioncontract"
	"github.com/kombifyio/techstack/pkg/ril/primitives"
)

const executionAdmissionVersion = "techstack.ril-execution-admission/v2"

// ExecutionAdmission is Techstack's internal immutable binding between an
// approved action and the current Runtime Inventory and lifecycle authority
// heads (LifecycleAuthority). It is never serialized into the provider-free
// RIL request or public evidence.
type ExecutionAdmission struct {
	InventoryRevision int64
	ServerRevision    int64
	ServerGeneration  int64
	// LifecycleAuthorityID is the VM lease id or `server:<id>`;
	// LifecycleRevision the lease revision or the server generation; and
	// LifecycleGenerationID the lease's resource generation or the name-based
	// UUID of the owner server's generation identity.
	LifecycleAuthorityID  string
	LifecycleRevision     int64
	LifecycleGenerationID string
	RequestDigest         string
	Digest                string
}

type executionAdmissionPayload struct {
	Version                string `json:"version"`
	TenantID               string `json:"tenant_id"`
	OwnerSubjectID         string `json:"owner_subject_id"`
	ServerID               string `json:"server_id"`
	InventoryRevision      int64  `json:"inventory_revision"`
	ServerRevision         int64  `json:"server_revision"`
	ServerGeneration       int64  `json:"server_generation"`
	LifecycleAuthorityKind string `json:"lifecycle_authority_kind"`
	LifecycleAuthorityID   string `json:"lifecycle_authority_id"`
	LifecycleRevision      int64  `json:"lifecycle_revision"`
	LifecycleGenerationRef string `json:"lifecycle_generation_ref"`
	RequestDigest          string `json:"request_digest"`
}

func admitExecutionTx(ctx context.Context, tx *sql.Tx, card *GovernedCard, request rilaction.Request, now time.Time) (ExecutionAdmission, rilaction.Request, error) {
	authority, err := LockLifecycleAuthorityTx(ctx, tx, request.TenantID, card.OwnerSubjectID, card.ServerID, request.StackID, now)
	if err != nil {
		return ExecutionAdmission{}, rilaction.Request{}, err
	}
	if !admittedTarget(request, card.ServerID, authority.WorkerID) {
		return ExecutionAdmission{}, rilaction.Request{}, fmt.Errorf("%w: execution target is not the current runtime worker", ErrExecutionAdmission)
	}
	requestValidUntil, err := time.Parse(time.RFC3339Nano, request.ValidUntil)
	if err != nil {
		return ExecutionAdmission{}, rilaction.Request{}, fmt.Errorf("%w: action request validity is invalid", ErrExecutionAdmission)
	}
	if authority.ValidUntil.Before(requestValidUntil) {
		request.ValidUntil = authority.ValidUntil.UTC().Format(time.RFC3339Nano)
	}
	if err := rilaction.ValidateRequestAt(request, now); err != nil {
		return ExecutionAdmission{}, rilaction.Request{}, err
	}
	requestDigest, err := rilaction.ComputeRequestDigest(request)
	if err != nil {
		return ExecutionAdmission{}, rilaction.Request{}, err
	}
	payload := executionAdmissionPayload{
		Version: executionAdmissionVersion, TenantID: request.TenantID,
		OwnerSubjectID: card.OwnerSubjectID, ServerID: card.ServerID,
		InventoryRevision: authority.InventoryRevision, ServerRevision: authority.ServerRevision,
		ServerGeneration: authority.ServerGeneration, LifecycleAuthorityKind: string(authority.Kind),
		LifecycleAuthorityID: authority.ID, LifecycleRevision: authority.Revision,
		LifecycleGenerationRef: authority.GenerationRef, RequestDigest: requestDigest,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return ExecutionAdmission{}, rilaction.Request{}, fmt.Errorf("encode execution admission: %w", err)
	}
	return ExecutionAdmission{
		InventoryRevision: authority.InventoryRevision, ServerRevision: authority.ServerRevision,
		ServerGeneration: authority.ServerGeneration, LifecycleAuthorityID: authority.ID,
		LifecycleRevision: authority.Revision, LifecycleGenerationID: authority.GenerationID,
		RequestDigest: requestDigest, Digest: digestText(string(data)),
	}, request, nil
}

// admittedTarget binds the provider-free target to the locked server. An
// executor-bound primitive must match its catalog profile exactly: a stack
// target carries nothing narrower; a runtime-instance target names this
// server and its current worker as both runtime instance and execution
// channel; a module-instance target names this server. Any other governed
// primitive keeps its runtime-instance rule (the current worker).
func admittedTarget(request rilaction.Request, serverID, workerID string) bool {
	target := request.Target
	profile, executorBound := primitives.For(request.Primitive)
	if !executorBound {
		return !primitives.IsExecutorBound(request.Primitive.ID) && target.Scope == rilaction.TargetScopeRuntimeInstance && target.RuntimeInstanceRef == workerID
	}
	if !profile.Admits(target, request.Inputs) {
		return false
	}
	switch target.Scope {
	case rilaction.TargetScopeStack:
		return true
	case rilaction.TargetScopeRuntimeInstance:
		return target.SiteRef == serverID && target.RuntimeInstanceRef == workerID && target.ExecutionChannelRef == workerID
	case rilaction.TargetScopeModuleInstance:
		return target.SiteRef == serverID && target.ModuleInstanceRef == primitives.BackupModuleInstanceRef(request.StackID) &&
			primitives.InputValue(request.Inputs, primitives.InputBackupContractRef) == request.StackID
	default:
		return false
	}
}
