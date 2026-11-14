package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	agentpb "github.com/kombifyio/techstack/pkg/api/agentpb"
	"github.com/kombifyio/techstack/pkg/secrets"
	"github.com/kombifyio/techstack/pkg/stackkitcommand"
)

// Guard host operations run in the Guard itself with fixed argv: the caller
// supplies at most one validated container name or unit name and one action of
// a closed set, never an executable, argument, environment or shell fragment.
const (
	hostOperationOutputLimit = 4 << 20
	hostContainerStopSeconds = "20"
	// Exit codes of a refused or failed host operation, like the host
	// maintenance commands: 3 refused (nothing ran), 1 failed.
	hostOperationRefusedExit = 3
)

// Stable refusal and failure codes of the Guard host operations.
const (
	hostCodeContainerManaged   = "container_managed"
	hostCodeContainerProtected = "container_protected"
	hostCodeContainerNotFound  = "container_not_found"
	hostCodeContainerAutoClean = "container_autoremove"
	hostCodeUnitNotFound       = "unit_not_found"
	hostCodeUnitInstance       = "unit_instance_unknown"
	hostCodeUnitProtected      = "unit_protected"
	hostCodeEngineUnavailable  = "engine_unavailable"
	hostCodeActionFailed       = "action_failed"
	hostCodePrecheckFailed     = "precheck_failed"
	hostCodeReadbackFailed     = "readback_failed"
	hostCodeStateNotReached    = "state_not_reached"
)

// hostOperationRefusal is a refusal before any change: the node is unchanged.
type hostOperationRefusal struct{ code, message string }

func (r *hostOperationRefusal) Error() string { return r.code + ": " + r.message }

// hostOperationCapabilities reports what this Guard can run. Reading needs only
// the process table; the actions also need root and a reachable engine or
// systemd. It is independent of the pinned StackKits release.
func hostOperationCapabilities() []string {
	if runtime.GOOS != "linux" {
		return nil
	}
	if _, err := exec.LookPath("ps"); err != nil {
		return nil
	}
	capabilities := []string{stackkitcommand.HostInspectCapability}
	if os.Geteuid() != 0 {
		return capabilities
	}
	_, dockerErr := exec.LookPath("docker")
	_, systemctlErr := exec.LookPath("systemctl")
	if dockerErr == nil || systemctlErr == nil {
		capabilities = append(capabilities, stackkitcommand.HostOperateCapability)
	}
	return capabilities
}

// executeHostOperation runs one Guard host operation and shapes its result as
// a stackkit.command-result/v1 document.
func executeHostOperation(ctx context.Context, command *agentpb.StackKitCommand, started time.Time) *agentpb.StackKitResult {
	result := &agentpb.StackKitResult{
		CommandId:                  command.CommandId,
		CommandResultSchemaVersion: stackKitCommandResultVersion,
		EventsSchemaVersion:        stackKitRolloutEventVersion,
		StartedAtUnix:              started.Unix(),
		Release:                    cloneStackKitReleasePin(command.Release),
	}
	timeout := 2 * time.Minute
	if command.TimeoutSeconds > 0 {
		timeout = time.Duration(command.TimeoutSeconds) * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var data stackkitcommand.HostOperationData
	var err error
	switch command.Operation {
	case agentpb.StackKitOperation_STACKKIT_OPERATION_HOST_LIST_PROCESSES:
		data, err = listHostProcesses(runCtx, command)
	case agentpb.StackKitOperation_STACKKIT_OPERATION_HOST_LIST_CONTAINERS:
		data, err = listHostContainers(runCtx, command)
	case agentpb.StackKitOperation_STACKKIT_OPERATION_HOST_CONTAINER_ACTION:
		data, err = runContainerAction(runCtx, command)
	case agentpb.StackKitOperation_STACKKIT_OPERATION_HOST_SYSTEMD_ACTION:
		data, err = runSystemdAction(runCtx, command)
	default:
		err = fmt.Errorf("unsupported Guard host operation %s", command.Operation.String())
	}
	result.FinishedAtUnix = time.Now().UTC().Unix()
	return finishHostOperationResult(result, command, data, err)
}

func finishHostOperationResult(result *agentpb.StackKitResult, command *agentpb.StackKitCommand, data stackkitcommand.HostOperationData, err error) *agentpb.StackKitResult {
	envelope := struct {
		SchemaVersion string `json:"schemaVersion"`
		Command       string `json:"command"`
		Status        string `json:"status"`
		Data          any    `json:"data"`
	}{SchemaVersion: stackKitCommandResultVersion, Command: stackkitcommand.ResultCommandName(command.Operation), Status: "success"}
	data.SchemaVersion = stackkitcommand.HostOperationsSchema
	envelope.Data = data
	result.Success = err == nil
	var refusal *hostOperationRefusal
	switch {
	case err == nil:
	case errors.As(err, &refusal):
		envelope.Status, result.ExitCode = "denied", hostOperationRefusedExit
		envelope.Data = hostOperationProblem{SchemaVersion: stackkitcommand.HostOperationsSchema, Refusal: &hostOperationCode{Code: refusal.code, Message: refusal.message}}
		result.Stderr = secrets.Redact(refusal.Error())
	default:
		envelope.Status, result.ExitCode = "failed", 1
		code := hostCodePrecheckFailed
		var failure *hostOperationFailure
		if errors.As(err, &failure) {
			code = failure.code
		}
		envelope.Data = hostOperationProblem{SchemaVersion: stackkitcommand.HostOperationsSchema, Failure: &hostOperationCode{Code: code, Message: secrets.Redact(err.Error())}}
		result.Stderr = secrets.Redact(err.Error())
	}
	encoded, encodeErr := json.Marshal(envelope)
	if encodeErr != nil {
		result.Success, result.ExitCode = false, 1
		result.Stderr = secrets.Redact("encode Guard host operation result: " + encodeErr.Error())
		return result
	}
	result.CommandResultJson = encoded
	return result
}

type hostOperationCode struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type hostOperationProblem struct {
	SchemaVersion string             `json:"schema_version"`
	Refusal       *hostOperationCode `json:"refusal,omitempty"`
	Failure       *hostOperationCode `json:"failure,omitempty"`
}

// hostOperationFailure is a failure after the Guard started work.
type hostOperationFailure struct {
	code string
	err  error
}

func (f *hostOperationFailure) Error() string { return f.err.Error() }
func (f *hostOperationFailure) Unwrap() error { return f.err }

// runHostTool runs one executable with fixed argv and a minimal environment. The
// locale is pinned so numbers and states parse the same on every node.
func runHostTool(ctx context.Context, name string, args ...string) ([]byte, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return nil, &hostOperationFailure{code: hostCodeEngineUnavailable, err: fmt.Errorf("%s is not available on this node", name)}
	}
	process := exec.CommandContext(ctx, path, args...) // #nosec G204 -- fixed executable names and validated, flag-free arguments.
	process.Env = []string{"LC_ALL=C", "LANG=C", "PATH=" + os.Getenv("PATH")}
	var stdout, stderr bytes.Buffer
	process.Stdout = &limitedWriter{buffer: &stdout, remaining: hostOperationOutputLimit}
	process.Stderr = &limitedWriter{buffer: &stderr, remaining: 64 << 10}
	if err := process.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return stdout.Bytes(), fmt.Errorf("%s %s failed: %s", name, args[0], message)
	}
	return stdout.Bytes(), nil
}

type limitedWriter struct {
	buffer    *bytes.Buffer
	remaining int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.remaining > 0 {
		keep := p
		if len(keep) > w.remaining {
			keep = keep[:w.remaining]
		}
		w.buffer.Write(keep)
		w.remaining -= len(keep)
	}
	return len(p), nil
}

func hostListLimit(command *agentpb.StackKitCommand) int {
	if command.HostListLimit > 0 {
		return int(command.HostListLimit)
	}
	return stackkitcommand.HostListDefaultLimit
}

// listHostProcesses reads the process table, busiest first. Only the
// executable name is reported, never the command line.
func listHostProcesses(ctx context.Context, command *agentpb.StackKitCommand) (stackkitcommand.HostOperationData, error) {
	output, err := runHostTool(ctx, "ps", "-eo", "pid=,ppid=,user:32=,pcpu=,pmem=,etimes=,unit:64=,comm=", "--sort=-pcpu")
	if err != nil {
		return stackkitcommand.HostOperationData{}, err
	}
	processes := parseHostProcesses(string(output))
	data := stackkitcommand.HostOperationData{Total: len(processes)}
	if limit := hostListLimit(command); len(processes) > limit {
		processes, data.Truncated = processes[:limit], true
	}
	data.Processes = processes
	return data, nil
}

func parseHostProcesses(output string) []stackkitcommand.HostProcess {
	processes := make([]stackkitcommand.HostProcess, 0, 128)
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		ppid, ppidErr := strconv.Atoi(fields[1])
		cpu, cpuErr := strconv.ParseFloat(fields[3], 64)
		memory, memoryErr := strconv.ParseFloat(fields[4], 64)
		elapsed, elapsedErr := strconv.ParseInt(fields[5], 10, 64)
		if pidErr != nil || ppidErr != nil || cpuErr != nil || memoryErr != nil || elapsedErr != nil {
			continue
		}
		unit := fields[6]
		if unit == "-" {
			unit = ""
		}
		processes = append(processes, stackkitcommand.HostProcess{
			PID: pid, ParentPID: ppid, User: fields[2], CPUPercent: cpu, MemoryPercent: memory, ElapsedSeconds: elapsed,
			Unit: unit, Command: strings.Join(fields[7:], " "),
		})
	}
	sort.SliceStable(processes, func(i, j int) bool { return processes[i].CPUPercent > processes[j].CPUPercent })
	return processes
}

const containerListFormat = "{{.ID}}\t{{.Names}}\t{{.Image}}\t{{.State}}\t{{.Status}}\t{{.Ports}}\t" +
	`{{.Label "com.docker.compose.project"}}` + "\t" + `{{.Labels}}`

// parseDockerLabels reads the `k=v,k2=v2` summary of `docker ps`. The format
// escapes nothing, so a segment that does not start like a label key continues
// the previous value.
func parseDockerLabels(raw string) map[string]string {
	labels := map[string]string{}
	last := ""
	for _, segment := range strings.Split(raw, ",") {
		if key, value, ok := strings.Cut(segment, "="); ok && dockerLabelKeyPattern.MatchString(strings.TrimSpace(key)) {
			last = strings.TrimSpace(key)
			labels[last] = value
			continue
		}
		if last != "" {
			labels[last] += "," + segment
		}
	}
	return labels
}

var dockerLabelKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// listHostContainers lists every container, running or not, and marks the
// StackKits-managed ones. Only the Compose project and that flag are reported
// from the labels; the rest of the label set stays on the host.
func listHostContainers(ctx context.Context, command *agentpb.StackKitCommand) (stackkitcommand.HostOperationData, error) {
	output, err := runHostTool(ctx, "docker", "ps", "--all", "--no-trunc", "--format", containerListFormat)
	if err != nil {
		return stackkitcommand.HostOperationData{}, err
	}
	containers := make([]stackkitcommand.HostContainer, 0, 32)
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(fields) != 8 || strings.TrimSpace(fields[0]) == "" {
			continue
		}
		containers = append(containers, stackkitcommand.HostContainer{
			ID: fields[0], Name: firstContainerName(fields[1]), Image: fields[2], State: fields[3], Status: fields[4],
			Ports: fields[5], Project: fields[6], Managed: stackkitcommand.IsManagedContainer(parseDockerLabels(fields[7])),
		})
	}
	data := stackkitcommand.HostOperationData{Total: len(containers)}
	if limit := hostListLimit(command); len(containers) > limit {
		containers, data.Truncated = containers[:limit], true
	}
	data.Containers = containers
	return data, nil
}

func firstContainerName(names string) string {
	name, _, _ := strings.Cut(names, ",")
	return strings.TrimPrefix(strings.TrimSpace(name), "/")
}

type containerFacts struct {
	id, name, state     string
	managed, autoRemove bool
}

func inspectContainer(ctx context.Context, ref string) (containerFacts, error) {
	output, err := runHostTool(ctx, "docker", "inspect", "--type", "container", "--format",
		`{{.Id}}`+"\t"+`{{.Name}}`+"\t"+`{{.State.Status}}`+"\t"+`{{.HostConfig.AutoRemove}}`+"\t"+`{{json .Config.Labels}}`, ref)
	if err != nil {
		if strings.Contains(err.Error(), "No such") {
			return containerFacts{}, &hostOperationRefusal{code: hostCodeContainerNotFound, message: "no container named " + ref + " exists on this node"}
		}
		return containerFacts{}, err
	}
	fields := strings.Split(strings.TrimSpace(string(output)), "\t")
	if len(fields) != 5 || fields[0] == "" {
		return containerFacts{}, fmt.Errorf("docker inspect returned an unreadable container record")
	}
	var labels map[string]string
	if err := json.Unmarshal([]byte(fields[4]), &labels); err != nil && fields[4] != "null" {
		return containerFacts{}, fmt.Errorf("docker inspect returned unreadable container labels")
	}
	return containerFacts{
		id: fields[0], name: strings.TrimPrefix(fields[1], "/"), state: fields[2],
		autoRemove: fields[3] == "true", managed: stackkitcommand.IsManagedContainer(labels),
	}, nil
}

// runContainerAction starts, stops or restarts one unmanaged container. A
// StackKits-managed container and the platform's own agents are refused: the
// former change only through managed service actions.
func runContainerAction(ctx context.Context, command *agentpb.StackKitCommand) (stackkitcommand.HostOperationData, error) {
	ref, action := strings.TrimSpace(command.HostTarget), strings.TrimSpace(command.HostAction)
	before, err := inspectContainer(ctx, ref)
	if err != nil {
		return stackkitcommand.HostOperationData{}, err
	}
	if before.managed {
		return stackkitcommand.HostOperationData{}, &hostOperationRefusal{code: hostCodeContainerManaged, message: "container " + before.name + " is managed by StackKits; use the managed service actions"}
	}
	if before.autoRemove {
		// Stopping such a container deletes it, and deleting is no host action.
		return stackkitcommand.HostOperationData{}, &hostOperationRefusal{code: hostCodeContainerAutoClean, message: "container " + before.name + " is removed when it stops, which is a deletion and no host action"}
	}
	if stackkitcommand.ProtectedHostContainer(before.name) {
		return stackkitcommand.HostOperationData{}, &hostOperationRefusal{code: hostCodeContainerProtected, message: "container " + before.name + " belongs to the management path"}
	}
	// The resolved id, not the name, is acted on, so a rename between the
	// check and the action cannot redirect it.
	args := []string{action, before.id}
	if action != stackkitcommand.HostActionStart {
		args = []string{action, "--time", hostContainerStopSeconds, before.id}
	}
	if _, err := runHostTool(ctx, "docker", args...); err != nil {
		return stackkitcommand.HostOperationData{}, &hostOperationFailure{code: hostCodeActionFailed, err: err}
	}
	after, err := inspectContainer(ctx, before.id)
	if err != nil {
		return stackkitcommand.HostOperationData{}, &hostOperationFailure{code: hostCodeReadbackFailed, err: err}
	}
	want := "running"
	if action == stackkitcommand.HostActionStop {
		want = "exited"
	}
	if after.state != want {
		return stackkitcommand.HostOperationData{}, &hostOperationFailure{code: hostCodeStateNotReached, err: fmt.Errorf("container %s is %s after %s, expected %s", before.name, after.state, action, want)}
	}
	return stackkitcommand.HostOperationData{Receipt: &stackkitcommand.HostActionReceipt{
		Target: ref, Action: action, StateBefore: before.state, StateAfter: after.state,
	}}, nil
}

type unitFacts struct {
	load, active string
	// names are the unit's id and aliases; related are the units that require
	// or are bound to it, which stop together with it, and the units it
	// conflicts with, which its start stops.
	names, related []string
}

func readUnit(ctx context.Context, unit string) (unitFacts, error) {
	output, err := runHostTool(ctx, "systemctl", "show", "--property=Id", "--property=Names", "--property=LoadState",
		"--property=ActiveState", "--property=RequiredBy", "--property=BoundBy", "--property=ConsistsOf", "--property=Conflicts", "--property=ConflictedBy", unit)
	if err != nil {
		return unitFacts{}, err
	}
	var facts unitFacts
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "LoadState":
			facts.load = value
		case "ActiveState":
			facts.active = value
		case "Id", "Names":
			facts.names = append(facts.names, strings.Fields(value)...)
		case "RequiredBy", "BoundBy", "ConsistsOf", "Conflicts", "ConflictedBy":
			facts.related = append(facts.related, strings.Fields(value)...)
		}
	}
	if facts.load == "" || facts.active == "" {
		return unitFacts{}, fmt.Errorf("systemctl returned no state for %s", unit)
	}
	return facts, nil
}

// protectedUnitReach names the protected unit a change of this unit would hit:
// one of its aliases, or one that requires it, is bound to it, consists of it or
// conflicts with it (a conflict stops either side when the other starts).
func protectedUnitReach(facts unitFacts) string {
	for _, name := range append(append([]string(nil), facts.names...), facts.related...) {
		if stackkitcommand.ProtectedHostUnit(name) {
			return name
		}
	}
	return ""
}

// runSystemdAction starts, stops or restarts one service unit. Units of the
// management path are refused again here: the node does not trust the caller.
func runSystemdAction(ctx context.Context, command *agentpb.StackKitCommand) (stackkitcommand.HostOperationData, error) {
	unit, action := strings.TrimSpace(command.HostTarget), strings.TrimSpace(command.HostAction)
	if stackkitcommand.ProtectedHostUnit(unit) {
		return stackkitcommand.HostOperationData{}, &hostOperationRefusal{code: hostCodeUnitProtected, message: "unit " + unit + " belongs to the management path"}
	}
	facts, err := readUnit(ctx, unit)
	if err != nil {
		return stackkitcommand.HostOperationData{}, err
	}
	if facts.load != "loaded" {
		return stackkitcommand.HostOperationData{}, &hostOperationRefusal{code: hostCodeUnitNotFound, message: "unit " + unit + " is " + facts.load}
	}
	if reach := protectedUnitReach(facts); reach != "" {
		return stackkitcommand.HostOperationData{}, &hostOperationRefusal{code: hostCodeUnitProtected, message: "unit " + unit + " is tied to " + reach + ", which belongs to the management path"}
	}
	// systemd reports any instance of an existing template as loaded, so a
	// start would create an instance the caller made up (modprobe@<module>).
	// Only an instance that is already up, or failed, can be acted on.
	if strings.Contains(unit, "@") && facts.active != "active" && facts.active != "activating" && facts.active != "failed" {
		return stackkitcommand.HostOperationData{}, &hostOperationRefusal{code: hostCodeUnitInstance, message: "unit instance " + unit + " is not running"}
	}
	before := facts.active
	if _, err := runHostTool(ctx, "systemctl", action, unit); err != nil {
		return stackkitcommand.HostOperationData{}, &hostOperationFailure{code: hostCodeActionFailed, err: err}
	}
	afterFacts, err := readUnit(ctx, unit)
	if err != nil {
		return stackkitcommand.HostOperationData{}, &hostOperationFailure{code: hostCodeReadbackFailed, err: err}
	}
	after := afterFacts.active
	ok := after == "active" || after == "activating"
	if action == stackkitcommand.HostActionStop {
		ok = after == "inactive" || after == "failed"
	}
	if !ok {
		return stackkitcommand.HostOperationData{}, &hostOperationFailure{code: hostCodeStateNotReached, err: fmt.Errorf("unit %s is %s after %s", unit, after, action)}
	}
	return stackkitcommand.HostOperationData{Receipt: &stackkitcommand.HostActionReceipt{
		Target: unit, Action: action, StateBefore: before, StateAfter: after,
	}}, nil
}
