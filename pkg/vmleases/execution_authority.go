package vmleases

import (
	"context"
	"errors"
	"strings"

	"github.com/kombifyio/homelab/internal/runtimeproduct/vmlease"
)

// LeaseExecutionAuthority is the immutable execution lane selected before a
// managed lease may cause its first provider side effect. Historical Simulate
// custody and Homelab-native provider control are deliberately disjoint.
type LeaseExecutionAuthority string

const (
	// LeaseExecutionAuthorityLegacySimulate is a non-executable quarantine
	// marker for exact historical provider custody. It never authorizes a call.
	LeaseExecutionAuthorityLegacySimulate LeaseExecutionAuthority = "legacy_simulate"
	// LeaseExecutionAuthorityHomelabProviderControl is the only authority
	// eligible for newly admitted managed-provider operations.
	// Persisted legacy bindings retain their immutable authority spelling.
	LeaseExecutionAuthorityHomelabProviderControl       LeaseExecutionAuthority = "homelab_provider_control"
	LeaseExecutionAuthorityHomelabProviderControlLegacy LeaseExecutionAuthority = "techstack_provider_control"
)

var (
	ErrLeaseExecutionAuthorityUnbound  = errors.New("vmleases: lease execution authority is unbound")
	ErrLeaseExecutionAuthorityConflict = errors.New("vmleases: lease execution authority conflicts with requested lane")
)

// LeaseExecutionAuthorityReader exposes only an already-persisted authority.
// Selecting or changing a lane is intentionally absent from this interface.
type LeaseExecutionAuthorityReader interface {
	ExecutionAuthority(ctx context.Context, tenantID string, leaseID vmlease.LeaseID) (LeaseExecutionAuthority, error)
}

func normalizeLeaseExecutionAuthority(authority LeaseExecutionAuthority) LeaseExecutionAuthority {
	return LeaseExecutionAuthority(strings.ToLower(strings.TrimSpace(string(authority))))
}

// IsHomelabProviderControlExecutionAuthority reports whether authority names
// Homelab's native provider-control lane (current or legacy spelling).
func IsHomelabProviderControlExecutionAuthority(authority LeaseExecutionAuthority) bool {
	switch normalizeLeaseExecutionAuthority(authority) {
	case LeaseExecutionAuthorityHomelabProviderControl, LeaseExecutionAuthorityHomelabProviderControlLegacy:
		return true
	default:
		return false
	}
}

func validateLeaseExecutionAuthority(authority LeaseExecutionAuthority) error {
	if normalizeLeaseExecutionAuthority(authority) == LeaseExecutionAuthorityLegacySimulate ||
		IsHomelabProviderControlExecutionAuthority(authority) {
		return nil
	}
	return ErrLeaseExecutionAuthorityUnbound
}
