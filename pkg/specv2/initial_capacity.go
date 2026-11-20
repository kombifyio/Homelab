package specv2

import (
	"encoding/json"
	"github.com/kombifyio/homelab/internal/runtimeproduct/runtimeaction"
	"strings"
)

// WithInitialCapacityFailures projects durable wizard outcomes without altering failures.
// Initial exclusions are kept in the durable desired configuration. Only a
// successful apply can be completed degraded: core, auth, custody and compiler
// failures return before this projection and retain their original semantics.
func WithInitialCapacityFailures(result, payload map[string]interface{}) map[string]interface{} {
	if result == nil {
		return result
	}
	status := capacityOutcomeString(result["status"])
	if status != string(runtimeaction.StatusApplied) && status != string(runtimeaction.StatusCompletedDegraded) {
		return result
	}
	spec, _ := payload["spec"].(map[string]any)
	raw := spec["initial_capacity_failures"]
	if raw == nil {
		return result
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return result
	}
	var failures []map[string]interface{}
	if json.Unmarshal(data, &failures) != nil {
		return result
	}
	components, _ := result["degraded_components"].([]interface{})
	seen := map[string]bool{}
	for _, raw := range components {
		if component, ok := raw.(map[string]any); ok {
			seen[capacityOutcomeString(component["component"])] = true
		}
	}
	for _, failure := range failures {
		if !strings.HasPrefix(capacityOutcomeString(failure["component"]), "use_case:") || failure["reason_code"] != "capacity_memory" || failure["stage"] != "stackkit_rollout" {
			continue
		}
		name := capacityOutcomeString(failure["component"])
		if seen[name] {
			continue
		}
		seen[name] = true
		components = append(components, failure)
	}
	if len(components) > 0 {
		result["degraded_components"] = components
		result["status"] = string(runtimeaction.StatusCompletedDegraded)
	}
	return result
}

func capacityOutcomeString(value any) string { text, _ := value.(string); return text }
