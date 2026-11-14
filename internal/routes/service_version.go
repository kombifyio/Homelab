package routes

import (
	"context"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	ksapi "github.com/kombifyio/techstack/pkg/api"
	"github.com/kombifyio/techstack/pkg/appcatalog"
	"github.com/kombifyio/techstack/pkg/controlplane"
	"github.com/kombifyio/techstack/pkg/httpx"
	"github.com/kombifyio/techstack/pkg/jobs"
	"github.com/kombifyio/techstack/pkg/serviceregistry"
)

const (
	serviceActionUpdate = "update"
	serviceActionAdopt  = "adopt"
	// serviceActionBackup runs the stack's StackKits backup from a service
	// card. StackKits executes it on the node; the service is only the handle.
	serviceActionBackup = "backup"

	updateModeStackKitUpgrade = "stackkit_upgrade"
	updateModeAdoptRequired   = "adopt_required"
	updateModeUnavailable     = "unavailable"

	versionSourceImageTag        = "image_tag"
	versionSourceImageLabel      = "image_label"
	versionSourceStackKit        = "stackkit"
	versionSourceImageDigest     = "image_digest"
	reasonCompilerReleaseUnknown = "compiler_release_unknown"
	reasonUpdateCheckDisabled    = "update_check_disabled"
)

// serviceVersion is the version/update fact of one service or application.
// Every field is measured or compared, never invented: `latest` is only set
// after a real registry or release-pin comparison.
type serviceVersion struct {
	Current         string     `json:"current,omitempty"`
	Source          string     `json:"source,omitempty"`
	Image           string     `json:"image,omitempty"`
	Latest          string     `json:"latest,omitempty"`
	UpdateAvailable bool       `json:"update_available"`
	CheckedAt       *time.Time `json:"checked_at,omitempty"`
	UpdateMode      string     `json:"update_mode"`
	ReasonCode      string     `json:"reason_code,omitempty"`
	// The application release facts of a StackKits-managed application
	// (D-2026-11-12-06), from the verified StackKits application release
	// feed; see applyApplicationReleases.
	applicationReleaseFacts
}

// serviceVersionFor derives the version fact of one service row.
//
// Managed components report their measured image version. The access manifest's
// StackKitVersion identifies the Kit Definition, not the CLI compiler release,
// so it cannot admit a comparison with the pinned compiler; the release a
// managed service can be updated to is the newer StackKits release its node's
// Guard reports as withheld (see managedVersion). Observed containers are
// compared with their registry and are only updatable once adopted.
//
// fresh re-reads the node state for an action that is about to dispatch; a
// read projection reuses it for nodeStackKitsUpdateTTL.
func (h *serviceRuntimeHandlers) serviceVersionFor(ctx context.Context, service controlplane.ServiceRuntime, server *controlplane.ServerRuntime, fresh bool) *serviceVersion {
	image := stringFromAnyMap(service.Metadata, "image")
	label := stringFromAnyMap(service.Metadata, "image_version")
	if serviceregistry.CanonicalManagementState(service.ManagementState) == serviceregistry.ManagementManaged {
		// Handlers built without their own pin (the inventory and stack
		// operations projections) use the shared one, so every read advertises
		// the update the action admits.
		release := h.stackKitRelease
		if release == nil {
			release = sharedPinnedStackKitRelease
		}
		pinned := release()
		version := managedVersion(ctx, service, server, image, label, pinned, fresh)
		applyApplicationReleases(ctx, version, service, server, image, label)
		return version
	}
	if appcatalog.ParseImage(image).Repo == "" && label == "" {
		return nil
	}
	current, source, reason := appcatalog.CurrentVersion(image, label)
	version := &serviceVersion{Image: image, UpdateMode: updateModeAdoptRequired, ReasonCode: reason}
	if current == "" {
		return version
	}
	version.Current, version.Source = current, source
	if h.updates == nil {
		version.ReasonCode = reasonUpdateCheckDisabled
		return version
	}
	result := h.updates.Check(image, current)
	if !result.CheckedAt.IsZero() {
		checkedAt := result.CheckedAt.UTC()
		version.CheckedAt = &checkedAt
	}
	version.ReasonCode = result.ReasonCode
	if result.Latest != "" {
		version.Latest, version.UpdateAvailable = result.Latest, result.Newer
	}
	return version
}

// managedVersion is the update fact of a StackKit-managed service. A managed
// service changes version only with its stack's StackKits release, so its
// update is the governed stack upgrade (`update` action): it is offered when
// the server's Guard runs an older StackKits release than the published one
// Techstack withholds from it. Current and latest are then both StackKits
// releases (source `stackkit`), never an image tag compared with a CLI
// release. The withheld release must be exactly the published release the
// `update` action installs (pinned); while the Guard still reports another
// one, no update is offered, so what is advertised is what runs. The upgrade checkpoints the node first, verifies the target live
// and automatically restores the previous release when that verification
// fails; the job result carries that outcome as `update_outcome`.
func managedVersion(ctx context.Context, service controlplane.ServiceRuntime, server *controlplane.ServerRuntime, image, label, pinned string, fresh bool) *serviceVersion {
	current, source, _ := appcatalog.CurrentVersion(image, label)
	measured := &serviceVersion{Current: current, Source: source, Image: image,
		UpdateMode: updateModeUnavailable, ReasonCode: reasonCompilerReleaseUnknown}
	if server == nil || strings.TrimSpace(server.WorkerID) == "" {
		return measured
	}
	read := cachedNodeStackKitsUpdate
	if fresh {
		read = jobs.ReadNodeStackKitRelease
	}
	node, ok := read(ctx, service.TenantID, server.WorkerID)
	if !ok {
		return measured
	}
	installed, available := stackKitReleaseVersion(node.Installed.GetVersion()), stackKitReleaseVersion(node.UpdateAvailable)
	if order, comparable := appcatalog.CompareVersions(available, installed); installed == "" || available == "" || !comparable || order <= 0 ||
		available != stackKitReleaseVersion(pinned) {
		return measured
	}
	return &serviceVersion{Current: installed, Source: versionSourceStackKit, Image: image, Latest: available,
		UpdateAvailable: true, UpdateMode: updateModeStackKitUpgrade}
}

// nodeStackKitsUpdateTTL bounds how long read projections reuse one node's
// withheld StackKits release; an update action re-reads it before dispatch.
const nodeStackKitsUpdateTTL = 30 * time.Second

type nodeStackKitsUpdateEntry struct {
	node    jobs.NodeStackKitRelease
	ok      bool
	expires time.Time
}

var nodeStackKitsUpdates = struct {
	sync.Mutex
	entries map[string]nodeStackKitsUpdateEntry
}{entries: map[string]nodeStackKitsUpdateEntry{}}

// cachedNodeStackKitsUpdate serves list projections, which advertise one
// update fact per service row, from one node read per worker and TTL. The read
// keeps the request's values but not its cancellation, and is bounded, so a
// cancelled request does not cache a failed read and none can hang on it.
func cachedNodeStackKitsUpdate(ctx context.Context, tenantID, agentID string) (jobs.NodeStackKitRelease, bool) {
	key := tenantID + "\x00" + agentID
	now := time.Now()
	nodeStackKitsUpdates.Lock()
	entry, found := nodeStackKitsUpdates.entries[key]
	nodeStackKitsUpdates.Unlock()
	if found && now.Before(entry.expires) {
		return entry.node, entry.ok
	}
	if ctx == nil {
		ctx = context.Background()
	}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	node, ok := jobs.ReadNodeStackKitRelease(readCtx, tenantID, agentID)
	nodeStackKitsUpdates.Lock()
	if len(nodeStackKitsUpdates.entries) >= 4096 {
		nodeStackKitsUpdates.entries = map[string]nodeStackKitsUpdateEntry{}
	}
	nodeStackKitsUpdates.entries[key] = nodeStackKitsUpdateEntry{node: node, ok: ok, expires: now.Add(nodeStackKitsUpdateTTL)}
	nodeStackKitsUpdates.Unlock()
	return node, ok
}

// stackKitReleaseVersion reduces "kit@5.1.0", "v5.1.0" and "5.1.0" to "5.1.0".
// Anything that is not a plain version yields "": a lifecycle label is not a
// release identity.
func stackKitReleaseVersion(identity string) string {
	_, version, found := strings.Cut(strings.TrimSpace(identity), "@")
	if !found {
		version = identity
	}
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if _, ok := appcatalog.CompareVersions(version, version); !ok {
		return ""
	}
	return version
}

// updateAllowed is the single rule for the `update` action, used both to
// advertise it and to enforce it. Only a StackKit-managed service whose stack
// is behind the pinned release can be updated, by upgrading its stack, and
// only while its server admits the stack upgrade.
func updateAllowed(service controlplane.ServiceRuntime, version *serviceVersion, server *controlplane.ServerRuntime) bool {
	if version == nil || !version.UpdateAvailable || version.UpdateMode != updateModeStackKitUpgrade ||
		service.MutationLock.Locked() || len(serviceGroupLockSources(service)) != 0 || server == nil || adoptedApplication(&service) {
		return false
	}
	for _, action := range serverStackActions(*server) {
		if action == serverStackActionUpgrade {
			return true
		}
	}
	return false
}

// backupAllowed advertises and enforces Back up now for a stacked service whose
// node accepts direct StackKits backup operations (Basement kits; managed kits
// keep their backup schedule). Reading a locked service is not a mutation, and
// neither is backing it up, so the owner guardrail does not suppress it.
func backupAllowed(service controlplane.ServiceRuntime, server *controlplane.ServerRuntime) bool {
	if server == nil || adoptedApplication(&service) || !serverKitHoldsBackupCustody(*server) ||
		serviceregistry.CanonicalManagementState(service.ManagementState) != serviceregistry.ManagementManaged {
		return false
	}
	for _, action := range serverStackActions(*server) {
		if action == serverStackActionBackupStatus {
			return true
		}
	}
	return false
}

// sharedPinnedStackKitRelease serves projections built outside a configured
// service handler (the inventory update read) from one cached resolution.
var sharedPinnedStackKitRelease = pinnedStackKitRelease()

// pinnedStackKitRelease returns the StackKits release served to managed agents.
// Resolving the bundle verifies its files, so the answer is kept for a short
// time instead of being recomputed per service row.
func pinnedStackKitRelease() func() string {
	var (
		mu      sync.Mutex
		value   string
		expires time.Time
	)
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		if time.Now().Before(expires) {
			return value
		}
		value = ""
		if release, err := jobs.CachedTargetStackKitRelease(); err == nil && release != nil {
			value = release.Receipt().Version
		}
		expires = time.Now().Add(5 * time.Minute)
		return value
	}
}

// applicationActionUnion folds per-component allowed actions into the
// application-wide set the card offers.
func applicationActionUnion(components []serviceRuntimeResponse) []string {
	offered := map[string]bool{}
	for _, component := range components {
		for _, action := range component.AllowedActions {
			switch action {
			case serviceActionStart, serviceActionStop, serviceActionRestart, serviceActionLogs, serviceActionUpdate, serviceActionAdopt:
				offered[action] = true
			}
		}
	}
	union := make([]string, 0, len(offered))
	for action := range offered {
		union = append(union, action)
	}
	sort.Strings(union)
	return union
}

var upgradeReleasePattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:-(?:beta|edge)\.[0-9]+)?$`)

// upgradeTargetRelease names the pinned release for the stack upgrade; a pin
// the upgrade command would not accept falls back to its own `latest`.
func upgradeTargetRelease(pinned string) string {
	target := "v" + stackKitReleaseVersion(pinned)
	if !upgradeReleasePattern.MatchString(target) {
		return "latest"
	}
	return target
}

// refuseLockedStackServices fails closed: a stack upgrade writes to every
// service of the stack, so one owner-locked service refuses it, and an
// unreadable lock state refuses it too.
func (h *serviceRuntimeHandlers) refuseLockedStackServices(e *httpx.Event, tenantID, stackID string) error {
	rows, err := h.store.ListServiceRuntimes(e.Request.Context(), tenantID, stackID, "")
	if err != nil {
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Failed to read service lock state", nil)
	}
	locked := make([]string, 0, len(rows))
	for i := range rows {
		if rows[i].MutationLock.Locked() {
			locked = append(locked, rows[i].ID)
		}
	}
	if len(locked) == 0 {
		return nil
	}
	return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "Stack contains locked services", map[string]any{
		"reason": "service_locked", "locked_service_ids": locked,
	})
}
