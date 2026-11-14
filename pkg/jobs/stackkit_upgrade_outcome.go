package jobs

import (
	"encoding/json"
	"strings"
)

// UpgradeOutcomeResultField is the job result field that states, in one
// object, what an owner-approved StackKits upgrade (the service `update`
// action) did on the node: applied and verified, or automatically returned to
// the release it ran before because the target failed its live verification.
const UpgradeOutcomeResultField = "update_outcome"

const upgradeTransactionAPIVersion = "stackkit.upgrade-transaction/v1"

// upgradeOutcome projects the node's `stackkit.upgrade-transaction/v1` result
// (the CLI's upgrade transaction, carried in the command result) into the
// compact outcome the job reports. It returns nil when the command result
// holds no upgrade transaction; it never infers an outcome from stderr. A
// committed transaction counts as updated only when the command itself
// succeeded: the CLI can commit the release and still fail afterwards (for
// example while it records application evidence), and Techstack then records
// the target as failed.
func upgradeOutcome(commandResult map[string]interface{}, commandSucceeded bool) map[string]interface{} {
	transaction := findUpgradeTransaction(commandResult, 0)
	if transaction == nil {
		return nil
	}
	target, _ := transaction["target"].(map[string]interface{})
	rollback, _ := transaction["rollback"].(map[string]interface{})
	status := strings.TrimSpace(stringFromInterface(transaction["status"]))
	outcome := map[string]interface{}{
		"status":            status,
		"operation_id":      stringFromInterface(transaction["operationId"]),
		"target_release":    stringFromInterface(target["releaseVersion"]),
		"target_verified":   target["verified"] == true,
		"restore_point":     stringFromInterface(rollback["recoverySnapshotId"]),
		"rollback_status":   stringFromInterface(rollback["status"]),
		"rollback_verified": rollback["verified"] == true,
	}
	if prior := stringFromInterface(rollback["priorReleaseVersion"]); prior != "" {
		outcome["previous_release"] = prior
	}
	if phase := stringFromInterface(transaction["failedPhase"]); phase != "" {
		outcome["failed_phase"] = phase
	}
	switch {
	case status == "succeeded" && commandSucceeded:
		outcome["result"] = "updated"
	case status == "succeeded":
		outcome["result"] = "failed"
		outcome["failed_phase"] = "after-commit"
	case status == "rolled-back":
		outcome["result"] = "returned_to_previous_release"
	default:
		if strings.HasPrefix(stringFromInterface(rollback["status"]), "rollback-failed") {
			outcome["result"] = "rollback_failed"
		} else {
			outcome["result"] = "failed"
		}
	}
	return outcome
}

// findUpgradeTransaction finds the upgrade transaction object in the bounded
// command result: the CLI's typed JSON (its result in `data`), or the Guard
// envelope that carries the CLI's JSON stdout as the `data.output` string.
func findUpgradeTransaction(value interface{}, depth int) map[string]interface{} {
	if depth > 4 {
		return nil
	}
	switch typed := value.(type) {
	case map[string]interface{}:
		if stringFromInterface(typed["apiVersion"]) == upgradeTransactionAPIVersion {
			return typed
		}
		for _, key := range []string{"data", "result", "transaction"} {
			if found := findUpgradeTransaction(typed[key], depth+1); found != nil {
				return found
			}
		}
		if output := strings.TrimSpace(stringFromInterface(typed["output"])); strings.HasPrefix(output, "{") && len(output) <= 1<<20 {
			var parsed map[string]interface{}
			if json.Unmarshal([]byte(output), &parsed) == nil {
				return findUpgradeTransaction(parsed, depth+1)
			}
		}
	}
	return nil
}
