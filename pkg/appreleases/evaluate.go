package appreleases

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kombifyio/techstack/pkg/appcatalog"
)

// Evidence states of an evaluation.
const (
	EvidenceVerified    = "verified"
	EvidenceNotVerified = "not_verified"
)

// Reason codes of an evaluation that is not verified.
const (
	// ReasonAdvisoryFeedUnverified: the feed is absent, unreadable, does not
	// match its receipt or is older than MaxFeedAge.
	ReasonAdvisoryFeedUnverified = "advisory_feed_unverified"
	// ReasonUpstreamUnverified: the feed could not read the application's
	// registry or advisory source.
	ReasonUpstreamUnverified = "upstream_unverified"
	// ReasonInstalledVersionUnknown: the node reported no comparable version
	// or digest of the running image.
	ReasonInstalledVersionUnknown = "installed_version_unknown"
)

// Advisory exposure of the installed version.
const (
	AffectsInstalled = "installed"
	AffectsUnknown   = "unknown"
)

// HighCVSS is the score from which an advisory counts as high or critical
// (owner decision D-2026-11-12-06).
const HighCVSS = 7.0

// Offer is one newer version of the installed application.
type Offer struct {
	Version         string `json:"version"`
	Digest          string `json:"digest,omitempty"`
	Tested          bool   `json:"tested"`
	ReleaseNotesURL string `json:"release_notes_url,omitempty"`
	PublishedAt     string `json:"published_at,omitempty"`
}

// AdvisoryFact is one advisory that affects, or may affect, the installed
// version.
type AdvisoryFact struct {
	ID           string   `json:"id"`
	Severity     string   `json:"severity"`
	CVSS         *float64 `json:"cvss,omitempty"`
	FixedVersion string   `json:"fixed_version,omitempty"`
	URL          string   `json:"url"`
	// Affects is `installed` when the range was evaluated against the
	// installed version, `unknown` when it could not be.
	Affects string `json:"affects"`
	// High is CVSS >= 7, or severity high/critical without a score.
	High bool `json:"high"`
}

// SecurityFix is the lowest in-track version that fixes every high advisory
// affecting the installed version it can fix.
type SecurityFix struct {
	Offer
	Advisories []string `json:"advisories"`
}

// Evaluation is the feed's verdict on one installed application.
type Evaluation struct {
	// Matched is false when the verified feed watches no application with
	// this image; nothing is then claimed.
	Matched        bool
	EvidenceStatus string
	ReasonCode     string
	ObservedAt     *time.Time
	Installed      string
	// Newer lists every newer in-track version, newest first.
	Newer        []Offer
	LatestTested *Offer
	Advisories   []AdvisoryFact
	SecurityFix  *SecurityFix
}

// Evaluate matches the image the node reports (repository, tag or digest,
// and the measured version label) against the feed. Advisories are evaluated
// against that installed version, not against the catalog pin the feed
// observed from.
func (s Status) Evaluate(image, label string) Evaluation {
	if !s.Verified {
		return Evaluation{Matched: true, EvidenceStatus: EvidenceNotVerified, ReasonCode: ReasonAdvisoryFeedUnverified}
	}
	application, ok := s.Application(image)
	if !ok {
		return Evaluation{}
	}
	observedAt := application.ObservedAt.UTC()
	out := Evaluation{Matched: true, EvidenceStatus: EvidenceVerified, ObservedAt: &observedAt}
	if application.Status != EvidenceVerified {
		out.EvidenceStatus, out.ReasonCode = EvidenceNotVerified, ReasonUpstreamUnverified
		return out
	}
	installed := installedVersion(application, image, label)
	if installed == "" {
		out.EvidenceStatus, out.ReasonCode = EvidenceNotVerified, ReasonInstalledVersionUnknown
		return out
	}
	out.Installed = installed
	for _, candidate := range application.Candidates {
		if order, comparable := appcatalog.CompareVersions(candidate.Version, installed); comparable && order > 0 {
			out.Newer = append(out.Newer, offerOf(candidate))
		}
	}
	sort.SliceStable(out.Newer, func(i, j int) bool {
		order, _ := appcatalog.CompareVersions(out.Newer[i].Version, out.Newer[j].Version)
		return order > 0
	})
	for i := range out.Newer {
		if out.Newer[i].Tested {
			tested := out.Newer[i]
			out.LatestTested = &tested
			break
		}
	}
	pinned := sameVersion(installed, application.Current.Version)
	var high []Advisory
	for _, advisory := range application.Advisories {
		affects, known := advisoryAffects(advisory, installed, pinned)
		if known && !affects {
			continue
		}
		fact := AdvisoryFact{
			ID: advisory.ID, Severity: advisory.Severity, FixedVersion: advisory.FixedVersion, URL: advisory.URL,
			Affects: AffectsInstalled, High: highAdvisory(advisory),
		}
		if advisory.CVSS != nil && advisory.CVSS.Score != nil {
			score := *advisory.CVSS.Score
			fact.CVSS = &score
		}
		if !known {
			fact.Affects = AffectsUnknown
		} else if fact.High {
			high = append(high, advisory)
		}
		out.Advisories = append(out.Advisories, fact)
	}
	out.SecurityFix = securityFix(high, out.Newer)
	return out
}

func offerOf(candidate Candidate) Offer {
	return Offer{
		Version: candidate.Version, Digest: candidate.Digest, Tested: candidate.Tested,
		ReleaseNotesURL: safeHTTPS(candidate.ReleaseNotesURL), PublishedAt: candidate.PublishedAt,
	}
}

func safeHTTPS(raw string) string {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "https://") || len(raw) > 2048 {
		return ""
	}
	return raw
}

// installedVersion is the version the node runs: the measured tag or label,
// else the version whose digest the running image carries.
func installedVersion(application *Application, image, label string) string {
	if current, _, _ := appcatalog.CurrentVersion(image, label); current != "" {
		return current
	}
	digest := appcatalog.ParseImage(image).Digest
	if digest == "" {
		return ""
	}
	if application.Current.Digest == digest {
		return strings.TrimPrefix(application.Current.Version, "v")
	}
	for _, candidate := range application.Candidates {
		if candidate.Digest == digest {
			return strings.TrimPrefix(candidate.Version, "v")
		}
		for _, platform := range candidate.Platforms {
			if platform == digest {
				return strings.TrimPrefix(candidate.Version, "v")
			}
		}
	}
	return ""
}

func sameVersion(a, b string) bool {
	order, comparable := appcatalog.CompareVersions(a, b)
	return comparable && order == 0
}

func highAdvisory(advisory Advisory) bool {
	if advisory.CVSS != nil && advisory.CVSS.Score != nil {
		return *advisory.CVSS.Score >= HighCVSS
	}
	return advisory.Severity == "high" || advisory.Severity == "critical"
}

// advisoryAffects reports whether the advisory affects version and whether
// that could be decided. The producer's own verdict is used only for the
// catalog pin it evaluated.
func advisoryAffects(advisory Advisory, version string, pinned bool) (affects, known bool) {
	if pinned && advisory.AffectsCurrent != nil {
		return *advisory.AffectsCurrent, true
	}
	if ranges := strings.TrimSpace(advisory.AffectedRange); ranges != "" {
		known = true
		for _, expression := range strings.Split(ranges, ";") {
			inside, understood := inVersionRange(expression, version)
			if inside {
				return true, true
			}
			known = known && understood
		}
		return false, known
	}
	return belowFix(advisory.FixedVersion, version)
}

var versionConstraint = regexp.MustCompile(`^(<=|>=|<|>|=)?\s*v?(\d+(?:\.\d+){0,3})`)

// inVersionRange reads a GitHub vulnerable_version_range such as
// ">= 2.0.0, < 2.3.1".
func inVersionRange(expression, version string) (bool, bool) {
	expression = strings.TrimSpace(expression)
	current, ok := versionCore(version)
	if expression == "" || !ok {
		return false, false
	}
	inside := true
	for _, part := range strings.Split(expression, ",") {
		match := versionConstraint.FindStringSubmatch(strings.TrimSpace(part))
		if match == nil {
			return false, false
		}
		bound, _ := versionCore(match[2])
		order := compareCore(current, bound)
		switch match[1] {
		case "<":
			inside = inside && order < 0
		case "<=":
			inside = inside && order <= 0
		case ">":
			inside = inside && order > 0
		case ">=":
			inside = inside && order >= 0
		default:
			inside = inside && order == 0
		}
	}
	return inside, true
}

// belowFix decides from fixed versions alone (OSV gives no range): a fix in
// the installed major decides; without one nothing is known.
func belowFix(fixes, version string) (bool, bool) {
	current, ok := versionCore(version)
	if !ok {
		return false, false
	}
	for _, fix := range splitVersions(fixes) {
		bound, ok := versionCore(fix)
		if ok && bound[0] == current[0] {
			return compareCore(current, bound) < 0, true
		}
	}
	return false, false
}

// securityFix is the lowest newer version that is not affected by any high
// advisory it can be evaluated against; a tested one wins over an untested
// one. The fix must clear at least one advisory.
func securityFix(high []Advisory, newer []Offer) *SecurityFix {
	if len(high) == 0 || len(newer) == 0 {
		return nil
	}
	ascending := append([]Offer(nil), newer...)
	sort.SliceStable(ascending, func(i, j int) bool {
		order, _ := appcatalog.CompareVersions(ascending[i].Version, ascending[j].Version)
		return order < 0
	})
	var untested *SecurityFix
	for _, offer := range ascending {
		fixed := make([]string, 0, len(high))
		clean := true
		for _, advisory := range high {
			affects, known := advisoryAffects(advisory, offer.Version, false)
			switch {
			case !known:
				clean = false
			case affects:
				clean = false
			default:
				fixed = append(fixed, advisory.ID)
			}
		}
		if !clean || len(fixed) == 0 {
			continue
		}
		fix := &SecurityFix{Offer: offer, Advisories: fixed}
		if offer.Tested {
			return fix
		}
		if untested == nil {
			untested = fix
		}
	}
	return untested
}

func splitVersions(list string) []string {
	var out []string
	for _, part := range strings.Split(list, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

var corePattern = regexp.MustCompile(`^v?(\d+(?:\.\d+){0,3})`)

func versionCore(version string) ([]int, bool) {
	match := corePattern.FindStringSubmatch(strings.TrimSpace(version))
	if match == nil {
		return nil, false
	}
	var core []int
	for _, part := range strings.Split(match[1], ".") {
		number, err := strconv.Atoi(part)
		if err != nil {
			return nil, false
		}
		core = append(core, number)
	}
	return core, true
}

func compareCore(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
	}
	return 0
}
