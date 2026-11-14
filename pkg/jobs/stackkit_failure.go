package jobs

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/kombifyio/techstack/pkg/api/agentpb"
)

// StackKitReasonACMERateLimited is the StackKits outcome class for a public
// certificate the ACME certificate authority refused under its rate limit.
// A new server for the same address requests the same certificate, so neither
// a retry nor a replacement can succeed before the authority's retry-after.
const StackKitReasonACMERateLimited = "acme_rate_limited"

// StackKitReasonUpgradeKitUnsupported is the StackKits refusal of a fresh
// upgrade for a kit whose CUE upgrade policy declares support "unsupported".
// StackKits refuses before it resolves a release or changes the node.
const StackKitReasonUpgradeKitUnsupported = "upgrade_kit_unsupported"

// StackKits refusals of a saved Plan that another release compiled. A CLI that
// orders the two releases and is newer than the Plan's compiler reports
// stored_plan_regenerate_required: a new rollout on its own release is the one
// action that works, and an approved StackKits update cannot take its
// checkpoint before that (a Guard that converged to a newer CLI before
// Techstack withheld it, #2746). It reports stored_plan_upgrade_required when
// it is older than the Plan or cannot order the releases; CLIs released before
// the regenerate code report stored_plan_upgrade_required in both directions,
// and an earlier v0.55 CLI the strict contract refusal stored_plan_invalid.
// Each answers every verify, drift detect and plan (platform-j0ap6).
const (
	StackKitReasonStoredPlanRegenerateRequired = "stored_plan_regenerate_required"
	StackKitReasonStoredPlanUpgradeRequired    = "stored_plan_upgrade_required"
	StackKitReasonStoredPlanInvalid            = "stored_plan_invalid"
)

// storedPlanNeedsRollout reports a saved Plan the node's CLI refuses. A new
// rollout compiles it with the node's release and works for every StackKit;
// the in-place `upgrade` works only where the kit's upgrade policy allows it
// and never for a Plan older than the node's release.
func storedPlanNeedsRollout(reasonCode string) bool {
	return reasonCode == StackKitReasonStoredPlanRegenerateRequired ||
		reasonCode == StackKitReasonStoredPlanUpgradeRequired || reasonCode == StackKitReasonStoredPlanInvalid
}

// StackKitRefusedBeforeChange reports a typed StackKits refusal that the CLI
// returns before it changes the node: an upgrade the kit's policy does not
// support, or a saved Plan it does not accept. Such a failed job leaves the
// deployment as it was.
func StackKitRefusedBeforeChange(reasonCode string) bool {
	return reasonCode == StackKitReasonUpgradeKitUnsupported || storedPlanNeedsRollout(reasonCode)
}

// storedPlanRolloutHint is the operator text for storedPlanNeedsRollout.
func storedPlanRolloutHint(reasonCode string) string {
	if reasonCode == StackKitReasonStoredPlanRegenerateRequired {
		return "the node's StackKits release is newer than the release that compiled the saved plan; roll the stack out again (deploy) so the node's release compiles and applies the plan; an approved StackKits update cannot proceed before that"
	}
	return "the node's StackKits release did not compile the saved plan; roll the stack out again (deploy) so it compiles the plan, or approve the node's StackKits update where the StackKit supports an in-place upgrade"
}

// StackKitLifecycleFailedError is a lifecycle command that the pinned
// StackKits ended with its typed actionable error. The job keeps that reason
// instead of the generic error category.
type StackKitLifecycleFailedError struct {
	Operation string
	Failure   typedStackKitFailure
}

func (e *StackKitLifecycleFailedError) Error() string {
	if e.Failure.ReasonCode == StackKitReasonUpgradeKitUnsupported {
		return "StackKits refused the upgrade: the StackKit's upgrade policy does not support an in-place upgrade (" +
			StackKitReasonUpgradeKitUnsupported + ")"
	}
	if storedPlanNeedsRollout(e.Failure.ReasonCode) {
		return fmt.Sprintf("StackKits %s stopped: %s (%s)", e.Operation, storedPlanRolloutHint(e.Failure.ReasonCode), e.Failure.ReasonCode)
	}
	return fmt.Sprintf("StackKits %s stopped: %s", e.Operation, e.Failure.ReasonCode)
}

// typedStackKitFailure is the closed recovery fact a pinned StackKits command
// reported in its actionable-error envelope. It never carries message text.
type typedStackKitFailure struct {
	ReasonCode string
	Retryable  bool
	RetryAfter time.Time
}

var stackKitReasonCodePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,63}$`)

// typedStackKitFailureFromResult reads the stackkit.actionable-error/v1 detail
// of a failed command result. An absent, foreign, or malformed detail yields
// the zero value: the orchestrator then keeps its generic operation reason.
func typedStackKitFailureFromResult(result *agentpb.StackKitResult) typedStackKitFailure {
	if result == nil || result.Success || len(result.CommandResultJson) == 0 {
		return typedStackKitFailure{}
	}
	var envelope struct {
		Status string `json:"status"`
		Data   struct {
			SchemaVersion string `json:"schemaVersion"`
			ReasonCode    string `json:"reasonCode"`
			Retryable     bool   `json:"retryable"`
			RetryAfter    string `json:"retryAfter"`
		} `json:"data"`
	}
	if json.Unmarshal(result.CommandResultJson, &envelope) != nil || envelope.Status != "failed" ||
		envelope.Data.SchemaVersion != "stackkit.actionable-error/v1" ||
		!stackKitReasonCodePattern.MatchString(envelope.Data.ReasonCode) {
		return typedStackKitFailure{}
	}
	failure := typedStackKitFailure{ReasonCode: envelope.Data.ReasonCode, Retryable: envelope.Data.Retryable}
	if raw := strings.TrimSpace(envelope.Data.RetryAfter); raw != "" && failure.Retryable {
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			failure.RetryAfter = parsed.UTC()
		}
	}
	return failure
}

// JobResultRetryAfter returns the authority-named retry time a failed job
// recorded in its outcome envelope or typed StackKits rollout failure, if any.
func JobResultRetryAfter(result map[string]interface{}) (time.Time, bool) {
	if retryAfter, found := retryAfterValue(result["retry_after"]); found {
		return retryAfter, true
	}
	rollout, _ := result["typed_stackkit_rollout"].(map[string]interface{})
	return retryAfterValue(rollout["retry_after"])
}

func retryAfterValue(candidate interface{}) (time.Time, bool) {
	raw := ""
	switch value := candidate.(type) {
	case string:
		raw = value
	case time.Time:
		return value.UTC(), !value.IsZero()
	case *time.Time:
		if value == nil || value.IsZero() {
			return time.Time{}, false
		}
		return value.UTC(), true
	}
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil || parsed.IsZero() {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}
