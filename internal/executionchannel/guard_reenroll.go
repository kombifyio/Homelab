package executionchannel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/guardlayout"
	"github.com/kombifyio/homelab/pkg/jobs"
	"github.com/kombifyio/homelab/pkg/monthlyruntime"
	"github.com/kombifyio/homelab/pkg/serverregistry"
)

// The managed Guard re-enrollment (platform-2jlnx7.14). After the Techstack to
// Homelab rename a Guard enrolled under 0.49.x cannot be updated in place: the
// control plane now derives every worker's command-binding key under the
// Homelab label (pkg/guardbinding/signature.go DeriveWorkerSigner), the Guard
// keeps the key it pinned at pairing, and a pin rotation must itself be signed
// by a currently pinned key (pkg/guardbinding/pin_rotation.go). The only
// forward route is a fresh pairing, which this action runs over the execution
// channel the control plane already holds for the managed lease: it stops the
// old Guard, moves its enrollment, identity state and old binary into a
// root-only archive on the node, and runs the control plane's own install.sh
// with a lease-bound capability. The redemption binds the new host key to the
// lease's existing worker and server (new credential epoch).

const (
	// GuardReenrollmentMetadataKey holds the per-node result on the canonical
	// RuntimeServer aggregate. While a run holds the node it carries the
	// running claim, so it is also the per-node lock.
	GuardReenrollmentMetadataKey = "guard_reenrollment"
	guardReenrollmentSource      = "guard-reenrollment"

	GuardReenrollStateRunning        = "running"
	GuardReenrollStateEnrolled       = "enrolled"
	GuardReenrollStateAlreadyCurrent = "already_current"
	// GuardReenrollStateUnconfirmed is an installer run that finished without
	// the control plane accepting a key-bound heartbeat of the new identity at
	// the target version within guardReenrollProofWindow. It is a failure.
	GuardReenrollStateUnconfirmed = "installed_unconfirmed"
	GuardReenrollStateFailed      = "failed"

	guardReenrollArchiveRoot = "/var/lib/kombify-guard-reenroll"
	guardBinaryPath          = guardlayout.DefaultBinaryPath

	// guardReenrollTimeout bounds the installer run: download, package
	// preparation, layout and redemption on a small VM.
	guardReenrollTimeout = 15 * time.Minute
	// guardReenrollProofWindow bounds the wait for the new identity's first
	// accepted heartbeat and inventory after the installer finished. The Guard
	// publishes both on start and then every guardcompat.HeartbeatInterval
	// (30 s), so three minutes cover start-up plus several cycles.
	guardReenrollProofWindow = 3 * time.Minute
	guardReenrollProofPoll   = 5 * time.Second
	// guardReenrollRunBound is the whole run after the claim: probe, installer
	// and proof. guardReenrollClaimTTL outlives it, so only a claim whose run
	// died (process restart) ever expires and becomes reclaimable.
	guardReenrollRunBound = guardReenrollTimeout + guardReenrollProofWindow + time.Minute
	guardReenrollClaimTTL = guardReenrollRunBound + 5*time.Minute
	// guardReenrollFreshHeartbeat is how recent an accepted heartbeat must be
	// for an already current node.
	guardReenrollFreshHeartbeat = 2 * time.Minute
)

// GuardReenrollCapabilityIssuer mints the lease-bound pairing capability of one
// attempt (guardbootstrap.EnrollmentIssuer.IssueReenrollCapability).
type GuardReenrollCapabilityIssuer func(ctx context.Context, server controlplane.ServerRuntime, attemptID string) (string, error)

// GuardReenrollRequest selects one managed node. Actor is the authenticated
// service caller; the action never runs in an owner's session.
type GuardReenrollRequest struct {
	TenantID        string
	LeaseID         string
	Actor           string
	TargetVersion   string
	ControlPlaneURL string
	Capability      GuardReenrollCapabilityIssuer
}

// GuardReenrollment is the recorded result per node. It carries versions,
// states, epochs and timestamps only, never a capability or credential.
type GuardReenrollment struct {
	State          string `json:"state"`
	Attempt        int    `json:"attempt"`
	TargetVersion  string `json:"target_version"`
	BeforeVersion  string `json:"before_version"`
	AfterVersion   string `json:"after_version,omitempty"`
	BeforeBinary   string `json:"before_binary,omitempty"`
	Reason         string `json:"reason,omitempty"`
	Archive        string `json:"archive,omitempty"`
	IdentityEpoch  int64  `json:"identity_epoch,omitempty"`
	HeartbeatAt    string `json:"heartbeat_at,omitempty"`
	StartedAt      string `json:"started_at"`
	ClaimExpiresAt string `json:"claim_expires_at,omitempty"`
	FinishedAt     string `json:"finished_at,omitempty"`
	Actor          string `json:"actor"`
}

// GuardReenrollmentFromMetadata decodes the last recorded result.
func GuardReenrollmentFromMetadata(metadata map[string]any) GuardReenrollment {
	var record GuardReenrollment
	if raw, ok := jsonMap(metadata[GuardReenrollmentMetadataKey]); ok {
		record.State, _ = raw["state"].(string)
		record.TargetVersion, _ = raw["target_version"].(string)
		record.AfterVersion, _ = raw["after_version"].(string)
		record.StartedAt, _ = raw["started_at"].(string)
		record.ClaimExpiresAt, _ = raw["claim_expires_at"].(string)
		switch attempt := raw["attempt"].(type) {
		case float64:
			record.Attempt = int(attempt)
		case int:
			record.Attempt = attempt
		}
	}
	return record
}

// metadataValue is the record as the JSON object the aggregate stores, so
// every store reads it back in the same shape.
func (r GuardReenrollment) metadataValue() map[string]any {
	encoded, _ := json.Marshal(r)
	var out map[string]any
	_ = json.Unmarshal(encoded, &out)
	return out
}

// claimHeld reports whether a running claim still holds the node at now. A
// running record without a readable expiry falls back to its start plus the
// claim lifetime; one with neither cannot belong to a live run.
func (r GuardReenrollment) claimHeld(now time.Time) bool {
	if r.State != GuardReenrollStateRunning {
		return false
	}
	if expires, err := time.Parse(time.RFC3339, r.ClaimExpiresAt); err == nil {
		return now.Before(expires)
	}
	if started, err := time.Parse(time.RFC3339, r.StartedAt); err == nil {
		return now.Before(started.Add(guardReenrollClaimTTL))
	}
	return false
}

var guardVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.+-]*$`)

var (
	// ErrGuardReenrollRefused is a request the action cannot run for this node.
	ErrGuardReenrollRefused = errors.New("executionchannel: guard re-enrollment refused")
	// ErrGuardReenrollInProgress is a node another run holds; nothing ran.
	ErrGuardReenrollInProgress = errors.New("executionchannel: a guard re-enrollment of this node is running")
	// ErrGuardReenrollUnconfirmed is an installer run the control plane could
	// not prove: no accepted key-bound heartbeat of the new identity at the
	// target version within the proof window.
	ErrGuardReenrollUnconfirmed = errors.New("executionchannel: guard re-enrollment unconfirmed")
)

// guardIdentityReader reads the host-key identity of a worker; the canonical
// control-plane store provides it beside the server aggregate.
type guardIdentityReader interface {
	GetWorkerIdentity(ctx context.Context, tenantID, workerID string) (*controlplane.WorkerIdentity, error)
}

// ReenrollGuard brings one managed node's Guard to the control plane's own
// release by a fresh, lease-bound enrollment.
//
//   - One run per node: the attempt is claimed on the server aggregate by
//     compare-and-set before any host work, and a call while a claim holds
//     answers ErrGuardReenrollInProgress.
//   - already_current is decided from the node itself (Homelab binary at the
//     target version, service active, enrollment present) together with a
//     fresh accepted heartbeat of its bound identity; the host is not touched
//     and no capability is minted.
//   - enrolled is reported only once the control plane accepted the first
//     key-bound heartbeat of the new identity (credential epoch advanced,
//     identity bound) and the Guard reported the target version, within
//     guardReenrollProofWindow; otherwise the run ends installed_unconfirmed.
//
// Every run records its result on the server.
func (c *Channel) ReenrollGuard(ctx context.Context, req GuardReenrollRequest) (GuardReenrollment, error) {
	target := strings.TrimSpace(req.TargetVersion)
	origin, originErr := reenrollOrigin(req.ControlPlaneURL)
	if !guardVersionPattern.MatchString(target) || originErr != nil || req.Capability == nil || strings.TrimSpace(req.Actor) == "" {
		return GuardReenrollment{}, fmt.Errorf("%w: target version, control-plane origin, capability issuer and actor are required", ErrGuardReenrollRefused)
	}
	identities, ok := c.serversAs()
	if !ok {
		return GuardReenrollment{}, fmt.Errorf("%w: worker identity store unavailable", ErrGuardReenrollRefused)
	}
	server, err := c.leaseServer(ctx, req.TenantID, req.LeaseID)
	if err != nil {
		return GuardReenrollment{}, err
	}
	if server.LifecycleState != string(serverregistry.LifecycleActive) || strings.TrimSpace(server.OwnerSubjectID) == "" || strings.TrimSpace(server.StackID) == "" {
		return GuardReenrollment{}, fmt.Errorf("%w: server is not an active managed server", ErrGuardReenrollRefused)
	}
	record, err := c.claimReenrollment(ctx, *server, target, strings.TrimSpace(req.Actor))
	if err != nil {
		// record is the holding run's claim when one holds the node.
		return record, err
	}
	// From here on every exit records the claimed attempt, which releases the
	// claim. The run is bounded below the claim lifetime.
	ctx, cancel := context.WithTimeout(ctx, guardReenrollRunBound)
	defer cancel()
	started, _ := time.Parse(time.RFC3339, record.StartedAt)

	host, fingerprint, err := c.ResolveTarget(ctx, *server, server.OwnerSubjectID)
	if err != nil {
		// No host-key pin or channel credential is held for this lease: the
		// control plane has no admitted route to the node.
		record.State, record.Reason = GuardReenrollStateFailed, "channel_custody_missing"
		return c.finishReenrollment(ctx, *server, record, err)
	}
	epochBefore, err := c.identityEpoch(ctx, identities, *server)
	if err != nil {
		record.State, record.Reason = GuardReenrollStateFailed, "identity_unavailable"
		return c.finishReenrollment(ctx, *server, record, fmt.Errorf("%w: worker identity unavailable", ErrGuardReenrollRefused))
	}
	output, err := c.runCommand(ctx, host, fingerprint, "sudo -n sh -c "+shellQuote(guardProbeScript), "")
	if err != nil {
		record.State, record.Reason = GuardReenrollStateFailed, "probe_failed"
		return c.finishReenrollment(ctx, *server, record, fmt.Errorf("%w: guard probe failed: %v", monthlyruntime.ErrNodeChannelUnavailable, err))
	}
	before := parseFields(output)
	record.BeforeVersion, record.BeforeBinary = before["guard_version"], before["guard_binary"]
	// The node itself must run the current Guard, enrolled and active, and the
	// control plane must hold a fresh heartbeat of its bound identity. A stale
	// connected row over a stopped Guard, or a current binary that never
	// reached the control plane, is not current and is re-enrolled.
	if guardProbeCurrent(before, target) {
		if proof, proven := c.guardHeartbeatProof(ctx, identities, *server, target, 1, c.now().Add(-guardReenrollFreshHeartbeat)); proven {
			record.State, record.AfterVersion = GuardReenrollStateAlreadyCurrent, before["guard_version"]
			record.IdentityEpoch, record.HeartbeatAt = proof.epoch, proof.heartbeatAt.Format(time.RFC3339)
			return c.finishReenrollment(ctx, *server, record, nil)
		}
	}
	// The claimed attempt has its own one-use capability; the attempt number
	// is persisted before the capability exists, so it is never reissued.
	capability, issueErr := req.Capability(ctx, *server, target+"-"+strconv.Itoa(record.Attempt))
	if issueErr != nil || strings.TrimSpace(capability) == "" || strings.ContainsAny(capability, "\r\n") {
		record.State, record.Reason = GuardReenrollStateFailed, "capability_unavailable"
		return c.finishReenrollment(ctx, *server, record, fmt.Errorf("%w: re-enrollment capability unavailable", ErrGuardReenrollRefused))
	}
	output, err = c.runLongCommand(ctx, host, fingerprint, "sudo -n bash -c "+shellQuote(guardReenrollScript(origin)), capability+"\n")
	after := parseFields(output)
	record.AfterVersion, record.Archive = after["guard_version"], after["archive"]
	if err != nil || after["state"] != "installed" {
		record.State, record.Reason = GuardReenrollStateFailed, firstField(after["reason"], "installer_failed")
		return c.finishReenrollment(ctx, *server, record, fmt.Errorf("%w: guard re-enrollment failed: %s", monthlyruntime.ErrNodeChannelUnavailable, record.Reason))
	}
	// The service may still be starting; its liveness is what the heartbeat
	// proof below establishes.
	if !guardProbeInstalled(after, target) {
		record.State, record.Reason = GuardReenrollStateUnconfirmed, unconfirmedProbeReason(after, target)
		return c.finishReenrollment(ctx, *server, record, fmt.Errorf("%w: %s", ErrGuardReenrollUnconfirmed, record.Reason))
	}
	proof, proven := c.awaitGuardHeartbeatProof(ctx, identities, *server, target, epochBefore+1, started)
	if !proven {
		record.State, record.Reason = GuardReenrollStateUnconfirmed, "heartbeat_not_observed"
		return c.finishReenrollment(ctx, *server, record, fmt.Errorf("%w: %s within %s", ErrGuardReenrollUnconfirmed, record.Reason, guardReenrollProofWindow))
	}
	record.State = GuardReenrollStateEnrolled
	record.IdentityEpoch, record.HeartbeatAt = proof.epoch, proof.heartbeatAt.Format(time.RFC3339)
	return c.finishReenrollment(ctx, *server, record, nil)
}

func (c *Channel) serversAs() (guardIdentityReader, bool) {
	if c == nil || c.Servers == nil {
		return nil, false
	}
	if _, ok := c.Servers.(controlplane.ServerEventStore); !ok {
		return nil, false
	}
	identities, ok := c.Servers.(guardIdentityReader)
	return identities, ok
}

// claimReenrollment takes the node for one run: it writes a running record
// with the next attempt number under the server revision it read, so of two
// overlapping calls exactly one wins and the other sees the claim.
func (c *Channel) claimReenrollment(ctx context.Context, server controlplane.ServerRuntime, target, actor string) (GuardReenrollment, error) {
	events, _ := c.Servers.(controlplane.ServerEventStore)
	current := &server
	for try := 0; try < 4; try++ {
		if try > 0 {
			reread, err := c.Servers.GetServerRuntime(ctx, server.TenantID, server.ID)
			if err != nil || reread == nil {
				return GuardReenrollment{}, fmt.Errorf("%w: canonical server for the lease is unavailable", monthlyruntime.ErrNodeChannelUnavailable)
			}
			current = reread
		}
		previous := GuardReenrollmentFromMetadata(current.Metadata)
		now := c.now()
		if previous.claimHeld(now) {
			return GuardReenrollment{
				State: GuardReenrollStateRunning, Attempt: previous.Attempt, TargetVersion: previous.TargetVersion,
				StartedAt: previous.StartedAt, ClaimExpiresAt: previous.ClaimExpiresAt, Reason: "reenrollment_running",
			}, ErrGuardReenrollInProgress
		}
		claim := GuardReenrollment{
			State: GuardReenrollStateRunning, Attempt: previous.Attempt + 1, TargetVersion: target, Actor: actor,
			StartedAt: now.Format(time.RFC3339), ClaimExpiresAt: now.Add(guardReenrollClaimTTL).Format(time.RFC3339),
		}
		_, err := events.ApplyServerEvent(ctx, controlplane.ServerEvent{
			TenantID: current.TenantID, ServerID: current.ID, ExpectedRevision: current.Revision,
			Generation: current.Generation, Authority: controlplane.ServerEventAuthorityControlPlane,
			Source: guardReenrollmentSource, SourceID: guardReenrollmentSource, ObservedAt: now,
			Runtime: controlplane.ServerRuntime{Metadata: map[string]any{GuardReenrollmentMetadataKey: claim.metadataValue()}},
		})
		if err == nil {
			return claim, nil
		}
		if !errors.Is(err, controlplane.ErrConflict) {
			return GuardReenrollment{}, err
		}
	}
	// The aggregate kept moving under four reads; another writer, possibly a
	// concurrent claim, holds it. Nothing ran.
	return GuardReenrollment{State: GuardReenrollStateRunning, Reason: "reenrollment_contended"}, ErrGuardReenrollInProgress
}

// guardProbeCurrent reads a node probe: the Homelab binary at the target
// version, an enrollment on disk and the Guard service active.
func guardProbeCurrent(fields map[string]string, target string) bool {
	return guardProbeInstalled(fields, target) && fields["service"] == "active"
}

func guardProbeInstalled(fields map[string]string, target string) bool {
	return fields["guard_binary"] == guardBinaryPath && sameGuardVersion(fields["guard_version"], target) &&
		fields["enrollment"] == "present"
}

func unconfirmedProbeReason(fields map[string]string, target string) string {
	switch {
	case fields["guard_binary"] != guardBinaryPath:
		return "guard_binary_missing"
	case !sameGuardVersion(fields["guard_version"], target):
		return "installed_version_differs"
	case fields["enrollment"] != "present":
		return "enrollment_missing"
	default:
		return "service_inactive"
	}
}

func sameGuardVersion(observed, target string) bool {
	observed = strings.TrimPrefix(strings.TrimSpace(observed), "v")
	return observed != "" && observed == strings.TrimPrefix(strings.TrimSpace(target), "v")
}

type guardHeartbeatProof struct {
	epoch       int64
	heartbeatAt time.Time
}

func (c *Channel) identityEpoch(ctx context.Context, identities guardIdentityReader, server controlplane.ServerRuntime) (int64, error) {
	workerID := strings.TrimSpace(server.WorkerID)
	if workerID == "" {
		return 0, nil
	}
	identity, err := identities.GetWorkerIdentity(ctx, server.TenantID, workerID)
	if errors.Is(err, controlplane.ErrNotFound) {
		return 0, nil
	}
	if err != nil || identity == nil {
		return 0, errors.New("executionchannel: worker identity unavailable")
	}
	return identity.CredentialEpoch, nil
}

// guardHeartbeatProof reads the control plane's own evidence that the node's
// Guard works: the lease worker's host-key identity is bound at credential
// epoch minEpoch or later, a heartbeat was accepted after that binding was
// confirmed (heartbeats authenticate with an access token bound to the
// current key only, so it came from this identity) and not before since, the
// server is connected, and the Guard's own inventory reported the target
// version.
func (c *Channel) guardHeartbeatProof(ctx context.Context, identities guardIdentityReader, server controlplane.ServerRuntime, target string, minEpoch int64, since time.Time) (guardHeartbeatProof, bool) {
	current, err := c.Servers.GetServerRuntime(ctx, server.TenantID, server.ID)
	if err != nil || current == nil || current.LeaseID != server.LeaseID || strings.TrimSpace(current.WorkerID) == "" {
		return guardHeartbeatProof{}, false
	}
	identity, err := identities.GetWorkerIdentity(ctx, current.TenantID, current.WorkerID)
	if err != nil || identity == nil || identity.State != controlplane.WorkerIdentityBound ||
		identity.CredentialEpoch < minEpoch || identity.ConfirmedAt == nil {
		return guardHeartbeatProof{}, false
	}
	heartbeat := current.LastHeartbeatAt
	if heartbeat == nil || heartbeat.Before(*identity.ConfirmedAt) || heartbeat.Before(since) ||
		current.ConnectionState != string(serverregistry.ConnectionConnected) {
		return guardHeartbeatProof{}, false
	}
	reported, _ := current.Metadata["agent_version"].(string)
	if !sameGuardVersion(reported, target) {
		return guardHeartbeatProof{}, false
	}
	return guardHeartbeatProof{epoch: identity.CredentialEpoch, heartbeatAt: heartbeat.UTC()}, true
}

func (c *Channel) awaitGuardHeartbeatProof(ctx context.Context, identities guardIdentityReader, server controlplane.ServerRuntime, target string, minEpoch int64, since time.Time) (guardHeartbeatProof, bool) {
	deadline := c.now().Add(guardReenrollProofWindow)
	for {
		if proof, ok := c.guardHeartbeatProof(ctx, identities, server, target, minEpoch, since); ok {
			return proof, true
		}
		if !c.now().Before(deadline) || c.pause(ctx, guardReenrollProofPoll) != nil {
			return guardHeartbeatProof{}, false
		}
	}
}

func (c *Channel) pause(ctx context.Context, d time.Duration) error {
	if c.wait != nil {
		return c.wait(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Channel) runLongCommand(ctx context.Context, target *jobs.ManagedRuntimeTarget, fingerprint, command, stdin string) (string, error) {
	if c.run != nil {
		return c.run(ctx, target, fingerprint, command, stdin)
	}
	return RunWithin(ctx, guardReenrollTimeout, target, fingerprint, command, stdin)
}

func (c *Channel) finishReenrollment(ctx context.Context, server controlplane.ServerRuntime, record GuardReenrollment, cause error) (GuardReenrollment, error) {
	record.FinishedAt = c.now().Format(time.RFC3339)
	record.ClaimExpiresAt = ""
	// The result releases the claim, so it is written even when the run's own
	// bound has passed.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := c.recordServerMetadata(writeCtx, server, guardReenrollmentSource, GuardReenrollmentMetadataKey, record.metadataValue()); err != nil && cause == nil {
		cause = err
	}
	slog.Info("managed_guard_reenrollment", "tenant_id", server.TenantID, "lease_id", server.LeaseID,
		"state", record.State, "attempt", record.Attempt, "before_version", record.BeforeVersion,
		"after_version", record.AfterVersion, "reason", record.Reason, "actor", record.Actor)
	return record, cause
}

// recordServerMetadata writes one control-plane metadata key on the canonical
// server with optimistic concurrency, as recordGrant does. Guard heartbeats
// move the same aggregate, so it retries a few conflicts.
func (c *Channel) recordServerMetadata(ctx context.Context, server controlplane.ServerRuntime, source, key string, value any) error {
	events, ok := c.Servers.(controlplane.ServerEventStore)
	if !ok {
		return errors.New("executionchannel: canonical server event store unavailable")
	}
	for attempt := 0; attempt < 5; attempt++ {
		current, err := c.Servers.GetServerRuntime(ctx, server.TenantID, server.ID)
		if err != nil || current == nil {
			if err == nil {
				err = controlplane.ErrNotFound
			}
			return err
		}
		_, err = events.ApplyServerEvent(ctx, controlplane.ServerEvent{
			TenantID: current.TenantID, ServerID: current.ID, ExpectedRevision: current.Revision,
			Generation: current.Generation, Authority: controlplane.ServerEventAuthorityControlPlane,
			Source: source, SourceID: source, ObservedAt: c.now(),
			Runtime: controlplane.ServerRuntime{Metadata: map[string]any{key: value}},
		})
		if !errors.Is(err, controlplane.ErrConflict) {
			return err
		}
	}
	return controlplane.ErrConflict
}

func reenrollOrigin(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("control-plane origin must be an https origin")
	}
	return "https://" + parsed.Host, nil
}

func parseFields(output string) map[string]string {
	fields := map[string]string{}
	for _, field := range strings.Fields(output) {
		if name, value, ok := strings.Cut(field, "="); ok && name != "" {
			fields[name] = value
		}
	}
	return fields
}

func firstField(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// guardProbeFields is the shell fragment both scripts end with: the
// installed Guard binary, its version, whether an enrollment file exists and
// whether the Guard service runs. Only the Homelab layout counts as installed.
const guardProbeFields = `bin=none
if [ -x ` + guardBinaryPath + ` ]; then bin=` + guardBinaryPath + `; fi
version=none
if [ "$bin" != none ]; then
  found=$("$bin" version 2>/dev/null | sed -n 's/^kombify[A-Za-z]* v\([0-9][0-9A-Za-z.+-]*\).*/\1/p' | head -n 1)
  [ -n "$found" ] && version="$found"
fi
enrollment=absent
if [ -s ` + guardlayout.EnrollmentFile + ` ]; then enrollment=present; fi
service=inactive
if systemctl is-active --quiet ` + guardlayout.AgentUnit + `; then service=active; fi
`

const guardProbeScript = "set -u\n" + guardProbeFields +
	`printf 'guard_binary=%s guard_version=%s enrollment=%s service=%s\n' "$bin" "$version" "$enrollment" "$service"`

// guardReenrollScript runs as root. Standard input carries the capability as
// its only line, so it never appears in an argument list. It fetches and
// checks the control plane's install.sh before it changes anything, so a
// download failure leaves the node as it was. Only then does it stop the old
// Guard and its retired executor units (disabled, so a reboot does not start
// them from an archived binary) and move the enrollment, identity state and
// old binaries into a root-only archive (kept for diagnosis, never read
// again). install.sh installs the current Guard and its layout and redeems the
// capability; the redemption replaces the worker's credential, so the
// archived enrollment copies are deleted once it succeeded. The installer log
// stays in the archive because the control plane relays no installer output.
// It reports state=installed with the probe fields; whether the Guard works is
// decided by the control plane from its first accepted heartbeat.
func guardReenrollScript(origin string) string {
	return `set -eu
umask 077
IFS= read -r KOMBI_TOKEN || { echo "state=failed reason=capability_missing"; exit 64; }
[ -n "$KOMBI_TOKEN" ] || { echo "state=failed reason=capability_missing"; exit 64; }
export KOMBI_TOKEN
origin=` + shellQuote(origin) + `
stamp=$(date -u +%Y%m%dT%H%M%SZ)
archive=` + guardReenrollArchiveRoot + `/$stamp
install -d -m 0700 ` + guardReenrollArchiveRoot + `
install -d -m 0700 "$archive"
if ! curl -fsSL --proto '=https' --retry 5 --retry-connrefused --retry-delay 5 "$origin/install.sh" -o "$archive/install.sh"; then
  echo "state=failed reason=installer_download_failed archive=$archive"; exit 65
fi
if [ "$(head -c 2 "$archive/install.sh")" != '#!' ]; then
  echo "state=failed reason=installer_invalid archive=$archive"; exit 65
fi
systemctl stop ` + guardlayout.ExecutorSocketUnit + ` ` + guardlayout.RILKeysTimer + ` ` + guardlayout.RILKeysUnit + ` ` + guardlayout.AgentUnit + ` 2>/dev/null || true
# Stop and disable the retired activation units before archiving their files.
for unit in techstack-guard-executor.socket techstack-ril-keys.timer techstack-ril-keys.service techstack-agent.service; do
  systemctl stop "$unit" 2>/dev/null || true
  systemctl disable "$unit" 2>/dev/null || true
done
for unit in $(systemctl list-units --plain --no-legend --all '` + guardlayout.ExecutorInstancePrefix + `*.service' 'techstack-guard-executor@*.service' 2>/dev/null | awk '{print $1}'); do
  systemctl stop "$unit" 2>/dev/null || true
  systemctl disable "$unit" 2>/dev/null || true
done
for path in \
  ` + guardlayout.EnrollmentFile + ` ` + guardlayout.GuardStateDir + ` \
  /etc/techstack/agent-enrollment.json /var/lib/techstack/guard \
  /var/lib/techstack/guard-handoff /var/lib/techstack-guard-executor /etc/techstack \
  /usr/local/bin/techstack /usr/local/libexec/techstack-stackkit-operations \
  /etc/systemd/system/techstack-agent.service /etc/systemd/system/techstack-guard-executor.socket \
  /etc/systemd/system/techstack-guard-executor@.service /etc/systemd/system/techstack-ril-keys.service \
  /etc/systemd/system/techstack-ril-keys.timer; do
  if [ -e "$path" ] || [ -L "$path" ]; then
    mkdir -p "$archive$(dirname "$path")"
    mv "$path" "$archive$path"
  fi
done
systemctl daemon-reload 2>/dev/null || true
if ! KOMBI_SERVER="$origin" bash "$archive/install.sh" --service >"$archive/install.log" 2>&1; then
  echo "state=failed reason=installer_failed archive=$archive"; exit 66
fi
unset KOMBI_TOKEN
rm -f "$archive` + guardlayout.EnrollmentFile + `" "$archive/etc/techstack/agent-enrollment.json" \
  "$archive` + guardlayout.GuardStateDir + `/agent-enrollment.json" "$archive/var/lib/techstack/guard/agent-enrollment.json"
` + guardProbeFields + `printf 'state=installed guard_binary=%s guard_version=%s enrollment=%s service=%s archive=%s\n' "$bin" "$version" "$enrollment" "$service" "$archive"`
}
