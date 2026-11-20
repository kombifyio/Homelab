package routes

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/kombifyio/homelab/pkg/appreleases"
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/jobs"
)

// Application updates of StackKits-managed applications (owner decision
// D-2026-11-12-06). An application can change independently of a StackKits
// release; the node's one update setting decides how a newer version is
// offered:
//
//   - ask and automatic: the newest checked (tested) version is the update
//     (`application_update`), run by the `update` action with
//     {version, digest} (application_update_action.go); unchecked versions
//     are listed only as newer versions and need the owner's
//     acknowledgement. Automatic application of checked versions is not
//     scheduled yet.
//   - with_stackkits_releases: a newer version arrives with a StackKits
//     release (`bundled`).
//
// A high advisory (CVSS >= 7) affecting the installed version with an
// in-track fix is offered as `application_update` in every setting. A feed
// that is absent, unreadable, unmatched by its receipt or older than 48 hours
// makes the facts `not_verified`, never "up to date" or "no known
// vulnerabilities".
const (
	updateModeApplicationUpdate = "application_update"
	updateModeBundled           = "bundled"
)

// applicationReleaseFacts are the additive version fields of a managed
// application. Every field is omitted when the feed watches no application
// with the service's image.
type applicationReleaseFacts struct {
	// EvidenceStatus is `verified` or `not_verified`; EvidenceReasonCode says
	// why not (advisory_feed_unverified, upstream_unverified,
	// installed_version_unknown).
	EvidenceStatus     string     `json:"evidence_status,omitempty"`
	EvidenceReasonCode string     `json:"evidence_reason_code,omitempty"`
	ObservedAt         *time.Time `json:"observed_at,omitempty"`
	// UpdateSetting is the node's update setting the offer follows.
	UpdateSetting   string `json:"update_setting,omitempty"`
	LatestDigest    string `json:"latest_digest,omitempty"`
	Tested          *bool  `json:"tested,omitempty"`
	ReleaseNotesURL string `json:"release_notes_url,omitempty"`
	// NewerVersions lists every newer in-track version, newest first,
	// checked or not.
	NewerVersions []appreleases.Offer        `json:"newer_versions,omitempty"`
	Advisories    []appreleases.AdvisoryFact `json:"advisories,omitempty"`
	SecurityFix   *appreleases.SecurityFix   `json:"security_fix,omitempty"`
}

var (
	applicationReleasesOnce   sync.Once
	applicationReleasesReader *appreleases.Reader
)

// applicationReleases is the process-wide feed reader, created on first use
// so the edition (Full Local reads no public feed) is already resolved.
func applicationReleases() appreleases.Status {
	applicationReleasesOnce.Do(func() { applicationReleasesReader = appreleases.NewReader() })
	return applicationReleasesReader.Status()
}

// applyApplicationReleases adds the feed facts to a managed service's version.
// A pending StackKits release stays the update (`stackkit_upgrade`); the
// application facts then only add advisories and the security fix.
func applyApplicationReleases(ctx context.Context, version *serviceVersion, service controlplane.ServiceRuntime, server *controlplane.ServerRuntime, image, label string) {
	if version == nil || strings.TrimSpace(image) == "" {
		return
	}
	evaluation := applicationReleases().Evaluate(image, label)
	if !evaluation.Matched {
		return
	}
	facts := &version.applicationReleaseFacts
	facts.EvidenceStatus, facts.EvidenceReasonCode, facts.ObservedAt = evaluation.EvidenceStatus, evaluation.ReasonCode, evaluation.ObservedAt
	if evaluation.EvidenceStatus != appreleases.EvidenceVerified {
		if version.UpdateMode != updateModeStackKitUpgrade {
			version.ReasonCode = evaluation.ReasonCode
		}
		return
	}
	facts.UpdateSetting = nodeUpdateSetting(ctx, service.TenantID, server)
	facts.NewerVersions, facts.Advisories, facts.SecurityFix = evaluation.Newer, evaluation.Advisories, evaluation.SecurityFix
	if version.UpdateMode == updateModeStackKitUpgrade {
		return
	}
	if version.Current == "" {
		version.Current, version.Source = evaluation.Installed, versionSourceImageDigest
	}
	offer := applicationOffer(evaluation)
	switch {
	case offer == nil:
		// Verified and nothing checked is newer: compared, with the installed
		// version as the latest one Homelab offers.
		version.Latest, version.UpdateAvailable, version.UpdateMode = version.Current, false, updateModeUnavailable
		version.ReasonCode = ""
	case evaluation.SecurityFix == nil && facts.UpdateSetting == controlplane.StackKitsUpdateWithStackKitsReleases:
		version.Latest, version.UpdateAvailable, version.UpdateMode, version.ReasonCode = offer.Version, true, updateModeBundled, ""
	default:
		version.Latest, version.UpdateAvailable, version.UpdateMode, version.ReasonCode = offer.Version, true, updateModeApplicationUpdate, ""
	}
	if offer != nil {
		tested := offer.Tested
		facts.LatestDigest, facts.Tested, facts.ReleaseNotesURL = offer.Digest, &tested, offer.ReleaseNotesURL
	}
}

// applicationOffer is the version the setting offers: the security fix in
// every setting, else the newest checked version. An unchecked version is
// never the offer; it stays in newer_versions.
func applicationOffer(evaluation appreleases.Evaluation) *appreleases.Offer {
	if evaluation.SecurityFix != nil {
		return &evaluation.SecurityFix.Offer
	}
	return evaluation.LatestTested
}

// nodeUpdateSetting reads the update setting of the service's node for
// nodeStackKitsUpdateTTL; an unreadable node asks.
func nodeUpdateSetting(ctx context.Context, tenantID string, server *controlplane.ServerRuntime) string {
	if server == nil || strings.TrimSpace(server.WorkerID) == "" {
		return controlplane.StackKitsUpdateAsk
	}
	key := tenantID + "\x00" + strings.TrimSpace(server.WorkerID)
	now := time.Now()
	nodeUpdateSettings.Lock()
	entry, found := nodeUpdateSettings.entries[key]
	nodeUpdateSettings.Unlock()
	if found && now.Before(entry.expires) {
		return entry.setting
	}
	if ctx == nil {
		ctx = context.Background()
	}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	setting, ok := jobs.ReadNodeUpdateSetting(readCtx, tenantID, strings.TrimSpace(server.WorkerID))
	if !ok {
		setting = controlplane.StackKitsUpdateAsk
	}
	nodeUpdateSettings.Lock()
	if len(nodeUpdateSettings.entries) >= 4096 {
		nodeUpdateSettings.entries = map[string]nodeUpdateSettingEntry{}
	}
	nodeUpdateSettings.entries[key] = nodeUpdateSettingEntry{setting: setting, expires: now.Add(nodeStackKitsUpdateTTL)}
	nodeUpdateSettings.Unlock()
	return setting
}

type nodeUpdateSettingEntry struct {
	setting string
	expires time.Time
}

var nodeUpdateSettings = struct {
	sync.Mutex
	entries map[string]nodeUpdateSettingEntry
}{entries: map[string]nodeUpdateSettingEntry{}}
