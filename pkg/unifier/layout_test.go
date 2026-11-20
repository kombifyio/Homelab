package unifier

import (
	"context"
	"fmt"
	"testing"

	"github.com/kombifyio/homelab/internal/stackkitrelease"
	"github.com/kombifyio/homelab/pkg/core"
	"github.com/kombifyio/homelab/pkg/layout"
	"github.com/kombifyio/homelab/pkg/specv2"
)

// A use case and its add-on workloads are one placed unit, so an add-on can
// never land away from its primary (StackKits rejects such a split).
func TestFootprintDemandsGroupAddOnsWithTheirUseCase(t *testing.T) {
	catalog := stackkitrelease.UseCaseCatalog{UseCases: []stackkitrelease.UseCase{
		{ID: "photos", Title: "Photos", AddOns: []stackkitrelease.UseCaseAddOn{{ID: "photos-share"}}},
	}}
	demands := footprintDemands(catalog, []stackkitrelease.WorkloadFootprint{
		{Workload: "photos", Module: "stackkits-immich-runtime", ReservationProvenance: "measured", Reservation: stackkitrelease.UseCaseResources{RAMGB: 2}},
		{Workload: "photos-share", Module: "stackkits-photos-share-runtime", ReservationProvenance: "measured", Reservation: stackkitrelease.UseCaseResources{RAMGB: 0.5}},
	})
	if len(demands) != 1 || demands[0].Workload != "photos" || len(demands[0].Workloads) != 2 || demands[0].Reservation.RAMGB != 2.5 {
		t.Fatalf("want one photos unit with both workloads, got %+v", demands)
	}
}

// Creation Decision Matrix row "Choose another server" (docs/CREATION-DECISION-MATRIX.md):
// the owner's server choice for a use case is a pin the planner re-solves
// with. A feasible choice places the use case on the chosen server under
// that server's StackSpec node, which becomes its placement.nodeRefs; a
// choice without room is a conflict that names the chosen server, never a
// silent move elsewhere.
func TestOwnerPinPlacesUseCaseOnTheChosenServer(t *testing.T) {
	catalog, err := stackkitrelease.PinnedUseCaseCatalog()
	if err != nil {
		t.Fatal(err)
	}
	worker := func(id, name string, cores int, ramGB int) core.Worker {
		return core.Worker{ID: "worker-" + id, ServerID: id, Name: name, Status: "online", Capabilities: core.WorkerCapabilities{
			CPU: cores, RAM: ramGB * 1024, Disk: 2048, Arch: "amd64", AMD64MicroarchitectureLevel: 3,
		}}
	}
	input := func(pins map[string]string, workers ...core.Worker) LayoutInput {
		in := LayoutInput{
			Scope: "stack:alpha", StackID: "alpha", OwnerID: "owner", Workers: workers, PrimaryServerID: "srv-main",
			SpecNodeRefs: map[string]string{}, NodeRoles: map[string][]string{}, Goals: []string{"photos"}, Pins: pins,
		}
		for _, w := range workers {
			in.SpecNodeRefs[w.ServerID] = "node-" + w.ServerID
			in.NodeRoles[w.ServerID] = []string{"controller"}
		}
		return in
	}
	main, spare, tiny := worker("srv-main", "main", 16, 64), worker("srv-spare", "spare", 8, 32), worker("srv-tiny", "tiny", 1, 1)
	plan := func(in LayoutInput) LayoutComputation {
		t.Helper()
		computation, err := ComputeLayout(context.Background(), catalog, in, nil)
		if err != nil {
			t.Fatal(err)
		}
		return computation
	}

	recommended := plan(input(nil, main, spare))
	if recommended.Result.Outcome != layout.OutcomeFeasible || len(recommended.Result.Options[0].Assignments) != 1 {
		t.Fatalf("photos must fit the homelab without a choice, got %+v", recommended.Result)
	}
	other := "srv-spare"
	if recommended.Result.Options[0].Assignments[0].NodeID == other {
		other = "srv-main"
	}

	chosen := plan(input(map[string]string{"photos": other}, main, spare))
	if chosen.Result.Outcome != layout.OutcomeFeasible || len(chosen.Result.Options) == 0 {
		t.Fatalf("a choice with room must stay feasible, got %+v", chosen.Result)
	}
	assignment := chosen.Result.Options[0].Assignments[0]
	if assignment.NodeID != other || assignment.SpecNodeRef != "node-"+other || len(assignment.Reasons) == 0 || assignment.Reasons[0].Code != layout.PinOwnerChoice {
		t.Fatalf("photos must run on the chosen %s under its node ref with reason %s, got %+v", other, layout.PinOwnerChoice, assignment)
	}
	if len(chosen.Problem.Pins) != 1 || chosen.Problem.Pins[0] != (layout.Pin{Workload: "photos", NodeID: other, Reason: layout.PinOwnerChoice}) {
		t.Fatalf("the choice must be a pin of the problem, got %+v", chosen.Problem.Pins)
	}
	if chosen.Problem.Digest == recommended.Problem.Digest {
		t.Fatal("a choice must change the problem digest, so acceptance re-solves it")
	}
	// The accepted assignment is what reaches the StackSpec.
	spec := map[string]any{
		"nodes":     []any{map[string]any{"id": "node-srv-main"}, map[string]any{"id": "node-srv-spare"}},
		"workloads": map[string]any{"photos": map[string]any{"placement": map[string]any{"nodeRefs": []any{}}}},
	}
	if err := specv2.ApplyLayoutNodeRefs(spec, map[string]string{"photos": assignment.SpecNodeRef}); err != nil {
		t.Fatal(err)
	}
	refs := spec["workloads"].(map[string]any)["photos"].(map[string]any)["placement"].(map[string]any)["nodeRefs"]
	if got := fmt.Sprint(refs); got != "[node-"+other+"]" {
		t.Fatalf("photos.placement.nodeRefs must name the chosen server's node, got %s", got)
	}

	refused := plan(input(map[string]string{"photos": "srv-tiny"}, main, spare, tiny))
	if refused.Result.Outcome != layout.OutcomeProvenInfeasible || len(refused.Result.Options) != 0 || len(refused.Result.Conflicts) != 1 {
		t.Fatalf("a choice without room must be proven infeasible with no option, got %+v", refused.Result)
	}
	var named bool
	for _, reason := range refused.Result.Conflicts[0].Reasons {
		named = named || (reason.Code == "chosen_node_lacks_room" && reason.Params["node"] == "tiny")
	}
	if !named {
		t.Fatalf("the conflict must name the chosen server, got %+v", refused.Result.Conflicts[0].Reasons)
	}

	// An add-on of a running use case stays with it: the choice never
	// splits it away (StackKits rejects such a split).
	addOn := input(map[string]string{"photos": "srv-spare"}, main, spare)
	addOn.Settings = map[string]map[string]any{"photos": {"public-sharing": true}}
	addOn.Existing = []layout.Component{{ID: "photos", Workload: "photos", NodeID: "srv-main", Management: layout.ManagementManaged}}
	kept := plan(addOn)
	if len(kept.Problem.Pins) != 1 || kept.Problem.Pins[0].NodeID != "srv-main" || kept.Problem.Pins[0].Reason != "with_use_case" {
		t.Fatalf("the add-on must stay with its running use case despite a choice, got %+v", kept.Problem.Pins)
	}
}
