package routes

import (
	"sort"
	"strings"

	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/jobs"
	"github.com/kombifyio/homelab/pkg/serviceregistry"
	"github.com/kombifyio/homelab/pkg/stackkitcommand"
)

// registryServiceMove is the Placement Board's move of one application: the
// exact body identities of POST /api/v1/stacks/{kit_deployment_id}/relocations
// with engine native and `workloads: [workload_ref]`.
type registryServiceMove struct {
	WorkloadRef   string                    `json:"workload_ref"`
	SourceAgentID string                    `json:"source_agent_id"`
	Destinations  []registryMoveDestination `json:"destinations"`
}

// registryMoveDestination is one server of another of the owner's StackKits
// that the relocation API admits as destination.
type registryMoveDestination struct {
	KitDeploymentID string `json:"kit_deployment_id"`
	ServerID        string `json:"server_id"`
	AgentID         string `json:"agent_id"`
}

type registryMoveAgent struct {
	agentID string
	nodeRef string
}

// registryApplicationRuntimeRef returns the runtime ref of a StackKits-managed
// application control. StackKits declares one control per selected
// application workload with runtimeRef `<workloadRef>-<nodeRef>` and an
// application adapter (internal/servicecontrol applicationServiceControls);
// only such a row can be moved by the native application transfer.
func registryApplicationRuntimeRef(row controlplane.ServiceRuntime, managementState string) string {
	if row.Source != serviceregistry.SourceStackKitsInventory || managementState != registryManagedState ||
		row.MutationLock.Locked() || strings.TrimSpace(row.ServerID) == "" {
		return ""
	}
	identity := map[string]string{}
	switch raw := row.Metadata["runtime_identity"].(type) {
	case map[string]string:
		identity = raw
	case map[string]any:
		for key, value := range raw {
			if text, ok := value.(string); ok {
				identity[key] = text
			}
		}
	}
	switch strings.TrimSpace(identity["adapter"]) {
	case "application-compose", "komodo", "coolify":
		return strings.TrimSpace(identity["runtime_ref"])
	default:
		return ""
	}
}

// registryStackRelocatable mirrors the relocation API's static admission of a
// source or destination StackKit: an Advanced deployment enrolled with its
// StackKits instance that is not being removed.
func registryStackRelocatable(stack controlplane.Stack) bool {
	if stack.DeletedAt != nil || strings.TrimSpace(stack.StackKitInstanceID) == "" {
		return false
	}
	if stringFromAnyMap(stack.Config, jobs.StackKitModeField) != jobs.StackKitModeAdvanced {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(stack.Status)) {
	case "destroyed", "destroying", "deleted":
		return false
	}
	return true
}

// registryServerMoveAgent is the approved StackKit lifecycle agent of a
// rollout-ready server, bound the way the relocation API binds it: the
// owner's approved worker of that StackKit, its runtime agent id, and the
// node ref its host name spells.
func registryServerMoveAgent(server registryServer, workersByID map[string]controlplane.Worker) (registryMoveAgent, bool) {
	if !server.RolloutReady || strings.TrimSpace(server.WorkerID) == "" {
		return registryMoveAgent{}, false
	}
	worker, ok := workersByID[server.WorkerID]
	if !ok || !worker.Approved || worker.StackID != server.KitDeploymentID {
		return registryMoveAgent{}, false
	}
	// The relocation handler refuses a binding without a server id, so the
	// board offers only a worker bound to exactly this server.
	if bound := strings.TrimSpace(stringFromAnyMap(worker.Capabilities, "server_id")); bound != server.ID {
		return registryMoveAgent{}, false
	}
	agent := registryMoveAgent{
		agentID: firstNonEmptyString(stringFromAnyMap(worker.Capabilities, "runtime_agent_id"), worker.ID),
		nodeRef: strings.TrimSpace(worker.Hostname),
	}
	return agent, agent.agentID != "" && agent.nodeRef != ""
}

// registryInstalledWorkload resolves the runtime ref of an application
// control to the one installed workload of the StackKit's persisted desired
// spec that StackKits placed on nodeRef. Anything other than exactly one
// match leaves the application without a move.
func registryInstalledWorkload(stack controlplane.Stack, nodeRef, runtimeRef string) (string, bool) {
	spec, _ := stack.Config["stack_spec_v2"].(map[string]any)
	workloads, _ := spec["workloads"].(map[string]any)
	nodes, _ := spec["nodes"].([]any)
	match := ""
	for workloadRef, raw := range workloads {
		if !strings.EqualFold(runtimeRef, workloadRef+"-"+nodeRef) {
			continue
		}
		entry, _ := raw.(map[string]any)
		placement, _ := entry["placement"].(map[string]any)
		refs, _ := placement["nodeRefs"].([]any)
		placed := len(refs) == 0 && len(nodes) == 1
		for _, ref := range refs {
			if text, ok := ref.(string); ok && strings.EqualFold(text, nodeRef) {
				placed = true
			}
		}
		if !placed {
			continue
		}
		if match != "" {
			return "", false
		}
		match = workloadRef
	}
	if match == "" || stackkitcommand.ValidateApplicationTransferSelection("", []string{match}) != nil {
		return "", false
	}
	return match, true
}

// attachRegistryMoves gives each movable application card its native move:
// the installed workload, its source agent and every server of the owner's
// other relocatable StackKits. A card without all three carries no move.
func attachRegistryMoves(payload *registryPayload, stacks []controlplane.Stack, workersByID map[string]controlplane.Worker) {
	relocatable := map[string]controlplane.Stack{}
	for _, stack := range stacks {
		if registryStackRelocatable(stack) {
			relocatable[stack.ID] = stack
		}
	}
	agents := map[string]registryMoveAgent{}
	var destinations []registryMoveDestination
	for _, server := range payload.Servers {
		if _, ok := relocatable[server.KitDeploymentID]; !ok {
			continue
		}
		agent, ok := registryServerMoveAgent(server, workersByID)
		if !ok {
			continue
		}
		agents[server.KitDeploymentID+"\x00"+server.ID] = agent
		destinations = append(destinations, registryMoveDestination{KitDeploymentID: server.KitDeploymentID, ServerID: server.ID, AgentID: agent.agentID})
	}
	sort.SliceStable(destinations, func(i, j int) bool {
		if destinations[i].KitDeploymentID != destinations[j].KitDeploymentID {
			return destinations[i].KitDeploymentID < destinations[j].KitDeploymentID
		}
		return destinations[i].ServerID < destinations[j].ServerID
	})
	for index := range payload.Services {
		service := &payload.Services[index]
		stack, ok := relocatable[service.KitDeploymentID]
		if !ok || service.moveRuntimeRef == "" {
			continue
		}
		source, ok := agents[service.KitDeploymentID+"\x00"+service.ServerID]
		if !ok {
			continue
		}
		workloadRef, ok := registryInstalledWorkload(stack, source.nodeRef, service.moveRuntimeRef)
		if !ok {
			continue
		}
		move := registryServiceMove{WorkloadRef: workloadRef, SourceAgentID: source.agentID, Destinations: []registryMoveDestination{}}
		for _, destination := range destinations {
			if destination.KitDeploymentID != service.KitDeploymentID && destination.AgentID != source.agentID {
				move.Destinations = append(move.Destinations, destination)
			}
		}
		if len(move.Destinations) > 0 {
			service.Move = &move
		}
	}
}
