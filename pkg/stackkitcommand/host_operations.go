package stackkitcommand

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/kombifyio/homelab/pkg/api/agentpb"
)

// Guard host operations: typed, read-only inspection of a node's processes and
// containers, and bounded start/stop/restart of one unmanaged container or one
// service unit. The Guard runs them itself with fixed argv (no StackKits CLI,
// no shell), so a closed vocabulary here is the whole input surface. An action
// is one governed RIL primitive (start|stop|restart-host-container|unit) and
// carries the Gateway proof of its exact verb and object; no flag the caller
// sets stands in for it.
const (
	// HostInspectCapability marks an agent that can run the two read-only list
	// operations.
	HostInspectCapability = "stackkit.host-inspect.v1"
	// HostOperateCapability marks an agent that can also run the bounded
	// container and service actions: it runs as root with a reachable Docker
	// engine or systemd.
	HostOperateCapability = "stackkit.host-operate.v1"
	// HostOperationsSchema identifies the data document of every Guard host
	// operation result.
	HostOperationsSchema = "kombify.guard-host-operations/v1"
	// HostListDefaultLimit and HostListMaxLimit bound a list result.
	HostListDefaultLimit = 200
	HostListMaxLimit     = 1000

	HostActionStart   = "start"
	HostActionStop    = "stop"
	HostActionRestart = "restart"
)

var (
	hostContainerRefPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	hostUnitPattern         = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9:_.@-]{0,118}\.service$`)
)

// protectedHostUnits name the units of the management path and of the node
// itself: stopping one would cut the Guard off, take the network or the
// firewall down, break the clock the binding authority depends on, or stop the
// node. A name matches when it equals an entry or continues it at a "-", "@",
// "." or "_" boundary; protectedHostUnitPrefixes match as plain prefixes. No
// caller may act on them; the Guard and the control plane refuse alike.
var protectedHostUnits = []string{
	"ssh", "sshd", "docker", "containerd", "snap.docker", "snap.lxd", "podman", "k3s", "incus", "lxd", "dbus", "polkit", "openvswitch",
	"networkd", "network", "networking", "networkmanager", "wpa_supplicant", "dhcpcd", "ifup", "ifdown", "iwd", "connman",
	"nftables", "firewalld", "iptables", "netfilter-persistent", "ufw",
	"tailscale", "tailscaled", "netbird", "wg-quick", "cloudflared", "zerotier-one", "openvpn",
	"chrony", "chronyd", "ntp", "ntpd", "getty", "serial-getty", "autovt", "user", "udev", "rsyslog", "cloud-init",
	"multipathd", "rescue", "emergency", "modprobe", "kmod",
}

var protectedHostUnitPrefixes = []string{"homelab", "kombify", "stackkit", "systemd", "pve"}

// protectedHostContainerPrefixes name containers of the platform's own agents.
var protectedHostContainerPrefixes = []string{"homelab", "kombify", "stackkit"}

// IsManagedContainer reports a container that StackKits owns, from its labels
// as docker reports them. The kits label their containers in several ways
// (stackkit.layer, stackkit.service, stackkit.managed-by, the native host's
// stackkit.owner.* labels, com.kombify.stackkits.*, managed-by=kombistack), so
// any one of them marks the container as managed: a managed container changes
// only through the managed service actions and their owner lock.
func IsManagedContainer(labels map[string]string) bool {
	for key, value := range labels {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "stackkit.") || strings.HasPrefix(lower, "com.kombify.stackkits.") ||
			(lower == "managed-by" && strings.EqualFold(strings.TrimSpace(value), "kombistack")) {
			return true
		}
	}
	return false
}

// IsGuardHostOperation reports the four Guard host operations.
func IsGuardHostOperation(operation agentpb.StackKitOperation) bool {
	switch operation {
	case agentpb.StackKitOperation_STACKKIT_OPERATION_HOST_LIST_PROCESSES,
		agentpb.StackKitOperation_STACKKIT_OPERATION_HOST_LIST_CONTAINERS,
		agentpb.StackKitOperation_STACKKIT_OPERATION_HOST_CONTAINER_ACTION,
		agentpb.StackKitOperation_STACKKIT_OPERATION_HOST_SYSTEMD_ACTION:
		return true
	default:
		return false
	}
}

// IsGuardHostMutation reports the two operations that change the node.
func IsGuardHostMutation(operation agentpb.StackKitOperation) bool {
	return operation == agentpb.StackKitOperation_STACKKIT_OPERATION_HOST_CONTAINER_ACTION ||
		operation == agentpb.StackKitOperation_STACKKIT_OPERATION_HOST_SYSTEMD_ACTION
}

// ValidHostAction reports the closed action set.
func ValidHostAction(action string) bool {
	return action == HostActionStart || action == HostActionStop || action == HostActionRestart
}

// ValidHostContainerRef reports a container name or id Docker accepts. It never
// starts with a dash, so it cannot be read as a flag.
func ValidHostContainerRef(ref string) bool { return hostContainerRefPattern.MatchString(ref) }

// ValidHostUnit reports one .service unit name.
func ValidHostUnit(unit string) bool { return hostUnitPattern.MatchString(unit) }

// ProtectedHostUnit reports a unit of the management path or the node itself.
func ProtectedHostUnit(unit string) bool {
	name := strings.ToLower(strings.TrimSuffix(unit, ".service"))
	for _, prefix := range protectedHostUnitPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	for _, entry := range protectedHostUnits {
		if name == entry || (strings.HasPrefix(name, entry) && strings.ContainsRune("-@._", rune(name[len(entry)]))) {
			return true
		}
	}
	return false
}

// ProtectedHostContainer reports a container of the platform's own agents.
func ProtectedHostContainer(name string) bool {
	name = strings.ToLower(strings.TrimPrefix(name, "/"))
	for _, prefix := range protectedHostContainerPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// HostProcess is one row of the process list. The command line is never
// reported, only the executable name: arguments routinely carry secrets.
type HostProcess struct {
	PID            int     `json:"pid"`
	ParentPID      int     `json:"ppid"`
	User           string  `json:"user"`
	CPUPercent     float64 `json:"cpu_percent"`
	MemoryPercent  float64 `json:"memory_percent"`
	ElapsedSeconds int64   `json:"elapsed_seconds"`
	Unit           string  `json:"unit,omitempty"`
	Command        string  `json:"command"`
}

// HostContainer is one row of the container list. Managed is true for a
// StackKits-managed container, which no host action may touch.
type HostContainer struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Image   string `json:"image"`
	State   string `json:"state"`
	Status  string `json:"status"`
	Ports   string `json:"ports,omitempty"`
	Project string `json:"compose_project,omitempty"`
	Managed bool   `json:"managed"`
}

// HostActionReceipt is the Guard's record of one container or unit action:
// the state observed before and after it.
type HostActionReceipt struct {
	Target      string `json:"target"`
	Action      string `json:"action"`
	StateBefore string `json:"state_before"`
	StateAfter  string `json:"state_after"`
}

// HostOperationData is the data document of a successful Guard host operation.
// Exactly the member of its operation is set.
type HostOperationData struct {
	SchemaVersion string             `json:"schema_version"`
	Processes     []HostProcess      `json:"processes,omitempty"`
	Containers    []HostContainer    `json:"containers,omitempty"`
	Receipt       *HostActionReceipt `json:"receipt,omitempty"`
	// Total is the row count before the limit; Truncated reports a cut list.
	Total     int  `json:"total,omitempty"`
	Truncated bool `json:"truncated,omitempty"`
}

// validateGuardHostCommand admits the four operations. A list takes only a
// bounded row count and no proof; an action takes one validated object, one
// action and the Gateway proof of its governed primitive (the node binds the
// proof to exactly this verb and object). OwnerApproved never admits one.
func validateGuardHostCommand(command *agentpb.StackKitCommand) error {
	if len(command.CandidateSpecJson) != 0 || len(command.InventoryJson) != 0 || len(command.AdvancedCapability) != 0 ||
		strings.TrimSpace(command.SpecPath) != "" || strings.TrimSpace(command.StackkitInstanceId) != "" ||
		strings.TrimSpace(command.ServiceKey) != "" || strings.TrimSpace(command.ExpectedPlanHash) != "" ||
		strings.TrimSpace(command.WorkingDirectory) != "" || strings.TrimSpace(command.HostPlanDigest) != "" {
		return fmt.Errorf("StackKit Guard host operations take no stack inputs")
	}
	if command.HostListLimit < 0 || command.HostListLimit > HostListMaxLimit {
		return fmt.Errorf("StackKit host_list_limit must be between 0 and %d", HostListMaxLimit)
	}
	target, action := strings.TrimSpace(command.HostTarget), strings.TrimSpace(command.HostAction)
	switch command.Operation {
	case agentpb.StackKitOperation_STACKKIT_OPERATION_HOST_LIST_PROCESSES,
		agentpb.StackKitOperation_STACKKIT_OPERATION_HOST_LIST_CONTAINERS:
		if command.OwnerApproved || command.RilExecutionProof != nil {
			return fmt.Errorf("StackKit host list must not carry Owner approval or a RIL proof")
		}
		if target != "" || action != "" {
			return fmt.Errorf("StackKit host list takes no target or action")
		}
		return nil
	}
	if command.OwnerApproved || command.RilExecutionProof == nil {
		return fmt.Errorf("StackKit host action requires the Gateway proof of its governed primitive and no OwnerApproved assertion")
	}
	if command.HostListLimit != 0 {
		return fmt.Errorf("StackKit host_list_limit is only valid for a host list")
	}
	if !ValidHostAction(action) {
		return fmt.Errorf("StackKit host action must be start, stop or restart")
	}
	if command.Operation == agentpb.StackKitOperation_STACKKIT_OPERATION_HOST_CONTAINER_ACTION {
		if !ValidHostContainerRef(target) {
			return fmt.Errorf("StackKit host container target is invalid")
		}
		return nil
	}
	if !ValidHostUnit(target) {
		return fmt.Errorf("StackKit host unit target must be one .service unit name")
	}
	if ProtectedHostUnit(target) {
		return fmt.Errorf("StackKit host unit %s belongs to the management path and cannot be changed", target)
	}
	return nil
}

// guardHostCommandName is the command identity carried by the result envelope.
func guardHostCommandName(operation agentpb.StackKitOperation) string {
	switch operation {
	case agentpb.StackKitOperation_STACKKIT_OPERATION_HOST_LIST_PROCESSES:
		return "kombify guard host processes"
	case agentpb.StackKitOperation_STACKKIT_OPERATION_HOST_LIST_CONTAINERS:
		return "kombify guard host containers"
	case agentpb.StackKitOperation_STACKKIT_OPERATION_HOST_CONTAINER_ACTION:
		return "kombify guard host container-action"
	case agentpb.StackKitOperation_STACKKIT_OPERATION_HOST_SYSTEMD_ACTION:
		return "kombify guard host systemd-action"
	default:
		return ""
	}
}

// ParseHostOperationResult reads the typed data document of a Guard host
// operation. A failed or denied result carries a reason in its error document
// instead; the document of a success must match its operation exactly.
func ParseHostOperationResult(command *agentpb.StackKitCommand, result *agentpb.StackKitResult) (HostOperationData, error) {
	var data HostOperationData
	if err := ValidateCommand(command); err != nil {
		return data, err
	}
	if !IsGuardHostOperation(command.Operation) {
		return data, fmt.Errorf("Guard host operation is required")
	}
	if err := ValidateResult(result, command); err != nil {
		return data, err
	}
	if !result.Success {
		return data, fmt.Errorf("Guard host operation did not succeed")
	}
	return decodeHostOperationData(command, result)
}

// decodeHostOperationData reads and checks the data document of a successful
// result against the command that produced it.
func decodeHostOperationData(command *agentpb.StackKitCommand, result *agentpb.StackKitResult) (HostOperationData, error) {
	var data HostOperationData
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(result.CommandResultJson, &envelope); err != nil {
		return data, fmt.Errorf("decode Guard host operation result: %w", err)
	}
	if err := json.Unmarshal(envelope.Data, &data); err != nil || data.SchemaVersion != HostOperationsSchema {
		return data, fmt.Errorf("Guard host operation returned no %s document", HostOperationsSchema)
	}
	if len(data.Processes) > HostListMaxLimit || len(data.Containers) > HostListMaxLimit {
		return data, fmt.Errorf("Guard host list exceeds its row bound")
	}
	switch command.Operation {
	case agentpb.StackKitOperation_STACKKIT_OPERATION_HOST_LIST_PROCESSES:
		if data.Containers != nil || data.Receipt != nil {
			return data, fmt.Errorf("Guard host process list carries foreign members")
		}
	case agentpb.StackKitOperation_STACKKIT_OPERATION_HOST_LIST_CONTAINERS:
		if data.Processes != nil || data.Receipt != nil {
			return data, fmt.Errorf("Guard host container list carries foreign members")
		}
	default:
		if data.Processes != nil || data.Containers != nil || data.Receipt == nil ||
			data.Receipt.Target != strings.TrimSpace(command.HostTarget) || data.Receipt.Action != strings.TrimSpace(command.HostAction) {
			return data, fmt.Errorf("Guard host action receipt does not match the dispatched action")
		}
	}
	return data, nil
}
