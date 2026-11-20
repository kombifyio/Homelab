package substrateruntime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/kombifyio/homelab/internal/providercontrol"
	"github.com/kombifyio/homelab/internal/providerevidence"
	"github.com/kombifyio/homelab/internal/substrate"
	providerexecutor "github.com/kombifyio/stackkits/contracts/providerexecutor/v1beta1"
)

// reservedGuestObserveWait bounds one Guard read. The read is sent only while
// the substrate Guard reports in, so a decommission cycle never waits on an
// offline Guard.
const reservedGuestObserveWait = 15 * time.Second

var (
	// ErrReservedGuestPresent reports that the reserved guest of a failed
	// provision still exists on its Proxmox node. This path never deletes it:
	// a present guest needs its own cleanup decision.
	ErrReservedGuestPresent = errors.New("reserved substrate guest is present")
	// ErrReservedGuestUnknown reports a read that established neither presence
	// nor absence. No absence is recorded and the head keeps waiting.
	ErrReservedGuestUnknown = errors.New("reserved substrate guest absence is not established")
)

// ObserveReservedGuestAbsence produces the observation migration 190
// terminalizes a failed customer-substrate provision on (kombify-StackKits-4ab3).
// A provision that failed before binding any guest may still have started to
// create its reserved guest id, so only a node-local read of the complete
// Proxmox VM list that misses that exact id, taken once the failure settled,
// proves there is nothing to delete. The Guard reads it through the claimless
// reserved-guest observe fence of pkg/agentcontrol, and the absence is
// recorded under the failed provision itself (same lease and resource
// generation, after the failed receipt) with binding guest-<generation> and
// native ref <substrate server>/<guest id>.
func (a *Adapter) ObserveReservedGuestAbsence(ctx context.Context, record providercontrol.OperationRecord) error {
	command := record.Command
	if command.Operation != providerexecutor.OperationProvision || command.ProviderID != "proxmox" ||
		command.AdapterID != substrate.AdapterID || record.Head.Status != providerexecutor.StatusFailed ||
		record.Head.Phase != providerexecutor.PhaseFailed || len(record.Head.Resources) != 0 {
		return fmt.Errorf("%w: %s is not a failed resource-free substrate provision", ErrReservedGuestUnknown, command.OperationID)
	}
	reserved, err := a.reservedGuest(ctx, command, record.Head)
	if err != nil {
		return fmt.Errorf("%w: reserved guest of %s: %v", ErrReservedGuestUnknown, command.OperationID, err)
	}
	nativeRef := reserved.substrateServerID + "/" + strconv.Itoa(reserved.guestID)
	switch {
	case reserved.recorded:
		return nil
	case reserved.stale:
		// The observation is immutable and one per binding, so a row that does
		// not postdate the failure can never prove it.
		return fmt.Errorf("%w: guest %s has an absence observation that does not postdate the failure", ErrReservedGuestUnknown, nativeRef)
	case !reserved.settled:
		return fmt.Errorf("%w: guest %s is read once the failure has settled for %s", ErrReservedGuestUnknown, nativeRef, substrate.ReservedGuestSettle)
	case !reserved.guardOnline:
		return fmt.Errorf("%w: guest %s: the substrate Guard is not connected", ErrReservedGuestUnknown, nativeRef)
	}
	spec, payload, leaseRef, err := a.spec(ctx, command)
	if err != nil || spec.Action != "provision" || spec.Guest.ID != reserved.guestID || leaseRef != nativeRef {
		return fmt.Errorf("%w: desired spec of %s does not reserve guest %s", ErrReservedGuestUnknown, command.OperationID, nativeRef)
	}
	deadline := time.Now().UTC().Add(reservedGuestObserveWait)
	observeCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	result, err := a.transport.SendWorkerExecution(observeCtx, providerexecutor.WorkerExecution{
		Command: command,
		InvocationID: substrate.ReservedGuestObserve{
			OperationID: command.OperationID, SubstrateServerID: reserved.substrateServerID, GuestID: reserved.guestID, Nonce: uuid.NewString(),
		}.InvocationID(),
		WorkerID: substrateWorker(command), HeadSequence: record.Head.Sequence, HeadDigest: record.Head.ReceiptDigest,
		Phase: providerexecutor.WorkerPhaseObserve, Stage: "guest", Payload: payload, ExpiresAt: deadline,
	})
	if err != nil {
		return fmt.Errorf("%w: guest %s: %v", ErrReservedGuestUnknown, nativeRef, err)
	}
	if result.ErrorCode != "" {
		return fmt.Errorf("%w: guest %s: Guard answered %s", ErrReservedGuestUnknown, nativeRef, result.ErrorCode)
	}
	var observed substrate.GuestObservation
	if !result.Complete || !result.Succeeded || result.TaskRef != "" ||
		json.Unmarshal(result.Observation, &observed) != nil || observed.ID != reserved.guestID {
		return fmt.Errorf("%w: guest %s: incomplete or unreadable observation", ErrReservedGuestUnknown, nativeRef)
	}
	if observed.Present {
		return fmt.Errorf("%w: guest %s on its Proxmox node", ErrReservedGuestPresent, nativeRef)
	}
	// The collection time comes from the database clock that also orders the
	// failed receipt, not from this host.
	var collectedAt time.Time
	if err := a.db.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&collectedAt); err != nil {
		return fmt.Errorf("%w: %v", ErrReservedGuestUnknown, err)
	}
	recorder, err := providerevidence.NewRecorder(a.db)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrReservedGuestUnknown, err)
	}
	guest := binding(command, spec.Guest.GuestIdentity, nativeRef)
	target := providerexecutor.ResourceTarget{
		BindingID: guest.BindingID, Kind: guest.Kind, NativeRef: guest.NativeRef,
		OwnershipHash: guest.OwnershipHash, Disposition: guest.Disposition,
	}
	if _, err := recorder.Record(ctx, command, target, providerevidence.Observation{
		Endpoint: "proxmox-node-local:/qemu", StatusCode: 200, CollectedAt: collectedAt.UTC(), AuthoritativeListAbsent: nativeRef,
	}); err != nil {
		return fmt.Errorf("%w: record guest %s absence: %v", ErrReservedGuestUnknown, nativeRef, err)
	}
	return nil
}

type reservedGuestState struct {
	substrateServerID string
	guestID           int
	// recorded is an absence observation of this guest that postdates the
	// failed receipt; stale is one that does not.
	recorded, stale bool
	settled         bool
	guardOnline     bool
}

// reservedGuest reads the provision's own guest custody, whether its
// absence is already recorded (so a recorded head sends no further read),
// whether the failure has settled and whether the binding's Guard reports in.
func (a *Adapter) reservedGuest(ctx context.Context, command providerexecutor.Command, head providerexecutor.Receipt) (reservedGuestState, error) {
	var state reservedGuestState
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return state, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id',$1,true)`, command.TenantID); err != nil {
		return state, err
	}
	var observedAfter sql.NullBool
	err = tx.QueryRowContext(ctx, `SELECT guest.substrate_server_id,guest.guest_id,
 (SELECT observation.collected_at>failed_receipt.issued_at FROM provider_absence_observations observation
  WHERE observation.tenant_id=guest.tenant_id AND observation.operation_id=guest.operation_id AND observation.binding_id='guest-' || guest.resource_generation_id::text),
 failed_receipt.issued_at<clock_timestamp()-make_interval(secs => $8),
 EXISTS(SELECT 1 FROM substrate_bindings binding JOIN servers substrate_server ON substrate_server.tenant_id=binding.tenant_id AND substrate_server.id=binding.server_id
  WHERE binding.tenant_id=guest.tenant_id AND binding.server_id=guest.substrate_server_id AND binding.enabled
  AND substrate_server.connection_state='connected' AND substrate_server.last_heartbeat_at>clock_timestamp()-interval '2 minutes')
 FROM substrate_guest_leases guest
 JOIN provider_operation_receipts failed_receipt ON failed_receipt.tenant_id=guest.tenant_id AND failed_receipt.operation_id=guest.operation_id
  AND failed_receipt.sequence=$6 AND failed_receipt.receipt_digest=$7
 WHERE guest.tenant_id=$1 AND guest.lease_id=$2 AND guest.operation_id=$3 AND guest.resource_generation_id=$4::uuid AND guest.server_id=$5`,
		command.TenantID, command.LeaseID, command.OperationID, command.ResourceGenerationID, command.RuntimeServerID,
		head.Sequence, head.ReceiptDigest, substrate.ReservedGuestSettle.Seconds(),
	).Scan(&state.substrateServerID, &state.guestID, &observedAfter, &state.settled, &state.guardOnline)
	state.recorded = observedAfter.Valid && observedAfter.Bool
	state.stale = observedAfter.Valid && !observedAfter.Bool
	return state, err
}
