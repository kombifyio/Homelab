package guardexec

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/kombifyio/techstack/pkg/guardrelease"
)

// AuthenticateManifest verifies a release manifest from the handoff directory
// against the pinned trust and remembers it. A manifest older than one this host
// already accepted is refused (manifest_superseded): a stale but still-fresh
// manifest must not undo a newer revocation. What is remembered is the floor,
// the newest issue time, the highest minimum version and the union of every
// revocation ever seen; the newest verified manifest also decides which StackKits
// runtime may install.
func (u *Updater) AuthenticateManifest(manifestFileName, bundleFileName string) (*guardrelease.Manifest, error) {
	manifestRaw, err := readHandoff(u.cfg.HandoffDir, manifestFileName, maxManifestFile)
	if err != nil {
		return nil, err
	}
	manifestSig, err := readHandoff(u.cfg.HandoffDir, bundleFileName, maxBundleFile)
	if err != nil {
		return nil, err
	}
	u.floorMu.Lock()
	defer u.floorMu.Unlock()
	manifest, next, err := u.acceptManifest(manifestRaw, manifestSig)
	if err != nil {
		return nil, err
	}
	// The floor is written before the cached manifest: a crash between the two
	// leaves the stricter state.
	if err := writeJSONAtomic(u.path(floorFile), next, 0o600); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(u.path(manifestFile), manifestRaw, 0o600); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(u.path(manifestBundle), manifestSig, 0o600); err != nil {
		return nil, err
	}
	return manifest, nil
}

// acceptManifest verifies a manifest and applies the host's floor to it without
// persisting anything. It returns the trusted manifest and the floor that
// includes it; a manifest the floor refuses is returned with its denial. The
// caller holds floorMu.
func (u *Updater) acceptManifest(raw, bundle []byte) (*guardrelease.Manifest, guardrelease.Floor, error) {
	if u.cfg.Verifier == nil {
		return nil, guardrelease.Floor{}, deny(CodeUnavailable, "the release verifier is not configured")
	}
	manifest, err := u.cfg.Verifier.VerifyManifest(raw, bundle)
	if err != nil {
		return nil, guardrelease.Floor{}, asDenial(err)
	}
	floor, err := u.loadFloor()
	if err != nil {
		return nil, guardrelease.Floor{}, err
	}
	next, denial := floor.Accept(manifest)
	if denial != nil {
		return manifest, guardrelease.Floor{}, &Denial{Code: denial.Code, Message: denial.Message}
	}
	return manifest, next, nil
}

// platformDenial applies, in Stage's order, what a verified manifest decides for
// this platform's artifact: presence, the manifest's version policy, the
// downgrade guard and the host's accumulated floor.
func (u *Updater) platformDenial(manifest *guardrelease.Manifest, floor guardrelease.Floor) *guardrelease.Denial {
	entry, ok := manifest.Artifact(u.cfg.GOOS, u.cfg.GOARCH)
	if !ok {
		return &guardrelease.Denial{Code: guardrelease.CodeNotInManifest, Message: fmt.Sprintf("the release has no %s/%s artifact", u.cfg.GOOS, u.cfg.GOARCH)}
	}
	if denial := manifest.Admit(manifest.Version, entry.SHA256); denial != nil {
		return denial
	}
	if denial := manifest.CheckDowngrade(u.cfg.CurrentVersion); denial != nil {
		return denial
	}
	return floor.Admit(manifest.Version, entry.SHA256)
}

// ManifestAdmission is the read-only answer to "would this host admit the
// release this signed manifest describes?".
type ManifestAdmission struct {
	Admitted         bool   `json:"admitted"`
	Code             string `json:"code,omitempty"`
	Message          string `json:"message,omitempty"`
	InstalledVersion string `json:"installed_version"`
	ManifestVersion  string `json:"manifest_version,omitempty"`
}

// CheckManifest runs the decision Stage takes for a release, from the signed
// manifest alone: signature, freshness, the host's floor (manifest_superseded),
// this platform's artifact, the version policy and the downgrade guard, against
// the installed version. It reads the host's state and changes nothing: the
// floor, the cached manifest, the status and every executable stay as they are.
// A refusal is the returned decision; only an unreadable input or state is an
// error.
func (u *Updater) CheckManifest(manifestPath, bundlePath string) (ManifestAdmission, error) {
	result := ManifestAdmission{InstalledVersion: u.cfg.CurrentVersion}
	manifestRaw, err := readBounded(manifestPath, maxManifestFile)
	if err != nil {
		return result, err
	}
	manifestSig, err := readBounded(bundlePath, maxBundleFile)
	if err != nil {
		return result, err
	}
	u.floorMu.Lock()
	defer u.floorMu.Unlock()
	manifest, floor, err := u.acceptManifest(manifestRaw, manifestSig)
	if manifest != nil {
		result.ManifestVersion = manifest.Version
	}
	if err != nil {
		var denied *Denial
		if errors.As(err, &denied) {
			result.Code, result.Message = denied.Code, denied.Message
			return result, nil
		}
		return result, err
	}
	if denial := u.platformDenial(manifest, floor); denial != nil {
		result.Code, result.Message = denial.Code, denial.Message
		return result, nil
	}
	result.Admitted = true
	return result, nil
}

// refuseBeforeDownload applies, from the signed manifest in the handoff directory
// and the digest it names, every refusal Stage would reach only after the
// artifact arrived. Like Stage it remembers the manifest first, so a release this
// host refuses still advances its floor and its revocations.
func (u *Updater) refuseBeforeDownload(params StageParams, digest string) error {
	if params.ManifestFile == "" && params.ManifestBundleFile == "" {
		// No signed release: Stage would refuse whatever arrives unless this
		// executor was configured for unsigned builds.
		if !u.cfg.AllowUnsigned {
			return u.recordDenial(deny(guardrelease.CodeEvidenceMissing, "the control plane publishes no signed release and this host admits only signed Guard releases"))
		}
		return nil
	}
	manifest, err := u.AuthenticateManifest(params.ManifestFile, params.ManifestBundleFile)
	if err != nil {
		return u.recordDenial(err)
	}
	u.floorMu.Lock()
	floor, err := u.loadFloor()
	u.floorMu.Unlock()
	if err != nil {
		return err
	}
	if denial := u.platformDenial(manifest, floor); denial != nil {
		return u.recordDenial(&Denial{Code: denial.Code, Message: denial.Message})
	}
	if failedAt, failed := u.failedCandidate(digest); failed && u.cfg.Now().Sub(failedAt) < failedCandidateCooldown {
		return u.recordDenial(deny(CodeNotAllowed, "this release failed its trial at %s and is not staged again yet", failedAt.UTC().Format(time.RFC3339)))
	}
	// A refusal Stage reaches only with the artifact in hand (its signature,
	// a revocation of its digest) holds for that digest; downloading it again
	// every cycle would only be refused again.
	if denied, found := u.deniedCandidate(digest); found && u.cfg.Now().Sub(denied.At) < deniedCandidateCooldown {
		return u.recordDenial(deny(denied.Code, "this release was refused at %s and is not downloaded again yet: %s", denied.At.UTC().Format(time.RFC3339), denied.Message))
	}
	return nil
}

// loadFloor reads the persisted manifest floor. A host that has none yet starts
// from the manifest it cached before floors existed, so the revocations it
// already knew are kept. An unreadable floor is an error, never an empty floor:
// resetting it would forget every revocation.
func (u *Updater) loadFloor() (guardrelease.Floor, error) {
	raw, err := os.ReadFile(u.path(floorFile))
	if errors.Is(err, os.ErrNotExist) {
		var floor guardrelease.Floor
		if manifest := u.cachedManifest(); manifest != nil {
			floor, _ = floor.Accept(manifest)
		}
		return floor, nil
	}
	if err != nil {
		return guardrelease.Floor{}, deny(CodeFailed, "the release manifest floor cannot be read: %v", err)
	}
	var floor guardrelease.Floor
	if err := json.Unmarshal(raw, &floor); err != nil {
		return guardrelease.Floor{}, deny(CodeFailed, "the release manifest floor is unreadable")
	}
	return floor, nil
}

// AllowsUnsigned reports whether this executor was configured to admit builds
// without release evidence.
func (u *Updater) AllowsUnsigned() bool { return u.cfg.AllowUnsigned }

// recordDenial publishes a refused candidate in the update status so the owner
// sees it in the heartbeat, and returns the denial unchanged. The status of an
// update in progress, of a rollback and of a recovery need is not overwritten.
func (u *Updater) recordDenial(err error) error {
	var denial *Denial
	if !errors.As(err, &denial) {
		return err
	}
	switch u.Status().State {
	case StateTrial, StateRolledBack, StateRecoveryNeeded:
		// A trial in progress, a rollback and a recovery need stay visible.
		return err
	}
	_ = u.record(UpdateStatus{State: StateDenied, Version: u.cfg.CurrentVersion, Reason: denial.Code})
	return err
}

func asDenial(err error) error {
	var release *guardrelease.Denial
	if errors.As(err, &release) {
		return &Denial{Code: release.Code, Message: release.Message}
	}
	return err
}

// cachedManifest returns the last manifest that passed verification. It only
// seeds the floor of a host that predates floors; admission decisions use the
// floor.
func (u *Updater) cachedManifest() *guardrelease.Manifest {
	raw, err := os.ReadFile(u.path(manifestFile))
	if err != nil {
		return nil
	}
	manifest, err := guardrelease.ParseManifest(raw)
	if err != nil {
		return nil
	}
	return manifest
}
