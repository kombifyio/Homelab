package stackkitcommand

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kombifyio/techstack/pkg/api/agentpb"
)

// Own-application lifecycle (StackKits #3267, docs/OWN_APPLICATIONS.md
// "Managed nodes"): the catalog operations own-application.* that operate an
// own application, including one rebuilt by "Stack it". Every mutation is a
// reviewed dry run first; its review digest is bound into a Techstack
// own-application.lifecycle capability (name, action, operation ID, review
// digest, at most 15 minutes) and the execution passes --capability.
const (
	// OwnApplicationCapability is what a Guard advertises when its pinned
	// release dispatches every own-application.* operation.
	OwnApplicationCapability = "stackkit.own-application-lifecycle.v1"
	// OwnApplicationResultVersion is the raw result of status, logs, review,
	// start, stop, restart and rollback.
	OwnApplicationResultVersion = "stackkit.own-application-result/v1"
	// OwnApplicationUpdateResultVersion is the raw result of a tag update,
	// a checkpoint rollback and a checkpoint discard (and their reviews).
	OwnApplicationUpdateResultVersion = "stackkit.own-application-update/v1"
	// OwnApplicationTargetCheck is the only target check of a tag update:
	// kombify tests catalog releases, never the image of an own application.
	OwnApplicationTargetCheck = "not_checked_by_kombify"
)

// Own-application steps, as the catalog names them after "own-application.".
const (
	OwnApplicationStatus                   = "status"
	OwnApplicationLogs                     = "logs"
	OwnApplicationReview                   = "review"
	OwnApplicationUpdateReview             = "update.review"
	OwnApplicationCheckpointRollbackReview = "checkpoint-rollback.review"
	OwnApplicationCheckpointDiscardReview  = "checkpoint-discard.review"
	OwnApplicationStart                    = "start"
	OwnApplicationStop                     = "stop"
	OwnApplicationRestart                  = "restart"
	OwnApplicationUpdate                   = "update"
	OwnApplicationRollback                 = "rollback"
	OwnApplicationCheckpointRollback       = "checkpoint-rollback"
	OwnApplicationCheckpointDiscard        = "checkpoint-discard"
	OwnApplicationBackup                   = "backup"
)

// ownApplicationReviewOf maps each execution to the dry run that reviews it.
var ownApplicationReviewOf = map[string]string{
	OwnApplicationStart: OwnApplicationReview, OwnApplicationStop: OwnApplicationReview, OwnApplicationRestart: OwnApplicationReview,
	OwnApplicationRollback: OwnApplicationReview, OwnApplicationUpdate: OwnApplicationUpdateReview,
	OwnApplicationCheckpointRollback: OwnApplicationCheckpointRollbackReview, OwnApplicationCheckpointDiscard: OwnApplicationCheckpointDiscardReview,
}

// ownApplicationCapabilityAction is the capability scope action and the CLI
// --action of each execution.
var ownApplicationCapabilityAction = map[string]string{
	OwnApplicationStart: "start", OwnApplicationStop: "stop", OwnApplicationRestart: "restart", OwnApplicationUpdate: "update",
	OwnApplicationRollback: "rollback", OwnApplicationCheckpointRollback: "rollback", OwnApplicationCheckpointDiscard: "discard-checkpoint",
}

var (
	ownApplicationTagPattern       = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	ownApplicationImagePattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*(?::[0-9]+)?/[a-z0-9._/-]+@sha256:[a-f0-9]{64}$`)
	ownApplicationOperationPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

// OwnApplicationOperations are every catalog operation Techstack dispatches.
func OwnApplicationOperations() []string {
	steps := []string{OwnApplicationStatus, OwnApplicationLogs, OwnApplicationReview, OwnApplicationUpdateReview, OwnApplicationCheckpointRollbackReview,
		OwnApplicationCheckpointDiscardReview, OwnApplicationStart, OwnApplicationStop, OwnApplicationRestart, OwnApplicationUpdate,
		OwnApplicationRollback, OwnApplicationCheckpointRollback, OwnApplicationCheckpointDiscard, OwnApplicationBackup}
	out := make([]string, 0, len(steps))
	for _, step := range steps {
		out = append(out, "own-application."+step)
	}
	return out
}

// IsOwnApplicationOperation reports the own-application agent operations.
func IsOwnApplicationOperation(op agentpb.StackKitOperation) bool {
	return op == agentpb.StackKitOperation_STACKKIT_OPERATION_OWN_APPLICATION || op == agentpb.StackKitOperation_STACKKIT_OPERATION_OWN_APPLICATION_BACKUP
}

// OwnApplicationStepExecutes reports an execution that needs a reviewed dry
// run and the own-application.lifecycle capability.
func OwnApplicationStepExecutes(step string) bool {
	_, ok := ownApplicationReviewOf[step]
	return ok
}

// OwnApplicationReviewStep names the dry run that reviews an execution.
func OwnApplicationReviewStep(step string) string { return ownApplicationReviewOf[step] }

// OwnApplicationCapabilityAction is the action an execution's capability
// scope names.
func OwnApplicationCapabilityAction(step string) string { return ownApplicationCapabilityAction[step] }

// OwnApplicationStepRead reports a step that changes nothing.
func OwnApplicationStepRead(step string) bool {
	switch step {
	case OwnApplicationStatus, OwnApplicationLogs, OwnApplicationReview, OwnApplicationUpdateReview, OwnApplicationCheckpointRollbackReview, OwnApplicationCheckpointDiscardReview:
		return true
	}
	return false
}

// ValidOwnApplicationImageTag and ValidOwnApplicationOperationID bound the
// free-form CLI inputs.
func ValidOwnApplicationImageTag(tag string) bool { return ownApplicationTagPattern.MatchString(tag) }
func ValidOwnApplicationOperationID(id string) bool {
	return ownApplicationOperationPattern.MatchString(id)
}

func ownApplicationStep(c *agentpb.StackKitCommand) string {
	return strings.TrimPrefix(c.OwnApplicationOperation, "own-application.")
}

func ownApplicationFieldsPresent(c *agentpb.StackKitCommand) bool {
	return c.OwnApplicationOperation != "" || c.OwnApplicationReviewDigest != "" || c.OwnApplicationAction != "" || c.OwnApplicationImageTag != "" ||
		c.OwnApplicationResolvedImage != "" || c.OwnApplicationAllowMajor || c.OwnApplicationCheckpoint != ""
}

func validateOwnApplicationFields(c *agentpb.StackKitCommand) error {
	if !IsOwnApplicationOperation(c.Operation) && ownApplicationFieldsPresent(c) {
		return fmt.Errorf("own application lifecycle fields require an own application operation")
	}
	return nil
}

func validateOwnApplicationCommand(c *agentpb.StackKitCommand) error {
	if c.Release == nil || c.StackkitInstanceId == "" || !ValidOwnApplicationName(c.OwnApplicationName) || c.ServiceKey != "" || c.RilExecutionProof != nil ||
		c.ApplicationContainerId != "" || c.ApplicationPlanDigest != "" || c.ApplicationAction != "" || c.DryRun || c.Offline {
		return fmt.Errorf("own application lifecycle requires its pinned release, workspace identity and application name only")
	}
	if c.Operation == agentpb.StackKitOperation_STACKKIT_OPERATION_OWN_APPLICATION_BACKUP {
		if ownApplicationFieldsPresent(c) || len(c.AdvancedCapability) != 0 || !c.OwnerApproved || !ValidOwnApplicationOperationID(c.ApplicationOperationId) {
			return fmt.Errorf("own application backup requires Owner approval and its operation ID only")
		}
		return nil
	}
	step := ownApplicationStep(c)
	if !slices.Contains(OwnApplicationOperations(), c.OwnApplicationOperation) || step == OwnApplicationBackup {
		return fmt.Errorf("own application operation %q is not dispatched", c.OwnApplicationOperation)
	}
	wantsTag := step == OwnApplicationUpdate || step == OwnApplicationUpdateReview
	wantsCheckpoint := step == OwnApplicationCheckpointRollback || step == OwnApplicationCheckpointRollbackReview || step == OwnApplicationCheckpointDiscard || step == OwnApplicationCheckpointDiscardReview
	if wantsTag != (c.OwnApplicationImageTag != "") || (!wantsTag && c.OwnApplicationAllowMajor) || (c.OwnApplicationImageTag != "" && !ValidOwnApplicationImageTag(c.OwnApplicationImageTag)) {
		return fmt.Errorf("own application tag update requires exactly its version tag")
	}
	if wantsCheckpoint != (c.OwnApplicationCheckpoint != "") || (c.OwnApplicationCheckpoint != "" && !ValidOwnApplicationOperationID(c.OwnApplicationCheckpoint)) {
		return fmt.Errorf("own application checkpoint operations require exactly the checkpoint operation ID")
	}
	if (step == OwnApplicationReview) != (c.OwnApplicationAction != "") || (step == OwnApplicationReview && !slices.Contains([]string{"start", "stop", "restart", "rollback"}, c.OwnApplicationAction)) {
		return fmt.Errorf("own application review names exactly one of start, stop, restart or rollback")
	}
	if (step == OwnApplicationUpdate) != (c.OwnApplicationResolvedImage != "") || (c.OwnApplicationResolvedImage != "" && !ownApplicationImagePattern.MatchString(c.OwnApplicationResolvedImage)) {
		return fmt.Errorf("own application update requires the reviewed digest reference")
	}
	if OwnApplicationStepRead(step) {
		if c.OwnerApproved || c.ApplicationOperationId != "" || c.OwnApplicationReviewDigest != "" || len(c.AdvancedCapability) != 0 {
			return fmt.Errorf("own application read cannot carry execution fields")
		}
		return nil
	}
	if !c.OwnerApproved || !ValidOwnApplicationOperationID(c.ApplicationOperationId) || !specHashPattern.MatchString(c.OwnApplicationReviewDigest) {
		return fmt.Errorf("own application execution requires Owner approval, its operation ID and the reviewed digest")
	}
	if len(c.AdvancedCapability) == 0 || len(c.AdvancedCapability) > MaxAdvancedCapabilityBytes {
		return fmt.Errorf("own application execution requires its own-application.lifecycle capability within %d bytes", MaxAdvancedCapabilityBytes)
	}
	return nil
}

// OwnApplicationArgValues are the catalog placeholder values of one command;
// the Guard adds the private capability file it wrote.
func OwnApplicationArgValues(c *agentpb.StackKitCommand, capabilityFile string) map[string]string {
	values := map[string]string{"ownApplicationName": c.OwnApplicationName, "operationId": c.ApplicationOperationId, "capabilityFile": capabilityFile}
	if c.Operation == agentpb.StackKitOperation_STACKKIT_OPERATION_OWN_APPLICATION_BACKUP {
		return values
	}
	values["reviewDigest"] = c.OwnApplicationReviewDigest
	values["ownApplicationAction"] = c.OwnApplicationAction
	values["imageTag"] = c.OwnApplicationImageTag
	values["resolvedImage"] = c.OwnApplicationResolvedImage
	values["checkpointOperationId"] = c.OwnApplicationCheckpoint
	return values
}

// OwnApplicationCatalogOperation is the catalog operation of a command.
func OwnApplicationCatalogOperation(c *agentpb.StackKitCommand) string {
	if c.Operation == agentpb.StackKitOperation_STACKKIT_OPERATION_OWN_APPLICATION_BACKUP {
		return "own-application." + OwnApplicationBackup
	}
	return c.OwnApplicationOperation
}

// OwnApplicationContainer is one container of the status observation.
type OwnApplicationContainer struct {
	Service string `json:"service"`
	State   string `json:"state"`
	Health  string `json:"health,omitempty"`
}

// OwnApplicationImage is the prior or target image of a tag update.
type OwnApplicationImage struct {
	Image         string `json:"image,omitempty"`
	Version       string `json:"version,omitempty"`
	ResolvedImage string `json:"resolved_image,omitempty"`
}

// OwnApplicationCheckpoint is the application-scoped data checkpoint of the
// latest tag update: its state (planned, taken, restoring, retained,
// restored, kept, discarded) and the size of its retained copy.
type OwnApplicationCheckpoint struct {
	OperationID   string     `json:"operation_id"`
	State         string     `json:"state"`
	RetainedBytes int64      `json:"retained_bytes"`
	At            *time.Time `json:"at,omitempty"`
}

// OwnApplicationResult is the secret-free projection Techstack keeps of a
// stackkit.own-application-result/v1 or stackkit.own-application-update/v1.
type OwnApplicationResult struct {
	APIVersion         string                    `json:"api_version"`
	Operation          string                    `json:"operation"`
	Outcome            string                    `json:"outcome"`
	ReasonCode         string                    `json:"reason_code,omitempty"`
	ReviewDigest       string                    `json:"review_digest,omitempty"`
	ReviewedAction     string                    `json:"reviewed_action,omitempty"`
	State              string                    `json:"state,omitempty"`
	Image              string                    `json:"image,omitempty"`
	Containers         []OwnApplicationContainer `json:"containers,omitempty"`
	Logs               string                    `json:"logs,omitempty"`
	AllowMajor         bool                      `json:"allow_major,omitempty"`
	TargetCheck        string                    `json:"target_check,omitempty"`
	Prior              *OwnApplicationImage      `json:"prior,omitempty"`
	Target             *OwnApplicationImage      `json:"target,omitempty"`
	Checkpoint         *OwnApplicationCheckpoint `json:"checkpoint,omitempty"`
	ReplacesCheckpoint *OwnApplicationCheckpoint `json:"replaces_checkpoint,omitempty"`
	SnapshotID         string                    `json:"snapshot_id,omitempty"`
}

type rawOwnApplicationCheckpoint struct {
	OperationID   string     `json:"operationId"`
	State         string     `json:"state"`
	RetainedBytes int64      `json:"retainedBytes"`
	At            *time.Time `json:"at"`
	Volumes       []struct {
		Bytes int64 `json:"bytes"`
	} `json:"volumes"`
}

func (raw *rawOwnApplicationCheckpoint) project() *OwnApplicationCheckpoint {
	if raw == nil || raw.OperationID == "" {
		return nil
	}
	size := raw.RetainedBytes
	if size == 0 {
		for _, volume := range raw.Volumes {
			size += volume.Bytes
		}
	}
	var at *time.Time
	if raw.At != nil && !raw.At.IsZero() {
		utc := raw.At.UTC()
		at = &utc
	}
	return &OwnApplicationCheckpoint{OperationID: raw.OperationID, State: raw.State, RetainedBytes: size, At: at}
}

type rawOwnApplicationImage struct {
	Image    string `json:"image"`
	Version  string `json:"version"`
	Resolved string `json:"resolvedImage"`
}

func (raw rawOwnApplicationImage) project() *OwnApplicationImage {
	if raw.Image == "" && raw.Version == "" && raw.Resolved == "" {
		return nil
	}
	return &OwnApplicationImage{Image: raw.Image, Version: raw.Version, ResolvedImage: raw.Resolved}
}

type rawOwnApplicationResult struct {
	APIVersion   string          `json:"apiVersion"`
	Name         string          `json:"name"`
	Action       string          `json:"action"`
	Outcome      string          `json:"outcome"`
	ReasonCode   string          `json:"reasonCode"`
	ReviewDigest string          `json:"reviewDigest"`
	Observation  json.RawMessage `json:"observation"`
	Record       struct {
		State  string `json:"state"`
		Bundle struct {
			Application struct {
				Name  string `json:"name"`
				Image string `json:"image"`
			} `json:"application"`
		} `json:"bundle"`
		Checkpoint *rawOwnApplicationCheckpoint `json:"checkpoint"`
	} `json:"record"`
	AllowMajor         bool                         `json:"allowMajor"`
	TargetCheck        string                       `json:"targetCheck"`
	Prior              rawOwnApplicationImage       `json:"prior"`
	Target             rawOwnApplicationImage       `json:"target"`
	Checkpoint         *rawOwnApplicationCheckpoint `json:"checkpoint"`
	ReplacesCheckpoint *rawOwnApplicationCheckpoint `json:"replacesCheckpoint"`
}

var ownApplicationOutcomes = []string{"reviewed", "observed", "observation_unavailable", "started", "stopped", "restarted", "updated", "rolled_back", "rollback_failed", "discarded", "refused", "pending", "denied"}

// ParseOwnApplicationResult reads the raw own-application result the Guard
// wrapped as data.output, for a successful and a failed run alike: a refused,
// pending or denied call still prints its result before the nonzero exit.
func ParseOwnApplicationResult(c *agentpb.StackKitCommand, r *agentpb.StackKitResult) (OwnApplicationResult, error) {
	var out OwnApplicationResult
	if c == nil || r == nil || !IsOwnApplicationOperation(c.Operation) {
		return out, fmt.Errorf("own application result is required")
	}
	out.Operation = OwnApplicationCatalogOperation(c)
	var envelope struct {
		Status string `json:"status"`
		Data   struct {
			Output string `json:"output"`
			ID     string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(r.CommandResultJson, &envelope); err != nil {
		return out, fmt.Errorf("own application returned no result")
	}
	if c.Operation == agentpb.StackKitOperation_STACKKIT_OPERATION_OWN_APPLICATION_BACKUP {
		// stackkit backup run answers with the typed command result.
		out.APIVersion, out.Outcome, out.SnapshotID = "stackkit.command-result/v1", "refused", strings.TrimSpace(envelope.Data.ID)
		if r.Success && envelope.Status == "success" {
			out.Outcome = "backed_up"
		}
		return out, nil
	}
	if strings.TrimSpace(envelope.Data.Output) == "" {
		return out, fmt.Errorf("own application returned no result")
	}
	var raw rawOwnApplicationResult
	if err := json.Unmarshal([]byte(envelope.Data.Output), &raw); err != nil {
		return out, fmt.Errorf("decode own application result: %w", err)
	}
	step := ownApplicationStep(c)
	update := step == OwnApplicationUpdate || step == OwnApplicationUpdateReview || step == OwnApplicationCheckpointRollback ||
		step == OwnApplicationCheckpointRollbackReview || step == OwnApplicationCheckpointDiscard || step == OwnApplicationCheckpointDiscardReview
	want := OwnApplicationResultVersion
	if update {
		want = OwnApplicationUpdateResultVersion
	}
	if raw.APIVersion != want {
		return out, fmt.Errorf("own application result has version %q, want %q", raw.APIVersion, want)
	}
	name := raw.Name
	if name == "" {
		name = raw.Record.Bundle.Application.Name
	}
	if name != "" && name != c.OwnApplicationName {
		return out, fmt.Errorf("own application result names another application")
	}
	if !slices.Contains(ownApplicationOutcomes, raw.Outcome) {
		return out, fmt.Errorf("own application result has an unknown outcome")
	}
	out.APIVersion, out.Outcome, out.ReasonCode, out.ReviewDigest = raw.APIVersion, raw.Outcome, raw.ReasonCode, raw.ReviewDigest
	out.State, out.Image = raw.Record.State, raw.Record.Bundle.Application.Image
	if step == OwnApplicationReview {
		out.ReviewedAction = c.OwnApplicationAction
	}
	out.Checkpoint = raw.Record.Checkpoint.project()
	if update {
		out.AllowMajor, out.TargetCheck = raw.AllowMajor, raw.TargetCheck
		out.Prior, out.Target = raw.Prior.project(), raw.Target.project()
		if checkpoint := raw.Checkpoint.project(); checkpoint != nil {
			out.Checkpoint = checkpoint
		}
		out.ReplacesCheckpoint = raw.ReplacesCheckpoint.project()
		if out.TargetCheck != "" && out.TargetCheck != OwnApplicationTargetCheck {
			return out, fmt.Errorf("own application tag update claims a target check")
		}
	}
	if len(raw.Observation) != 0 {
		if step == OwnApplicationLogs {
			var logs struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(raw.Observation, &logs)
			out.Logs = boundedLogText(logs.Text)
		} else {
			var containers map[string]struct {
				State  string `json:"State"`
				Health string `json:"Health"`
			}
			if json.Unmarshal(raw.Observation, &containers) == nil {
				for service, container := range containers {
					out.Containers = append(out.Containers, OwnApplicationContainer{Service: service, State: container.State, Health: container.Health})
				}
				sort.Slice(out.Containers, func(i, j int) bool { return out.Containers[i].Service < out.Containers[j].Service })
			}
		}
	}
	if !r.Success {
		return out, nil
	}
	if OwnApplicationStepRead(step) && step != OwnApplicationStatus && step != OwnApplicationLogs {
		if out.Outcome == "reviewed" && !specHashPattern.MatchString(out.ReviewDigest) {
			return out, fmt.Errorf("own application review lacks its review digest")
		}
	} else if OwnApplicationStepExecutes(step) && out.Outcome != "refused" && out.Outcome != "pending" && out.Outcome != "denied" && out.ReviewDigest != "" && out.ReviewDigest != c.OwnApplicationReviewDigest {
		return out, fmt.Errorf("own application execution answers another review")
	}
	return out, nil
}

// MaxOwnApplicationLogBytes bounds the log tail Techstack keeps and returns.
const MaxOwnApplicationLogBytes = 64 << 10

// boundedLogText keeps the last MaxOwnApplicationLogBytes of valid UTF-8 and
// drops every control character except newline and tab, so a container's
// output cannot carry terminal escapes into a job or a browser.
func boundedLogText(text string) string {
	text = strings.ToValidUTF8(text, "\uFFFD")
	text = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, text)
	if len(text) > MaxOwnApplicationLogBytes {
		text = text[len(text)-MaxOwnApplicationLogBytes:]
		for len(text) > 0 && !utf8.RuneStart(text[0]) {
			text = text[1:]
		}
	}
	return text
}

// OwnApplicationResultMap is the stored, secret-free projection.
func OwnApplicationResultMap(result OwnApplicationResult) map[string]any {
	raw, _ := json.Marshal(result)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}
