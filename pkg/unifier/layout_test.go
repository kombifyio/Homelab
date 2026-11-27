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
	worker := func(id, name string, cores int, ramGB int, ip string) core.Worker {
		return core.Worker{
			ID: "worker-" + id, ServerID: id, Name: name, Status: "online",
			Tags: map[string]string{"ip": ip},
			Capabilities: core.WorkerCapabilities{
				CPU: cores, RAM: ramGB * 1024, Disk: 2048, Arch: "amd64", AMD64MicroarchitectureLevel: 3,
			},
		}
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
	main, spare, tiny := worker("srv-main", "main", 16, 64, "192.168.10.10"), worker("srv-spare", "spare", 8, 32, "192.168.10.11"), worker("srv-tiny", "tiny", 1, 1, "192.168.10.12")
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

func TestMultiNodeLayoutRejectsUnreachableNodeAddress(t *testing.T) {
	catalog, err := stackkitrelease.PinnedUseCaseCatalog()
	if err != nil {
		t.Fatal(err)
	}
	worker := func(id, name, ip string) core.Worker {
		return core.Worker{
			ID: "worker-" + id, ServerID: id, Name: name, Status: "online",
			Tags: map[string]string{"ip": ip},
			Capabilities: core.WorkerCapabilities{
				CPU: 8, RAM: 16384, Disk: 500, Arch: "amd64", AMD64MicroarchitectureLevel: 3,
			},
		}
	}

	// Both connected nodes lack concrete non-loopback unicast addresses ->
	// OutcomeUnknown with node_address_unreachable note.
	badA := worker("srv-a", "node-a", "")
	badB := worker("srv-b", "node-b", "127.0.0.1")
	comp, err := ComputeLayout(context.Background(), catalog, LayoutInput{
		Scope:           "stack:beta",
		StackID:         "beta",
		OwnerID:         "owner",
		Workers:         []core.Worker{badA, badB},
		PrimaryServerID: "srv-a",
		SpecNodeRefs:    map[string]string{"srv-a": "node-a", "srv-b": "node-b"},
		NodeRoles:       map[string][]string{"srv-a": {"controller"}, "srv-b": {"worker"}},
		Goals:           []string{"files"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if comp.Result.Outcome != layout.OutcomeUnknown {
		t.Fatalf("expected OutcomeUnknown for unreachable multi-node addresses, got %+v", comp.Result)
	}
	foundNote := false
	for _, note := range comp.Result.Notes {
		if note.Code == "node_address_unreachable" {
			foundNote = true
			break
		}
	}
	if !foundNote {
		t.Fatalf("expected node_address_unreachable note, got %+v", comp.Result.Notes)
	}

	// Two connected nodes with link-local / IPv6 loopback addresses ->
	// OutcomeUnknown with node_address_unreachable note.
	badLL := worker("srv-a", "node-a", "fe80::1")
	badV6 := worker("srv-b", "node-b", "::1")
	compLL, err := ComputeLayout(context.Background(), catalog, LayoutInput{
		Scope:           "stack:beta-ll",
		StackID:         "beta-ll",
		OwnerID:         "owner",
		Workers:         []core.Worker{badLL, badV6},
		PrimaryServerID: "srv-a",
		SpecNodeRefs:    map[string]string{"srv-a": "node-a", "srv-b": "node-b"},
		NodeRoles:       map[string][]string{"srv-a": {"controller"}, "srv-b": {"worker"}},
		Goals:           []string{"files"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if compLL.Result.Outcome != layout.OutcomeUnknown {
		t.Fatalf("expected OutcomeUnknown for link-local/loopback multi-node addresses, got %+v", compLL.Result)
	}
	foundLLNote := false
	for _, note := range compLL.Result.Notes {
		if note.Code == "node_address_unreachable" {
			foundLLNote = true
			break
		}
	}
	if !foundLLNote {
		t.Fatalf("expected node_address_unreachable note for link-local multi-node, got %+v", compLL.Result.Notes)
	}

	// Mixed multi-node: reachable node remains eligible, link-local node is excluded with node_address_unreachable.
	goodA := worker("srv-a", "node-a", "192.168.10.10")
	linkLocalB := worker("srv-b", "node-b", "fe80::1")
	compMixed, err := ComputeLayout(context.Background(), catalog, LayoutInput{
		Scope:           "stack:beta-mixed",
		StackID:         "beta-mixed",
		OwnerID:         "owner",
		Workers:         []core.Worker{goodA, linkLocalB},
		PrimaryServerID: "srv-a",
		SpecNodeRefs:    map[string]string{"srv-a": "node-a", "srv-b": "node-b"},
		NodeRoles:       map[string][]string{"srv-a": {"controller"}, "srv-b": {"worker"}},
		Goals:           []string{"files"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	foundMixedNote := false
	for _, note := range compMixed.Result.Notes {
		if note.Code == "node_address_unreachable" && note.Params["node"] == "node-b" {
			foundMixedNote = true
			break
		}
	}
	if !foundMixedNote || compMixed.Result.Outcome != layout.OutcomeFeasible || len(compMixed.Result.Options) == 0 || len(compMixed.Result.Options[0].Loads) != 1 || compMixed.Result.Options[0].Loads[0].NodeID != "srv-a" {
		t.Fatalf("expected reachable node-a placed and link-local node-b excluded with node_address_unreachable, got %+v", compMixed.Result)
	}
}

func TestSingleNodeLoopbackAndLinkLocalRemainPlaceable(t *testing.T) {
	catalog, err := stackkitrelease.PinnedUseCaseCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{"127.0.0.1", "::1", "fe80::1"} {
		single := core.Worker{
			ID: "worker-srv-1", ServerID: "srv-1", Name: "single-node", Status: "online",
			Tags: map[string]string{"ip": addr},
			Capabilities: core.WorkerCapabilities{
				CPU: 8, RAM: 16384, Disk: 500, Arch: "amd64", AMD64MicroarchitectureLevel: 3,
			},
		}
		comp, err := ComputeLayout(context.Background(), catalog, LayoutInput{
			Scope:           "stack:single",
			StackID:         "single",
			OwnerID:         "owner",
			Workers:         []core.Worker{single},
			PrimaryServerID: "srv-1",
			SpecNodeRefs:    map[string]string{"srv-1": "node-srv-1"},
			NodeRoles:       map[string][]string{"srv-1": {"controller"}},
			Goals:           []string{"files"},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if comp.Result.Outcome != layout.OutcomeFeasible || len(comp.Result.Options) == 0 || len(comp.Result.Options[0].Assignments) != 1 {
			t.Fatalf("single-node homelab with address %q must remain placeable, got %+v", addr, comp.Result)
		}
	}
}
