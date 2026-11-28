package routes

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/kombifyio/homelab/internal/stackkitrelease"
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/jobs"
	"github.com/kombifyio/homelab/pkg/placement"
	"github.com/kombifyio/homelab/pkg/serviceregistry"
	"github.com/kombifyio/homelab/pkg/stackkitcommand"
)

// Move eligibility reason codes emitted in ineligible_destinations and warnings.
const (
	moveCodeUnsupportedAdapter        = "unsupported_adapter"
	moveCodeMultiAppUseCase           = "multi_app_use_case"
	moveCodeLocalHardwareBound        = "local_hardware_bound"
	moveCodeInsufficientCapacity      = "insufficient_capacity"
	moveCodeSameStackDeployment       = "same_stack_deployment"
	moveCodeDestinationNotRelocatable = "destination_not_relocatable"
	moveCodeServerNotReady            = "server_not_ready"
	moveWarnDirectTransferNetworkReq  = "direct_transfer_network_required"
	moveWarnEncryptedRelayTransport   = "encrypted_relay_transport"
	moveAdapterApplicationCompose     = "application-compose"
	moveAdapterStandaloneCompose      = "standalone-compose"
)

// registryServiceMove is the Placement Board's move of one application: the
// exact body identities of POST /api/v1/stacks/{kit_deployment_id}/relocations
// with engine native and `workloads: [workload_ref]`, plus pre-drop
// eligibility gating metadata for blocked destinations and pre-flight warnings.
type registryServiceMove struct {
	WorkloadRef            string                              `json:"workload_ref"`
	SourceAgentID          string                              `json:"source_agent_id"`
	Destinations           []registryMoveDestination           `json:"destinations"`
	IneligibleDestinations []registryMoveIneligibleDestination `json:"ineligible_destinations,omitempty"`
	Warnings               []registryMoveWarning               `json:"warnings,omitempty"`
}

// registryMoveDestination is one server of another of the owner's StackKits
// that the relocation API admits as destination.
type registryMoveDestination struct {
	KitDeploymentID string `json:"kit_deployment_id"`
	ServerID        string `json:"server_id"`
	AgentID         string `json:"agent_id"`
}

// registryMoveIneligibleDestination explains why a specific server column on
// the Placement Board cannot accept a drop of this service.
type registryMoveIneligibleDestination struct {
	KitDeploymentID string `json:"kit_deployment_id"`
	ServerID        string `json:"server_id"`
	Code            string `json:"code"`
	Reason          string `json:"reason"`
}

// registryMoveWarning is a non-blocking pre-flight notice shown before an
// operator confirms an admitted move (for example, direct TCP:8124 reachability
// when either node is behind NAT).
type registryMoveWarning struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	ServerID string `json:"server_id,omitempty"`
}

type registryMoveAgent struct {
	agentID string
	nodeRef string
}

type registryServerMoveFacts struct {
	server        registryServer
	cloud         bool
	relocatable   bool
	agent         registryMoveAgent
	hasAgent      bool
	totalRAMMiB   int
	availRAMMiB   int
	hasRAM        bool
	totalDiskGiB  float64
	availDiskGiB  float64
	hasDisk       bool
	publicIP      string
	hasRoutableIP bool
}

// registryApplicationMoveMeta extracts the runtime ref, adapter name, and
// local-hardware binding hints from a StackKits-managed application control.
func registryApplicationMoveMeta(row controlplane.ServiceRuntime, managementState string) (string, string, bool) {
	if row.Source != serviceregistry.SourceStackKitsInventory || managementState != registryManagedState ||
		row.MutationLock.Locked() || strings.TrimSpace(row.ServerID) == "" {
		return "", "", false
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
	adapter := strings.TrimSpace(identity["adapter"])
	runtimeRef := strings.TrimSpace(identity["runtime_ref"])
	switch adapter {
	case moveAdapterApplicationCompose, moveAdapterStandaloneCompose, "komodo", "coolify":
		return runtimeRef, adapter, metadataIndicatesHostHardware(row.Metadata)
	default:
		return "", "", false
	}
}

// metadataIndicatesHostHardware inspects observed service metadata for host
// device mounts, host networking, or local hardware bind mounts.
func metadataIndicatesHostHardware(metadata map[string]any) bool {
	if len(metadata) == 0 {
		return false
	}
	if strings.EqualFold(stringFromAnyMap(metadata, "network_mode"), "host") {
		return true
	}
	if hostNet, ok := metadata["host_network"].(bool); ok && hostNet {
		return true
	}
	for _, key := range []string{"host_devices", "devices"} {
		if hasNonEmptySliceOrString(metadata[key]) {
			return true
		}
	}
	for _, key := range []string{"bind_mounts", "mounts", "volumes"} {
		if sliceContainsDeviceMount(metadata[key]) {
			return true
		}
	}
	return false
}

func hasNonEmptySliceOrString(raw any) bool {
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value) != ""
	case []string:
		return len(value) > 0
	case []any:
		return len(value) > 0
	default:
		return false
	}
}

func sliceContainsDeviceMount(raw any) bool {
	switch value := raw.(type) {
	case []string:
		for _, entry := range value {
			if strings.Contains(entry, "/dev/") || strings.Contains(entry, "/run/udev") {
				return true
			}
		}
	case []any:
		for _, entry := range value {
			switch item := entry.(type) {
			case string:
				if strings.Contains(item, "/dev/") || strings.Contains(item, "/run/udev") {
					return true
				}
			case map[string]any:
				src := firstNonEmptyString(stringFromAnyMap(item, "source"), stringFromAnyMap(item, "host_path"))
				if strings.HasPrefix(src, "/dev/") || strings.HasPrefix(src, "/run/udev") {
					return true
				}
			}
		}
	}
	return false
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
func registryInstalledWorkload(stack controlplane.Stack, nodeRef, runtimeRef string) (string, map[string]any, bool) {
	spec, _ := stack.Config["stack_spec_v2"].(map[string]any)
	workloads, _ := spec["workloads"].(map[string]any)
	nodes, _ := spec["nodes"].([]any)
	match := ""
	var matchEntry map[string]any
	for workloadRef, raw := range workloads {
		if !strings.EqualFold(runtimeRef, workloadRef+"-"+nodeRef) {
			continue
		}
		entry, _ := raw.(map[string]any)
		placementMap, _ := entry["placement"].(map[string]any)
		refs, _ := placementMap["nodeRefs"].([]any)
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
			return "", nil, false
		}
		match = workloadRef
		matchEntry = entry
	}
	if match == "" || stackkitcommand.ValidateApplicationTransferSelection("", []string{match}) != nil {
		return "", nil, false
	}
	return match, matchEntry, true
}

func isNativeTransferAdapter(adapter string) bool {
	switch strings.ToLower(strings.TrimSpace(adapter)) {
	case "", moveAdapterApplicationCompose, moveAdapterStandaloneCompose:
		return true
	default:
		return false
	}
}

func unsupportedAdapterReason(serviceAdapter string, workloadEntry map[string]any) (string, bool) {
	adapter := strings.TrimSpace(serviceAdapter)
	if adapter != "" && !isNativeTransferAdapter(adapter) {
		return fmt.Sprintf("Native transfer requires the standalone-compose adapter (current adapter: %s).", adapter), true
	}
	specAdapter := firstNonEmptyString(
		stringFromAnyMap(workloadEntry, "runtimeAdapterRef"),
		stringFromAnyMap(workloadEntry, "runtime_adapter_ref"),
	)
	if specAdapter != "" && !isNativeTransferAdapter(specAdapter) {
		return fmt.Sprintf("Native transfer requires the standalone-compose adapter (configured adapter: %s).", specAdapter), true
	}
	return "", false
}

// multiAppUseCaseForWorkload returns the shared use-case name if single-app
// native transfer would be rejected because multiple workloads in this stack
// belong to the same use case.
func multiAppUseCaseForWorkload(stack controlplane.Stack, workloadRef string, workloadEntry map[string]any, catalog *stackkitrelease.UseCaseCatalog) (string, bool) {
	spec, _ := stack.Config["stack_spec_v2"].(map[string]any)
	workloads, _ := spec["workloads"].(map[string]any)
	if len(workloads) < 2 {
		return "", false
	}

	useCaseFor := func(ref string, entry map[string]any) string {
		if explicit := firstNonEmptyString(
			stringFromAnyMap(entry, "useCase"),
			stringFromAnyMap(entry, "use_case"),
			stringFromAnyMap(entry, "goal"),
		); explicit != "" {
			return explicit
		}
		if catalog != nil {
			for _, uc := range catalog.UseCases {
				if strings.EqualFold(uc.ID, ref) {
					return uc.ID
				}
				for _, setting := range uc.Settings {
					if strings.EqualFold(strings.TrimSpace(setting.WorkloadRef), ref) {
						return uc.ID
					}
				}
			}
		}
		return ""
	}

	targetUseCase := useCaseFor(workloadRef, workloadEntry)
	if targetUseCase == "" {
		return "", false
	}
	count := 0
	for ref, raw := range workloads {
		entry, _ := raw.(map[string]any)
		if strings.EqualFold(useCaseFor(ref, entry), targetUseCase) {
			count++
		}
	}
	if count > 1 {
		return targetUseCase, true
	}
	return "", false
}

func isLocalHardwareBoundWorkload(service *registryService, workloadRef string, workloadEntry map[string]any) bool {
	if service.moveHostDeviceBound {
		return true
	}
	candidates := []string{
		workloadRef,
		service.Name,
		service.ApplicationKey,
		stringFromAnyMap(workloadEntry, "alternative"),
	}
	for _, candidate := range candidates {
		switch strings.ToLower(strings.TrimSpace(candidate)) {
		case "home-assistant", "homeassistant", "smart-home", "smarthome",
			"zigbee2mqtt", "zwave-js-ui", "esphome", "frigate", "matter-server":
			return true
		}
	}
	if workloadEntry != nil {
		if hostNet, ok := workloadEntry["hostNetwork"].(bool); ok && hostNet {
			return true
		}
		if strings.EqualFold(stringFromAnyMap(workloadEntry, "networkMode"), "host") {
			return true
		}
		if hasNonEmptySliceOrString(workloadEntry["devices"]) || hasNonEmptySliceOrString(workloadEntry["hostDevices"]) {
			return true
		}
	}
	return false
}

func isCloudServer(server registryServer, stackByID map[string]registryStack) bool {
	if strings.EqualFold(strings.TrimSpace(server.EnvironmentClass), "cloud") {
		return true
	}
	stack, ok := stackByID[server.KitDeploymentID]
	if !ok {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(stack.ServerMode), "cloud") ||
		strings.TrimSpace(stack.LeaseProvider) != ""
}

func buildServerMoveFacts(
	server registryServer,
	stackByID map[string]registryStack,
	relocatable map[string]controlplane.Stack,
	workersByID map[string]controlplane.Worker,
	canonicalByID map[string]controlplane.ServerRuntime,
) registryServerMoveFacts {
	facts := registryServerMoveFacts{
		server: server,
		cloud:  isCloudServer(server, stackByID),
	}
	if _, ok := relocatable[server.KitDeploymentID]; ok {
		facts.relocatable = true
		if agent, ok := registryServerMoveAgent(server, workersByID); ok {
			facts.agent = agent
			facts.hasAgent = true
		}
	}

	var host map[string]any
	if runtime, ok := canonicalByID[server.ID]; ok && runtime.Metadata != nil {
		host, _ = runtime.Metadata["host"].(map[string]any)
	}
	worker := workersByID[server.WorkerID]

	// Resolve RAM (MiB) from canonical host telemetry first, then worker capabilities.
	var totalRAMMiB float64
	if totalBytes, ok := float64FromAny(host["memory_total_bytes"]); ok && totalBytes > 0 {
		totalRAMMiB = totalBytes / (1024 * 1024)
	} else if ramMB, ok := float64FromAny(host["ram_mb"]); ok && ramMB > 0 {
		totalRAMMiB = ramMB
	} else if worker.RAMMB > 0 {
		totalRAMMiB = float64(worker.RAMMB)
	} else if ramMB, ok := float64FromAny(worker.Capabilities["ram_mb"]); ok && ramMB > 0 {
		totalRAMMiB = ramMB
	}
	if totalRAMMiB > 0 {
		facts.hasRAM = true
		facts.totalRAMMiB = int(math.Round(totalRAMMiB))
		availRAM := totalRAMMiB
		if usedBytes, ok := float64FromAny(host["memory_used_bytes"]); ok && usedBytes >= 0 {
			usedMiB := usedBytes / (1024 * 1024)
			availRAM = math.Max(0, totalRAMMiB-usedMiB)
		} else if memPct, ok := float64FromAny(host["memory_percent"]); ok && memPct >= 0 && memPct <= 100 {
			availRAM = totalRAMMiB * (1 - memPct/100)
		} else if memPct, ok := float64FromAny(worker.Resources["memory_percent"]); ok && memPct >= 0 && memPct <= 100 {
			availRAM = totalRAMMiB * (1 - memPct/100)
		}
		facts.availRAMMiB = int(math.Round(availRAM))
	}

	// Resolve Disk (GiB) from canonical host telemetry first, then worker capabilities.
	var totalDiskGiB float64
	if totalBytes, ok := float64FromAny(host["disk_total_bytes"]); ok && totalBytes > 0 {
		totalDiskGiB = totalBytes / (1024 * 1024 * 1024)
	} else if diskGB, ok := float64FromAny(host["disk_gb"]); ok && diskGB > 0 {
		totalDiskGiB = diskGB
	} else if worker.DiskGB > 0 {
		totalDiskGiB = float64(worker.DiskGB)
	} else if diskGB, ok := float64FromAny(worker.Capabilities["disk_gb"]); ok && diskGB > 0 {
		totalDiskGiB = diskGB
	}
	if totalDiskGiB > 0 {
		facts.hasDisk = true
		facts.totalDiskGiB = totalDiskGiB
		availDisk := totalDiskGiB
		if usedBytes, ok := float64FromAny(host["disk_used_bytes"]); ok && usedBytes >= 0 {
			usedGiB := usedBytes / (1024 * 1024 * 1024)
			availDisk = math.Max(0, totalDiskGiB-usedGiB)
		} else if diskPct, ok := float64FromAny(host["disk_percent"]); ok && diskPct >= 0 && diskPct <= 100 {
			availDisk = totalDiskGiB * (1 - diskPct/100)
		} else if diskPct, ok := float64FromAny(worker.Resources["disk_percent"]); ok && diskPct >= 0 && diskPct <= 100 {
			availDisk = totalDiskGiB * (1 - diskPct/100)
		}
		facts.availDiskGiB = availDisk
	}

	facts.publicIP = firstNonEmptyString(
		stringFromAnyMap(host, "public_ip"),
		stringFromAnyMap(worker.Capabilities, "public_ip"),
		worker.IP,
	)
	facts.hasRoutableIP = jobs.IsDirectlyRoutablePublicIP(facts.publicIP)
	return facts
}

func capacityIneligibilityReason(dest registryServerMoveFacts, need placement.ServiceResourceMinimums) (string, bool) {
	var shortages []string
	if dest.hasRAM {
		if need.HostMinRAMGB > 0 && float64(dest.totalRAMMiB)/1024 < need.HostMinRAMGB {
			shortages = append(shortages, fmt.Sprintf("RAM %.1f GiB total < %.1f GiB minimum", float64(dest.totalRAMMiB)/1024, need.HostMinRAMGB))
		} else if need.RAMMiB > 0 && dest.availRAMMiB < need.RAMMiB {
			shortages = append(shortages, fmt.Sprintf("RAM ~%d MiB available < %d MiB required", dest.availRAMMiB, need.RAMMiB))
		}
	}
	if dest.hasDisk {
		if need.HostMinDiskGB > 0 && dest.totalDiskGiB < need.HostMinDiskGB {
			shortages = append(shortages, fmt.Sprintf("disk %.1f GiB total < %.1f GiB minimum", dest.totalDiskGiB, need.HostMinDiskGB))
		} else if need.DiskGiB > 0 && dest.availDiskGiB < float64(need.DiskGiB) {
			shortages = append(shortages, fmt.Sprintf("disk ~%.1f GiB free < %d GiB required", dest.availDiskGiB, need.DiskGiB))
		}
	}
	if len(shortages) == 0 {
		return "", false
	}
	return "Insufficient capacity on destination (" + strings.Join(shortages, "; ") + ").", true
}

// attachRegistryMoves gives each movable application card its native move:
// the installed workload, its source agent, admitted destinations, structured
// ineligible destinations with reasons, and pre-flight warnings.
func attachRegistryMoves(
	payload *registryPayload,
	stacks []controlplane.Stack,
	workersByID map[string]controlplane.Worker,
	canonicalServers map[string][]controlplane.ServerRuntime,
	transferRelayActive bool,
) {
	relocatable := map[string]controlplane.Stack{}
	for _, stack := range stacks {
		if registryStackRelocatable(stack) {
			relocatable[stack.ID] = stack
		}
	}
	stackByID := make(map[string]registryStack, len(payload.Stacks))
	for _, stack := range payload.Stacks {
		stackByID[stack.ID] = stack
	}
	canonicalByID := map[string]controlplane.ServerRuntime{}
	for _, list := range canonicalServers {
		for _, runtime := range list {
			canonicalByID[runtime.ID] = runtime
		}
	}

	serverFacts := make([]registryServerMoveFacts, 0, len(payload.Servers))
	factsByKey := make(map[string]registryServerMoveFacts, len(payload.Servers))
	for _, server := range payload.Servers {
		facts := buildServerMoveFacts(server, stackByID, relocatable, workersByID, canonicalByID)
		serverFacts = append(serverFacts, facts)
		factsByKey[server.KitDeploymentID+"\x00"+server.ID] = facts
	}
	sort.SliceStable(serverFacts, func(i, j int) bool {
		if serverFacts[i].server.KitDeploymentID != serverFacts[j].server.KitDeploymentID {
			return serverFacts[i].server.KitDeploymentID < serverFacts[j].server.KitDeploymentID
		}
		return serverFacts[i].server.ID < serverFacts[j].server.ID
	})

	var catalogPtr *stackkitrelease.UseCaseCatalog
	if resolved, err := stackkitrelease.ResolveUseCaseCatalog(); err == nil {
		catalogPtr = &resolved
	}

	for index := range payload.Services {
		service := &payload.Services[index]
		stack, ok := relocatable[service.KitDeploymentID]
		if !ok || service.moveRuntimeRef == "" {
			continue
		}
		sourceFacts, ok := factsByKey[service.KitDeploymentID+"\x00"+service.ServerID]
		if !ok || !sourceFacts.hasAgent {
			continue
		}
		workloadRef, workloadEntry, ok := registryInstalledWorkload(stack, sourceFacts.agent.nodeRef, service.moveRuntimeRef)
		if !ok {
			continue
		}

		adapterReason, adapterBlocked := unsupportedAdapterReason(service.moveRuntimeAdapter, workloadEntry)
		multiUseCase, isMultiApp := multiAppUseCaseForWorkload(stack, workloadRef, workloadEntry, catalogPtr)
		hardwareBound := isLocalHardwareBoundWorkload(service, workloadRef, workloadEntry)
		need := placement.EstimateServiceResourceMinimums(workloadRef, service.Type)

		move := registryServiceMove{
			WorkloadRef:   workloadRef,
			SourceAgentID: sourceFacts.agent.agentID,
			Destinations:  []registryMoveDestination{},
		}

		sourceLacksPublicIP := !sourceFacts.hasRoutableIP

		for _, dest := range serverFacts {
			if dest.server.KitDeploymentID == service.KitDeploymentID && dest.server.ID == service.ServerID {
				continue
			}
			if dest.server.KitDeploymentID == service.KitDeploymentID {
				move.IneligibleDestinations = append(move.IneligibleDestinations, registryMoveIneligibleDestination{
					KitDeploymentID: dest.server.KitDeploymentID,
					ServerID:        dest.server.ID,
					Code:            moveCodeSameStackDeployment,
					Reason:          "Already in the same StackKit deployment; native transfer moves applications between separate StackKit deployments.",
				})
				continue
			}
			if !dest.relocatable {
				move.IneligibleDestinations = append(move.IneligibleDestinations, registryMoveIneligibleDestination{
					KitDeploymentID: dest.server.KitDeploymentID,
					ServerID:        dest.server.ID,
					Code:            moveCodeDestinationNotRelocatable,
					Reason:          "Destination server is not part of an enrolled Advanced StackKit deployment.",
				})
				continue
			}
			if !dest.hasAgent || dest.agent.agentID == sourceFacts.agent.agentID {
				move.IneligibleDestinations = append(move.IneligibleDestinations, registryMoveIneligibleDestination{
					KitDeploymentID: dest.server.KitDeploymentID,
					ServerID:        dest.server.ID,
					Code:            moveCodeServerNotReady,
					Reason:          "Destination server is not rollout-ready or its Guard agent is not connected.",
				})
				continue
			}
			if adapterBlocked {
				move.IneligibleDestinations = append(move.IneligibleDestinations, registryMoveIneligibleDestination{
					KitDeploymentID: dest.server.KitDeploymentID,
					ServerID:        dest.server.ID,
					Code:            moveCodeUnsupportedAdapter,
					Reason:          adapterReason,
				})
				continue
			}
			if isMultiApp {
				move.IneligibleDestinations = append(move.IneligibleDestinations, registryMoveIneligibleDestination{
					KitDeploymentID: dest.server.KitDeploymentID,
					ServerID:        dest.server.ID,
					Code:            moveCodeMultiAppUseCase,
					Reason:          fmt.Sprintf("Belongs to the %q multi-application use case; single-application transfer is blocked until the whole use case is moved together.", multiUseCase),
				})
				continue
			}
			if hardwareBound && dest.cloud {
				move.IneligibleDestinations = append(move.IneligibleDestinations, registryMoveIneligibleDestination{
					KitDeploymentID: dest.server.KitDeploymentID,
					ServerID:        dest.server.ID,
					Code:            moveCodeLocalHardwareBound,
					Reason:          "Requires local host hardware or USB devices and cannot move to a Cloud VPS (use a local node or dedicated Home Assistant migration).",
				})
				continue
			}
			if capReason, insufficient := capacityIneligibilityReason(dest, need); insufficient {
				move.IneligibleDestinations = append(move.IneligibleDestinations, registryMoveIneligibleDestination{
					KitDeploymentID: dest.server.KitDeploymentID,
					ServerID:        dest.server.ID,
					Code:            moveCodeInsufficientCapacity,
					Reason:          capReason,
				})
				continue
			}

			move.Destinations = append(move.Destinations, registryMoveDestination{
				KitDeploymentID: dest.server.KitDeploymentID,
				ServerID:        dest.server.ID,
				AgentID:         dest.agent.agentID,
			})

			if sourceLacksPublicIP || !dest.hasRoutableIP {
				if transferRelayActive {
					move.Warnings = append(move.Warnings, registryMoveWarning{
						Code:     moveWarnEncryptedRelayTransport,
						ServerID: dest.server.ID,
						Message:  "Encrypted HTTPS ciphertext relay transport will be used because one or both nodes do not report a directly routable public IP (NAT/LAN).",
					})
				} else {
					move.Warnings = append(move.Warnings, registryMoveWarning{
						Code:     moveWarnDirectTransferNetworkReq,
						ServerID: dest.server.ID,
						Message: fmt.Sprintf(
							"Direct node-to-node TCP:%d reachability is required for native transfer. One of the nodes does not report a directly routable public IP (NAT/LAN), so ensure direct connectivity before starting.",
							stackkitcommand.ApplicationTransferPort,
						),
					})
				}
			}
		}

		if len(move.Destinations) > 0 || len(move.IneligibleDestinations) > 0 {
			service.Move = &move
		}
	}
}
