package stackkitcommand

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/kombifyio/homelab/pkg/api/agentpb"
)

// "Stack it": one existing single container StackKits did not install,
// rebuilt as an own application (StackKits `stackkit app own --from-container`,
// contract homelab.own-application/v4). The Home Assistant in-place adoption
// (application.go) is a separate profile and stays as it is.
const (
	// ApplicationStackCapability is what a Guard advertises when its pinned
	// StackKits release dispatches every application.stack.* operation.
	ApplicationStackCapability = "stackkit.application-stack.v1"
	// OwnApplicationAdoptedProfile names the adoption profile in Homelab.
	OwnApplicationAdoptedProfile = "own-application-adopted/v1"
	// MinApplicationStackRelease is the first StackKits release with
	// `stackkit app own --from-container` (StackKits #3257).
	MinApplicationStackRelease = "v0.57.10"
	// ApplicationStackResultVersion is the producer's result document.
	ApplicationStackResultVersion = "stackkit.own-application-adoption/v1"
	// ApplicationStackRetention is the producer's restore window.
	ApplicationStackRetention = 7 * 24 * time.Hour
)

var (
	applicationStackRowCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	ownApplicationNamePattern      = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
	ownApplicationNameCleaner      = regexp.MustCompile(`[^a-z0-9]+`)
)

// ApplicationStackOperations are the producer catalog operations, in the
// order Homelab dispatches them.
var ApplicationStackOperations = []string{"application.stack.plan", "application.stack", "application.stack.verify", "application.stack.restore-original", "application.stack.release-original"}

func IsApplicationStackOperation(op agentpb.StackKitOperation) bool {
	return op >= agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_STACK_PLAN && op <= agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_STACK_RELEASE_ORIGINAL
}

// ApplicationStackOperationName is the catalog operation of op.
func ApplicationStackOperationName(op agentpb.StackKitOperation) string {
	if !IsApplicationStackOperation(op) {
		return ""
	}
	return ApplicationStackOperations[op-agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_STACK_PLAN]
}

func applicationStackMutation(op agentpb.StackKitOperation) bool {
	return op == agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_STACK || op == agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_STACK_RESTORE_ORIGINAL || op == agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_STACK_RELEASE_ORIGINAL
}

// OwnApplicationName derives the producer's default own application name from
// a container name: lowercase DNS words, a letter first, at most 40 bytes.
func OwnApplicationName(containerName string) string {
	name := strings.Trim(ownApplicationNameCleaner.ReplaceAllString(strings.ToLower(strings.TrimPrefix(strings.TrimSpace(containerName), "/")), "-"), "-")
	if name != "" && (name[0] < 'a' || name[0] > 'z') {
		name = "app-" + name
	}
	if len(name) > 40 {
		name = strings.Trim(name[:40], "-")
	}
	if !ValidOwnApplicationName(name) {
		return ""
	}
	return name
}

func ValidOwnApplicationName(name string) bool {
	return len(name) <= 40 && ownApplicationNamePattern.MatchString(name) && !strings.HasPrefix(name, "kombify") && !strings.HasPrefix(name, "stackkit")
}

func validateApplicationStackCommand(c *agentpb.StackKitCommand) error {
	if !ReleaseAtLeast(c.Release.GetVersion(), MinApplicationStackRelease) {
		return fmt.Errorf("Stack it requires StackKits %s or newer", MinApplicationStackRelease)
	}
	if c.StackkitInstanceId == "" || c.ServiceKey != "" || c.ApplicationAction != "" || c.RilExecutionProof != nil {
		return fmt.Errorf("Stack it requires the exact workspace identity and carries no service key, action or RIL proof")
	}
	if !sha256Pattern.MatchString(c.ApplicationContainerId) || !ValidOwnApplicationName(c.OwnApplicationName) {
		return fmt.Errorf("Stack it requires a full container ID and a valid own application name")
	}
	if !applicationStackMutation(c.Operation) {
		if c.OwnerApproved || c.ApplicationPlanDigest != "" || c.ApplicationOperationId != "" {
			return fmt.Errorf("Stack it read cannot carry mutation fields")
		}
		return nil
	}
	if !c.OwnerApproved || !commandIDPattern.MatchString(c.ApplicationOperationId) {
		return fmt.Errorf("Stack it mutation requires Owner approval and stable operation identity")
	}
	if (c.Operation == agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_STACK) != specHashPattern.MatchString(c.ApplicationPlanDigest) {
		return fmt.Errorf("only Stack it adopt carries the reviewed plan digest")
	}
	return nil
}

// ApplicationStackRow is one reviewed setting: kept, changed or refused. Code
// and Values are the stable machine form clients translate
// (docs/APPLICATION_ADOPTION.md "Plan row codes"); Reason is the producer's
// English explanation and is never shown.
type ApplicationStackRow struct {
	Code     string            `json:"code"`
	Values   map[string]string `json:"values,omitempty"`
	Field    string            `json:"field"`
	Status   string            `json:"status"`
	Observed string            `json:"observed,omitempty"`
	Adopted  string            `json:"adopted,omitempty"`
	Reason   string            `json:"reason,omitempty"`
}

type ApplicationStackPort struct {
	HostIP        string `json:"hostIP,omitempty"`
	HostPort      int    `json:"hostPort"`
	ContainerPort int    `json:"containerPort"`
	Protocol      string `json:"protocol"`
}

type ApplicationStackData struct {
	Source    string `json:"source"`
	MountPath string `json:"mountPath"`
	Volume    string `json:"volume"`
	Measured  bool   `json:"measured"`
	Files     int64  `json:"files,omitempty"`
	Bytes     int64  `json:"bytes,omitempty"`
	FreeBytes int64  `json:"freeBytes,omitempty"`
}

// ApplicationStackResult is the secret-free projection of
// stackkit.own-application-adoption/v1. Environment values, secret values and
// command arguments never appear in the producer document; Homelab keeps
// only these fields.
type ApplicationStackResult struct {
	APIVersion       string                 `json:"apiVersion"`
	Phase            string                 `json:"phase"`
	Outcome          string                 `json:"outcome"`
	ContainerID      string                 `json:"containerId"`
	ContainerName    string                 `json:"containerName"`
	Name             string                 `json:"name,omitempty"`
	Admissible       bool                   `json:"admissible"`
	PlanDigest       string                 `json:"planDigest,omitempty"`
	Rows             []ApplicationStackRow  `json:"rows"`
	HiddenKeys       []string               `json:"hiddenKeys"`
	SecretKeys       []string               `json:"secretKeys"`
	OutboundInternet bool                   `json:"outboundInternet"`
	PublishedPorts   []ApplicationStackPort `json:"publishedPorts"`
	Data             *ApplicationStackData  `json:"data,omitempty"`
	DowntimeSeconds  int64                  `json:"downtimeSeconds,omitempty"`
	RetainUntil      *time.Time             `json:"retainUntil,omitempty"`
	OriginalState    string                 `json:"originalState,omitempty"`
	Notice           string                 `json:"notice"`
}

var applicationStackPhase = map[agentpb.StackKitOperation]string{
	agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_STACK_PLAN:             "plan",
	agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_STACK:                  "adopt",
	agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_STACK_VERIFY:           "verify",
	agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_STACK_RESTORE_ORIGINAL: "restore-original",
	agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_STACK_RELEASE_ORIGINAL: "release-original",
}

// ParseApplicationStackResult reads the producer document the Guard wrapped
// as data.output, for a successful or a failed run alike: a refused plan or a
// rolled-back adoption still prints its result before the nonzero exit.
func ParseApplicationStackResult(c *agentpb.StackKitCommand, r *agentpb.StackKitResult) (ApplicationStackResult, error) {
	var e ApplicationStackResult
	if c == nil || r == nil || !IsApplicationStackOperation(c.Operation) {
		return e, fmt.Errorf("Stack it result is required")
	}
	var envelope struct {
		Data struct {
			Output string `json:"output"`
		} `json:"data"`
	}
	if err := json.Unmarshal(r.CommandResultJson, &envelope); err != nil || strings.TrimSpace(envelope.Data.Output) == "" {
		return e, fmt.Errorf("Stack it returned no adoption result")
	}
	if err := json.Unmarshal([]byte(envelope.Data.Output), &e); err != nil {
		return e, fmt.Errorf("decode Stack it result: %w", err)
	}
	if e.APIVersion != ApplicationStackResultVersion || e.Phase != applicationStackPhase[c.Operation] || e.ContainerID != c.ApplicationContainerId {
		return e, fmt.Errorf("Stack it result does not match the dispatched container and phase")
	}
	if e.Name != "" && e.Name != c.OwnApplicationName {
		return e, fmt.Errorf("Stack it result names another own application")
	}
	for _, row := range e.Rows {
		if !slices.Contains([]string{"kept", "changed", "refused"}, row.Status) {
			return e, fmt.Errorf("Stack it result has an unknown row status")
		}
		if !applicationStackRowCodePattern.MatchString(row.Code) {
			return e, fmt.Errorf("Stack it result has a row without its machine code")
		}
	}
	if !r.Success {
		return e, nil
	}
	switch c.Operation {
	case agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_STACK_PLAN:
		if e.Outcome != "planned" && e.Outcome != "refused" {
			return e, fmt.Errorf("Stack it plan has no planned outcome")
		}
		if e.Outcome == "planned" && (!e.Admissible || !specHashPattern.MatchString(e.PlanDigest)) {
			return e, fmt.Errorf("Stack it plan lacks its plan digest")
		}
	case agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_STACK:
		if e.Outcome != "adopted" || e.PlanDigest != c.ApplicationPlanDigest || e.RetainUntil == nil {
			return e, fmt.Errorf("Stack it adoption does not match the approved plan")
		}
	case agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_STACK_VERIFY:
		if e.Outcome != "verified" && e.Outcome != "unhealthy" && e.Outcome != "in_progress" {
			return e, fmt.Errorf("Stack it verification has no verification outcome")
		}
	case agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_STACK_RESTORE_ORIGINAL:
		if e.Outcome != "restored" {
			return e, fmt.Errorf("Stack it restore did not restore the original")
		}
	case agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_STACK_RELEASE_ORIGINAL:
		if e.Outcome != "released" {
			return e, fmt.Errorf("Stack it release did not remove the original")
		}
	}
	return e, nil
}

// ApplicationStackResultMap is the stored, secret-free projection.
func ApplicationStackResultMap(e ApplicationStackResult) map[string]any {
	raw, _ := json.Marshal(e)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}
