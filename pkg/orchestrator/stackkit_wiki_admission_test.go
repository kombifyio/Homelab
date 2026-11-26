package orchestrator

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/jobs"
)

// No job store is configured: neither missing history nor a degraded/failed
// deployment may change the committed desired spec's admission.
func TestManagedWikiCommittedAdmissionIgnoresEmptyHistoryAndDegradedDeploy(t *testing.T) {
	spec := map[string]any{"nodes": []any{map[string]any{"id": "home"}}, "workloads": map[string]any{"wiki": map[string]any{"enabled": true, "placement": map[string]any{"nodeRefs": []any{"home"}}, "secretRefs": map[string]any{"registry-auth": "secret://stackkits/wiki/registry-auth"}}}}
	for _, state := range []string{"pending", "degraded", "failed", "running"} {
		t.Run(state, func(t *testing.T) {
			orchestrator := &Orchestrator{}
			stack := &orchestratorStack{id: "stack", tenantID: "tenant", ownerID: "owner", status: state, config: map[string]any{"stack_spec_v2": spec}}
			admission, err := orchestrator.prepareManagedWikiRollout(t.Context(), stack, spec)
			if err != nil || admission == "" {
				t.Fatalf("committed Wiki lost admission: %v", err)
			}
			raw, _ := registrySpecJSON(spec)
			if err := jobs.ValidateManagedWikiInstallCheckpoint(raw, jobs.StackKitLifecycleRequest{TenantID: "tenant", OwnerID: "owner", StackID: "stack", Operation: jobs.StackKitLifecycleApply, WikiInstallAdmission: admission}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestManagedWikiDurableRollbackResolvesOnlyItsRecordedDigest(t *testing.T) {
	ctx := t.Context()
	store := controlplane.NewMemoryStore()
	spec := json.RawMessage(`{"workloads":{}}`)
	if _, err := store.CreateStack(ctx, controlplane.CreateStackRequest{
		ID: "stack", TenantID: "tenant", OwnerSubjectID: "owner", StackKitInstanceID: "kit",
		Config: map[string]any{"stack_spec_v2": spec},
	}); err != nil {
		t.Fatal(err)
	}
	req := jobs.StackKitLifecycleRequest{TenantID: "tenant", OwnerID: "owner", StackID: "stack", StackKitInstanceID: "kit", AgentID: "agent", NodeRef: "home", Operation: jobs.StackKitLifecycleRollback, RollbackTargetRef: "target"}
	target := committedRollbackTarget(req, req.RollbackTargetRef)
	var err error
	target.SpecSHA256, err = controlplane.CommittedStackSpecDigest(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindCommittedStackSpecTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	if err := jobs.BindManagedWikiLifecycle(ctx, &req, spec); err != nil {
		t.Fatal(err)
	}
	o := &Orchestrator{stackStore: store}
	if _, err := o.customerRegistrySpec(ctx, req); err != nil {
		t.Fatalf("recorded same-stack target refused: %v", err)
	}
	var admission controlplane.ManagedWikiCommitAdmission
	if err := json.Unmarshal([]byte(req.WikiInstallAdmission), &admission); err != nil {
		t.Fatal(err)
	}
	admission.SpecSHA256 = "different-digest"
	raw, err := json.Marshal(admission)
	if err != nil {
		t.Fatal(err)
	}
	req.WikiInstallAdmission = string(raw)
	if _, err := o.customerRegistrySpec(ctx, req); !errors.Is(err, jobs.ErrManagedWikiInstallNotAdmitted) {
		t.Fatalf("rollback resolved a target outside its recorded digest: %v", err)
	}
}

func TestManagedWikiMissingObservationLeavesTargetsOpaque(t *testing.T) {
	o := &Orchestrator{}
	req := jobs.StackKitLifecycleRequest{TenantID: "tenant", OwnerID: "owner", StackID: "stack", CandidateSpecJSON: []byte(`{"workloads":{}}`)}
	if err := o.commitSpecCanonical(t.Context(), req, nil); err != nil {
		t.Fatalf("old Guard blocked deploy: %v", err)
	}
	if err := o.commitRollbackTarget(t.Context(), req, "target", nil); err != nil {
		t.Fatalf("old Guard blocked change set: %v", err)
	}
	// No canonical store/source means no attribution, even with an observation.
	if err := o.commitSpecCanonical(t.Context(), req, []byte(`{"workloads":{}}`)); err != nil {
		t.Fatalf("uncommitted legacy source blocked deploy: %v", err)
	}
	if err := o.commitRollbackTarget(t.Context(), req, "target", []byte(`{"workloads":{}}`)); err != nil {
		t.Fatalf("uncommitted legacy baseline blocked deploy: %v", err)
	}
}
