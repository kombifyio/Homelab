package specv2

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// UseCaseAccessIntent is a desired private access decision, never an identity
// grant. PersonRefs name household identities or invitation drafts; the node's
// identity authority must resolve them before any permission can be activated.
// The Owner always retains access.
type UseCaseAccessIntent struct {
	Mode       string   `json:"mode"`
	Audience   string   `json:"audience,omitempty"`
	PersonRefs []string `json:"person_refs,omitempty"`
}

var useCaseAccessSlug = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

func (intent WizardIntent) validateUseCaseAccess() error {
	for slug, decision := range intent.UseCaseAccess {
		if !useCaseAccessSlug.MatchString(slug) || !slices.ContainsFunc(intent.Goals, func(goal string) bool { return strings.ToLower(strings.TrimSpace(goal)) == slug }) {
			return fmt.Errorf("specv2: use_case_access %q requires its selected goal", slug)
		}
		if decision.Mode == "inherit" {
			if decision.Audience != "" || len(decision.PersonRefs) != 0 {
				return fmt.Errorf("specv2: inherited use_case_access %q cannot carry an audience or person_refs", slug)
			}
			continue
		}
		if decision.Mode != AccessModeLocal && decision.Mode != AccessModeRemotePrivate {
			return fmt.Errorf("specv2: use_case_access %q mode must be inherit, local or remote-private", slug)
		}
		if !slices.Contains([]string{"owner", "household", "selected"}, decision.Audience) {
			return fmt.Errorf("specv2: use_case_access %q audience must be owner, household or selected", slug)
		}
		if (decision.Audience == "selected") != (len(decision.PersonRefs) > 0) || len(decision.PersonRefs) > maxPlannedHouseholdPeople {
			return fmt.Errorf("specv2: use_case_access %q requires person_refs only for a selected audience (maximum %d)", slug, maxPlannedHouseholdPeople)
		}
		seen := map[string]bool{}
		for _, ref := range decision.PersonRefs {
			if ref == "" || ref != strings.TrimSpace(ref) || len(ref) > 200 || strings.ContainsAny(ref, "\r\n") || seen[ref] {
				return fmt.Errorf("specv2: use_case_access %q has an invalid or duplicate person_ref", slug)
			}
			seen[ref] = true
		}
	}
	return nil
}

// PendingUseCaseAccess identifies desired decisions the current release cannot
// enforce regardless of topology. The projector additionally checks the actual
// release-authored routes and runtime. An explicit inherit withdraws the decision.
func (intent WizardIntent) PendingUseCaseAccess() []string {
	var pending []string
	for slug, decision := range intent.UseCaseAccess {
		if decision.Mode != "inherit" && (decision.Mode != AccessModeLocal ||
			(decision.Audience != "owner" && decision.Audience != "household") ||
			(slug != "photos" && slug != "documents")) {
			pending = append(pending, slug)
		}
	}
	sort.Strings(pending)
	return pending
}
