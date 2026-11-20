package layoutadvice

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/kombifyio/homelab/internal/stackkitrelease"
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/core"
	"github.com/kombifyio/homelab/pkg/layout"
	"github.com/kombifyio/homelab/pkg/specv2"
	"github.com/kombifyio/homelab/pkg/unifier"
)

// Targets a recommendation names: where its selection is carried out.
const (
	// TargetExtend adds the selection to a kit deployment of the homelab: the
	// one the owner started from or another one. An extend run carries it
	// out; for an add-server change it is the join run of that deployment.
	TargetExtend = "extend"
	// TargetFound founds a new kit deployment on a connected bare server. It
	// is advice only: a found run does not accept a layout recommendation,
	// so no record is proposed for it.
	TargetFound = "found"
	// TargetJoin adds a connected bare server to a deployment as a new node,
	// where the pinned release declares multi-node join for its kit. It is
	// advice only, like TargetFound: the join run that binds the server
	// projects and checks the new node itself.
	TargetJoin = "join"
)

// MultiNodeJoin is the pinned-release gate for a server joining a deployment
// of a kit. A nil gate offers no join.
type MultiNodeJoin interface {
	RequireMultiNodeJoin(kit string) error
}

// Notes that explain the homelab-wide choice. Their English text is the
// fallback for a client without a message for the code.
const (
	reasonHomelabConsidered      = "homelab_considered"
	reasonExtendsOtherDeployment = "extends_other_deployment"
	reasonFoundOnBareServer      = "found_on_bare_server"
	reasonFoundNotChecked        = "found_not_checked"
	reasonJoinsBareServer        = "joins_bare_server"
	reasonJoinNotChecked         = "join_not_checked"
	reasonChosenDeployment       = "goes_to_chosen_deployment"
)

// Target is where an accepted recommendation is carried out.
type Target struct {
	Action     string `json:"action"`
	StackID    string `json:"stack_id,omitempty"`
	StackName  string `json:"stack_name,omitempty"`
	ServerID   string `json:"server_id,omitempty"`
	ServerName string `json:"server_name,omitempty"`
}

// deployment is one owned kit deployment with a canonical StackSpec and the
// servers assigned to it.
type deployment struct {
	stack   *controlplane.Stack
	spec    map[string]any
	servers []controlplane.ServerRuntime
}

// homelab is every server the owner has in the tenant: the deployment the
// owner started from first, the owner's other deployments sorted by id, and
// connected bare servers that no deployment uses yet.
type homelab struct {
	deployments []*deployment
	bare        []controlplane.ServerRuntime
	workers     map[string]core.Worker
}

func (h homelab) origin() *deployment { return h.deployments[0] }

func (h homelab) deployment(stackID string) *deployment {
	for _, d := range h.deployments {
		if d.stack.ID == stackID {
			return d
		}
	}
	return nil
}

// candidate is one place the selection can be carried out, with its layout.
type candidate struct {
	target      Target
	deployment  *deployment // nil when the target is a bare server
	projection  *specv2.Projection
	computation unifier.LayoutComputation
	// pinned is set when the owner chose the server of a use case.
	pinned bool
}

func (c candidate) feasible() bool {
	return c.computation.Result.Outcome == layout.OutcomeFeasible && len(c.computation.Result.Options) > 0
}

// places reports whether the layout places anything new.
func (c candidate) places() bool {
	return len(c.computation.Result.Options) > 0 && len(c.computation.Result.Options[0].Assignments) > 0
}

// projectionFor returns the selection projected onto one deployment.
type projectionFor func(*deployment) (*specv2.Projection, error)

// projections projects the selection once per deployment; known holds
// projections the caller already made (a wizard run's own).
func (s *Service) projections(ctx context.Context, selection Selection, known map[string]*specv2.Projection) projectionFor {
	cache := map[string]*specv2.Projection{}
	for id, projection := range known {
		cache[id] = projection
	}
	return func(d *deployment) (*specv2.Projection, error) {
		if projection, ok := cache[d.stack.ID]; ok {
			return projection, nil
		}
		if s.Projector == nil {
			return nil, ErrUnavailable
		}
		projection, err := s.project(ctx, d.stack, d.spec, selection)
		if err != nil {
			return nil, err
		}
		cache[d.stack.ID] = projection
		return projection, nil
	}
}

// loadHomelab reads the owner's servers and deployments of one tenant. Every
// read is tenant-scoped and every row is checked against the tenant and the
// owner again, so the planner never sees another tenant's or owner's server.
// An add-server change stays inside its deployment (wide is false).
func (s *Service) loadHomelab(ctx context.Context, scope Scope, origin *controlplane.Stack, originSpec map[string]any, wide bool) (homelab, error) {
	servers, err := s.Servers.ListServerRuntimesByTenant(ctx, scope.TenantID, "")
	if err != nil {
		return homelab{}, fmt.Errorf("layoutadvice: list servers: %w", err)
	}
	workers, err := s.Workers(ctx, scope.TenantID, scope.OwnerID)
	if err != nil {
		return homelab{}, fmt.Errorf("layoutadvice: read worker inventory: %w", err)
	}
	out := homelab{deployments: []*deployment{{stack: origin, spec: originSpec}}, workers: map[string]core.Worker{}}
	for _, worker := range workers {
		out.workers[worker.ID] = worker
	}
	if wide {
		stacks, err := s.Stacks.ListStacksByTenant(ctx, scope.TenantID)
		if err != nil {
			return homelab{}, fmt.Errorf("layoutadvice: list deployments: %w", err)
		}
		var others []*deployment
		for i := range stacks {
			stack := &stacks[i]
			if stack.ID == origin.ID || stack.TenantID != scope.TenantID || stack.OwnerSubjectID != scope.OwnerID || stack.DeletedAt != nil {
				continue
			}
			spec, _ := stack.Config[stackSpecV2Key].(map[string]any)
			if spec == nil || specv2.RequireCanonicalV2(spec) != nil {
				continue // nothing to extend before its first validated setup
			}
			others = append(others, &deployment{stack: stack, spec: spec})
		}
		sort.Slice(others, func(i, j int) bool { return others[i].stack.ID < others[j].stack.ID })
		out.deployments = append(out.deployments, others...)
	}
	for _, server := range servers {
		if server.TenantID != scope.TenantID || server.OwnerSubjectID != scope.OwnerID ||
			server.DesiredState == "decommissioned" || server.DecommissionedAt != nil {
			continue
		}
		stackID := strings.TrimSpace(server.StackID)
		if d := out.deployment(stackID); d != nil {
			d.servers = append(d.servers, server)
			continue
		}
		if wide && stackID == "" && strings.TrimSpace(server.WorkerID) != "" {
			out.bare = append(out.bare, server)
		}
	}
	sort.Slice(out.bare, func(i, j int) bool { return out.bare[i].ID < out.bare[j].ID })
	return out, nil
}

// plan chooses where the selection goes. The whole homelab is planned first
// and the deployment that receives most of the selection is preferred (ties
// keep the deployment the owner started from). The selection is then planned
// on one deployment's servers alone, because one run carries out one
// deployment's change: the preferred one, the origin, then the others, and
// the first that places it wins. When no deployment has room, a connected
// bare server can join one of them as a node, in the same order, where the
// pinned release allows it; otherwise it can found a new deployment.
func (s *Service) plan(ctx context.Context, scope Scope, hl homelab, selection Selection, catalog stackkitrelease.UseCaseCatalog, project projectionFor) (candidate, error) {
	pinned, err := pinnedDeployment(hl, selection)
	if err != nil {
		return candidate{}, err
	}
	if pinned != nil {
		// The owner chose where: the planner re-solves on that deployment
		// with the choice fixed and says honestly when it does not fit.
		chosen, err := s.deploymentCandidate(ctx, scope, hl, pinned, selection, catalog, project)
		chosen.pinned = true
		return chosen, err
	}
	origin := hl.origin()
	preferred, err := s.preferredDeployment(ctx, scope, hl, selection, catalog, project)
	if err != nil {
		return candidate{}, err
	}
	// The preferred deployment first, then the origin, then every other
	// deployment: an unmeasured selection is pinned to the origin's primary
	// in the homelab-wide pass, so that pass alone cannot rule out the rest.
	order := []*deployment{preferred}
	if preferred != origin {
		order = append(order, origin)
	}
	for _, d := range hl.deployments[1:] {
		if d != preferred {
			order = append(order, d)
		}
	}
	var chosen candidate
	for _, d := range order {
		next, err := s.deploymentCandidate(ctx, scope, hl, d, selection, catalog, project)
		if err != nil {
			return candidate{}, err
		}
		if d == origin {
			chosen = next // the honest answer when nothing else has room
			if next.feasible() {
				// Also when it places nothing: the origin already runs it.
				return next, nil
			}
		} else if next.feasible() && next.places() {
			return next, nil
		}
	}
	if len(hl.bare) == 0 {
		return chosen, nil
	}
	for _, d := range order {
		// The join run admits only a running deployment of a kit the pinned
		// release declares multi-node join for, and only a server it can bind.
		if strings.TrimSpace(d.stack.Status) != "running" || s.MultiNodeJoin == nil ||
			s.MultiNodeJoin.RequireMultiNodeJoin(deploymentKit(d)) != nil {
			continue
		}
		for _, server := range hl.bare {
			if !joinableBare(hl, server) {
				continue
			}
			join, err := s.joinCandidate(ctx, scope, hl, d, server, selection, catalog, project)
			if err != nil {
				return candidate{}, err
			}
			if join.feasible() && join.places() {
				return join, nil
			}
		}
	}
	found, err := s.bareCandidate(ctx, scope, hl, selection, catalog, project)
	if err != nil {
		return candidate{}, err
	}
	if found.feasible() && found.places() {
		return found, nil
	}
	return chosen, nil
}

// preferredDeployment plans the selection over the whole homelab and returns
// the deployment that receives most of it; a use case a deployment already
// runs never counts toward it. Ties keep the origin.
func (s *Service) preferredDeployment(ctx context.Context, scope Scope, hl homelab, selection Selection, catalog stackkitrelease.UseCaseCatalog, project projectionFor) (*deployment, error) {
	origin := hl.origin()
	if len(hl.deployments) < 2 {
		return origin, nil
	}
	projection, err := project(origin)
	if err != nil {
		return nil, err
	}
	input, home := s.homelabInput(scope, hl, selection, catalog, projection)
	computation, err := unifier.ComputeLayout(ctx, catalog, input, s.Planner)
	if err != nil || len(computation.Result.Options) == 0 {
		return origin, err
	}
	counts := map[string]int{}
	for _, assignment := range computation.Result.Options[0].Assignments {
		if d := hl.deployment(home[assignment.NodeID]); d != nil && !runsAny(d, assignmentWorkloads(assignment.Workload, assignment.Workloads)) {
			counts[d.stack.ID]++
		}
	}
	preferred := origin
	for _, d := range hl.deployments[1:] {
		if counts[d.stack.ID] > counts[preferred.stack.ID] {
			preferred = d
		}
	}
	return preferred, nil
}

// runsAny reports whether the deployment's spec already has one of the workloads.
func runsAny(d *deployment, workloads []string) bool {
	existing, _ := d.spec["workloads"].(map[string]any)
	for _, workload := range workloads {
		if _, ok := existing[workload]; ok {
			return true
		}
	}
	return false
}

func (s *Service) newInput(scope Scope, stackID string, selection Selection) unifier.LayoutInput {
	return unifier.LayoutInput{
		Scope: scopeKey(selection.StackID), StackID: stackID, OwnerID: scope.OwnerID,
		SpecNodeRefs: map[string]string{}, NodeRoles: map[string][]string{}, RequiresRoles: map[string][]string{},
		Goals: selection.Goals, ComputeProfile: selection.ComputeProfile, Settings: selection.Settings, Capabilities: selection.Capabilities,
	}
}

// deploymentCandidate plans the selection on one deployment's own servers.
func (s *Service) deploymentCandidate(ctx context.Context, scope Scope, hl homelab, d *deployment, selection Selection, catalog stackkitrelease.UseCaseCatalog, project projectionFor) (candidate, error) {
	projection, err := project(d)
	if err != nil {
		return candidate{}, err
	}
	input := s.newInput(scope, d.stack.ID, selection)
	input.Pins = selection.Pins
	serverOfNode, primary := addDeploymentServers(&input, hl, d, true)
	if selection.Flow == FlowAddServer && projection != nil && projection.NodeID != "" && serverOfNode[projection.NodeID] == "" {
		input.Planned = append(input.Planned, plannedLayoutNode(projection.NodeID, projection.Spec, selection.PlannedNode, scope.OwnerID))
		serverOfNode[projection.NodeID] = plannedPrefix + projection.NodeID
	}
	input.Existing = existingComponents(d.spec, catalog, selection.ComputeProfile, serverOfNode, primary)
	addRequiredRoles(&input, d.spec, projection)
	computation, err := unifier.ComputeLayout(ctx, catalog, input, s.Planner)
	if err != nil {
		return candidate{}, err
	}
	return candidate{
		target:     Target{Action: TargetExtend, StackID: d.stack.ID, StackName: d.stack.Name},
		deployment: d, projection: projection, computation: computation,
	}, nil
}

// pinnedDeployment returns the deployment that holds every server the owner
// chose, or nil without a choice. A choice names a use case of the selection
// and a server that one of the owner's deployments declares in this tenant;
// the homelab holds only such servers, so another tenant's or owner's server,
// a bare server and an unknown id are all refused alike.
func pinnedDeployment(hl homelab, selection Selection) (*deployment, error) {
	if len(selection.Pins) == 0 {
		return nil, nil
	}
	if selection.Flow != FlowAddUseCase {
		return nil, fmt.Errorf("%w: a server is chosen per use case only when use cases are added", ErrPinInvalid)
	}
	goals := make([]string, 0, len(selection.Pins))
	for goal := range selection.Pins {
		goals = append(goals, goal)
	}
	sort.Strings(goals)
	var target *deployment
	for _, goal := range goals {
		if !slices.Contains(selection.Goals, goal) {
			return nil, fmt.Errorf("%w: %s is not part of the selection", ErrPinInvalid, goal)
		}
		d := hl.deploymentOfServer(selection.Pins[goal])
		if d == nil {
			return nil, ErrPinServer
		}
		if target != nil && target != d {
			// One run carries out one deployment's change.
			return nil, fmt.Errorf("%w: the chosen servers belong to different deployments", ErrPinInvalid)
		}
		if runsAny(d, []string{goal}) {
			return nil, fmt.Errorf("%w: %s already runs in %s", ErrPinInvalid, goal, d.stack.Name)
		}
		target = d
	}
	return target, nil
}

// deploymentOfServer returns the deployment that declares the server.
func (h homelab) deploymentOfServer(serverID string) *deployment {
	for _, d := range h.deployments {
		nodes := nodeRoles(d.spec)
		for _, server := range d.servers {
			if server.ID == serverID && specNodeOfServer(d.stack.ID, server, nodes) != "" {
				return d
			}
		}
	}
	return nil
}

// serverChoices are the servers a use case can be pinned to: every server
// the owner's deployments declare, the deployment asked from first.
func serverChoices(hl homelab) []ServerChoice {
	var choices []ServerChoice
	for _, d := range hl.deployments {
		nodes := nodeRoles(d.spec)
		start := len(choices)
		for _, server := range d.servers {
			if specNodeOfServer(d.stack.ID, server, nodes) == "" {
				continue
			}
			choices = append(choices, ServerChoice{
				ID: server.ID, Name: firstNonEmpty(server.DisplayName, server.Name, server.ID),
				StackID: d.stack.ID, StackName: d.stack.Name,
				Connected: server.ConnectionState == "connected" || server.ConnectionState == "degraded",
			})
		}
		group := choices[start:]
		sort.SliceStable(group, func(i, j int) bool { return group[i].Name < group[j].Name })
	}
	return choices
}

// homelabInput is the layout input over every deployment's servers. The
// other deployments' workloads count as load only: their ids are qualified,
// so the selection is planned afresh and an add-on joins its use case only
// inside one deployment. home maps each server to its deployment.
func (s *Service) homelabInput(scope Scope, hl homelab, selection Selection, catalog stackkitrelease.UseCaseCatalog, projection *specv2.Projection) (unifier.LayoutInput, map[string]string) {
	origin := hl.origin()
	input := s.newInput(scope, origin.stack.ID, selection)
	home := map[string]string{}
	for index, d := range hl.deployments {
		serverOfNode, primary := addDeploymentServers(&input, hl, d, index == 0)
		components := existingComponents(d.spec, catalog, selection.ComputeProfile, serverOfNode, primary)
		for i := range components {
			home[components[i].NodeID] = d.stack.ID
			if index > 0 {
				components[i].ID = d.stack.ID + "/" + components[i].ID
				components[i].Workload = d.stack.ID + "/" + components[i].Workload
			}
		}
		for _, serverID := range serverOfNode {
			home[serverID] = d.stack.ID
		}
		input.Existing = append(input.Existing, components...)
	}
	addRequiredRoles(&input, origin.spec, projection)
	return input, home
}

// joinCandidate plans the selection on one deployment's servers together with
// one connected bare server that would join it as a worker node.
func (s *Service) joinCandidate(ctx context.Context, scope Scope, hl homelab, d *deployment, server controlplane.ServerRuntime, selection Selection, catalog stackkitrelease.UseCaseCatalog, project projectionFor) (candidate, error) {
	projection, err := project(d)
	if err != nil {
		return candidate{}, err
	}
	input := s.newInput(scope, d.stack.ID, selection)
	serverOfNode, primary := addDeploymentServers(&input, hl, d, true)
	input.Workers = append(input.Workers, serverWorker(hl, server, d.stack.ID))
	input.NodeRoles[server.ID] = []string{specv2.RoleWorker}
	input.Existing = existingComponents(d.spec, catalog, selection.ComputeProfile, serverOfNode, primary)
	addRequiredRoles(&input, d.spec, projection)
	computation, err := unifier.ComputeLayout(ctx, catalog, input, s.Planner)
	if err != nil {
		return candidate{}, err
	}
	return candidate{
		target: Target{Action: TargetJoin, StackID: d.stack.ID, StackName: d.stack.Name,
			ServerID: server.ID, ServerName: firstNonEmpty(server.DisplayName, server.Name, server.ID)},
		computation: computation,
	}, nil
}

// joinableBare reports whether a bare server can join a deployment: unleased,
// in the owner's worker inventory and connected through Guard.
func joinableBare(hl homelab, server controlplane.ServerRuntime) bool {
	if _, known := hl.workers[server.WorkerID]; !known || strings.TrimSpace(server.LeaseID) != "" {
		return false
	}
	return server.ConnectionState == "connected" || server.ConnectionState == "degraded"
}

// deploymentKit is the kit a deployment's StackSpec names.
func deploymentKit(d *deployment) string {
	kit, _ := d.spec["kit"].(map[string]any)
	slug, _ := kit["slug"].(string)
	return strings.TrimSpace(slug)
}

// bareCandidate plans the selection over the connected bare servers and then
// on the one that receives most of it alone, as the controller of a new
// deployment.
func (s *Service) bareCandidate(ctx context.Context, scope Scope, hl homelab, selection Selection, catalog stackkitrelease.UseCaseCatalog, project projectionFor) (candidate, error) {
	projection, err := project(hl.origin())
	if err != nil {
		return candidate{}, err
	}
	bareInput := func(servers []controlplane.ServerRuntime) unifier.LayoutInput {
		input := s.newInput(scope, "", selection)
		for _, server := range servers {
			input.Workers = append(input.Workers, serverWorker(hl, server, ""))
			input.NodeRoles[server.ID] = []string{specv2.RoleController}
		}
		if len(servers) == 1 {
			input.PrimaryServerID = servers[0].ID
		}
		addRequiredRoles(&input, hl.origin().spec, projection)
		return input
	}
	computation, err := unifier.ComputeLayout(ctx, catalog, bareInput(hl.bare), s.Planner)
	if err != nil || len(computation.Result.Options) == 0 {
		return candidate{computation: computation}, err
	}
	counts := map[string]int{}
	for _, assignment := range computation.Result.Options[0].Assignments {
		counts[assignment.NodeID]++
	}
	best := hl.bare[0]
	for _, server := range hl.bare[1:] {
		if counts[server.ID] > counts[best.ID] {
			best = server
		}
	}
	if computation, err = unifier.ComputeLayout(ctx, catalog, bareInput([]controlplane.ServerRuntime{best}), s.Planner); err != nil {
		return candidate{}, err
	}
	return candidate{
		target:      Target{Action: TargetFound, ServerID: best.ID, ServerName: firstNonEmpty(best.DisplayName, best.Name, best.ID)},
		computation: computation,
	}, nil
}

// addDeploymentServers adds the servers a deployment declares to the input
// and returns its spec node to server map and its primary spec node. Only
// the planned deployment names the primary server.
func addDeploymentServers(input *unifier.LayoutInput, hl homelab, d *deployment, planned bool) (map[string]string, string) {
	specNodes := nodeRoles(d.spec)
	primary := primarySpecNode(d.spec)
	serverOfNode := map[string]string{}
	for _, server := range d.servers {
		nodeRef := specNodeOfServer(d.stack.ID, server, specNodes)
		if nodeRef == "" {
			continue // a server of the deployment that its spec does not declare
		}
		input.Workers = append(input.Workers, serverWorker(hl, server, d.stack.ID))
		input.SpecNodeRefs[server.ID] = nodeRef
		input.NodeRoles[server.ID] = specNodes[nodeRef]
		serverOfNode[nodeRef] = server.ID
		if planned && nodeRef == primary {
			input.PrimaryServerID = server.ID
		}
	}
	return serverOfNode, primary
}

// serverWorker is the registry worker of a canonical server, offline unless
// Guard reports the server connected.
func serverWorker(hl homelab, server controlplane.ServerRuntime, stackID string) core.Worker {
	worker, ok := hl.workers[server.WorkerID]
	if !ok {
		worker = core.Worker{ID: server.WorkerID, Name: firstNonEmpty(server.DisplayName, server.Name), Status: "offline"}
	}
	worker.ServerID, worker.StackID = server.ID, stackID
	worker.Name = firstNonEmpty(server.DisplayName, server.Name, worker.Name)
	if server.ConnectionState != "connected" && server.ConnectionState != "degraded" {
		worker.Status = "offline"
	}
	return worker
}

func addRequiredRoles(input *unifier.LayoutInput, spec map[string]any, projection *specv2.Projection) {
	if projection == nil {
		return
	}
	for workload, roles := range authoredRequiredRoles(spec, projection.Spec) {
		input.RequiresRoles[workload] = roles
	}
}

// homelabNotes say what the homelab-wide choice looked at and where the
// selection goes when that is not the deployment the owner started from.
func homelabNotes(hl homelab, chosen candidate) []layout.Reason {
	var notes []layout.Reason
	if (len(hl.deployments) > 1 || len(hl.bare) > 0) && !chosen.pinned {
		servers := len(hl.bare)
		for _, d := range hl.deployments {
			servers += len(d.servers)
		}
		params := map[string]string{"servers": strconv.Itoa(servers), "deployments": strconv.Itoa(len(hl.deployments)), "bare": strconv.Itoa(len(hl.bare))}
		notes = append(notes, layout.Reason{Code: reasonHomelabConsidered, Params: params,
			Text: "All servers of your homelab were considered, including those of your other StackKit deployments and free servers."})
	}
	origin := hl.origin().stack
	switch {
	case chosen.pinned && chosen.target.StackID != origin.ID:
		notes = append(notes, layout.Reason{
			Code:   reasonChosenDeployment,
			Params: map[string]string{"deployment": chosen.target.StackName, "origin": origin.Name},
			Text:   "These apps go to " + chosen.target.StackName + " instead of " + origin.Name + ": the server you chose belongs to it.",
		})
	case chosen.target.Action == TargetExtend && chosen.target.StackID != origin.ID:
		notes = append(notes, layout.Reason{
			Code:   reasonExtendsOtherDeployment,
			Params: map[string]string{"deployment": chosen.target.StackName, "origin": origin.Name},
			Text:   "These apps go to " + chosen.target.StackName + " instead of " + origin.Name + ": its servers are the better place for them.",
		})
	case chosen.target.Action == TargetJoin:
		notes = append(notes, layout.Reason{
			Code:   reasonJoinsBareServer,
			Params: map[string]string{"server": chosen.target.ServerName, "deployment": chosen.target.StackName},
			Text:   "None of your StackKit deployments has room for these apps on its own servers. " + chosen.target.ServerName + " is connected and free, so it can join " + chosen.target.StackName + " and run them there.",
		})
	case chosen.target.Action == TargetFound:
		notes = append(notes, layout.Reason{
			Code:   reasonFoundOnBareServer,
			Params: map[string]string{"server": chosen.target.ServerName},
			Text:   "None of your StackKit deployments has room for these apps. " + chosen.target.ServerName + " is connected and free, so it can run them as a new deployment.",
		})
	}
	return notes
}
