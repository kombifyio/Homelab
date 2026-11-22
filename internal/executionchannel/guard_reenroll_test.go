package executionchannel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/jobs"
	"github.com/kombifyio/homelab/pkg/monthlyruntime"
	"github.com/kombifyio/homelab/pkg/runtimeidentity"
	"github.com/kombifyio/homelab/pkg/serverregistry"
)

// Behavior of the managed Guard re-enrollment (platform-2jlnx7.14): one run
// per node, already_current from the node itself, and enrolled only on the
// control plane's own proof of the new identity's heartbeat.

const (
	reenrollTenant  = "tenant-reenroll"
	reenrollLease   = "lease-reenroll"
	reenrollWorker  = "worker-reenroll"
	reenrollTarget  = "0.50.1"
	reenrollArchive = guardReenrollArchiveRoot + "/20261122T090000Z"

	probeOldGuard     = "guard_binary=none guard_version=none enrollment=absent service=inactive"
	probeCurrentGuard = "guard_binary=/usr/local/bin/homelab guard_version=0.50.1 enrollment=present service=active"
	probeStoppedGuard = "guard_binary=/usr/local/bin/homelab guard_version=0.50.1 enrollment=present service=inactive"
	installedCurrent  = "state=installed guard_binary=/usr/local/bin/homelab guard_version=0.50.1 enrollment=present service=active archive=" + reenrollArchive
)

// identityOverlayStore is the canonical memory store with a worker identity
// the test controls, standing in for the redemption and token exchange.
type identityOverlayStore struct {
	*controlplane.MemoryStore
	mu       sync.Mutex
	identity *controlplane.WorkerIdentity
}

func (s *identityOverlayStore) GetWorkerIdentity(_ context.Context, tenantID, workerID string) (*controlplane.WorkerIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.identity == nil || s.identity.TenantID != tenantID || s.identity.WorkerID != workerID {
		return nil, controlplane.ErrNotFound
	}
	identity := *s.identity
	return &identity, nil
}

type pinnedTarget struct{ hostKey string }

func (p pinnedTarget) ResolveManagedRuntimeTarget(context.Context, jobs.ManagedRuntimeTargetRequest) (*jobs.ManagedRuntimeTarget, error) {
	return &jobs.ManagedRuntimeTarget{Host: "203.0.113.10", SSHUser: "kombify", SSHPrivateKey: "test-channel-credential", SSHHostKey: p.hostKey}, nil
}

type reenrollFixture struct {
	t       *testing.T
	store   *identityOverlayStore
	channel *Channel

	clockMu sync.Mutex
	clock   time.Time

	probe    atomic.Value // string
	install  func(stdin string) (string, error)
	probes   atomic.Int32
	installs atomic.Int32
	issued   atomic.Int32
}

func newReenrollFixture(t *testing.T) *reenrollFixture {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPublic, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	f := &reenrollFixture{
		t:     t,
		store: &identityOverlayStore{MemoryStore: controlplane.NewMemoryStore()},
		clock: time.Now().UTC().Truncate(time.Second),
	}
	f.probe.Store(probeOldGuard)
	f.channel = &Channel{
		Servers: f.store,
		Targets: pinnedTarget{hostKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPublic)))},
		Now:     f.now,
		run: func(_ context.Context, _ *jobs.ManagedRuntimeTarget, _ string, command, stdin string) (string, error) {
			if strings.Contains(command, "KOMBI_TOKEN") {
				f.installs.Add(1)
				if f.install == nil {
					return "state=failed reason=installer_failed", errors.New("exit 66")
				}
				return f.install(stdin)
			}
			f.probes.Add(1)
			return f.probe.Load().(string), nil
		},
		wait: func(ctx context.Context, d time.Duration) error {
			f.advance(d)
			return ctx.Err()
		},
	}
	if _, err := f.store.UpsertServerRuntime(context.Background(), controlplane.ServerRuntime{
		ID: runtimeidentity.LeaseServerID(reenrollLease), TenantID: reenrollTenant, LeaseID: reenrollLease,
		StackID: "stack-reenroll", OwnerSubjectID: "owner-reenroll", WorkerID: reenrollWorker, Name: "node",
		LifecycleState: string(serverregistry.LifecycleActive), ConnectionState: string(serverregistry.ConnectionOffline),
	}); err != nil {
		t.Fatal(err)
	}
	// The 0.49.19 pairing: bound identity at epoch 1.
	confirmed := f.now().Add(-24 * time.Hour)
	f.setIdentity(1, confirmed)
	return f
}

func (f *reenrollFixture) now() time.Time {
	f.clockMu.Lock()
	defer f.clockMu.Unlock()
	return f.clock
}

func (f *reenrollFixture) advance(d time.Duration) {
	f.clockMu.Lock()
	defer f.clockMu.Unlock()
	f.clock = f.clock.Add(d)
}

func (f *reenrollFixture) setIdentity(epoch int64, confirmedAt time.Time) {
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	f.store.identity = &controlplane.WorkerIdentity{
		TenantID: reenrollTenant, WorkerID: reenrollWorker, State: controlplane.WorkerIdentityBound,
		CredentialEpoch: epoch, ConfirmedAt: &confirmedAt,
	}
}

// heartbeat records what an accepted Guard heartbeat and inventory leave on
// the server: connected, the receipt time and the reported Guard version.
func (f *reenrollFixture) heartbeat(at time.Time, version string) {
	f.t.Helper()
	ctx := context.Background()
	server, err := f.store.GetServerRuntime(ctx, reenrollTenant, runtimeidentity.LeaseServerID(reenrollLease))
	if err != nil {
		f.t.Fatal(err)
	}
	server.ConnectionState = string(serverregistry.ConnectionConnected)
	server.LastHeartbeatAt = &at
	if server.Metadata == nil {
		server.Metadata = map[string]any{}
	}
	server.Metadata["agent_version"] = version
	if _, err := f.store.UpsertServerRuntime(ctx, *server); err != nil {
		f.t.Fatal(err)
	}
}

func (f *reenrollFixture) request() GuardReenrollRequest {
	return GuardReenrollRequest{
		TenantID: reenrollTenant, LeaseID: reenrollLease, Actor: "service:homelab-operations",
		TargetVersion: reenrollTarget, ControlPlaneURL: "https://homelab.example.test",
		Capability: func(_ context.Context, _ controlplane.ServerRuntime, attemptID string) (string, error) {
			f.issued.Add(1)
			return "capability-" + attemptID, nil
		},
	}
}

func (f *reenrollFixture) stored() GuardReenrollment {
	f.t.Helper()
	server, err := f.store.GetServerRuntime(context.Background(), reenrollTenant, runtimeidentity.LeaseServerID(reenrollLease))
	if err != nil {
		f.t.Fatal(err)
	}
	return GuardReenrollmentFromMetadata(server.Metadata)
}

// installWithRedemption stands in for install.sh: the redemption issues the
// new identity at the next epoch, its first token exchange confirms it, and
// its first heartbeat and inventory report version.
func (f *reenrollFixture) installWithRedemption(version string) func(string) (string, error) {
	return func(string) (string, error) {
		at := f.now().Add(time.Second)
		f.setIdentity(2, at)
		f.heartbeat(at, version)
		return installedCurrent, nil
	}
}

func TestGuardReenrollOverlappingRunsClaimTheNodeOnce(t *testing.T) {
	f := newReenrollFixture(t)
	installing, release := make(chan struct{}), make(chan struct{})
	redeem := f.installWithRedemption(reenrollTarget)
	f.install = func(stdin string) (string, error) {
		close(installing)
		<-release
		return redeem(stdin)
	}
	type outcome struct {
		record GuardReenrollment
		err    error
	}
	const callers = 8
	results := make(chan outcome, callers)
	start := make(chan struct{})
	for range callers {
		go func() {
			<-start
			record, err := f.channel.ReenrollGuard(context.Background(), f.request())
			results <- outcome{record, err}
		}()
	}
	close(start)
	// Every caller but the one holding the node returns while it installs,
	// without running anything on the host.
	for range callers - 1 {
		got := <-results
		if !errors.Is(got.err, ErrGuardReenrollInProgress) || got.record.State != GuardReenrollStateRunning {
			t.Fatalf("overlapping call = %+v, %v; want running, ErrGuardReenrollInProgress", got.record, got.err)
		}
	}
	<-installing
	if f.probes.Load() != 1 || f.installs.Load() != 1 || f.issued.Load() != 1 {
		t.Fatalf("host work while claimed: probes=%d installs=%d capabilities=%d, want 1 each", f.probes.Load(), f.installs.Load(), f.issued.Load())
	}
	close(release)
	winner := <-results
	if winner.err != nil || winner.record.State != GuardReenrollStateEnrolled || winner.record.Attempt != 1 {
		t.Fatalf("holding run = %+v, %v; want enrolled attempt 1", winner.record, winner.err)
	}
	// The result released the claim: the next call runs (and finds the node current).
	f.probe.Store(probeCurrentGuard)
	next, err := f.channel.ReenrollGuard(context.Background(), f.request())
	if err != nil || next.State != GuardReenrollStateAlreadyCurrent || next.Attempt != 2 {
		t.Fatalf("call after the run = %+v, %v; want already_current attempt 2", next, err)
	}
}

func TestGuardReenrollReclaimsOnlyAClaimWhoseRunDied(t *testing.T) {
	f := newReenrollFixture(t)
	f.install = f.installWithRedemption(reenrollTarget)
	held := GuardReenrollment{
		State: GuardReenrollStateRunning, Attempt: 4, TargetVersion: reenrollTarget, Actor: "service:homelab-operations",
		StartedAt: f.now().Format(time.RFC3339), ClaimExpiresAt: f.now().Add(guardReenrollClaimTTL).Format(time.RFC3339),
	}
	server, _ := f.store.GetServerRuntime(context.Background(), reenrollTenant, runtimeidentity.LeaseServerID(reenrollLease))
	if err := f.channel.recordServerMetadata(context.Background(), *server, guardReenrollmentSource, GuardReenrollmentMetadataKey, held.metadataValue()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.channel.ReenrollGuard(context.Background(), f.request()); !errors.Is(err, ErrGuardReenrollInProgress) || f.probes.Load() != 0 {
		t.Fatalf("live claim: err=%v probes=%d; want ErrGuardReenrollInProgress and no host work", err, f.probes.Load())
	}
	f.advance(guardReenrollClaimTTL)
	record, err := f.channel.ReenrollGuard(context.Background(), f.request())
	if err != nil || record.State != GuardReenrollStateEnrolled || record.Attempt != 5 {
		t.Fatalf("expired claim = %+v, %v; want enrolled attempt 5", record, err)
	}
}

func TestGuardReenrollAlreadyCurrentComesFromTheNode(t *testing.T) {
	t.Run("current node with a fresh heartbeat of its bound identity", func(t *testing.T) {
		f := newReenrollFixture(t)
		f.probe.Store(probeCurrentGuard)
		f.heartbeat(f.now().Add(-30*time.Second), reenrollTarget)
		record, err := f.channel.ReenrollGuard(context.Background(), f.request())
		if err != nil || record.State != GuardReenrollStateAlreadyCurrent || record.AfterVersion != reenrollTarget {
			t.Fatalf("got %+v, %v; want already_current", record, err)
		}
		if f.installs.Load() != 0 || f.issued.Load() != 0 {
			t.Fatalf("already_current touched the node: installs=%d capabilities=%d", f.installs.Load(), f.issued.Load())
		}
		if stored := f.stored(); stored.State != GuardReenrollStateAlreadyCurrent || stored.claimHeld(f.now()) {
			t.Fatalf("stored record = %+v; want already_current, claim released", stored)
		}
	})
	t.Run("connected server row over a stopped Guard is not current", func(t *testing.T) {
		f := newReenrollFixture(t)
		f.probe.Store(probeStoppedGuard)
		f.heartbeat(f.now().Add(-30*time.Second), reenrollTarget)
		f.install = f.installWithRedemption(reenrollTarget)
		record, err := f.channel.ReenrollGuard(context.Background(), f.request())
		if err != nil || record.State != GuardReenrollStateEnrolled || f.installs.Load() != 1 {
			t.Fatalf("got %+v, %v, installs=%d; want a re-enrollment, enrolled", record, err, f.installs.Load())
		}
	})
	t.Run("current binary without a fresh heartbeat is not current", func(t *testing.T) {
		f := newReenrollFixture(t)
		f.probe.Store(probeCurrentGuard)
		f.heartbeat(f.now().Add(-guardReenrollFreshHeartbeat-time.Minute), reenrollTarget)
		f.install = f.installWithRedemption(reenrollTarget)
		record, err := f.channel.ReenrollGuard(context.Background(), f.request())
		if err != nil || record.State != GuardReenrollStateEnrolled || f.installs.Load() != 1 {
			t.Fatalf("got %+v, %v, installs=%d; want a re-enrollment, enrolled", record, err, f.installs.Load())
		}
	})
}

type inactiveLeaseTarget struct{}

func (inactiveLeaseTarget) ResolveManagedRuntimeTarget(context.Context, jobs.ManagedRuntimeTargetRequest) (*jobs.ManagedRuntimeTarget, error) {
	return nil, fmt.Errorf("resolve lease: %w", monthlyruntime.ErrExecutionAuthorityInactive)
}

// A lease past its provider-control validity is refused as lease_not_active,
// not reported as missing channel custody, and nothing reaches the node.
func TestGuardReenrollRefusesALeaseOutsideProviderControl(t *testing.T) {
	f := newReenrollFixture(t)
	f.channel.Targets = inactiveLeaseTarget{}
	record, err := f.channel.ReenrollGuard(context.Background(), f.request())
	if !errors.Is(err, ErrGuardReenrollRefused) || errors.Is(err, monthlyruntime.ErrNodeChannelUnavailable) {
		t.Fatalf("err = %v; want ErrGuardReenrollRefused only", err)
	}
	if record.State != GuardReenrollStateFailed || record.Reason != "lease_not_active" {
		t.Fatalf("record = %+v; want failed, lease_not_active", record)
	}
	if f.probes.Load() != 0 || f.installs.Load() != 0 || f.issued.Load() != 0 {
		t.Fatalf("refused run touched the node: probes=%d installs=%d capabilities=%d", f.probes.Load(), f.installs.Load(), f.issued.Load())
	}
	if stored := f.stored(); stored.State != GuardReenrollStateFailed || stored.claimHeld(f.now()) {
		t.Fatalf("stored record = %+v; want failed, claim released", stored)
	}
}

func TestGuardReenrollEnrolledNeedsTheNewIdentitysHeartbeat(t *testing.T) {
	t.Run("first key-bound heartbeat at the target version", func(t *testing.T) {
		f := newReenrollFixture(t)
		f.install = f.installWithRedemption(reenrollTarget)
		record, err := f.channel.ReenrollGuard(context.Background(), f.request())
		if err != nil || record.State != GuardReenrollStateEnrolled || record.IdentityEpoch != 2 || record.HeartbeatAt == "" {
			t.Fatalf("got %+v, %v; want enrolled at identity epoch 2 with the heartbeat time", record, err)
		}
		if stored := f.stored(); stored.State != GuardReenrollStateEnrolled || stored.AfterVersion != reenrollTarget {
			t.Fatalf("stored record = %+v; want enrolled at the target", stored)
		}
	})
	t.Run("installer succeeds but no heartbeat arrives", func(t *testing.T) {
		f := newReenrollFixture(t)
		// The old Guard's last heartbeat before the run proves nothing.
		f.heartbeat(f.now().Add(-10*time.Second), "0.49.19")
		f.install = func(string) (string, error) { return installedCurrent, nil }
		started := f.now()
		record, err := f.channel.ReenrollGuard(context.Background(), f.request())
		if !errors.Is(err, ErrGuardReenrollUnconfirmed) || record.State != GuardReenrollStateUnconfirmed || record.Reason != "heartbeat_not_observed" {
			t.Fatalf("got %+v, %v; want installed_unconfirmed heartbeat_not_observed", record, err)
		}
		waited := f.now().Sub(started)
		if waited < guardReenrollProofWindow || waited > guardReenrollProofWindow+guardReenrollProofPoll {
			t.Fatalf("waited %s; want the stated bound %s", waited, guardReenrollProofWindow)
		}
		if stored := f.stored(); stored.State != GuardReenrollStateUnconfirmed || stored.claimHeld(f.now()) {
			t.Fatalf("stored record = %+v; want installed_unconfirmed, claim released", stored)
		}
	})
	t.Run("heartbeat of the new identity at another version", func(t *testing.T) {
		f := newReenrollFixture(t)
		f.install = f.installWithRedemption("0.50.0")
		record, err := f.channel.ReenrollGuard(context.Background(), f.request())
		if !errors.Is(err, ErrGuardReenrollUnconfirmed) || record.State != GuardReenrollStateUnconfirmed {
			t.Fatalf("got %+v, %v; want installed_unconfirmed", record, err)
		}
	})
	t.Run("installed binary at another version", func(t *testing.T) {
		f := newReenrollFixture(t)
		f.install = func(string) (string, error) {
			return strings.Replace(installedCurrent, "guard_version=0.50.1", "guard_version=0.50.0", 1), nil
		}
		record, err := f.channel.ReenrollGuard(context.Background(), f.request())
		if !errors.Is(err, ErrGuardReenrollUnconfirmed) || record.Reason != "installed_version_differs" {
			t.Fatalf("got %+v, %v; want installed_unconfirmed installed_version_differs", record, err)
		}
	})
	t.Run("installer failure is failed, not enrolled", func(t *testing.T) {
		f := newReenrollFixture(t)
		record, err := f.channel.ReenrollGuard(context.Background(), f.request())
		if err == nil || record.State != GuardReenrollStateFailed || record.Reason != "installer_failed" {
			t.Fatalf("got %+v, %v; want failed installer_failed", record, err)
		}
	})
}
