package controlplane

import (
	"context"
	"encoding/json"
	"time"
)

// Home graph sync states (HOME-GRAPH-TARGET.md, D-2026-11-09-03).
const (
	HomeGraphStatusPending = "pending"
	HomeGraphStatusSynced  = "synced"
	HomeGraphStatusFailed  = "failed"
)

// MaxHomeGraphBytes bounds the stored graph document.
const MaxHomeGraphBytes = 512 << 10

// HomeGraphState is the latest kombify home graph of one owner and space and
// the state of its sync to the owner's Home Assistant. Graph and Restore are
// JSON documents the caller validated; Result is the validated
// stackkit.home-graph-sync-result/v1 of the last successful run. The Home
// Assistant credential is never part of it.
type HomeGraphState struct {
	TenantID       string
	OwnerSubjectID string
	SpaceID        string
	Graph          json.RawMessage
	GraphRevision  int64
	// Restore is a JSON array of the drift items the owner chose to resolve
	// with the kombify value; kept until one sync ran with them.
	Restore        json.RawMessage
	Status         string
	SyncedRevision int64 // -1 until a sync succeeded
	Result         json.RawMessage
	ErrorCode      string
	FailureCount   int // consecutive failed runs; reset by a new revision, restore or success
	SyncedAt       *time.Time
	LastAttemptAt  *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// HomeGraphPutOutcome is what PutHomeGraph did.
type HomeGraphPutOutcome string

const (
	// HomeGraphPutChanged: a higher revision replaced the graph, or restore items were added.
	HomeGraphPutChanged HomeGraphPutOutcome = "changed"
	// HomeGraphPutUnchanged: the same revision again with nothing new.
	HomeGraphPutUnchanged HomeGraphPutOutcome = "unchanged"
	// HomeGraphPutStale: a lower revision than the stored one, ignored.
	HomeGraphPutStale HomeGraphPutOutcome = "stale"
)

// HomeGraphStore keeps the latest graph per tenant, owner and space.
type HomeGraphStore interface {
	// PutHomeGraph stores the graph when its revision is not lower than the
	// stored one and merges the restore items (a JSON array). A lower revision
	// changes nothing and returns the stored state. The state is pending when
	// the revision is unsynced or a restore waits.
	//
	// Restore is cumulative and sticky: the items merge into the stored pending
	// set (dedupe by kind, id and field, newest 256 kept) and an empty list never
	// clears it. A POST at the stored revision stores nothing new unless it adds
	// restore items. The outcome says whether a sync run is due.
	PutHomeGraph(ctx context.Context, tenantID, ownerSubjectID, spaceID string, graph json.RawMessage, revision int64, restore json.RawMessage) (*HomeGraphState, HomeGraphPutOutcome, error)
	// GetHomeGraph returns ErrNotFound when the owner stored no graph for the space.
	GetHomeGraph(ctx context.Context, tenantID, ownerSubjectID, spaceID string) (*HomeGraphState, error)
	ListHomeGraphs(ctx context.Context, tenantID, ownerSubjectID string) ([]HomeGraphState, error)
	// MarkHomeGraphAttempt records that a sync run was enqueued.
	MarkHomeGraphAttempt(ctx context.Context, tenantID, ownerSubjectID, spaceID string) error
	// CompleteHomeGraphSync stores the validated result of a run that synced
	// revision with the sent restore items, and removes exactly those items
	// from the waiting restore list.
	CompleteHomeGraphSync(ctx context.Context, tenantID, ownerSubjectID, spaceID string, revision int64, sentRestore, result json.RawMessage) error
	// FailHomeGraphSync records a failed run of revision with the sent restore
	// items and keeps the restore list. A newer pending revision or a restore
	// item added after the run was sent keeps the state pending (only the
	// attempt time is recorded): the failure belongs to a superseded request.
	FailHomeGraphSync(ctx context.Context, tenantID, ownerSubjectID, spaceID string, revision int64, sentRestore json.RawMessage, errorCode string) error
}

// HomeGraphRequestInvalidCode is the failure code of a request the node
// refused as malformed; it is not retried until a new revision or restore.
const HomeGraphRequestInvalidCode = "home_graph_request_invalid"

// HomeGraphRetryMax caps the retry backoff of a failing sync.
const HomeGraphRetryMax = 6 * time.Hour
