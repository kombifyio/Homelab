package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/kombifyio/homelab/internal/stackkitrelease"
	"github.com/kombifyio/homelab/pkg/api/agentpb"
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/stackkitcommand"
	"google.golang.org/protobuf/proto"
)

var homeGraphSpacePattern = regexp.MustCompile(`^spc_[a-f0-9]{24}$`)

// ApplicationHomeGraphReleaseAvailable reports whether the pinned StackKits
// release of the stack lists catalog operation application.home-graph.sync as
// dispatchable. cached reads the release admission the per-inventory reconcile
// already holds; an enqueued run admits the release afresh.
func ApplicationHomeGraphReleaseAvailable(ctx context.Context, tenantID, stackID string, cached bool) error {
	var release *stackkitrelease.Release
	var err error
	if cached {
		release, err = CachedTargetStackKitReleaseForStack(ctx, tenantID, stackID)
	} else {
		release, err = TargetStackKitReleaseForStack(ctx, tenantID, stackID)
	}
	if err != nil {
		return err
	}
	if release == nil {
		return fmt.Errorf("published home graph release unavailable")
	}
	catalog, err := release.AdvancedOperations()
	if err != nil {
		return err
	}
	return catalog.Dispatchable(stackkitcommand.HomeGraphSyncOperationName)
}

// ApplicationHomeGraphSyncRequestForStack reuses the original connector setup
// binding exactly as the connector refresh does (completed setup, canonical
// projection, current node), and turns it into one distinct sync run. The
// graph is not carried: the run reads the latest stored state. A run is
// identified by its original setup, graph revision, waiting restore items and
// the minute, so a second enqueue in the same minute replays the first.
func ApplicationHomeGraphSyncRequestForStack(ctx context.Context, original *controlplane.Job, service *controlplane.ServiceRuntime, spaceID string, revision int64, restoreDigest string, now time.Time) (StackKitLifecycleRequest, error) {
	req, err := ApplicationConnectorRefreshRequestForStack(ctx, original, service)
	if err != nil {
		return StackKitLifecycleRequest{}, err
	}
	key := fmt.Sprintf("home-graph:%s:%s:%d:%s:%d", original.ID, spaceID, revision, restoreDigest, now.Unix()/60)
	fingerprint := sha256.Sum256([]byte(key))
	req.Operation, req.OwnerApproved = StackKitLifecycleApplicationHomeGraphSync, false
	req.Connector.HomeGraphSpaceID = spaceID
	req.DurableJobID = StackKitServiceActionJobID(req.TenantID, req.OwnerID, key)
	req.ServiceActionDigest = hex.EncodeToString(fingerprint[:])
	return NormalizeStackKitLifecycleRequest(req)
}

func homeGraphSyncCommand(id string, req StackKitLifecycleRequest, release stackkitrelease.Release, document []byte) (*agentpb.StackKitCommand, error) {
	c, err := stackKitLifecycleCommand(id, req, release)
	if err != nil {
		return nil, err
	}
	c.ApplicationOperationId = connectorOperationID(req)
	if c.ApplicationConnectorRequestJson, err = json.Marshal(req.Connector.Request); err != nil {
		return nil, err
	}
	c.HomeGraphSyncRequest = document
	return c, stackkitcommand.ValidateCommand(c)
}

// runApplicationHomeGraphJob sends the latest stored home graph to the node.
// The command is idempotent (the node converges Home Assistant to the graph and
// writes nothing for an unchanged one), so a re-run after a lost
// acknowledgement or a restart is safe and needs no outcome fence: the stored
// state keeps the restore items until a run with them succeeded.
func runApplicationHomeGraphJob(ctx context.Context, cfg StackKitLifecycleConfig, job *Job, q *Queue, req StackKitLifecycleRequest, release stackkitrelease.Release) error {
	ctx = contextWithJobAuthorization(ctx, job)
	if cfg.HomeGraph == nil {
		return NewConfigError(fmt.Errorf("home graph store unavailable"))
	}
	if req.Connector == nil || req.Connector.ReleasePin == nil {
		return NewPermanentError(fmt.Errorf("home graph sync requires the original connector binding"))
	}
	spaceID := req.Connector.HomeGraphSpaceID
	state, err := cfg.HomeGraph.GetHomeGraph(ctx, req.TenantID, req.OwnerID, spaceID)
	if errors.Is(err, controlplane.ErrNotFound) {
		return NewPermanentError(fmt.Errorf("no home graph is stored for the space"))
	}
	if err != nil {
		return NewResourceError(err)
	}
	// A failure is recorded against the revision and restore items this run
	// was built from, so a newer revision or restore stays pending.
	fail := func(code string) {
		_ = cfg.HomeGraph.FailHomeGraphSync(context.WithoutCancel(ctx), req.TenantID, req.OwnerID, spaceID, state.GraphRevision, state.Restore, code)
	}
	catalog, err := release.AdvancedOperations()
	if err != nil {
		fail("home_graph_connector_unavailable")
		return NewConfigError(err)
	}
	if err = catalog.Dispatchable(stackkitcommand.HomeGraphSyncOperationName); err != nil {
		fail("home_graph_connector_unavailable")
		return NewConfigError(err)
	}
	if _, err = connectorHead(ctx, cfg, req); err != nil {
		fail("home_graph_connector_unavailable")
		return NewPermanentError(err)
	}
	var restore []stackkitcommand.HomeGraphRestoreItem
	if len(state.Restore) != 0 {
		if err = json.Unmarshal(state.Restore, &restore); err != nil {
			fail(controlplane.HomeGraphRequestInvalidCode)
			return NewPermanentError(fmt.Errorf("stored home graph restore list is invalid"))
		}
	}
	document, err := stackkitcommand.EncodeHomeGraphSyncRequest(state.Graph, restore)
	if err != nil {
		fail(controlplane.HomeGraphRequestInvalidCode)
		return NewPermanentError(err)
	}
	command, err := homeGraphSyncCommand(fmt.Sprintf("%s-sync-%d", job.ID, time.Now().UnixNano()), req, release, document)
	if err != nil {
		fail("home_graph_apply_failed")
		return NewPermanentError(err)
	}
	response, sendErr := sendStackKitCommandBoundedForTenant(ctx, cfg.Sender, req.TenantID, req.AgentID, command)
	if sendErr != nil || response == nil {
		fail("home_graph_connector_unavailable")
		if sendErr == nil {
			sendErr = fmt.Errorf("home graph result unavailable")
		}
		return NewResourceError(sendErr)
	}
	var result agentpb.StackKitResult
	proto.Merge(&result, response)
	if err = stackkitcommand.ValidateResult(&result, command); err != nil {
		fail("home_graph_apply_failed")
		return NewPermanentError(err)
	}
	if !result.Success {
		code := stackkitcommand.HomeGraphRefusal(&result)
		if code == "" {
			code = "home_graph_apply_failed"
		}
		fail(code)
		return NewPermanentError(fmt.Errorf("home graph sync was refused: %s", code))
	}
	parsed, err := stackkitcommand.ParseHomeGraphSyncResult(command, &result)
	if err != nil {
		fail("home_graph_apply_failed")
		return NewPermanentError(err)
	}
	stored, err := stackkitcommand.MarshalHomeGraphSyncResult(parsed)
	if err != nil {
		return NewPermanentError(err)
	}
	sent, _ := json.Marshal(restore)
	if err = cfg.HomeGraph.CompleteHomeGraphSync(ctx, req.TenantID, req.OwnerID, spaceID, parsed.Revision, sent, stored); err != nil {
		return NewResourceError(err)
	}
	job.replaceResult(map[string]interface{}{"home_graph_revision": parsed.Revision, "home_graph_status": parsed.Status, "home_graph_operations": parsed.Operations})
	q.UpdateProgress(job.ID, 100, "Home graph synchronized with the node")
	return nil
}

// Result fields of a Loxone import run. The proposal is the merged
// kombify.home_graph.v1 document; Cloud reads it and stores it in the
// household home graph, Homelab never treats it as the stored graph.
const (
	HomeGraphLoxoneProposalResultField  = "home_graph_loxone_proposal"
	HomeGraphLoxoneSpaceResultField     = "home_graph_space_id"
	HomeGraphLoxoneOwnerResultField     = "home_graph_owner_subject"
	HomeGraphLoxoneRevisionResultField  = "home_graph_revision"
	HomeGraphLoxoneRoomsResultField     = "loxone_rooms_added"
	HomeGraphLoxoneDevicesResultField   = "loxone_devices_added"
	HomeGraphLoxoneErrorCodeResultField = "loxone_error_code"
)

// ApplicationHomeGraphLoxoneImportReleaseAvailable reports whether the pinned
// StackKits release of the stack lists catalog operation
// application.home-graph.loxone-import as dispatchable.
func ApplicationHomeGraphLoxoneImportReleaseAvailable(ctx context.Context, tenantID, stackID string) error {
	release, err := TargetStackKitReleaseForStack(ctx, tenantID, stackID)
	if err != nil {
		return err
	}
	if release == nil {
		return fmt.Errorf("published Loxone import release unavailable")
	}
	catalog, err := release.AdvancedOperations()
	if err != nil {
		return err
	}
	return catalog.Dispatchable(stackkitcommand.HomeGraphLoxoneImportOperationName)
}

// ApplicationHomeGraphLoxoneImportRequestForStack binds one import run to the
// node of the home graph sync through the original connector setup, exactly
// as the sync does. The graph is read at run time. A run is identified by the
// setup, space, graph revision and the minute, so a second request in the
// same minute replays the first.
func ApplicationHomeGraphLoxoneImportRequestForStack(ctx context.Context, original *controlplane.Job, service *controlplane.ServiceRuntime, spaceID string, revision int64, now time.Time) (StackKitLifecycleRequest, error) {
	req, err := ApplicationConnectorRefreshRequestForStack(ctx, original, service)
	if err != nil {
		return StackKitLifecycleRequest{}, err
	}
	key := fmt.Sprintf("home-graph-loxone:%s:%s:%d:%d", original.ID, spaceID, revision, now.Unix()/60)
	fingerprint := sha256.Sum256([]byte(key))
	req.Operation, req.OwnerApproved = StackKitLifecycleApplicationHomeGraphLoxoneImport, false
	req.Connector.HomeGraphSpaceID = spaceID
	req.DurableJobID = StackKitServiceActionJobID(req.TenantID, req.OwnerID, key)
	req.ServiceActionDigest = hex.EncodeToString(fingerprint[:])
	return NormalizeStackKitLifecycleRequest(req)
}

func homeGraphLoxoneImportCommand(id string, req StackKitLifecycleRequest, release stackkitrelease.Release, graph []byte) (*agentpb.StackKitCommand, error) {
	c, err := stackKitLifecycleCommand(id, req, release)
	if err != nil {
		return nil, err
	}
	c.ApplicationOperationId = connectorOperationID(req)
	if c.ApplicationConnectorRequestJson, err = json.Marshal(req.Connector.Request); err != nil {
		return nil, err
	}
	c.HomeGraphDocument = graph
	return c, stackkitcommand.ValidateCommand(c)
}

// runApplicationHomeGraphLoxoneImportJob sends the owner's stored home graph
// to the node, which merges the Miniserver's rooms and controls into it
// read-only, and keeps the validated merged graph in the job result for Cloud
// to store in the household home graph. Nothing is written to the Miniserver
// or to the stored graph, so a re-run is safe.
func runApplicationHomeGraphLoxoneImportJob(ctx context.Context, cfg StackKitLifecycleConfig, job *Job, q *Queue, req StackKitLifecycleRequest, release stackkitrelease.Release) error {
	ctx = contextWithJobAuthorization(ctx, job)
	if cfg.HomeGraph == nil {
		return NewConfigError(fmt.Errorf("home graph store unavailable"))
	}
	if req.Connector == nil || req.Connector.ReleasePin == nil {
		return NewPermanentError(fmt.Errorf("loxone import requires the original connector binding"))
	}
	spaceID := req.Connector.HomeGraphSpaceID
	fail := func(code string) {
		job.replaceResult(map[string]interface{}{HomeGraphLoxoneSpaceResultField: spaceID, HomeGraphLoxoneOwnerResultField: req.OwnerID, HomeGraphLoxoneErrorCodeResultField: code})
	}
	state, err := cfg.HomeGraph.GetHomeGraph(ctx, req.TenantID, req.OwnerID, spaceID)
	if errors.Is(err, controlplane.ErrNotFound) {
		fail("loxone_import_request_invalid")
		return NewPermanentError(fmt.Errorf("no home graph is stored for the space"))
	}
	if err != nil {
		return NewResourceError(err)
	}
	catalog, err := release.AdvancedOperations()
	if err == nil {
		err = catalog.Dispatchable(stackkitcommand.HomeGraphLoxoneImportOperationName)
	}
	if err != nil {
		fail("loxone_import_unavailable")
		return NewConfigError(err)
	}
	if _, err = connectorHead(ctx, cfg, req); err != nil {
		fail("loxone_import_unavailable")
		return NewPermanentError(err)
	}
	command, err := homeGraphLoxoneImportCommand(fmt.Sprintf("%s-loxone-%d", job.ID, time.Now().UnixNano()), req, release, state.Graph)
	if err != nil {
		fail("loxone_import_request_invalid")
		return NewPermanentError(err)
	}
	response, sendErr := sendStackKitCommandBoundedForTenant(ctx, cfg.Sender, req.TenantID, req.AgentID, command)
	if sendErr != nil || response == nil {
		fail("loxone_import_unavailable")
		if sendErr == nil {
			sendErr = fmt.Errorf("loxone import result unavailable")
		}
		return NewResourceError(sendErr)
	}
	var result agentpb.StackKitResult
	proto.Merge(&result, response)
	if err = stackkitcommand.ValidateResult(&result, command); err != nil {
		fail("loxone_import_failed")
		return NewPermanentError(err)
	}
	if !result.Success {
		code := stackkitcommand.HomeGraphLoxoneImportRefusal(&result)
		if code == "" {
			code = "loxone_import_failed"
		}
		fail(code)
		return NewPermanentError(fmt.Errorf("loxone import was refused: %s", code))
	}
	parsed, err := stackkitcommand.ParseHomeGraphLoxoneImportResult(command, &result)
	if err != nil {
		fail("loxone_import_failed")
		return NewPermanentError(err)
	}
	var proposal map[string]interface{}
	if err = json.Unmarshal(parsed.Graph, &proposal); err != nil {
		fail("loxone_import_failed")
		return NewPermanentError(err)
	}
	job.replaceResult(map[string]interface{}{
		HomeGraphLoxoneSpaceResultField:    spaceID,
		HomeGraphLoxoneOwnerResultField:    req.OwnerID,
		HomeGraphLoxoneRevisionResultField: state.GraphRevision,
		HomeGraphLoxoneRoomsResultField:    parsed.RoomsAdded,
		HomeGraphLoxoneDevicesResultField:  parsed.DevicesAdded,
		HomeGraphLoxoneProposalResultField: proposal,
	})
	q.UpdateProgress(job.ID, 100, "Loxone structure merged into the home graph proposal")
	return nil
}
