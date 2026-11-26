package stacks

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/jobs"
)

// Exercise the closed HTTP inputs, not the recoverable internal job type.
func TestManagedWikiHTTPInputsRejectServerOwnedAdmissions(t *testing.T) {
	hash := "sha256:" + strings.Repeat("a", 64)
	cases := []struct {
		name  string
		valid map[string]any
		read  func(*httpx.Event) error
	}{
		{"configuration", map[string]any{"expected_spec_hash": hash, "compute_profile": "small"}, func(e *httpx.Event) error { _, err := readUseCaseConfigurationRequest(e); return err }},
		{"removal", map[string]any{"expected_spec_hash": hash, "selector_kind": "workload", "selector_ref": "wiki"}, func(e *httpx.Event) error { _, err := readUseCaseRemovalRequest(e); return err }},
		{"lifecycle", map[string]any{"agent_id": "agent", "operation": "apply"}, func(e *httpx.Event) error {
			var body stackKitLifecycleRequest
			return decodeStrictJSONBody(e.Request.Body, &body)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			read := func(body map[string]any) error {
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodPost, "/api/v1/stacks/stack/use-cases/wiki/configuration", strings.NewReader(string(raw)))
				request.SetPathValue("workload", "wiki")
				return tc.read(&httpx.Event{Request: request, Response: httptest.NewRecorder()})
			}
			if err := read(tc.valid); err != nil {
				t.Fatalf("valid input: %v", err)
			}
			for _, key := range []string{"wiki_install_admission", "managed_wiki_install_admission", "WikiInstallAdmission"} {
				tc.valid[key] = `{"tenant_id":"tenant","owner_id":"owner","stack_id":"stack","source":"rollout-decision"}`
				if err := read(tc.valid); err == nil {
					t.Fatalf("HTTP input accepted %s", key)
				}
				delete(tc.valid, key)
			}
		})
	}
}

func TestManagedWikiOffJoinPreservesCommittedSelectionWithoutHistory(t *testing.T) {
	const committedJSON = `{"nodes":[{"id":"home"}],"modules":{"stackkits-wiki-runtime":{"computeProfile":"small"}},"workloads":{"wiki":{"enabled":true,"placement":{"nodeRefs":["home"]},"secretRefs":{"registry-auth":"secret://stackkits/wiki/registry-auth"}},"photos":{"enabled":true}}}`
	load := func(raw string) map[string]any {
		var spec map[string]any
		if err := json.Unmarshal([]byte(raw), &spec); err != nil {
			t.Fatal(err)
		}
		return spec
	}
	committed := load(committedJSON)
	projection := load(committedJSON)
	delete(projection["workloads"].(map[string]any), "wiki")
	delete(projection["modules"].(map[string]any), "stackkits-wiki-runtime")
	if err := preserveExistingWikiOnly(projection, committed); err != nil {
		t.Fatalf("preserve committed: %v", err)
	}
	if projection["workloads"].(map[string]any)["wiki"] == nil || projection["workloads"].(map[string]any)["photos"] == nil {
		t.Fatal("committed Wiki or unrelated goal lost")
	}
	for _, change := range []func(map[string]any){
		func(s map[string]any) { s["workloads"].(map[string]any)["wiki"].(map[string]any)["enabled"] = false },
		func(s map[string]any) {
			s["modules"].(map[string]any)["stackkits-wiki-runtime"].(map[string]any)["computeProfile"] = "large"
		},
	} {
		projection := load(committedJSON)
		change(projection)
		if err := preserveExistingWikiOnly(projection, committed); !errors.Is(err, jobs.ErrManagedWikiInstallNotAdmitted) {
			t.Fatalf("conflicting projection: %v", err)
		}
		raw, _ := json.Marshal(projection)
		selected, err := jobs.ManagedWikiSelectionPresent(raw)
		if err != nil {
			t.Fatal(err)
		}
		err = preserveExistingWikiOnly(projection, committed, true)
		if selected && err != nil {
			t.Fatalf("admitted changed selection: %v", err)
		}
		if !selected && !errors.Is(err, jobs.ErrManagedWikiInstallNotAdmitted) {
			t.Fatalf("flag silently removed Wiki: %v", err)
		}
	}
	projection = load(committedJSON)
	if err := preserveExistingWikiOnly(projection, nil); err != nil {
		t.Fatal(err)
	}
	if projection["workloads"].(map[string]any)["wiki"] != nil || projection["modules"].(map[string]any)["stackkits-wiki-runtime"] != nil {
		t.Fatal("unadmitted seed selected Wiki")
	}
}

func TestManagedWikiRecoveryDenialsAre403(t *testing.T) {
	for _, write := range []func(*httpx.Event, error) error{writeRolloutRetryError, writeEnrollmentResumeError} {
		response := httptest.NewRecorder()
		event := &httpx.Event{Request: httptest.NewRequest(http.MethodPost, "/", nil), Response: response}
		_ = write(event, jobs.ErrManagedWikiInstallNotAdmitted)
		if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "managed_wiki_install_disabled") {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
}

func TestManagedWikiProjectionComparesSelectionDigest(t *testing.T) {
	committed := map[string]any{"workloads": map[string]any{"wiki": map[string]any{"enabled": true, "replicas": float64(1)}}}
	projection := map[string]any{"workloads": map[string]any{"wiki": map[string]any{"enabled": true, "replicas": 1}}}
	if err := preserveExistingWikiOnly(projection, committed); err != nil {
		t.Fatalf("JSON-equivalent numeric representation refused: %v", err)
	}
}
