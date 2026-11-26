package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// These are pure authorization-boundary checks; no substitute database.
func TestCommittedRollbackTargetAttribution(t *testing.T) {
	for _, raw := range []string{`{"workloads":{}}`, `{"workloads":{"wiki":{"enabled":true}}}`} {
		var spec map[string]any
		if err := json.Unmarshal([]byte(raw), &spec); err != nil {
			t.Fatal(err)
		}
		stack := Stack{ID: "stack", TenantID: "tenant", OwnerSubjectID: "owner"}
		var err error
		stack.Config, err = retainCommittedStackSpecs(stack, map[string]any{"stack_spec_v2": spec}, nil, nil, committedProof(t, stack, spec, false))
		if err != nil {
			t.Fatal(err)
		}
		digest, err := CommittedStackSpecDigest([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		target := CommittedStackSpecTarget{TenantID: "tenant", OwnerID: "owner", StackID: "stack", AgentID: "agent", NodeRef: "home", TargetRef: "sha256:target", SpecSHA256: digest}
		// A later desired spec does not revoke a committed rollback snapshot.
		stack.Config, err = retainCommittedStackSpecs(stack, map[string]any{"stack_spec_v2": map[string]any{"workloads": map[string]any{"photos": map[string]any{"enabled": true}}}}, &target, nil, committedProof(t, stack, map[string]any{"workloads": map[string]any{"photos": map[string]any{"enabled": true}}}, true))
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := ResolveCommittedStackSpecTarget(stack, target)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := CommittedStackSpecDigest(snapshot); err != nil || got != digest {
			t.Fatalf("target digest %q %v", got, err)
		}
		for _, mutate := range []func(*CommittedStackSpecTarget){
			func(v *CommittedStackSpecTarget) { v.TenantID = "foreign" }, func(v *CommittedStackSpecTarget) { v.OwnerID = "foreign" }, func(v *CommittedStackSpecTarget) { v.StackID = "foreign" },
			func(v *CommittedStackSpecTarget) { v.AgentID = "foreign" }, func(v *CommittedStackSpecTarget) { v.NodeRef = "foreign" }, func(v *CommittedStackSpecTarget) { v.TargetRef = "opaque" },
		} {
			foreign := target
			mutate(&foreign)
			if _, err := ResolveCommittedStackSpecTarget(stack, foreign); !errors.Is(err, ErrNotFound) {
				t.Fatalf("foreign or opaque target: %v", err)
			}
		}
		changed := target
		changed.SpecSHA256 = "uncommitted"
		if _, err := retainCommittedStackSpecs(stack, stack.Config, &changed, nil); !errors.Is(err, ErrConflict) {
			t.Fatalf("uncommitted baseline: %v", err)
		}
	}
}

func TestCommittedRollbackCatalogCannotBeSuppliedByConfig(t *testing.T) {
	stack := Stack{ID: "stack", TenantID: "tenant", OwnerSubjectID: "owner", Config: map[string]any{}}
	forged := map[string]any{committedStackSpecsKey: map[string]any{"specs": map[string]any{"forged": map[string]any{"workloads": map[string]any{"wiki": map[string]any{"enabled": true}}}}}}
	config, err := retainCommittedStackSpecs(stack, forged, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := config[committedStackSpecsKey]; ok {
		t.Fatal("config supplied its own committed catalog")
	}
}

func TestCommittedNativeNormalizationRequiresACommittedSource(t *testing.T) {
	stack := Stack{ID: "stack", TenantID: "tenant", OwnerSubjectID: "owner"}
	source := map[string]any{"workloads": map[string]any{}}
	var err error
	stack.Config, err = retainCommittedStackSpecs(stack, map[string]any{"stack_spec_v2": source}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(source)
	digest, _ := CommittedStackSpecDigest(raw)
	canonical := CommittedStackSpecCanonical{SourceSHA256: digest, SpecJSON: json.RawMessage(`{"workloads":{},"generation":{"target":"terramate"},"nodes":[]}`)}
	config, err := retainCommittedStackSpecs(stack, stack.Config, nil, &canonical)
	if err != nil {
		t.Fatal(err)
	}
	stack.Config = config
	alternatives, err := CommittedStackSpecCanonicals(stack.Config, raw)
	if err != nil || len(alternatives) != 1 {
		t.Fatalf("canonical binding: %v %d", err, len(alternatives))
	}
	unrelated, _ := json.Marshal(map[string]any{"workloads": map[string]any{}, "metadata": map[string]any{"name": "unrelated"}})
	if alternatives, err := CommittedStackSpecCanonicals(stack.Config, unrelated); err != nil || len(alternatives) != 0 {
		t.Fatalf("foreign source inherited canonical: %v", err)
	}
	nativeDigest, _ := CommittedStackSpecDigest(canonical.SpecJSON)
	target := CommittedStackSpecTarget{TenantID: "tenant", OwnerID: "owner", StackID: "stack", AgentID: "agent", NodeRef: "home", TargetRef: "sha256:native", SpecSHA256: nativeDigest}
	stack.Config, err = retainCommittedStackSpecs(stack, stack.Config, &target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveCommittedStackSpecTarget(stack, target); err != nil {
		t.Fatal(err)
	}
	canonical.SourceSHA256 = "uncommitted"
	if _, err := retainCommittedStackSpecs(stack, stack.Config, nil, &canonical); !errors.Is(err, ErrNotFound) {
		t.Fatalf("uncommitted native source: %v", err)
	}
}

func committedProof(t *testing.T, stack Stack, spec map[string]any, removal bool) ManagedWikiCommitProof {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := CommittedStackSpecDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	source, err := json.Marshal(stack.Config["stack_spec_v2"])
	if err != nil {
		t.Fatal(err)
	}
	sourceDigest, err := CommittedStackSpecDigest(source)
	if err != nil {
		t.Fatal(err)
	}
	before, err := committedWikiPresent(stack.Config)
	if err != nil {
		t.Fatal(err)
	}
	after, err := committedWikiPresent(map[string]any{"stack_spec_v2": spec})
	if err != nil {
		t.Fatal(err)
	}
	kind := ManagedWikiAdmissionExisting
	if !before && after {
		kind = ManagedWikiAdmissionAddition
	}
	if before && !after {
		kind = ManagedWikiAdmissionRemoval
	}
	receipt, err := json.Marshal(ManagedWikiCommitAdmission{TenantID: stack.TenantID, OwnerID: stack.OwnerSubjectID, StackID: stack.ID, SpecSHA256: digest, SourceSHA256: sourceDigest, Kind: kind})
	if err != nil {
		t.Fatal(err)
	}
	return ManagedWikiCommitProof{Admission: string(receipt), ExplicitRemoval: removal}
}

func TestCommittedWikiStoreRequiresBoundAuthoringProof(t *testing.T) {
	stack := Stack{ID: "stack", TenantID: "tenant", OwnerSubjectID: "owner"}
	wiki := map[string]any{"workloads": map[string]any{"wiki": map[string]any{"enabled": true}}}
	candidate := map[string]any{"stack_spec_v2": wiki}
	good := committedProof(t, stack, wiki, false)
	foreign := stack
	foreign.OwnerSubjectID = "other"
	wrongDigest := committedProof(t, stack, map[string]any{"metadata": map[string]any{"name": "other"}}, false)
	for _, proof := range []ManagedWikiCommitProof{{}, committedProof(t, foreign, wiki, false), wrongDigest} {
		if _, err := retainCommittedStackSpecs(stack, candidate, nil, nil, proof); !errors.Is(err, ErrManagedWikiInstallNotAdmitted) {
			t.Fatalf("unadmitted store addition: %v", err)
		}
	}
	var err error
	stack.Config, err = retainCommittedStackSpecs(stack, candidate, nil, nil, good)
	if err != nil {
		t.Fatal(err)
	}
	empty := map[string]any{"workloads": map[string]any{}}
	removal := map[string]any{"stack_spec_v2": empty}
	for _, proof := range []ManagedWikiCommitProof{{}, committedProof(t, stack, empty, false)} {
		if _, err := retainCommittedStackSpecs(stack, removal, nil, nil, proof); !errors.Is(err, ErrManagedWikiInstallNotAdmitted) {
			t.Fatalf("implicit removal: %v", err)
		}
	}
	if _, err := retainCommittedStackSpecs(stack, removal, nil, nil, committedProof(t, stack, empty, true)); err != nil {
		t.Fatal(err)
	}
	// A native observation cannot introduce Wiki on behalf of the compiler.
	plain := Stack{ID: stack.ID, TenantID: stack.TenantID, OwnerSubjectID: stack.OwnerSubjectID}
	plain.Config, err = retainCommittedStackSpecs(plain, removal, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, _ := json.Marshal(empty)
	digest, _ := CommittedStackSpecDigest(source)
	raw, _ := json.Marshal(wiki)
	if _, err := retainCommittedStackSpecs(plain, plain.Config, nil, &CommittedStackSpecCanonical{SourceSHA256: digest, SpecJSON: raw}); !errors.Is(err, ErrManagedWikiInstallNotAdmitted) {
		t.Fatalf("compiler added Wiki: %v", err)
	}
}

func TestCommittedCatalogPrunesOnlyUnreferencedSpecs(t *testing.T) {
	stack := Stack{ID: "stack", TenantID: "tenant", OwnerSubjectID: "owner"}
	var target CommittedStackSpecTarget
	for i := 0; i < maxUnreferencedCommittedSpecs+10; i++ {
		spec := map[string]any{"metadata": map[string]any{"name": fmt.Sprint(i)}}
		var err error
		stack.Config, err = retainCommittedStackSpecs(stack, map[string]any{"stack_spec_v2": spec}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			raw, _ := json.Marshal(spec)
			digest, _ := CommittedStackSpecDigest(raw)
			target = CommittedStackSpecTarget{TenantID: stack.TenantID, OwnerID: stack.OwnerSubjectID, StackID: stack.ID, AgentID: "agent", NodeRef: "home", TargetRef: "target", SpecSHA256: digest}
			stack.Config, err = retainCommittedStackSpecs(stack, stack.Config, &target, nil)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	catalog, err := readCommittedStackSpecs(stack.Config)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Specs) != maxUnreferencedCommittedSpecs+1 {
		t.Fatalf("retained %d specs", len(catalog.Specs))
	}
	if _, err := ResolveCommittedStackSpecTarget(stack, target); err != nil {
		t.Fatalf("referenced snapshot pruned: %v", err)
	}
	current, _ := json.Marshal(stack.Config["stack_spec_v2"])
	if known, err := HasCommittedStackSpec(stack.Config, current); err != nil || !known {
		t.Fatalf("current spec pruned: %v", err)
	}
}

func TestCommittedWikiStoreProofCannotChangeSourceOrDirection(t *testing.T) {
	stack := Stack{ID: "stack", TenantID: "tenant", OwnerSubjectID: "owner", Config: map[string]any{"stack_spec_v2": map[string]any{"workloads": map[string]any{}}}}
	wiki := map[string]any{"workloads": map[string]any{"wiki": map[string]any{"enabled": true}}}
	good := committedProof(t, stack, wiki, false)
	var admission ManagedWikiCommitAdmission
	if err := json.Unmarshal([]byte(good.Admission), &admission); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*ManagedWikiCommitAdmission){
		func(a *ManagedWikiCommitAdmission) { a.Kind = ManagedWikiAdmissionExisting },
		func(a *ManagedWikiCommitAdmission) { a.Kind = ManagedWikiAdmissionRemoval },
		func(a *ManagedWikiCommitAdmission) { a.SourceSHA256 = "different-source" },
		func(a *ManagedWikiCommitAdmission) { a.SourceSHA256 = ""; a.Kind = "" },
	} {
		changed := admission
		mutate(&changed)
		raw, _ := json.Marshal(changed)
		if _, err := retainCommittedStackSpecs(stack, map[string]any{"stack_spec_v2": wiki}, nil, nil, ManagedWikiCommitProof{Admission: string(raw), ExplicitRemoval: true}); !errors.Is(err, ErrManagedWikiInstallNotAdmitted) {
			t.Fatalf("wrong direction/source admitted addition: %v", err)
		}
	}
	// Removal must also name the current source and removal direction.
	installedForRemoval := stack
	installedForRemoval.Config = map[string]any{"stack_spec_v2": wiki}
	empty := map[string]any{"workloads": map[string]any{}}
	removal := committedProof(t, installedForRemoval, empty, true)
	var removalAdmission ManagedWikiCommitAdmission
	if err := json.Unmarshal([]byte(removal.Admission), &removalAdmission); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*ManagedWikiCommitAdmission){func(a *ManagedWikiCommitAdmission) { a.Kind = ManagedWikiAdmissionExisting }, func(a *ManagedWikiCommitAdmission) { a.SourceSHA256 = "stale" }} {
		changed := removalAdmission
		mutate(&changed)
		raw, _ := json.Marshal(changed)
		if _, err := retainCommittedStackSpecs(installedForRemoval, map[string]any{"stack_spec_v2": empty}, nil, nil, ManagedWikiCommitProof{Admission: string(raw), ExplicitRemoval: true}); !errors.Is(err, ErrManagedWikiInstallNotAdmitted) {
			t.Fatalf("wrong removal direction/source admitted: %v", err)
		}
	}
	// A proof for retaining Wiki cannot be replayed after an intervening removal.
	installed := stack
	installed.Config = map[string]any{"stack_spec_v2": wiki}
	existing := committedProof(t, installed, wiki, false)
	if _, err := retainCommittedStackSpecs(stack, map[string]any{"stack_spec_v2": wiki}, nil, nil, existing); !errors.Is(err, ErrManagedWikiInstallNotAdmitted) {
		t.Fatalf("existing admission replayed as addition: %v", err)
	}
	// Addition receipt is also stale when the Wiki-free baseline changed.
	stack.Config = map[string]any{"stack_spec_v2": map[string]any{"metadata": map[string]any{"name": "changed"}}}
	if _, err := retainCommittedStackSpecs(stack, map[string]any{"stack_spec_v2": wiki}, nil, nil, good); !errors.Is(err, ErrManagedWikiInstallNotAdmitted) {
		t.Fatalf("stale source admitted addition: %v", err)
	}
}

func TestCommittedCatalogBoundsTargetAliases(t *testing.T) {
	stack := Stack{ID: "stack", TenantID: "tenant", OwnerSubjectID: "owner"}
	var first, last CommittedStackSpecTarget
	for i := 0; i < maxCommittedStackSpecTargets+1; i++ {
		spec := map[string]any{"metadata": map[string]any{"name": fmt.Sprint(i)}}
		var err error
		stack.Config, err = retainCommittedStackSpecs(stack, map[string]any{"stack_spec_v2": spec}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(spec)
		digest, _ := CommittedStackSpecDigest(raw)
		target := CommittedStackSpecTarget{TenantID: stack.TenantID, OwnerID: stack.OwnerSubjectID, StackID: stack.ID, AgentID: "agent", NodeRef: "home", TargetRef: fmt.Sprint(i), SpecSHA256: digest}
		stack.Config, err = retainCommittedStackSpecs(stack, stack.Config, &target, nil)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = target
		}
		last = target
	}
	catalog, err := readCommittedStackSpecs(stack.Config)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Targets) != maxCommittedStackSpecTargets {
		t.Fatalf("retained %d aliases", len(catalog.Targets))
	}
	if _, err := ResolveCommittedStackSpecTarget(stack, first); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired alias is not opaque: %v", err)
	}
	if _, err := ResolveCommittedStackSpecTarget(stack, last); err != nil {
		t.Fatalf("newest alias lost: %v", err)
	}
	wrong := last
	wrong.SpecSHA256 = "different-digest"
	if _, err := ResolveCommittedStackSpecTarget(stack, wrong); !errors.Is(err, ErrConflict) {
		t.Fatalf("mismatched target use admitted: %v", err)
	}
}
