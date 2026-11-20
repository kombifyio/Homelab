// Package releasechannel decides which deployment runs a homelab's work.
//
// Homelab has one channel (D-2026-11-11-07), so every admitted deployment is
// production and owns every tenant. The predicates remain because the queue and
// provider-control paths still call them; they are constant true.
package releasechannel

import (
	"errors"

	"github.com/kombifyio/homelab/pkg/config"
)

var ErrTenantNotOwned = errors.New("deployment does not own tenant execution")

// Running is the channel this deployment serves.
func Running() string { return config.ReleaseChannel() }

// OwnsSubject reports whether this deployment runs subject's homelab work.
func OwnsSubject(string) bool { return !config.SecondaryChannel() }

// OwnsTenant reports whether this deployment runs tenant's work.
func OwnsTenant(string) bool { return !config.SecondaryChannel() }
