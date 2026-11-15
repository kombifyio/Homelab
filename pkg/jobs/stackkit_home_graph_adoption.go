package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kombifyio/techstack/internal/stackkitrelease"
	"github.com/kombifyio/techstack/pkg/api/agentpb"
	"github.com/kombifyio/techstack/pkg/controlplane"
	"github.com/kombifyio/techstack/pkg/stackkitcommand"
	"google.golang.org/protobuf/proto"
	"time"
)

const (
	HomeGraphAdoptionResultField          = "home_graph_adoption_result"
	HomeGraphAdoptionSpaceResultField     = "home_graph_space_id"
	HomeGraphAdoptionOwnerResultField     = "home_graph_owner_subject"
	HomeGraphAdoptionErrorCodeResultField = "home_graph_adoption_error_code"
)

func ApplicationHomeGraphAdoptionReleaseAvailable(ctx context.Context, tenantID, stackID string) error {
	release, err := TargetStackKitReleaseForStack(ctx, tenantID, stackID)
	if err != nil {
		return err
	}
	if release == nil {
		return fmt.Errorf("published Adoption import release unavailable")
	}
	catalog, err := release.AdvancedOperations()
	if err != nil {
		return err
	}
	return catalog.Dispatchable(stackkitcommand.HomeGraphAdoptionOperationName)
}

// ApplicationHomeGraphAdoptionRequestForStack binds the immutable confirmed request to the original connector.
func ApplicationHomeGraphAdoptionRequestForStack(ctx context.Context, original *controlplane.Job, service *controlplane.ServiceRuntime, spaceID string, revision int64, document json.RawMessage, now time.Time) (StackKitLifecycleRequest, error) {
	req, err := ApplicationConnectorRefreshRequestForStack(ctx, original, service)
	if err != nil {
		return StackKitLifecycleRequest{}, err
	}
	sum := sha256.Sum256(document)
	key := fmt.Sprintf("home-graph-adoption:%s:%s:%d:%x", original.ID, spaceID, revision, sum)
	request, _, err := stackkitcommand.DecodeHomeGraphAdoptionRequest(document)
	if err != nil {
		return StackKitLifecycleRequest{}, err
	}
	// Observations must refresh even when Cloud's revision did not change.
	// A confirmed apply keeps its stable identity to prevent duplicate writes.
	if request.Mode == "inventory" {
		key += fmt.Sprintf(":%d", now.UnixNano())
	}
	fingerprint := sha256.Sum256([]byte(key))
	req.Operation, req.OwnerApproved = StackKitLifecycleApplicationHomeGraphAdopt, false
	req.Connector.HomeGraphSpaceID = spaceID
	req.Connector.HomeGraphAdoption = document
	req.DurableJobID = StackKitServiceActionJobID(req.TenantID, req.OwnerID, key)
	req.ServiceActionDigest = hex.EncodeToString(fingerprint[:])
	return NormalizeStackKitLifecycleRequest(req)
}

func homeGraphAdoptionCommand(id string, req StackKitLifecycleRequest, release stackkitrelease.Release, graph []byte) (*agentpb.StackKitCommand, error) {
	c, err := stackKitLifecycleCommand(id, req, release)
	if err != nil {
		return nil, err
	}
	c.ApplicationOperationId = connectorOperationID(req)
	if c.ApplicationConnectorRequestJson, err = json.Marshal(req.Connector.Request); err != nil {
		return nil, err
	}
	c.HomeGraphAdoptionRequest = graph
	return c, stackkitcommand.ValidateCommand(c)
}

// runApplicationHomeGraphAdoptionJob refuses a changed graph before dispatch and retains the result for Cloud.
func runApplicationHomeGraphAdoptionJob(ctx context.Context, cfg StackKitLifecycleConfig, job *Job, q *Queue, req StackKitLifecycleRequest, release stackkitrelease.Release) error {
	ctx = contextWithJobAuthorization(ctx, job)
	if cfg.HomeGraph == nil {
		return NewConfigError(fmt.Errorf("home graph store unavailable"))
	}
	if req.Connector == nil || req.Connector.ReleasePin == nil {
		return NewPermanentError(fmt.Errorf("adoption import requires the original connector binding"))
	}
	spaceID := req.Connector.HomeGraphSpaceID
	fail := func(code string) {
		job.replaceResult(map[string]interface{}{HomeGraphAdoptionSpaceResultField: spaceID, HomeGraphAdoptionOwnerResultField: req.OwnerID, HomeGraphAdoptionErrorCodeResultField: code})
	}
	request, header, err := stackkitcommand.DecodeHomeGraphAdoptionRequest(req.Connector.HomeGraphAdoption)
	if err != nil {
		return NewPermanentError(err)
	}
	state, err := cfg.HomeGraph.GetHomeGraph(ctx, req.TenantID, req.OwnerID, spaceID)
	if errors.Is(err, controlplane.ErrNotFound) {
		fail("home_graph_request_invalid")
		return NewPermanentError(fmt.Errorf("no home graph is stored for the space"))
	}
	if err != nil {
		return NewResourceError(err)
	}
	if state.GraphRevision != header.Revision || !stackkitcommand.SameHomeGraphContent(state.Graph, request.Graph) {
		fail("home_graph_adoption_stale")
		return NewPermanentError(fmt.Errorf("adoption graph changed; inventory again"))
	}
	catalog, err := release.AdvancedOperations()
	if err == nil {
		err = catalog.Dispatchable(stackkitcommand.HomeGraphAdoptionOperationName)
	}
	if err != nil {
		fail("home_graph_connector_unavailable")
		return NewConfigError(err)
	}
	if _, err = connectorHead(ctx, cfg, req); err != nil {
		fail("home_graph_connector_unavailable")
		return NewPermanentError(err)
	}
	command, err := homeGraphAdoptionCommand(fmt.Sprintf("%s-adoption-%d", job.ID, time.Now().UnixNano()), req, release, req.Connector.HomeGraphAdoption)
	if err != nil {
		fail("home_graph_request_invalid")
		return NewPermanentError(err)
	}
	response, sendErr := sendStackKitCommandBoundedForTenant(ctx, cfg.Sender, req.TenantID, req.AgentID, command)
	if sendErr != nil || response == nil {
		fail("home_graph_connector_unavailable")
		if sendErr == nil {
			sendErr = fmt.Errorf("adoption import result unavailable")
		}
		return NewPermanentError(sendErr)
	}
	var result agentpb.StackKitResult
	proto.Merge(&result, response)
	if err = stackkitcommand.ValidateResult(&result, command); err != nil {
		fail("home_graph_apply_failed")
		return NewPermanentError(err)
	}
	if !result.Success {
		code := stackkitcommand.HomeGraphAdoptionRefusal(&result)
		if code == "" {
			code = "home_graph_apply_failed"
		}
		fail(code)
		return NewPermanentError(fmt.Errorf("adoption import was refused: %s", code))
	}
	parsed, err := stackkitcommand.ParseHomeGraphAdoptionResult(command, &result)
	if err != nil {
		fail("home_graph_apply_failed")
		return NewPermanentError(err)
	}
	var document map[string]interface{}
	raw, err := json.Marshal(parsed)
	if err != nil || json.Unmarshal(raw, &document) != nil {
		return NewPermanentError(fmt.Errorf("invalid adoption result"))
	}
	job.replaceResult(map[string]interface{}{
		HomeGraphAdoptionSpaceResultField: spaceID,
		HomeGraphAdoptionOwnerResultField: req.OwnerID,
		HomeGraphAdoptionResultField:      document,
	})
	q.UpdateProgress(job.ID, 100, "Home Assistant adoption step completed")
	return nil
}
