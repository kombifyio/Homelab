package routes

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	ksapi "github.com/kombifyio/homelab/pkg/api"
	"github.com/kombifyio/homelab/pkg/guardbinding"
	"github.com/kombifyio/homelab/pkg/guardexec"
	"github.com/kombifyio/homelab/pkg/guardrelease"
	"github.com/kombifyio/homelab/pkg/httpx"
)

const (
	// guardRefusalHold is how long a Guard that refused a served Guard binary
	// is not sent that binary again. It matches the executor's own denial
	// cooldown; a new release has a new digest and is never held.
	guardRefusalHold = 6 * time.Hour
	// guardRefusalMaxTracked bounds the per-process record.
	guardRefusalMaxTracked = 8192
)

// guardUpdateRefusals stops the full transfer of a Guard binary to a Guard
// that already downloaded and refused it. Guards from before the refusal
// preflight download the served release every convergence cycle, refuse it
// only afterwards and ignore Retry-After, so without this stop they drained
// the download budget all day and met nothing but 429.
//
// The record is per process and best effort: a restart forgets it and admits
// one more transfer, after which the Guard's next heartbeat holds it again.
type guardUpdateRefusals struct {
	mu      sync.Mutex
	workers map[string]guardTransferRecord
}

type guardTransferRecord struct {
	digest string
	// completedAt is when the last transfer of digest reached its final byte.
	completedAt time.Time
	// refused is set when the Guard reported a refused update after that.
	refused bool
}

func newGuardUpdateRefusals() *guardUpdateRefusals {
	return &guardUpdateRefusals{workers: map[string]guardTransferRecord{}}
}

func guardRefusalKey(tenantID, workerID string) string {
	tenantID, workerID = strings.TrimSpace(tenantID), strings.TrimSpace(workerID)
	if tenantID == "" || workerID == "" {
		return ""
	}
	return tenantID + "\x00" + workerID
}

// transferred records that the worker received the whole of digest.
func (r *guardUpdateRefusals) transferred(key, digest string, now time.Time) {
	if r == nil || key == "" || digest == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, tracked := r.workers[key]; !tracked && len(r.workers) >= guardRefusalMaxTracked {
		for candidate, record := range r.workers {
			if now.Sub(record.completedAt) >= guardRefusalHold {
				delete(r.workers, candidate)
			}
		}
		if len(r.workers) >= guardRefusalMaxTracked {
			return
		}
	}
	r.workers[key] = guardTransferRecord{digest: digest, completedAt: now}
}

// observe reads a heartbeat's update posture. A refused or rolled-back update
// reported after a complete transfer means the Guard refused that binary.
func (r *guardUpdateRefusals) observe(key string, posture *guardbinding.Posture, now time.Time) {
	if r == nil || key == "" || posture == nil {
		return
	}
	state := posture.Normalize().UpdateState
	r.mu.Lock()
	defer r.mu.Unlock()
	record, tracked := r.workers[key]
	if !tracked {
		return
	}
	switch state {
	case guardexec.StateDenied, guardexec.StateRolledBack:
		if now.After(record.completedAt) {
			record.refused = true
		}
	case guardexec.StateStaged, guardexec.StateTrial:
		// An update in progress: the transfer was accepted.
		record.refused = false
	default:
		// A Guard that reports a current release holds nothing.
		delete(r.workers, key)
		return
	}
	r.workers[key] = record
}

// hold reports how long the worker must not be sent digest again.
func (r *guardUpdateRefusals) hold(key, digest string, now time.Time) (time.Duration, bool) {
	if r == nil || key == "" {
		return 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	record, tracked := r.workers[key]
	if !tracked || !record.refused || record.digest != digest {
		return 0, false
	}
	remaining := guardRefusalHold - now.Sub(record.completedAt)
	if remaining <= 0 {
		delete(r.workers, key)
		return 0, false
	}
	return remaining, true
}

// guardBinaryUpdateRefusal answers, without any transfer, a Guard download the
// Guard cannot or will not install: a Guard below the signer move pins the
// retired release signer and refuses every signed release (only a reinstall
// in place brings it forward), and a Guard that already refused this digest is
// held until the served release changes or the hold ends. signed is false for
// a self-hosted control plane that serves an unsigned binary. It reports
// whether it wrote the answer.
func (h workerRouteHandlers) guardBinaryUpdateRefusal(e *httpx.Event, authorization agentBinaryAuthorization, digest string, signed bool, now time.Time) bool {
	if authorization.runtime == nil {
		// A download capability (install, reinstall) is never held.
		return false
	}
	if signed && authorization.runtime.Worker != nil {
		version := stringFromAny(authorization.runtime.Worker.Capabilities["agent_version"])
		if order, ok := guardrelease.CompareVersions(version, guardrelease.SignerMoveVersion); ok && order < 0 {
			_ = httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "This Guard pins a retired release signer and cannot update itself; reinstall it in place", map[string]any{
				"reason": "guard_reinstall_required", "guard_version": version, "minimum_version": guardrelease.SignerMoveVersion,
			})
			return true
		}
	}
	key := guardRefusalKey(authorization.runtime.TenantID, authorization.runtime.RuntimeAgentID)
	remaining, held := h.updateRefusals.hold(key, digest, now)
	if !held {
		return false
	}
	e.Response.Header().Set("Retry-After", strconv.FormatInt(int64(math.Ceil(remaining.Seconds())), 10))
	_ = httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "This Guard refused this release; it is not sent again until the release changes", map[string]any{
		"reason": "guard_update_refused",
	})
	return true
}

// recordGuardTransfer remembers a transfer that reaches the artifact's last
// byte, so a refusal the Guard reports afterwards is tied to its digest.
func (h workerRouteHandlers) recordGuardTransfer(request *http.Request, authorization agentBinaryAuthorization, digest string, size int64, now time.Time) {
	if authorization.runtime == nil || !artifactRangeReachesEnd(request, size) {
		return
	}
	h.updateRefusals.transferred(guardRefusalKey(authorization.runtime.TenantID, authorization.runtime.RuntimeAgentID), digest, now)
}

// artifactRangeReachesEnd reports whether a request covers the artifact's last
// byte: no range, an open range, a suffix range, or one that ends there.
func artifactRangeReachesEnd(request *http.Request, size int64) bool {
	value := strings.TrimSpace(request.Header.Get("Range"))
	if value == "" {
		return true
	}
	start, end, ok := strings.Cut(strings.TrimPrefix(value, "bytes="), "-")
	if !ok || strings.TrimSpace(start) == "" || strings.TrimSpace(end) == "" {
		return true
	}
	last, err := strconv.ParseInt(strings.TrimSpace(end), 10, 64)
	return err == nil && last >= size-1
}
