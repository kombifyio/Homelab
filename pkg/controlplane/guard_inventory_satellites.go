package controlplane

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// GuardInventorySatelliteProjection carries the non-authoritative worker and
// RIL read models derived from one already accepted canonical Guard snapshot.
// Stores must compare the complete source position and write both satellites
// atomically; a superseded position is a successful no-op.
type GuardInventorySatelliteProjection struct {
	TenantID          string
	ServerID          string
	Generation        int64
	SourceID          string
	SourceEpoch       string
	SourceSequence    int64
	SourceObservedAt  time.Time
	InventoryRevision int64
	Worker            Worker
	RILServer         RILServer
	// Identity is the reporting server's own identity fact: its applied
	// StackKit runs the managed Pocket ID for this origin. The caller sets it
	// only for a host-key-bound Guard; the store records the server as its
	// homelab's identity Authority (ADR-036) in the same fenced transaction.
	Identity *HomelabIdentityObservation
}

type GuardInventorySatelliteResult struct {
	Worker  *Worker
	Applied bool
}

type GuardInventorySatelliteStore interface {
	ApplyGuardInventorySatellites(context.Context, GuardInventorySatelliteProjection) (*GuardInventorySatelliteResult, error)
}

func normalizeGuardInventorySatelliteProjection(command GuardInventorySatelliteProjection) (GuardInventorySatelliteProjection, error) {
	command.TenantID = strings.TrimSpace(command.TenantID)
	command.ServerID = strings.TrimSpace(command.ServerID)
	command.SourceID = strings.TrimSpace(command.SourceID)
	command.SourceEpoch = strings.TrimSpace(command.SourceEpoch)
	command.SourceObservedAt = command.SourceObservedAt.UTC()
	if command.TenantID == "" || command.ServerID == "" || command.SourceID == "" || command.SourceEpoch == "" ||
		command.Generation <= 0 || command.SourceSequence <= 0 || command.InventoryRevision <= 0 || command.SourceObservedAt.IsZero() {
		return GuardInventorySatelliteProjection{}, fmt.Errorf("%w: Guard satellite source position is incomplete", ErrConflict)
	}
	if command.Worker.TenantID != command.TenantID || command.Worker.ID != command.SourceID ||
		command.RILServer.TenantID != command.TenantID || command.RILServer.ID != command.ServerID {
		return GuardInventorySatelliteProjection{}, fmt.Errorf("%w: Guard satellite bindings do not match the accepted source", ErrConflict)
	}
	return command, nil
}

func guardInventorySatelliteHeadMatches(server ServerRuntime, command GuardInventorySatelliteProjection) bool {
	return server.TenantID == command.TenantID && server.ID == command.ServerID &&
		server.Generation == command.Generation && server.SourceAuthority == ServerEventAuthorityGuard &&
		server.SourceID == command.SourceID && server.SourceEpoch == command.SourceEpoch &&
		server.SourceSequence == command.SourceSequence && server.InventoryRevision == command.InventoryRevision &&
		server.SourceObservedAt != nil && server.SourceObservedAt.UTC().Equal(command.SourceObservedAt)
}
