package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kombifyio/homelab/internal/stackkitrelease"
)

const selectedWiki = `{"nodes":[{"id":"home","siteRef":"personal"},{"id":"other","siteRef":"personal"}],"workloads":{"wiki":{"enabled":true,"placement":{"nodeRefs":["home"]},"secretRefs":{"registry-auth":"secret://stackkits/wiki/registry-auth"}}}}`
const noWiki = `{"nodes":[{"id":"home"}],"workloads":{}}`

func wikiRequest() StackKitLifecycleRequest {
	return StackKitLifecycleRequest{TenantID: "tenant", OwnerID: "owner", StackID: "stack", AgentID: "agent", NodeRef: "home", Operation: StackKitLifecycleApply, CandidateSpecJSON: []byte(selectedWiki)}
}

func TestManagedWikiAdditionNeedsTheActingOwnerAndTenantAtCommit(t *testing.T) {
	for _, ctx := range []context.Context{t.Context(), WithManagedWikiInstallAdmission(t.Context(), "other", "owner"), WithManagedWikiInstallAdmission(t.Context(), "tenant", "other")} {
		if _, err := PrepareManagedWikiChange(ctx, "tenant", "owner", "stack", []byte(noWiki), []byte(selectedWiki), false); !errors.Is(err, ErrManagedWikiInstallNotAdmitted) {
			t.Fatalf("unsigned or foreign addition: %v", err)
		}
	}
	admission, err := PrepareManagedWikiChange(WithManagedWikiInstallAdmission(t.Context(), "tenant", "owner"), "tenant", "owner", "stack", []byte(noWiki), []byte(selectedWiki), false)
	if err != nil {
		t.Fatal(err)
	}
	request := wikiRequest()
	request.WikiInstallAdmission = admission
	if err := ValidateManagedWikiInstallCheckpoint([]byte(selectedWiki), request); err != nil {
		t.Fatalf("committed addition: %v", err)
	}
	if err := ValidateManagedWikiInstallCheckpoint([]byte(noWiki), request); !errors.Is(err, ErrManagedWikiInstallNotAdmitted) {
		t.Fatalf("uncommitted candidate self-authorized: %v", err)
	}
}

func TestManagedWikiCommittedSelectionNeedsNoNewDecision(t *testing.T) {
	for _, operation := range []string{StackKitLifecycleApply, StackKitLifecycleAdvancedChangeSet, StackKitLifecycleDriftReconcile, StackKitLifecycleApplicationUpdate} {
		request := wikiRequest()
		request.Operation, request.UseCase = operation, ManagedWikiWorkload
		if err := BindManagedWikiLifecycle(t.Context(), &request, []byte(selectedWiki)); err != nil {
			t.Fatalf("%s: %v", operation, err)
		}
		if request.WikiInstallAdmission == "" {
			t.Fatal("committed digest was not carried")
		}
		if err := ValidateManagedWikiInstallCheckpoint([]byte(selectedWiki), request); err != nil {
			t.Fatal(err)
		}
	}
	request := wikiRequest()
	request.Operation, request.UseCase, request.CandidateSpecJSON = StackKitLifecycleApplicationUpdate, "paperwork", nil
	if err := BindManagedWikiLifecycle(t.Context(), &request, []byte(selectedWiki)); err != nil {
		t.Fatalf("unrelated application update on a Wiki stack: %v", err)
	}
	if request.WikiInstallAdmission == "" {
		t.Fatal("Paperwork update lost committed Wiki digest")
	}
	if err := ValidateManagedWikiExecution(t.Context(), StackKitLifecycleConfig{CustomerRegistrySpec: func(context.Context, StackKitLifecycleRequest) ([]byte, error) { return []byte(selectedWiki), nil }}, request); err != nil {
		t.Fatalf("Paperwork renewal refused committed Wiki: %v", err)
	}
}

func TestManagedWikiRemovalMustBeExplicit(t *testing.T) {
	if _, err := PrepareManagedWikiChange(t.Context(), "tenant", "owner", "stack", []byte(selectedWiki), []byte(noWiki), false); !errors.Is(err, ErrManagedWikiInstallNotAdmitted) {
		t.Fatalf("implicit removal: %v", err)
	}
	receipt, err := PrepareManagedWikiChange(t.Context(), "tenant", "owner", "stack", []byte(selectedWiki), []byte(noWiki), true)
	if err != nil {
		t.Fatal(err)
	}
	request := wikiRequest()
	request.CandidateSpecJSON, request.WikiInstallAdmission = []byte(noWiki), receipt
	if err := ValidateManagedWikiInstallCheckpoint([]byte(noWiki), request); err != nil {
		t.Fatalf("explicit committed removal: %v", err)
	}
}

func TestManagedWikiDurableDigestBindsTheCurrentCommittedSpec(t *testing.T) {
	request := wikiRequest()
	if err := BindManagedWikiLifecycle(t.Context(), &request, []byte(selectedWiki)); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var recovered StackKitLifecycleRequest
	if err := json.Unmarshal(encoded, &recovered); err != nil {
		t.Fatal(err)
	}
	if err := ValidateManagedWikiInstallCheckpoint([]byte(selectedWiki), recovered); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*StackKitLifecycleRequest){
		func(r *StackKitLifecycleRequest) { r.TenantID = "other" },
		func(r *StackKitLifecycleRequest) { r.OwnerID = "other" },
		func(r *StackKitLifecycleRequest) { r.StackID = "other" },
		func(r *StackKitLifecycleRequest) { r.CandidateSpecJSON = []byte(noWiki) },
		func(r *StackKitLifecycleRequest) {
			r.CandidateSpecJSON = []byte(strings.Replace(selectedWiki, `"workloads":{`, `"workloads":{"photos":{"enabled":true},`, 1))
		},
		func(r *StackKitLifecycleRequest) { r.WikiInstallAdmission = "fabricated" },
	} {
		changed := recovered
		change(&changed)
		if err := ValidateManagedWikiInstallCheckpoint([]byte(selectedWiki), changed); !errors.Is(err, ErrManagedWikiInstallNotAdmitted) {
			t.Fatalf("changed checkpoint: %v", err)
		}
	}
	changedSpec := strings.Replace(selectedWiki, `"workloads":{`, `"workloads":{"photos":{"enabled":true},`, 1)
	if err := ValidateManagedWikiInstallCheckpoint([]byte(changedSpec), recovered); !errors.Is(err, ErrManagedWikiInstallNotAdmitted) {
		t.Fatalf("digest ignored a newer committed spec: %v", err)
	}
}

func TestManagedWikiRollbackUsesCommittedTargetWithoutACurrentFlag(t *testing.T) {
	request := wikiRequest()
	request.Operation, request.CandidateSpecJSON, request.RollbackTargetRef = StackKitLifecycleRollback, nil, "snapshot-a"
	if err := BindManagedWikiLifecycle(t.Context(), &request, []byte(selectedWiki)); err != nil {
		t.Fatalf("committed Wiki rollback: %v", err)
	}
	request.RollbackTargetRef = "snapshot-b"
	if err := ValidateManagedWikiInstallCheckpoint([]byte(selectedWiki), request); !errors.Is(err, ErrManagedWikiInstallNotAdmitted) {
		t.Fatalf("changed target: %v", err)
	}
	request.WikiInstallAdmission = ""
	if err := BindManagedWikiLifecycle(t.Context(), &request, []byte(noWiki)); err != nil {
		t.Fatalf("attributed Wiki-free target with flag off: %v", err)
	}
	if err := ValidateManagedWikiInstallCheckpoint([]byte(noWiki), request); err != nil {
		t.Fatal(err)
	}
	request.WikiInstallAdmission = ""
	if err := BindManagedWikiLifecycle(t.Context(), &request, nil); !errors.Is(err, ErrManagedWikiInstallNotAdmitted) {
		t.Fatalf("unattributed target: %v", err)
	}
	request.WikiInstallAdmission = ""
	if err := BindManagedWikiLifecycle(WithManagedWikiInstallAdmission(t.Context(), "tenant", "owner"), &request, nil); !errors.Is(err, ErrManagedWikiInstallNotAdmitted) {
		t.Fatalf("flag authorized opaque target: %v", err)
	}
	ordinary := wikiRequest()
	if err := BindManagedWikiLifecycle(t.Context(), &ordinary, []byte(selectedWiki)); err != nil {
		t.Fatal(err)
	}
	ordinary.Operation, ordinary.RollbackTargetRef = StackKitLifecycleRollback, "snapshot"
	if err := ValidateManagedWikiInstallCheckpoint([]byte(selectedWiki), ordinary); !errors.Is(err, ErrManagedWikiInstallNotAdmitted) {
		t.Fatalf("ordinary digest replayed as rollback: %v", err)
	}
}

func TestManagedWikiRegistryComparesCommittedSpecBeforeIssuing(t *testing.T) {
	for _, offline := range []bool{false, true} {
		request := wikiRequest()
		request.Offline = offline
		if err := BindManagedWikiLifecycle(t.Context(), &request, []byte(selectedWiki)); err != nil {
			t.Fatal(err)
		}
		cfg := StackKitLifecycleConfig{
			CustomerRegistrySpec: func(context.Context, StackKitLifecycleRequest) ([]byte, error) { return []byte(noWiki), nil },
			CustomerRegistryRenew: func(context.Context, StackKitLifecycleRequest, string) (string, error) {
				t.Fatal("uncommitted Wiki reached registry issuance")
				return "", nil
			},
		}
		if err := renewCustomerRegistryPull(t.Context(), cfg, "job", request, stackkitrelease.Release{}, nil); !errors.Is(err, ErrManagedWikiInstallNotAdmitted) {
			t.Fatalf("offline=%t: %v", offline, err)
		}
	}
}

func TestManagedWikiParseErrorIsNotAnAdmissionDenial(t *testing.T) {
	_, err := PrepareManagedWikiChange(t.Context(), "tenant", "owner", "stack", []byte(noWiki), []byte(`{"workloads":`), false)
	if err == nil || errors.Is(err, ErrManagedWikiInstallNotAdmitted) {
		t.Fatalf("parse error classified as admission denial: %v", err)
	}
}

func TestManagedWikiExecutionCannotStripANewerCommittedSelection(t *testing.T) {
	request := wikiRequest()
	request.CandidateSpecJSON = []byte(noWiki)
	cfg := StackKitLifecycleConfig{CustomerRegistrySpec: func(context.Context, StackKitLifecycleRequest) ([]byte, error) {
		return []byte(selectedWiki), nil
	}}
	if err := ValidateManagedWikiExecution(t.Context(), cfg, request); !errors.Is(err, ErrManagedWikiInstallNotAdmitted) {
		t.Fatalf("stale Wiki-free job could strip committed Wiki: %v", err)
	}
	cfg.CustomerRegistrySpec = func(context.Context, StackKitLifecycleRequest) ([]byte, error) { return []byte(noWiki), nil }
	if err := ValidateManagedWikiExecution(t.Context(), cfg, request); err != nil {
		t.Fatalf("unrelated Wiki-free job: %v", err)
	}
	request.CandidateSpecJSON = []byte(selectedWiki)
	if err := ValidateManagedWikiExecution(t.Context(), cfg, request); !errors.Is(err, ErrManagedWikiInstallNotAdmitted) {
		t.Fatalf("unadmitted detached Wiki candidate: %v", err)
	}
}

func TestManagedWikiDryRunDoesNotGrantAdmission(t *testing.T) {
	request := wikiRequest()
	request.DryRun = true
	if err := BindManagedWikiLifecycle(t.Context(), &request, []byte(noWiki)); err != nil || request.WikiInstallAdmission != "" {
		t.Fatalf("dry run granted admission: %q %v", request.WikiInstallAdmission, err)
	}
}

func TestManagedWikiRollbackExecutionUsesTargetSnapshotDigest(t *testing.T) {
	request := wikiRequest()
	request.Operation, request.CandidateSpecJSON, request.RollbackTargetRef = StackKitLifecycleRollback, nil, "snapshot"
	if err := BindManagedWikiLifecycle(t.Context(), &request, []byte(selectedWiki)); err != nil {
		t.Fatal(err)
	}
	cfg := StackKitLifecycleConfig{CustomerRegistrySpec: func(context.Context, StackKitLifecycleRequest) ([]byte, error) { return []byte(selectedWiki), nil }}
	if err := ValidateManagedWikiExecution(t.Context(), cfg, request); err != nil {
		t.Fatal(err)
	}
	cfg.CustomerRegistrySpec = func(context.Context, StackKitLifecycleRequest) ([]byte, error) { return []byte(noWiki), nil }
	if err := ValidateManagedWikiExecution(t.Context(), cfg, request); !errors.Is(err, ErrManagedWikiInstallNotAdmitted) {
		t.Fatalf("changed rollback snapshot: %v", err)
	}
}

func TestManagedWikiCanonicalCandidateNeedsBoundSource(t *testing.T) {
	canonical := []byte(strings.Replace(selectedWiki, `"enabled":true`, `"enabled":true,"replicas":1`, 1))
	request := wikiRequest()
	request.CandidateSpecJSON = canonical
	if err := BindManagedWikiLifecycle(t.Context(), &request, []byte(selectedWiki)); !errors.Is(err, ErrManagedWikiInstallNotAdmitted) {
		t.Fatalf("unbound normalization: %v", err)
	}
	if err := BindManagedWikiLifecycle(t.Context(), &request, []byte(selectedWiki), canonical); err != nil {
		t.Fatalf("bound normalization: %v", err)
	}
	if err := ValidateManagedWikiInstallCheckpoint([]byte(selectedWiki), request); err != nil {
		t.Fatal(err)
	}
	request.CandidateSpecJSON = []byte(strings.Replace(string(canonical), `"replicas":1`, `"replicas":2`, 1))
	if err := ValidateManagedWikiInstallCheckpoint([]byte(selectedWiki), request); !errors.Is(err, ErrManagedWikiInstallNotAdmitted) {
		t.Fatalf("altered normalization: %v", err)
	}
}

func TestManagedWikiOldCheckpointShapeFailsClosed(t *testing.T) {
	request := wikiRequest()
	if err := BindManagedWikiLifecycle(t.Context(), &request, []byte(selectedWiki)); err != nil {
		t.Fatal(err)
	}
	var old map[string]any
	if err := json.Unmarshal([]byte(request.WikiInstallAdmission), &old); err != nil {
		t.Fatal(err)
	}
	delete(old, "kind")
	delete(old, "source_sha256")
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	request.WikiInstallAdmission = string(raw)
	if err := ValidateManagedWikiInstallCheckpoint([]byte(selectedWiki), request); !errors.Is(err, ErrManagedWikiInstallNotAdmitted) {
		t.Fatalf("old checkpoint fabricated admission: %v", err)
	}
}

func TestManagedWikiExistingEditCarriesItsSourceAndResult(t *testing.T) {
	candidate := []byte(strings.Replace(selectedWiki, `"enabled":true`, `"enabled":true,"replicas":2`, 1))
	admission, err := PrepareManagedWikiChange(t.Context(), "tenant", "owner", "stack", []byte(selectedWiki), candidate, false)
	if err != nil {
		t.Fatal(err)
	}
	request := wikiRequest()
	request.CandidateSpecJSON = candidate
	request.WikiInstallAdmission = admission
	if err := ValidateManagedWikiInstallCheckpoint(candidate, request); err != nil {
		t.Fatalf("existing edit rejected after commit: %v", err)
	}
	if err := ValidateManagedWikiInstallCheckpoint([]byte(selectedWiki), request); !errors.Is(err, ErrManagedWikiInstallNotAdmitted) {
		t.Fatalf("uncommitted edit accepted: %v", err)
	}
}

// The pinned catalog offers the Wiki only with an admitted installation and
// keeps every other use case either way.
func TestManagedWikiIsOfferedOnlyWhenAdmitted(t *testing.T) {
	catalog, err := stackkitrelease.PinnedUseCaseCatalog()
	if err != nil {
		t.Fatal(err)
	}
	ids := func(catalog stackkitrelease.UseCaseCatalog) map[string]bool {
		present := map[string]bool{}
		for _, useCase := range catalog.UseCases {
			present[useCase.ID] = true
		}
		return present
	}
	full, offered, admitted := ids(catalog), ids(OfferedUseCaseCatalog(catalog, false)), ids(OfferedUseCaseCatalog(catalog, true))
	if !full[ManagedWikiWorkload] || !admitted[ManagedWikiWorkload] {
		t.Fatal("the pinned catalog or an admitted person lost the Wiki")
	}
	for id := range full {
		needsWiki := id == ManagedWikiWorkload
		for _, slug := range catalog.UseCasesForModule(ManagedWikiModule) {
			needsWiki = needsWiki || slug == id
		}
		if offered[id] == needsWiki {
			t.Fatalf("use case %q offered=%v without admission", id, offered[id])
		}
	}
	if len(ids(catalog)) != len(full) {
		t.Fatal("offering changed the shared catalog")
	}
	if !SelectsManagedWiki([]string{"photos", " WIKI "}) || SelectsManagedWiki([]string{"photos", "wiki-graph"}) {
		t.Fatal("Wiki goal spelling not read as goal authoring reads it")
	}
}
