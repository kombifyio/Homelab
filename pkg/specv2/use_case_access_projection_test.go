package specv2

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// These fields are the v0.57.11 native authoring/access contract exercised by
// StackKits' TestLocalApplicationAudienceReachesEveryGeneratedAdmission.
func applicationAccessSeed(goal string) map[string]any {
	seed := basementSeed()
	seed["apiVersion"] = NativeSpecAPIVersion
	module, alternative := "stackkits-immich-runtime", "immich"
	if goal == "documents" {
		module, alternative = "stackkits-paperless-runtime", "paperless-ngx"
	}
	seed["workloads"] = map[string]any{goal: map[string]any{
		"alternative": alternative, "runtimeAdapterRef": "standalone-compose",
		"placement": map[string]any{"siteRefs": []any{"home"}},
	}}
	seed["routes"] = map[string]any{goal + "-https": map[string]any{
		"moduleRef": module, "serviceRef": goal, "protocol": "https", "exposure": "local",
		"host": goal + ".lab.home", "accessPolicyRef": goal + "-access",
	}}
	seed["access"] = map[string]any{goal + "-access": map[string]any{
		"exposure": "local-only", "authentication": "human", "privilege": "user",
	}}
	return seed
}

type applicationAccessAuthor struct{ document map[string]any }

func (a applicationAccessAuthor) AuthorGoals(context.Context, string, string, string, string, GoalSelection) (GoalAuthoring, error) {
	raw, err := json.Marshal(a.document)
	if err != nil {
		return GoalAuthoring{}, err
	}
	goal, module, alternative := "photos", "stackkits-immich-runtime", "immich"
	if a.document["workloads"].(map[string]any)[goal] == nil {
		goal, module, alternative = "documents", "stackkits-paperless-runtime", "paperless-ngx"
	}
	return goalAuthoringFromDocument(raw, KitSlugBasement, []string{goal}, nil, map[string]bool{goal: true},
		map[string]workloadModuleBinding{goal: {AlternativeRef: alternative, ModuleRef: module}})
}

func accessProjector(document map[string]any) *ReleaseProjector {
	p := NewReleaseProjector(applicationAccessAuthor{document})
	p.catalog = nil
	return p
}

func TestProjectUseCaseAccessAudience(t *testing.T) {
	for _, goal := range []string{"photos", "documents"} {
		for _, audience := range []string{"owner", "household"} {
			t.Run(goal+"/"+audience, func(t *testing.T) {
				seed := applicationAccessSeed(goal)
				seed["modules"] = map[string]any{"stackkits-immich-runtime": map[string]any{"computeProfile": "standard"}, "stackkits-paperless-runtime": map[string]any{"computeProfile": "standard"}}
				before := deepCopyValue(seed)
				intent := foundIntent("access", goal)
				intent.UseCaseAccess = map[string]UseCaseAccessIntent{goal: {Mode: AccessModeLocal, Audience: audience}}
				// Exercise a new workload, through release-owned authoring and merging.
				result, err := accessProjector(seed).Project(context.Background(), basementSeed(), intent, "")
				if err != nil {
					t.Fatal(err)
				}
				policy := result.Spec["access"].(map[string]any)[goal+"-access"].(map[string]any)
				want := map[string]any{"schema": "stackkit.application-audience/v1", "members": audience}
				if !reflect.DeepEqual(policy["audience"], want) {
					t.Fatalf("audience = %v", policy["audience"])
				}
				if !reflect.DeepEqual(seed, before) {
					t.Fatal("mutated release authoring")
				}
			})
		}
	}
}

func TestProjectUseCaseAccessRefusesUnenforceableDecisions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		goal     string
		decision UseCaseAccessIntent
		mutate   func(map[string]any)
	}{
		{"selected people", "photos", UseCaseAccessIntent{Mode: AccessModeLocal, Audience: "selected", PersonRefs: []string{"household-person"}}, nil},
		{"away from home", "photos", UseCaseAccessIntent{Mode: AccessModeRemotePrivate, Audience: "owner"}, nil},
		{"unsupported use case", "files", UseCaseAccessIntent{Mode: AccessModeLocal, Audience: "household"}, nil},
		{"missing route", "photos", UseCaseAccessIntent{Mode: AccessModeLocal, Audience: "owner"}, func(s map[string]any) { delete(s, "routes") }},
		{"different module", "photos", UseCaseAccessIntent{Mode: AccessModeLocal, Audience: "owner"}, func(s map[string]any) {
			s["routes"].(map[string]any)["photos-https"].(map[string]any)["moduleRef"] = "another-runtime"
		}},
		{"cloud placement", "photos", UseCaseAccessIntent{Mode: AccessModeLocal, Audience: "owner"}, func(s map[string]any) { s["sites"].([]any)[0].(map[string]any)["kind"] = "cloud" }},
		{"different runtime", "photos", UseCaseAccessIntent{Mode: AccessModeLocal, Audience: "owner"}, func(s map[string]any) {
			s["workloads"].(map[string]any)["photos"].(map[string]any)["runtimeAdapterRef"] = "komodo"
		}},
		{"runtime fallback", "photos", UseCaseAccessIntent{Mode: AccessModeLocal, Audience: "owner"}, func(s map[string]any) {
			s["workloads"].(map[string]any)["photos"].(map[string]any)["runtimeAdapterFallbackRefs"] = []any{"komodo"}
		}},
		{"remote alias", "photos", UseCaseAccessIntent{Mode: AccessModeLocal, Audience: "owner"}, func(s map[string]any) {
			route := deepCopyValue(s["routes"].(map[string]any)["photos-https"]).(map[string]any)
			route["exposure"] = "public"
			s["routes"].(map[string]any)["public-alias"] = route
		}},
		{"bridge publication", "photos", UseCaseAccessIntent{Mode: AccessModeLocal, Audience: "owner"}, func(s map[string]any) {
			s["bridge"] = map[string]any{"publications": []any{map[string]any{"serviceRef": "photos", "sourceSiteRef": "home", "edgeSiteRef": "cloud"}}}
		}},
		{"app access away", "photos", UseCaseAccessIntent{Mode: AccessModeLocal, Audience: "owner"}, func(s map[string]any) {
			s["access"].(map[string]any)["photos-access"].(map[string]any)["appAccessAway"] = true
		}},
		{"owner remote opt in", "photos", UseCaseAccessIntent{Mode: AccessModeLocal, Audience: "household"}, func(s map[string]any) {
			s["access"].(map[string]any)["photos-access"].(map[string]any)["ownerRemoteOptIn"] = true
		}},
		{"shared policy", "photos", UseCaseAccessIntent{Mode: AccessModeLocal, Audience: "owner"}, func(s map[string]any) {
			s["routes"].(map[string]any)["other"] = map[string]any{"serviceRef": "files", "accessPolicyRef": "photos-access"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seed := applicationAccessSeed(tc.goal)
			if tc.mutate != nil {
				tc.mutate(seed)
			}
			before := deepCopyValue(seed)
			intent := foundIntent("refused", tc.goal)
			intent.UseCaseAccess = map[string]UseCaseAccessIntent{tc.goal: tc.decision}
			p := NewReleaseProjector(releaseGoalAuthorFixture{})
			p.catalog = nil
			result, err := p.Project(context.Background(), seed, intent, "")
			var blocked *UseCaseAccessProjectionError
			if result != nil || !errors.As(err, &blocked) || len(blocked.Blocked) != 1 || blocked.Blocked[0].UseCase != tc.goal || blocked.Blocked[0].Capability != "use-case-access" || blocked.Blocked[0].Reason == "" {
				t.Fatalf("projection=%v refusal=%v", result, err)
			}
			if !reflect.DeepEqual(seed, before) {
				t.Fatal("refused decision mutated seed")
			}
		})
	}
}

func TestProjectUseCaseAccessInheritanceAndAliases(t *testing.T) {
	for _, mode := range []string{"omitted", "inherit", "owner", "household"} {
		t.Run(mode, func(t *testing.T) {
			seed := applicationAccessSeed("photos")
			policies := seed["access"].(map[string]any)
			policies["photos-access"].(map[string]any)["audience"] = map[string]any{"schema": "stackkit.application-audience/v1", "members": "owner"}
			policies["alias-access"] = deepCopyValue(policies["photos-access"])
			policies["unrelated"] = map[string]any{"authentication": "human+device", "privilege": "admin"}
			routes := seed["routes"].(map[string]any)
			routes["alias"] = deepCopyValue(routes["photos-https"])
			routes["alias"].(map[string]any)["accessPolicyRef"] = "alias-access"
			before := deepCopyValue(seed)
			intent := foundIntent("inherit", "photos")
			if mode == "inherit" {
				intent.UseCaseAccess = map[string]UseCaseAccessIntent{"photos": {Mode: "inherit"}}
			}
			if mode == "owner" || mode == "household" {
				intent.UseCaseAccess = map[string]UseCaseAccessIntent{"photos": {Mode: AccessModeLocal, Audience: mode}}
			}
			p := NewReleaseProjector(releaseGoalAuthorFixture{})
			p.catalog = nil
			result, err := p.Project(context.Background(), seed, intent, "")
			if err != nil {
				t.Fatal(err)
			}
			for _, ref := range []string{"photos-access", "alias-access"} {
				policy := result.Spec["access"].(map[string]any)[ref].(map[string]any)
				if mode == "inherit" {
					if _, exists := policy["audience"]; exists {
						t.Fatal("inherit kept audience")
					}
				} else {
					want := mode
					if mode == "omitted" {
						want = "owner"
					}
					if policy["audience"].(map[string]any)["members"] != want {
						t.Fatalf("alias audience=%v", policy)
					}
				}
			}
			if !reflect.DeepEqual(result.Spec["access"].(map[string]any)["unrelated"], policies["unrelated"]) || !reflect.DeepEqual(seed, before) {
				t.Fatal("changed unrelated policy or seed")
			}
		})
	}
}
