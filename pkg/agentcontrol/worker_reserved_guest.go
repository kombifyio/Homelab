package agentcontrol

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kombifyio/homelab/internal/substrate"
	providerexecutor "github.com/kombifyio/stackkits/contracts/providerexecutor/v1beta1"
)

var errReservedGuestObserveNotAdmitted = errors.New("reserved guest observe is not admitted for a failed substrate provision head")

// admitWorkerExecution selects the one admission an invocation may use. The
// claimed fence is unchanged; a reserved-guest observe never rides it and no
// other invocation reaches the claimless fence below.
func admitWorkerExecution(ctx context.Context, tx *sql.Tx, execution providerexecutor.WorkerExecution) error {
	if substrate.IsReservedGuestObserveInvocation(execution.InvocationID) {
		return validateReservedGuestObserveFence(ctx, tx, execution)
	}
	return validateWorkerFence(ctx, tx, execution)
}

// validateReservedGuestObserveFence admits a claimless, node-local guest read
// of exactly one customer-substrate provision that failed before binding any
// guest, after its owner requested teardown. A failed head is terminal and its
// lease is cancelled, so the claimed fence can never admit it; without this
// read migration 190 has no absence observation to terminalize it on
// (kombify-StackKits-4ab3). The read is the Guard's provision "guest" observe
// of the exact admitted desired spec: it creates, changes and deletes nothing.
// The invocation id names the operation, substrate server and reserved guest;
// the command names the lease, its revision and the resource generation. Each
// must match the provision's own guest custody on its enabled substrate binding
// at the revision it was admitted on, served by that binding's Guard for the
// lease owner. The read is admitted only once the failure has settled
// (substrate.ReservedGuestSettle), so a create Proxmox may still be finishing
// cannot read as absent, and never after the head was terminalized.
func validateReservedGuestObserveFence(ctx context.Context, tx *sql.Tx, execution providerexecutor.WorkerExecution) error {
	command := execution.Command
	observe, err := substrate.ParseReservedGuestObserve(execution.InvocationID)
	if err != nil || observe.OperationID != command.OperationID ||
		execution.Phase != providerexecutor.WorkerPhaseObserve || execution.Stage != "guest" ||
		execution.TaskRef != "" || execution.Prerequisite != nil ||
		command.Operation != providerexecutor.OperationProvision ||
		command.ProviderID != "proxmox" || command.AdapterID != substrate.AdapterID {
		return errReservedGuestObserveNotAdmitted
	}
	// WorkerExecution.Validate already bound the payload to the command's
	// desired spec hash; the spec must reserve exactly the named guest.
	var spec substrate.ExecutionSpec
	if json.Unmarshal(execution.Payload, &spec) != nil || spec.Action != "provision" || spec.Guest.ID != observe.GuestID {
		return errReservedGuestObserveNotAdmitted
	}
	var valid bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS (
SELECT 1 FROM provider_operations op
JOIN techstack_vm_leases lease ON lease.tenant_id=op.tenant_id AND lease.id=op.lease_id
JOIN substrate_guest_leases guest ON guest.tenant_id=op.tenant_id AND guest.lease_id=op.lease_id AND guest.operation_id=op.operation_id
 AND guest.server_id=lease.server_id AND guest.resource_generation_id=lease.resource_generation_id
JOIN substrate_bindings binding ON binding.tenant_id=guest.tenant_id AND binding.server_id=guest.substrate_server_id
JOIN servers substrate_server ON substrate_server.tenant_id=binding.tenant_id AND substrate_server.id=binding.server_id
JOIN provider_operation_receipts failed_receipt ON failed_receipt.tenant_id=op.tenant_id AND failed_receipt.operation_id=op.operation_id
 AND failed_receipt.sequence=op.head_sequence AND failed_receipt.receipt_digest=op.head_receipt_digest
WHERE op.tenant_id=$1 AND op.operation_id=$2 AND op.command_digest=$3
AND op.head_sequence=$4 AND op.head_receipt_digest=$5
AND op.operation='provision' AND op.status='failed' AND op.phase='failed'
AND op.command_json #>> '{command,provider_id}'='proxmox'
AND failed_receipt.status='failed' AND failed_receipt.issued_at<clock_timestamp()-make_interval(secs => $13)
AND NOT EXISTS (SELECT 1 FROM provider_operation_resources resource WHERE resource.tenant_id=op.tenant_id AND resource.operation_id=op.operation_id)
AND NOT EXISTS (SELECT 1 FROM provider_operation_resource_free_terminalizations terminalization WHERE terminalization.tenant_id=op.tenant_id AND terminalization.operation_id=op.operation_id)
AND lease.lease_revision=$6 AND lease.server_id=$7 AND lease.resource_generation_id=$8::uuid
AND lease.cancelled_at IS NOT NULL
AND lease.lease_json->>'custody_class'='customer_substrate'
AND guest.substrate_server_id=$9 AND guest.guest_id=$10
AND lease.lease_json #>> '{resource,engine_vm_id}'=guest.substrate_server_id || '/' || guest.guest_id::text
AND lease.lease_json #>> '{metadata,substrate_binding_revision}'=binding.revision::text
AND binding.enabled AND binding.worker_id=$11
AND substrate_server.worker_id=binding.worker_id AND substrate_server.owner_subject_id=lease.owner_subject_id
AND substrate_server.metadata_json->>'server_node_role'='substrate'
AND op.command_json #>> '{execution_profile,credential_mode}'='worker_held'
AND op.command_json #>> '{command,connection_ref}'=$12
)`, command.TenantID, command.OperationID, command.CommandDigest, execution.HeadSequence, execution.HeadDigest,
		command.LeaseRevision, command.RuntimeServerID, command.ResourceGenerationID,
		observe.SubstrateServerID, observe.GuestID, execution.WorkerID,
		"provider-connection://substrate/"+execution.WorkerID+"/"+command.LeaseID,
		substrate.ReservedGuestSettle.Seconds()).Scan(&valid)
	if err != nil {
		return fmt.Errorf("reserved guest observe fence lookup: %w", err)
	}
	if !valid {
		return errReservedGuestObserveNotAdmitted
	}
	return nil
}
