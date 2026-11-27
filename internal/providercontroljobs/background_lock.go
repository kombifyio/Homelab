package providercontroljobs

import (
	"context"
	"errors"

	"github.com/kombifyio/homelab/pkg/db/backgroundlock"
)

// Cluster-wide lock names of the provider-control background loops. Every
// replica of every revision derives the same advisory key from a name.
const (
	staleCapacityRecoveryLock = "providercontroljobs/stale-capacity-recovery"
	neverEnrolledRecoveryLock = "providercontroljobs/never-enrolled-recovery"
	managedLeaseRenewalLock   = "providercontroljobs/managed-lease-renewal"
)

// runLockedPass runs one bounded pass under its cluster-wide lock, so two
// replicas overlapping in a rolling deploy never sweep the same candidates at
// once. The decommission and renewal paths keep their own row locks and
// generation fences; the lock only removes the concurrent duplicate.
//
// A pass skipped because another replica holds the lock resets the keyset
// cursor: that replica advanced past this one's position, and restarting from
// the beginning is always safe because every candidate action is idempotent.
// A nil lock runs the pass directly (single-process deployments and tests).
func runLockedPass(
	ctx context.Context,
	lock *backgroundlock.Locker,
	name string,
	pass func(context.Context) error,
	resetCursor func(),
	onError func(error),
) {
	var passErr error
	ran, lockErr := lock.Run(ctx, name, func(passCtx context.Context) { passErr = pass(passCtx) })
	if lockErr != nil {
		passErr = lockErr
	} else if !ran && resetCursor != nil {
		resetCursor()
	}
	if passErr != nil && !errors.Is(passErr, context.Canceled) && onError != nil {
		onError(passErr)
	}
}
