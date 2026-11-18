package stacks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	productnotifications "github.com/kombifyio/techstack/internal/notifications"
	"github.com/kombifyio/techstack/pkg/controlplane"
	"github.com/kombifyio/techstack/pkg/logger"
	"github.com/kombifyio/techstack/pkg/specv2"
)

const wizardGoalActivationTopic = "product.news"

// loadStoredWizardGoals folds canonical wishes into the effective intent before
// admission. Explicit incoming fields win, including false and empty answers.
//
// Of the saved goals only the backlog is reconciled: unmapped_goals, the
// goals no earlier release could realize and every goal of a blocked run. A
// goal an earlier run realized stays recorded but is not applied again.
// Otherwise founding a new kit deployment would copy the homelab's earlier
// use cases into it, and a later run would project a use case the owner
// removed back in (kombify-StackKits-c0jn). A same-key retry keeps the goals
// its first attempt evaluated, even when that attempt already moved a goal
// out of the backlog. Saved decisions ride along only for the goals this run
// projects, because the projection refuses a decision without its goal.
func (h wizardRunHandlers) loadStoredWizardGoals(ctx context.Context, run *wizardRunState) error {
	homelab, err := h.crud.homelabStore.GetHomelabByOwner(ctx, run.tenantID, run.ownerID)
	if err != nil {
		if errors.Is(err, controlplane.ErrNotFound) {
			return nil
		}
		return err
	}
	homelab = h.crud.reconcileProfileExpansions(ctx, run.tenantID, run.ownerID, homelab)
	run.homelab = homelab
	wizard, _ := homelab.Intent["wizard"].(map[string]any)
	stored, err := wizardIntentFromDocument(wizard)
	if err != nil {
		return err
	}
	run.priorUnmappedGoals = wizardStringValues(wizard["unmapped_goals"])
	intent := &run.request.Intent
	intent.Goals = mergeWizardGoals(run.priorUnmappedGoals, intent.Goals)
	if prior := run.priorFailed; prior != nil && prior.RequestSHA256 == run.requestHash {
		if raw, ok := prior.Result["evaluated_goals"]; ok {
			intent.Goals = mergeWizardGoals(intent.Goals, wizardStringValues(raw))
		}
	}
	intent.UseCaseSettings = mergeWizardDecisions(decisionsForGoals(stored.UseCaseSettings, intent.Goals), intent.UseCaseSettings)
	intent.UseCaseCapabilities = mergeWizardDecisions(decisionsForGoals(stored.UseCaseCapabilities, intent.Goals), intent.UseCaseCapabilities)
	intent.UseCaseSetupAnswers = mergeWizardDecisions(decisionsForGoals(stored.UseCaseSetupAnswers, intent.Goals), intent.UseCaseSetupAnswers)
	access := make(map[string]specv2.UseCaseAccessIntent)
	for slug, decision := range stored.UseCaseAccess {
		if slices.Contains(intent.Goals, slug) {
			access[slug] = decision
		}
	}
	for slug, decision := range intent.UseCaseAccess {
		access[slug] = decision
	}
	intent.UseCaseAccess = access
	if intent.Access.Mode == "" && intent.Access.Publications == nil {
		intent.Access = stored.Access
	}
	if intent.Household.Profile == "" && intent.Household.PlannedPeople == nil {
		intent.Household = stored.Household
	}
	return nil
}

func wizardIntentFromDocument(wizard map[string]any) (specv2.WizardIntent, error) {
	var intent specv2.WizardIntent
	raw, err := json.Marshal(wizard)
	if err != nil {
		return intent, err
	}
	err = json.Unmarshal(raw, &intent)
	return intent, err
}

// decisionsForGoals keeps the saved decisions of the given goals only.
func decisionsForGoals[T any](stored map[string]map[string]T, goals []string) map[string]map[string]T {
	var kept map[string]map[string]T
	for slug, values := range stored {
		if !slices.Contains(goals, slug) {
			continue
		}
		if kept == nil {
			kept = make(map[string]map[string]T)
		}
		kept[slug] = values
	}
	return kept
}

func mergeWizardDecisions[T any](stored, incoming map[string]map[string]T) map[string]map[string]T {
	if stored == nil && incoming == nil {
		return nil
	}
	merged := make(map[string]map[string]T)
	for _, layer := range []map[string]map[string]T{stored, incoming} {
		for slug, values := range layer {
			if merged[slug] == nil {
				merged[slug] = make(map[string]T)
			}
			for id, value := range values {
				merged[slug][id] = value
			}
		}
	}
	return merged
}

// captureGoalActivations compares the release projection with the durable
// unmapped backlog. Only a goal that the exact pinned release stopped
// returning as unmapped is considered newly activated.
func (run *wizardRunState) captureGoalActivations(projection *specv2.Projection) {
	if projection == nil {
		return
	}
	run.evaluatedGoals = mergeWizardGoals(run.request.Intent.Goals)
	if len(run.priorUnmappedGoals) == 0 {
		return
	}
	stillUnmapped := make(map[string]struct{}, len(projection.UnmappedGoals))
	for _, goal := range projection.UnmappedGoals {
		stillUnmapped[strings.ToLower(strings.TrimSpace(goal))] = struct{}{}
	}
	for _, goal := range run.priorUnmappedGoals {
		normalized := strings.ToLower(strings.TrimSpace(goal))
		if normalized == "" {
			continue
		}
		if _, remains := stillUnmapped[normalized]; !remains {
			run.activatedGoals = append(run.activatedGoals, normalized)
		}
	}
	run.activatedGoals = mergeWizardGoals(run.activatedGoals)
}

func (h wizardRunHandlers) enqueueWizardGoalActivationNotifications(ctx context.Context, run *wizardRunState, stackID string) {
	if h.cfg.NotificationOutbox == nil || len(run.activatedGoals) == 0 {
		return
	}
	release := strings.TrimSpace(h.cfg.ReleaseVersion)
	for _, goal := range run.activatedGoals {
		key := fmt.Sprintf("techstack-wizard-goal-activated:%s:%s:%s:%s", run.tenantID, run.homelab.ID, release, goal)
		err := h.cfg.NotificationOutbox.Enqueue(ctx, productnotifications.ProductEvent{
			Topic:          wizardGoalActivationTopic,
			Channel:        "in_app",
			Auth0UserID:    run.ownerID,
			TenantID:       run.tenantID,
			IdempotencyKey: key,
			Payload: map[string]any{
				"subject":    "A saved Wizard goal is now available",
				"body":       fmt.Sprintf("Your saved %s goal is now included in this StackKit deployment.", goal),
				"severity":   "info",
				"goal":       goal,
				"release":    release,
				"homelab_id": run.homelab.ID,
				"stack_id":   stackID,
				"source_app": "techstack",
				"event_key":  "wizard.goal_activated",
				"link_url":   "/dashboard",
			},
		})
		if err != nil {
			logger.Default().Warn("wizard_goal_activation_notification_enqueue_failed",
				"tenant_id", run.tenantID, "homelab_id", run.homelab.ID,
				"goal", goal, "release", release, "error", err)
		}
	}
}

func wizardStringValues(raw any) []string {
	values := []string{}
	switch typed := raw.(type) {
	case []string:
		values = typed
	case []any:
		for _, value := range typed {
			if text, ok := value.(string); ok {
				values = append(values, text)
			}
		}
	}
	return mergeWizardGoals(values)
}

func mergeWizardGoals(groups ...[]string) []string {
	unique := map[string]struct{}{}
	for _, group := range groups {
		for _, goal := range group {
			normalized := strings.ToLower(strings.TrimSpace(goal))
			if normalized != "" {
				unique[normalized] = struct{}{}
			}
		}
	}
	goals := make([]string, 0, len(unique))
	for goal := range unique {
		goals = append(goals, goal)
	}
	sort.Strings(goals)
	return goals
}
