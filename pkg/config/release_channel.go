package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
)

// EnvReleaseChannel is the retired selector of a serving channel beside
// production. Techstack has one channel (D-2026-11-11-07): a process that still
// names another one refuses to start (RejectRetiredReleaseChannel) instead of
// running as production.
const EnvReleaseChannel = "TECHSTACK_RELEASE_CHANNEL"

// StableReleaseChannel is the one channel this product serves, and the constant
// value /api/v1/health and the client bootstrap keep reporting.
const StableReleaseChannel = "stable"

// RejectRetiredReleaseChannel refuses a deployment that still names a channel
// other than stable. Early Access and its retired predecessor "preview" are
// deleted rather than carried over, and a service left holding either value
// would otherwise start production work on a deployment that must not exist.
func RejectRetiredReleaseChannel() error {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(EnvReleaseChannel)))
	if value == "" || value == StableReleaseChannel {
		return nil
	}
	return fmt.Errorf("%s=%q is retired: this product serves one channel and refuses to start with another", EnvReleaseChannel, value)
}

// ReleaseChannel reports the one channel this deployment serves. The health and
// bootstrap answers keep their release_channel field at this constant value.
func ReleaseChannel() string { return StableReleaseChannel }

// SecondaryChannel reports a deployment beside the one channel. There is none:
// RejectRetiredReleaseChannel refuses one at boot, so this is false on every
// admitted process. It remains for the boot paths that still branch on it
// (cmd/techstack/startup.go and its admission wiring), and goes with them.
func SecondaryChannel() bool { return false }

// RunsSharedWork reports whether this deployment runs the work that is
// nobody's homelab: agent connections, monitoring, posture, outboxes, global
// boot writes. Every admitted deployment runs it.
func RunsSharedWork() bool { return true }

// SecondaryExecution records installed boundaries on a boot beside the one
// channel. Environment configuration alone never supplies a qualified health
// receipt. No admitted process has one, so NewSecondaryExecution returns nil
// and every method ignores a nil receiver.
type SecondaryExecution struct {
	mu                                                      sync.RWMutex
	httpInstalled, startupSuppressed, backgroundPartitioned bool
	fixture                                                 string
}

func NewSecondaryExecution() *SecondaryExecution {
	if !SecondaryChannel() {
		return nil
	}
	return &SecondaryExecution{fixture: strings.TrimSpace(os.Getenv("TEST_RIL_PRO_USER_ID"))}
}
func (s *SecondaryExecution) HTTPInstalled() {
	if s != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.httpInstalled = true
	}
}
func (s *SecondaryExecution) StartupSuppressed() {
	if s != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.startupSuppressed = true
	}
}

// BackgroundPartitioned records that homelab work runs only for tenants
// that this deployment owns.
func (s *SecondaryExecution) BackgroundPartitioned() {
	if s != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.backgroundPartitioned = true
	}
}
func (s *SecondaryExecution) Snapshot() map[string]string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	scope := map[string]string{"http": "unqualified", "scheduled": "unqualified", "queues": "unqualified", "startup": "unqualified"}
	if s.httpInstalled {
		scope["http"] = "opted-in-users"
	}
	if s.startupSuppressed {
		scope["startup"] = "verify-only"
	}
	if s.startupSuppressed && s.backgroundPartitioned {
		scope["scheduled"] = "owned-tenants"
		scope["queues"] = "owned-tenants"
	}
	if s.fixture != "" {
		digest := sha256.Sum256([]byte(s.fixture))
		scope["fixture_subject_sha256"] = hex.EncodeToString(digest[:])
	}
	return scope
}
