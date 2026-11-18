package stacks

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kombifyio/techstack/internal/stackkitrelease"
	"github.com/kombifyio/techstack/pkg/controlplane"
	"github.com/kombifyio/techstack/pkg/jobs"
)

const profileExpansionAttemptsKey = "profile_expansion_attempts"

type profileExpansionAttempt struct {
	RunID           string                       `json:"run_id"`
	StackID         string                       `json:"stack_id"`
	CandidateDigest string                       `json:"candidate_digest"`
	SpecDigest      string                       `json:"spec_digest"`
	GoalWorkloads   map[string]map[string]string `json:"goal_workloads,omitempty"`
	Goals           []string                     `json:"goals"`
	JobIDs          []string                     `json:"job_ids"`
}

func profileAttempts(value any) []profileExpansionAttempt {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var attempts []profileExpansionAttempt
	if json.Unmarshal(raw, &attempts) != nil {
		return nil
	}
	return attempts
}

// goalWorkloadBindings fixes the release-authored main workload and any
// selected add-ons to their exact alternatives and placement in the candidate.
func goalWorkloadBindings(catalog stackkitrelease.UseCaseCatalog, spec map[string]any, goals []string) map[string]map[string]string {
	workloads, _ := spec["workloads"].(map[string]any)
	result := map[string]map[string]string{}
	for _, goal := range goals {
		for _, uc := range catalog.UseCases {
			if uc.ID != goal || uc.WorkloadRef == "" {
				continue
			}
			main, ok := workloads[uc.WorkloadRef].(map[string]any)
			if !ok || main["alternative"] == nil {
				continue
			}
			bindings := map[string]string{uc.WorkloadRef: workloadBindingDigest(main)}
			for _, addon := range uc.AddOns {
				if value, ok := workloads[addon.ID].(map[string]any); ok {
					bindings[addon.ID] = workloadBindingDigest(value)
				}
			}
			result[goal] = bindings
		}
	}
	return result
}

func workloadBindingDigest(value map[string]any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func goalWorkloadsPresent(spec map[string]any, binding map[string]string) bool {
	if len(binding) == 0 {
		return false
	}
	workloads, _ := spec["workloads"].(map[string]any)
	for ref, expected := range binding {
		value, ok := workloads[ref].(map[string]any)
		if !ok || expected == "" || workloadBindingDigest(value) != expected {
			return false
		}
	}
	return true
}

// Record only explicit mapped goals and the exact deterministic group. This
// record is an observation to reconcile, not evidence of success.
func (h wizardRunHandlers) recordProfileExpansionAttempt(ctx context.Context, run *wizardRunState, stackID string, checkpoint *wizardExtendCheckpoint) error {
	if len(checkpoint.Requests) == 0 || len(run.explicitIntent.Goals) == 0 {
		return nil
	}
	if run.homelab == nil {
		return nil
	}
	wizard, _ := run.homelab.Intent["wizard"].(map[string]any)
	stored, err := wizardIntentFromDocument(wizard)
	if err != nil {
		return err
	}
	matched := false
	for _, profile := range stored.ProfileExpansions {
		for _, goal := range run.explicitIntent.Goals {
			if slices.Contains(profile.DeferredGoals, goal) && !slices.Contains(checkpoint.Projection.UnmappedGoals, goal) {
				matched = true
			}
		}
	}
	if !matched {
		return nil
	}
	store, ok := h.crud.homelabStore.(controlplane.AtomicHomelabIntentStore)
	if !ok {
		return errors.New("atomic homelab intent persistence unavailable")
	}
	catalog, err := jobs.UseCaseCatalogForStack(ctx, run.tenantID, stackID)
	if err != nil {
		return nil
	} // Keep wishes pending when the pinned catalog cannot be read.
	goalBindings := goalWorkloadBindings(catalog, checkpoint.Projection.Spec, run.explicitIntent.Goals)
	specDigest := wizardExtendSpecDigest(checkpoint.Projection.Spec)
	candidate := checkpoint.Requests[0].WizardExtend
	if candidate == nil {
		return errors.New("extension job lacks its candidate binding")
	}
	candidateDigest := candidate.CandidateDigest
	var ids []string
	for _, request := range checkpoint.Requests {
		if request.WizardExtend == nil || request.WizardExtend.RunID != run.runID || request.WizardExtend.CandidateDigest != candidateDigest || request.DurableJobID == "" {
			return errors.New("extension job lacks its candidate binding")
		}
		ids = append(ids, request.DurableJobID)
	}
	_, err = store.MutateHomelabIntent(ctx, run.tenantID, run.homelab.ID, func(intent map[string]any) error {
		wizard, _ := intent["wizard"].(map[string]any)
		if wizard == nil {
			return nil
		}
		stored, err := wizardIntentFromDocument(wizard)
		if err != nil {
			return err
		}
		var selected []string
		for _, goal := range run.explicitIntent.Goals {
			if slices.Contains(checkpoint.Projection.UnmappedGoals, goal) || len(goalBindings[goal]) == 0 {
				continue
			}
			for _, profile := range stored.ProfileExpansions {
				if slices.Contains(profile.DeferredGoals, goal) && !slices.Contains(selected, goal) {
					selected = append(selected, goal)
				}
			}
		}
		if len(selected) == 0 {
			return nil
		}
		attempts := profileAttempts(wizard[profileExpansionAttemptsKey])
		attempt := profileExpansionAttempt{RunID: run.runID, StackID: stackID, CandidateDigest: candidateDigest, SpecDigest: specDigest, GoalWorkloads: goalBindings, Goals: selected, JobIDs: ids}
		var keep []profileExpansionAttempt
		for _, previous := range attempts {
			if previous.RunID == run.runID {
				continue
			}
			// A later attempt takes over these wishes before any dispatch. An
			// old successful receipt cannot validate the new desired state.
			var remaining []string
			for _, goal := range previous.Goals {
				if !slices.Contains(selected, goal) {
					remaining = append(remaining, goal)
				}
			}
			if len(remaining) > 0 {
				previous.Goals = remaining
				keep = append(keep, previous)
			}
		}
		wizard[profileExpansionAttemptsKey] = append(keep, attempt)
		intent["wizard"] = wizard
		return nil
	})
	return err
}

// Reconciliation fails closed: an absent job, missing receipt, partial group,
// changed desired spec or failed operation leaves all wishes intact.
func (h crudRouteHandlers) reconcileProfileExpansions(ctx context.Context, tenantID, ownerID string, homelab *controlplane.Homelab) *controlplane.Homelab {
	if homelab == nil || h.jobStore == nil || h.stackStore == nil {
		return homelab
	}
	wizard, _ := homelab.Intent["wizard"].(map[string]any)
	var completed []profileExpansionAttempt
	for _, attempt := range profileAttempts(wizard[profileExpansionAttemptsKey]) {
		if attempt.RunID == "" || attempt.StackID == "" || len(attempt.Goals) == 0 || len(attempt.JobIDs) == 0 {
			continue
		}
		stack, err := h.stackStore.GetStack(ctx, tenantID, attempt.StackID)
		if err != nil || stack == nil || stack.OwnerSubjectID != ownerID || stack.HomelabID != homelab.ID {
			continue
		}
		spec, _ := stack.Config[stackConfigKeySpecV2].(map[string]any)
		if spec == nil {
			continue
		}
		valid := true
		for _, id := range attempt.JobIDs {
			job, err := h.jobStore.GetJob(ctx, tenantID, id)
			if err != nil || job == nil || job.StackID != attempt.StackID || !strings.EqualFold(job.State, "completed") {
				valid = false
				break
			}
			receipt, ok := job.Result[jobs.WizardExtendReceiptKey].(map[string]any)
			if !ok || receipt["run_id"] != attempt.RunID || receipt["stack_id"] != attempt.StackID || receipt["tenant_id"] != tenantID || receipt["owner_id"] != ownerID || receipt["candidate_digest"] != attempt.CandidateDigest || receipt["group_job_ids"] != strings.Join(attempt.JobIDs, ",") {
				valid = false
				break
			}
		}
		if valid {
			var realized []string
			for _, goal := range attempt.Goals {
				if goalWorkloadsPresent(spec, attempt.GoalWorkloads[goal]) {
					realized = append(realized, goal)
				}
			}
			if len(realized) > 0 {
				attempt.Goals = realized
				completed = append(completed, attempt)
			}
		}
	}
	if len(completed) == 0 {
		return homelab
	}
	// Project completion only in this response. Reads never mutate the durable
	// wish; the bound receipts remain available on later reads and retries.
	raw, err := json.Marshal(homelab.Intent)
	if err != nil {
		return homelab
	}
	var intent map[string]any
	if json.Unmarshal(raw, &intent) != nil {
		return homelab
	}
	projectedWizard, _ := intent["wizard"].(map[string]any)
	stored, err := wizardIntentFromDocument(projectedWizard)
	if err != nil {
		return homelab
	}
	for i := range stored.ProfileExpansions {
		var remaining []string
		for _, goal := range stored.ProfileExpansions[i].DeferredGoals {
			if !slices.ContainsFunc(completed, func(attempt profileExpansionAttempt) bool { return slices.Contains(attempt.Goals, goal) }) {
				remaining = append(remaining, goal)
			}
		}
		stored.ProfileExpansions[i].DeferredGoals = remaining
	}
	projectedWizard["profile_expansions"] = stored.ProfileExpansions
	copy := *homelab
	copy.Intent = intent
	return &copy
}
