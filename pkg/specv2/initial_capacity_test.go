package specv2

import (
	"encoding/json"
	"testing"
)

func TestInitialCapacityOutcomesSurviveJobWireAndSuccessfulApply(t *testing.T) {
	intent := WizardIntent{Goals: []string{"photos", "files", "vault"}}
	payload := map[string]any{"spec": map[string]any{"initial_requested_intent": intent, "initial_capacity_failures": []map[string]any{{"component": "use_case:photos", "reason_code": "capacity_memory", "stage": "stackkit_rollout", "status": "failed", "retryable": false, "params": map[string]any{"need_gb": 6, "have_gb": 4}}}}}
	// Durable job persistence round-trips the request through JSON.
	wire, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var restored map[string]any
	if err := json.Unmarshal(wire, &restored); err != nil {
		t.Fatal(err)
	}
	result := map[string]any{"status": "applied", "degraded_components": []any{map[string]any{"component": "public_tls", "retryable": true}}}
	for i := 0; i < 2; i++ {
		result = WithInitialCapacityFailures(result, restored)
	}
	components := result["degraded_components"].([]any)
	if result["status"] != "completed_degraded" || len(components) != 2 {
		t.Fatalf("successful apply did not retain independent outcomes exactly once: %+v", result)
	}
	failure := components[1].(map[string]any)
	if failure["component"] != "use_case:photos" || failure["retryable"] != false || failure["status"] != "failed" {
		t.Fatalf("capacity outcome wire changed: %+v", failure)
	}
	saved := restored["spec"].(map[string]any)["initial_requested_intent"].(map[string]any)
	if len(saved["goals"].([]any)) != 3 {
		t.Fatal("runtime subset erased desired selection")
	}
	failed := map[string]any{"status": "failed", "reason_code": "core_apply_failed"}
	WithInitialCapacityFailures(failed, restored)
	if failed["status"] != "failed" || failed["degraded_components"] != nil {
		t.Fatalf("core failure was masked: %+v", failed)
	}
}
