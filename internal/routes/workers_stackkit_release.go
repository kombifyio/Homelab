package routes

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	ksapi "github.com/kombifyio/homelab/pkg/api"
	"github.com/kombifyio/homelab/pkg/artifactdelivery"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/jobs"
)

// A hosted control plane only admits the edge transfer of the runtime. A
// self-hosted one without an artifact index has nothing in front of it, so it
// serves this bundle itself, outside Render, as it serves its Guard binary
// (servesOwnGuardBinary).
const stackKitReleaseBundleEnv = "HOMELAB_STACKKIT_RELEASE_BUNDLE"

func (h workerRouteHandlers) agentStackKitRelease(e *httpx.Event) error {
	if servesOwnGuardBinary() {
		return h.serveSelfHostedStackKitRuntime(e)
	}
	return h.admitArtifact(e, "stackkit_runtime")
}

var stackKitReleaseBundleCache struct {
	mu       sync.Mutex
	key      string
	artifact agentBinaryArtifact
}

// currentStackKitReleaseBundle digests the bundle once per file identity
// (path, size, modification time): every Guard checks this route, and hashing
// the whole bundle per request would saturate the control plane's CPU.
func currentStackKitReleaseBundle() (agentBinaryArtifact, error) {
	path := strings.TrimSpace(os.Getenv(stackKitReleaseBundleEnv))
	if path == "" {
		return agentBinaryArtifact{}, fmt.Errorf("%s is not configured", stackKitReleaseBundleEnv)
	}
	info, err := os.Stat(path)
	if err != nil {
		return agentBinaryArtifact{}, err
	}
	key := fmt.Sprintf("%s|%d|%d", path, info.Size(), info.ModTime().UnixNano())
	stackKitReleaseBundleCache.mu.Lock()
	defer stackKitReleaseBundleCache.mu.Unlock()
	if stackKitReleaseBundleCache.key == key {
		return stackKitReleaseBundleCache.artifact, nil
	}
	artifact, err := agentBinaryArtifactForPath(path, managedRuntimeOSLinux, "amd64")
	if err != nil {
		return agentBinaryArtifact{}, err
	}
	stackKitReleaseBundleCache.key, stackKitReleaseBundleCache.artifact = key, artifact
	return artifact, nil
}

// serveSelfHostedStackKitRuntime sends the StackKits runtime bundle this
// self-hosted control plane publishes. A deployed node keeps its installed
// runtime until an approved upgrade, exactly as the edge admission decides
// (withheldStackKitsRuntime). Render is refused before any file is opened.
func (h workerRouteHandlers) serveSelfHostedStackKitRuntime(e *httpx.Event) error {
	if err := artifactdelivery.RequireAllowed(e); err != nil {
		return err
	}
	requestedOS := strings.ToLower(strings.TrimSpace(e.Request.PathValue("os")))
	requestedArch := normalizeAgentBinaryArch(e.Request.PathValue("arch"))
	if requestedOS != managedRuntimeOSLinux || requestedArch == "" {
		return httpx.BadRequest(e, "Unsupported StackKits runtime platform", nil)
	}
	rawToken := workerRequestCredential(e.Request)
	if rawToken == "" {
		return httpx.Unauthorized(e, "A valid agent download capability is required")
	}
	now := time.Now().UTC()
	authorization, authorized, err := h.authorizeAgentBinary(e, rawToken, now)
	if err != nil || !authorized {
		return err
	}
	artifact, err := currentStackKitReleaseBundle()
	if err != nil {
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "StackKits release is temporarily unavailable", nil)
	}
	if artifact.os != requestedOS || artifact.arch != requestedArch {
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "The deployed control plane does not publish StackKits for the requested platform", nil)
	}
	e.Response.Header().Set("Cache-Control", "private, no-store")
	e.Response.Header().Set("Vary", "Authorization")
	// An unreadable published version withholds from every deployed node.
	published := ""
	if release, releaseErr := jobs.CachedTargetStackKitRelease(); releaseErr == nil && release != nil {
		published = release.Receipt().Version
	}
	if installed := strings.Trim(strings.TrimSpace(e.Request.Header.Get("If-None-Match")), `"`); h.withheldStackKitsRuntime(e, published, installed, artifact.sha256) {
		e.Response.Header().Set("ETag", `"`+installed+`"`)
		e.Response.Header().Set(agentBinaryChecksumHeader, installed)
		e.Response.WriteHeader(http.StatusNotModified)
		return nil
	}
	etag := `"` + artifact.sha256 + `"`
	e.Response.Header().Set("ETag", etag)
	e.Response.Header().Set(agentBinaryChecksumHeader, artifact.sha256)
	if strings.TrimSpace(e.Request.Header.Get("If-None-Match")) == etag {
		e.Response.WriteHeader(http.StatusNotModified)
		return nil
	}
	if h.binaryDownloadGuard == nil {
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Artifact download guard is unavailable", nil)
	}
	budgetKey := "stackkit:" + authorization.tokenHash
	release, refusal := h.binaryDownloadGuard.acquire(budgetKey, authorization.resetAt, now, artifact.size)
	if refusal != nil {
		return writeAgentBinaryRefusal(e, refusal, "StackKits release download")
	}
	defer release()
	file, err := os.Open(artifact.path)
	if err != nil {
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "StackKits release is temporarily unavailable", nil)
	}
	defer func() { _ = file.Close() }()
	// A bundle replaced since its digest was taken would be sent under the old
	// digest; refuse it and digest it afresh on the next request.
	if info, statErr := file.Stat(); statErr != nil || info.Size() != artifact.size || !info.ModTime().UTC().Equal(artifact.modTime.UTC()) {
		stackKitReleaseBundleCache.mu.Lock()
		stackKitReleaseBundleCache.key = ""
		stackKitReleaseBundleCache.mu.Unlock()
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "StackKits release is temporarily unavailable", nil)
	}
	e.Response.Header().Set("Content-Type", "application/gzip")
	deadlineErr := http.NewResponseController(e.Response).SetWriteDeadline(time.Now().Add(agentBinaryWriteTimeout))
	// Same streaming contract as the Guard binary: the handler owns the status
	// line, and only the bytes that reached the wire are charged to the budget.
	e.MarkResponseStreamed()
	counter := &agentBinaryTransferCounter{ResponseWriter: e.Response}
	started := time.Now()
	http.ServeContent(counter, e.Request, "homelab-stackkit-runtime.tar.gz", artifact.modTime, file)
	h.binaryDownloadGuard.record(budgetKey, counter.written)
	logAgentBinaryTransfer(e, artifact, counter, started, deadlineErr)
	return nil
}
