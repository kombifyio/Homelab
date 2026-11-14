package substrate

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// reservedGuestObservePrefix marks the one Guard invocation that reads the
// reserved guest of a customer-substrate provision that failed before binding
// any guest. The control plane admits it only through its own claimless,
// read-only fence (pkg/agentcontrol), never through the execution claim of a
// live operation, and no other invocation reaches that fence.
const reservedGuestObservePrefix = "reserved-guest-observe/"

// ReservedGuestSettle is how long after the failed receipt the reserved guest
// may first be read. A create whose response the Guard lost at its dispatch
// deadline can still be finishing in Proxmox, so an earlier read could miss a
// guest that is about to appear.
const ReservedGuestSettle = 10 * time.Minute

var errReservedGuestObserve = errors.New("substrate: invalid reserved guest observe invocation")

// ReservedGuestObserve is the auditable identity such an invocation carries in
// its id: the failed provision, the substrate server that reserved the guest,
// the reserved guest id and a nonce, so every read is a fresh one.
type ReservedGuestObserve struct {
	OperationID       string
	SubstrateServerID string
	GuestID           int
	Nonce             string
}

func (o ReservedGuestObserve) InvocationID() string {
	return reservedGuestObservePrefix + o.OperationID + "/" + o.SubstrateServerID + "/" + strconv.Itoa(o.GuestID) + "/" + o.Nonce
}

func IsReservedGuestObserveInvocation(id string) bool {
	return strings.HasPrefix(id, reservedGuestObservePrefix)
}

// ParseReservedGuestObserve accepts only the exact canonical form.
func ParseReservedGuestObserve(id string) (ReservedGuestObserve, error) {
	if !IsReservedGuestObserveInvocation(id) {
		return ReservedGuestObserve{}, errReservedGuestObserve
	}
	parts := strings.Split(strings.TrimPrefix(id, reservedGuestObservePrefix), "/")
	if len(parts) != 4 || parts[0] == "" || parts[1] == "" {
		return ReservedGuestObserve{}, errReservedGuestObserve
	}
	guestID, err := strconv.Atoi(parts[2])
	if err != nil || guestID < 100 || guestID > 999999999 || strconv.Itoa(guestID) != parts[2] {
		return ReservedGuestObserve{}, errReservedGuestObserve
	}
	nonce, err := uuid.Parse(parts[3])
	if err != nil || nonce.String() != parts[3] {
		return ReservedGuestObserve{}, errReservedGuestObserve
	}
	return ReservedGuestObserve{OperationID: parts[0], SubstrateServerID: parts[1], GuestID: guestID, Nonce: parts[3]}, nil
}
