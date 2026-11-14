package specv2

import (
	"slices"
	"sort"
)

// BlockedUseCaseAccess retains the wizard's capability refusal contract.
type BlockedUseCaseAccess struct {
	UseCase    string `json:"use_case"`
	Capability string `json:"capability"`
	Selection  string `json:"selection"`
	Reason     string `json:"reason,omitempty"`
}

type UseCaseAccessProjectionError struct {
	Blocked []BlockedUseCaseAccess
}

func (e *UseCaseAccessProjectionError) Error() string {
	return "specv2: use-case access cannot be enforced by the pinned StackKits release"
}

func blockedUseCaseAccess(intent WizardIntent, goals []string, reason string) *UseCaseAccessProjectionError {
	blocked := &UseCaseAccessProjectionError{}
	for _, goal := range goals {
		decision := intent.UseCaseAccess[goal]
		blocked.Blocked = append(blocked.Blocked, BlockedUseCaseAccess{
			UseCase: goal, Capability: "use-case-access", Selection: decision.Mode + "/" + decision.Audience, Reason: reason,
		})
	}
	return blocked
}

// v0.57.11's #ResolvedServiceRouteV2 admits application audiences only for
// native Immich/Immich Lite and Paperless, on every local HTTPS route at home.
// Project after release authoring, so neither missing routes nor a different
// installed alternative can turn an explicit narrowing into inherited access.
func projectUseCaseAccess(spec map[string]any, intent WizardIntent) error {
	goals := make([]string, 0, len(intent.UseCaseAccess))
	for goal := range intent.UseCaseAccess {
		goals = append(goals, goal)
	}
	sort.Strings(goals)
	routes, _ := spec["routes"].(map[string]any)
	policies, _ := spec["access"].(map[string]any)
	for _, goal := range goals {
		decision := intent.UseCaseAccess[goal]
		if decision.Mode != "inherit" {
			bridge, _ := spec["bridge"].(map[string]any)
			publications, _ := bridge["publications"].([]any)
			for _, raw := range publications {
				publication, _ := raw.(map[string]any)
				if publication["serviceRef"] == goal {
					return blockedUseCaseAccess(intent, []string{goal}, "the application is published away from home through a bridge")
				}
			}
		}
		var refs []string
		for _, raw := range routes {
			route, _ := raw.(map[string]any)
			if route["serviceRef"] != goal {
				continue
			}
			ref, _ := route["accessPolicyRef"].(string)
			policy, _ := policies[ref].(map[string]any)
			if decision.Mode != "inherit" {
				module, _ := route["moduleRef"].(string)
				supported := (goal == "photos" && slices.Contains([]string{"stackkits-immich-runtime", "stackkits-immich-lite-runtime"}, module)) ||
					(goal == "documents" && module == "stackkits-paperless-runtime")
				if !supported || route["protocol"] != "https" || (route["exposure"] != nil && route["exposure"] != "local") || policy == nil {
					return blockedUseCaseAccess(intent, []string{goal}, "every application route must have an enforceable local HTTPS access policy")
				}
				if (policy["exposure"] != nil && policy["exposure"] != "local-only") ||
					(policy["appAccessAway"] == true || policy["ownerRemoteOptIn"] == true) ||
					(policy["privilege"] != nil && policy["privilege"] != "user") ||
					(policy["authentication"] != nil && policy["authentication"] != "human" && policy["authentication"] != "human+device") {
					return blockedUseCaseAccess(intent, []string{goal}, "the application policy must require a local human user")
				}
			}
			if policy != nil {
				refs = append(refs, ref)
			}
		}
		if decision.Mode != "inherit" && (len(refs) == 0 || !localNativeAccessWorkload(spec, goal)) {
			return blockedUseCaseAccess(intent, []string{goal}, "the application requires a native workload at one home site, without runtime fallbacks")
		}
		// A shared policy must not change another application's audience.
		for _, raw := range routes {
			route, _ := raw.(map[string]any)
			ref, _ := route["accessPolicyRef"].(string)
			if route["serviceRef"] != goal && slices.Contains(refs, ref) {
				return blockedUseCaseAccess(intent, []string{goal}, "the application access policy is shared with another service")
			}
		}
		for _, ref := range refs {
			policy := policies[ref].(map[string]any)
			if decision.Mode == "inherit" {
				delete(policy, "audience")
			} else {
				policy["audience"] = map[string]any{"schema": "stackkit.application-audience/v1", "members": decision.Audience}
			}
		}
	}
	return nil
}

func localNativeAccessWorkload(spec map[string]any, goal string) bool {
	workloads, _ := spec["workloads"].(map[string]any)
	workload, _ := workloads[goal].(map[string]any)
	if workload == nil || (workload["runtimeAdapterRef"] != nil && workload["runtimeAdapterRef"] != "standalone-compose") {
		return false
	}
	fallbacks, _ := workload["runtimeAdapterFallbackRefs"].([]any)
	if len(fallbacks) > 0 {
		return false
	}
	placement, _ := workload["placement"].(map[string]any)
	siteRefs, _ := placement["siteRefs"].([]any)
	if len(siteRefs) == 0 {
		control, _ := spec["controlPlane"].(map[string]any)
		if authority, _ := control["authoritySiteRef"].(string); authority != "" {
			siteRefs = []any{authority}
		} else if nodes, err := specNodes(spec); err == nil {
			if primary := primaryNode(nodes); primary != nil {
				siteRefs = []any{primary["siteRef"]}
			}
		}
	}
	if len(siteRefs) != 1 {
		return false
	}
	sites, _ := spec["sites"].([]any)
	for _, raw := range sites {
		site, _ := raw.(map[string]any)
		if site["id"] == siteRefs[0] {
			return site["kind"] == "home"
		}
	}
	return false
}
