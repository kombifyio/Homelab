package stacks

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"

	ksapi "github.com/kombifyio/homelab/pkg/api"
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/features"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/jobs"
	"github.com/kombifyio/homelab/pkg/specv2"
)

func wikiInstallContext(e *httpx.Event, ownerID, tenantID string) context.Context {
	ctx := e.Request.Context()
	if features.ManagedWikiInstallEnabled(ctx, ownerID, tenantID) {
		return jobs.WithManagedWikiInstallAdmission(ctx, tenantID, ownerID)
	}
	return ctx
}

func withoutManagedWikiIntent(intent specv2.WizardIntent) (specv2.WizardIntent, bool) {
	if !slices.Contains(intent.Goals, jobs.ManagedWikiWorkload) {
		return intent, false
	}
	intent.Goals = slices.DeleteFunc(slices.Clone(intent.Goals), func(goal string) bool { return goal == jobs.ManagedWikiWorkload })
	intent.UseCaseSettings = withoutWikiKey(intent.UseCaseSettings)
	intent.UseCaseCapabilities = withoutWikiKey(intent.UseCaseCapabilities)
	intent.UseCaseSetupAnswers = withoutWikiKey(intent.UseCaseSetupAnswers)
	intent.UseCaseAccess = withoutWikiKey(intent.UseCaseAccess)
	return intent, true
}

func withoutWikiKey[Value any](source map[string]Value) map[string]Value {
	filtered := maps.Clone(source)
	delete(filtered, jobs.ManagedWikiWorkload)
	return filtered
}

func managedWikiInstallRefusal(e *httpx.Event) error {
	return httpx.Reject(e, http.StatusForbidden, ksapi.ErrCodeForbidden, "Managed Wiki installation is not enabled for this selection", map[string]any{"reason_code": "managed_wiki_install_disabled", "retryable": false})
}

// The committed spec carries prior admission. Wizard projection cannot remove
// its Wiki as a side effect of a flag decision.
func preserveExistingWikiOnly(projection, committed map[string]any, admitted ...bool) error {
	allowChange := len(admitted) > 0 && admitted[0]
	workloads, _ := projection["workloads"].(map[string]any)
	modules, _ := projection["modules"].(map[string]any)
	base, _ := committed["workloads"].(map[string]any)
	wiki, exists := base[jobs.ManagedWikiWorkload]
	if !exists {
		if !allowChange {
			delete(workloads, jobs.ManagedWikiWorkload)
			delete(modules, jobs.ManagedWikiModule)
		}
		return nil
	}
	if workloads == nil {
		workloads = map[string]any{}
		projection["workloads"] = workloads
	}
	if _, exists := workloads[jobs.ManagedWikiWorkload]; !exists {
		workloads[jobs.ManagedWikiWorkload] = wiki
	}
	baseModules, _ := committed["modules"].(map[string]any)
	if module, exists := baseModules[jobs.ManagedWikiModule]; exists {
		if modules == nil {
			modules = map[string]any{}
			projection["modules"] = modules
		}
		if _, exists := modules[jobs.ManagedWikiModule]; !exists {
			modules[jobs.ManagedWikiModule] = module
		}
	}
	before, err := json.Marshal(committed)
	if err != nil {
		return err
	}
	after, err := json.Marshal(projection)
	if err != nil {
		return err
	}
	same, err := jobs.SameManagedWikiSelection(before, after)
	if err != nil {
		return err
	}
	selected, err := jobs.ManagedWikiSelectionPresent(after)
	if err != nil {
		return err
	}
	if !selected || (!same && !allowChange) {
		return jobs.ErrManagedWikiInstallNotAdmitted
	}
	return nil
}

func requireManagedWikiChange(e *httpx.Event, tenantID, ownerID, stackID string, committed, candidate map[string]any, explicitRemoval bool) error {
	_, err := managedWikiCommitProof(e, tenantID, ownerID, stackID, committed, candidate, explicitRemoval)
	return err
}

func managedWikiCommitProof(e *httpx.Event, tenantID, ownerID, stackID string, committed, candidate map[string]any, explicitRemoval bool) (controlplane.ManagedWikiCommitProof, error) {
	var proof controlplane.ManagedWikiCommitProof
	before, err := json.Marshal(committed)
	if err != nil {
		return proof, httpx.BadRequest(e, "Invalid committed StackSpec")
	}
	after, err := json.Marshal(candidate)
	if err != nil {
		return proof, httpx.BadRequest(e, "Invalid candidate StackSpec")
	}
	proof.Admission, err = jobs.PrepareManagedWikiChange(wikiInstallContext(e, ownerID, tenantID), tenantID, ownerID, stackID, before, after, explicitRemoval)
	if errors.Is(err, jobs.ErrManagedWikiInstallNotAdmitted) {
		return proof, managedWikiInstallRefusal(e)
	}
	if err != nil {
		return proof, httpx.BadRequest(e, "Invalid managed Wiki StackSpec")
	}
	proof.ExplicitRemoval = explicitRemoval
	return proof, nil
}
