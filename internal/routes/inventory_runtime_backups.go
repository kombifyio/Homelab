package routes

import (
	"context"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/guardposture"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/secrets"
)

// ---------------------------------------------------------------------------
// GET /api/v1/internal/runtime/backups
// ---------------------------------------------------------------------------

type runtimeBackupDay struct {
	Date  string `json:"date"`
	State string `json:"state"`
}

type runtimeBackupDrill struct {
	State      string     `json:"state"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
}

type runtimeBackupSchedule struct {
	Cadence   string     `json:"cadence"`
	Enabled   bool       `json:"enabled"`
	LastRunAt *time.Time `json:"last_run_at,omitempty"`
}

type runtimeBackupServer struct {
	ServerID string `json:"server_id"`
	// State is the Guard-observed freshness of the newest snapshot: fresh,
	// stale or unknown. A fresh snapshot never proves a restore works; the
	// restore drill says that.
	State          string              `json:"state"`
	ReasonCode     string              `json:"reason_code,omitempty"`
	LastSnapshotAt *time.Time          `json:"last_snapshot_at,omitempty"`
	PolicyMaxAge   string              `json:"policy_max_age,omitempty"`
	RestoreDrill   *runtimeBackupDrill `json:"restore_drill,omitempty"`
	// OffsiteConfigured is true when a managed off-site store is bound to the
	// server's stack. OffsiteStoredBytes is the measured size of that whole
	// repository, not of one run.
	OffsiteConfigured  bool                   `json:"offsite_configured"`
	OffsiteStoredBytes *int64                 `json:"offsite_stored_bytes,omitempty"`
	OffsiteMeasuredAt  *time.Time             `json:"offsite_measured_at,omitempty"`
	Schedule           *runtimeBackupSchedule `json:"schedule,omitempty"`
	// Recent is the outcome of the newest backup run per UTC day, newest first,
	// for the last days that had a run. A day without a run has no entry.
	Recent []runtimeBackupDay `json:"recent"`
	// LastRun is the newest finished backup job in the summary window.
	LastRun *runtimeBackupRun `json:"last_run,omitempty"`
	// LastFailure is the newest failed backup job in the summary window with
	// the step it stopped in and the error it recorded: the cause an owner
	// asks for when a backup fails. Absent when no run failed in the window.
	LastFailure *runtimeBackupRun `json:"last_failure,omitempty"`
}

// runtimeBackupRun names one finished backup job. Error is the bounded,
// single-line error the job recorded; it never carries secret material, which
// no stored command, log or receipt holds.
type runtimeBackupRun struct {
	JobID      string    `json:"job_id"`
	State      string    `json:"state"`
	FinishedAt time.Time `json:"finished_at"`
	Step       string    `json:"step,omitempty"`
	Error      string    `json:"error,omitempty"`
}

type runtimeBackups struct {
	runtimeMonitoringEnvelope
	Servers []runtimeBackupServer `json:"servers"`
}

func (h inventoryHandlers) httpInternalRuntimeBackups(e *httpx.Event) error {
	return h.serveCloudRead(e, runtimeBackupsSurface, h.serveRuntimeBackups)
}

func (h inventoryHandlers) serveRuntimeBackups(e *httpx.Event, ctx context.Context, scope inventoryScope) {
	app, reader, readScope, ok := h.monitoringReader(e, ctx, runtimeBackupsSurface, scope)
	if !ok {
		return
	}
	response, err := h.runtimeBackupsFor(ctx, app, reader, readScope, scope)
	if err != nil {
		h.writeCloudReadError(e, runtimeBackupsSurface, err)
		return
	}
	_ = e.JSON(http.StatusOK, response)
}

// runtimeBackupsFor is the one backup projection the private Cloud read and the
// owner/agent read (GET /api/v1/inventory/backups) serve.
func (h inventoryHandlers) runtimeBackupsFor(ctx context.Context, app *inventoryApplication, reader controlplane.InventoryMonitoringReader, readScope controlplane.InventoryReadScope, scope inventoryScope) (runtimeBackups, error) {
	now := app.now().UTC()
	facts, err := reader.ListInventoryBackupFacts(ctx, readScope, now)
	if err != nil {
		return runtimeBackups{}, inventoryStoreError(err)
	}
	var views map[string]guardposture.View
	postureReadable := false
	if h.posture != nil {
		if read, readErr := h.posture.Views(ctx, scope.tenantID); readErr == nil {
			views, postureReadable = read, true
		}
	}
	response := runtimeBackups{runtimeMonitoringEnvelope: newRuntimeMonitoringEnvelope(now), Servers: make([]runtimeBackupServer, 0, len(facts))}
	for _, fact := range facts {
		response.Servers = append(response.Servers, h.runtimeBackupServerFrom(fact, views, postureReadable))
	}
	return response, nil
}

func (h inventoryHandlers) runtimeBackupServerFrom(fact controlplane.InventoryBackupFacts, views map[string]guardposture.View, postureReadable bool) runtimeBackupServer {
	server := runtimeBackupServer{
		ServerID: fact.ServerID, State: guardposture.RecoveryUnknown, ReasonCode: "posture_unavailable",
		OffsiteConfigured: fact.OffsiteConfigured, OffsiteStoredBytes: fact.StoredBytes, OffsiteMeasuredAt: fact.UsageMeasuredAt,
		Recent: []runtimeBackupDay{},
	}
	switch {
	case !postureReadable:
	case strings.TrimSpace(fact.WorkerID) == "":
		server.ReasonCode = "no_guard"
	default:
		view, ok := views[strings.TrimSpace(fact.WorkerID)]
		if !ok {
			view = h.posture.View(nil)
		}
		backup, drill := view.Recovery.Backup, view.Recovery.RestoreDrill
		server.State, server.ReasonCode = backup.State, backup.ReasonCode
		server.LastSnapshotAt, server.PolicyMaxAge = backup.LastSnapshotAt, backup.PolicyMaxAge
		server.RestoreDrill = &runtimeBackupDrill{State: drill.State, VerifiedAt: drill.VerifiedAt}
	}
	if fact.Schedule != nil {
		server.Schedule = &runtimeBackupSchedule{Cadence: fact.Schedule.Cadence, Enabled: fact.Schedule.Enabled, LastRunAt: fact.Schedule.LastRunAt}
	}
	for _, day := range controlplane.SummarizeBackupRuns(fact.Runs, controlplane.BackupRunWindow) {
		server.Recent = append(server.Recent, runtimeBackupDay{Date: day.Date, State: day.State})
	}
	server.LastRun, server.LastFailure = lastBackupRuns(fact.Runs)
	return server
}

// lastBackupRuns picks the newest finished run and the newest failed run.
func lastBackupRuns(runs []controlplane.BackupRunFact) (last, failure *runtimeBackupRun) {
	for _, run := range runs {
		if last == nil || run.At.After(last.FinishedAt) {
			last = backupRunFrom(run)
		}
		if run.State == "failed" && (failure == nil || run.At.After(failure.FinishedAt)) {
			failure = backupRunFrom(run)
		}
	}
	return last, failure
}

func backupRunFrom(run controlplane.BackupRunFact) *runtimeBackupRun {
	out := &runtimeBackupRun{JobID: safeLabel(run.JobID, 256), State: safeState(run.State, "unknown"), FinishedAt: run.At.UTC(), Step: safeLabel(run.Step, 128)}
	if run.State == "failed" {
		// Redacted before folding and bounding, like every other job read, so
		// no truncation can split a secret pattern out of the redactor's reach.
		out.Error = boundedSingleLine(secrets.Redact(run.Error), 512)
	}
	return out
}

// boundedSingleLine folds whitespace and control characters into single
// spaces and bounds the result, so a recorded multi-line error reads as one
// sentence in a card or a chat answer.
func boundedSingleLine(value string, limit int) string {
	value = strings.Join(strings.FieldsFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	if len(value) <= limit {
		return value
	}
	cut := value[:limit]
	for !utf8.ValidString(cut) && len(cut) > 0 {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}
