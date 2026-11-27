package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"

	"github.com/kombifyio/homelab/internal/stackkitrelease"
	"github.com/kombifyio/homelab/pkg/controlplane"
)

const ManagedWikiWorkload = "wiki"
const ManagedWikiModule = "stackkits-wiki-runtime"
const ManagedWikiInstallAdmissionKey = "managed_wiki_install_admission"

var ErrManagedWikiInstallNotAdmitted = controlplane.ErrManagedWikiInstallNotAdmitted

type wikiInstallAdmissionKey struct{}

type wikiInstallPrincipal struct {
	TenantID string
	OwnerID  string
}

func WithManagedWikiInstallAdmission(ctx context.Context, tenantID, ownerID string) context.Context {
	return context.WithValue(ctx, wikiInstallAdmissionKey{}, wikiInstallPrincipal{tenantID, ownerID})
}

func ManagedWikiInstallAdmitted(ctx context.Context, tenantID, ownerID string) bool {
	principal, ok := ctx.Value(wikiInstallAdmissionKey{}).(wikiInstallPrincipal)
	return ok && tenantID != "" && ownerID != "" && principal == (wikiInstallPrincipal{tenantID, ownerID})
}

// IsManagedWikiGoal compares a goal as goal authoring reads it (trimmed,
// lower case), so no spelling of the Wiki goal passes a check unseen.
func IsManagedWikiGoal(goal string) bool {
	return strings.ToLower(strings.TrimSpace(goal)) == ManagedWikiWorkload
}

// SelectsManagedWiki reports whether goals name the Wiki in any spelling.
func SelectsManagedWiki(goals []string) bool {
	return slices.ContainsFunc(goals, IsManagedWikiGoal)
}

// UnofferedUseCases names the use cases one person cannot receive in full.
// Until their managed Wiki installation is admitted, that is the Wiki and
// every use case built from the Wiki module, so the Wizard, the catalog and
// the recommendations never offer a solution that would install without it.
func UnofferedUseCases(catalog stackkitrelease.UseCaseCatalog, wikiAdmitted bool) []string {
	if wikiAdmitted {
		return nil
	}
	return append(catalog.UseCasesForModule(ManagedWikiModule), ManagedWikiWorkload)
}

// OfferedUseCaseCatalog is the catalog without UnofferedUseCases.
func OfferedUseCaseCatalog(catalog stackkitrelease.UseCaseCatalog, wikiAdmitted bool) stackkitrelease.UseCaseCatalog {
	unoffered := UnofferedUseCases(catalog, wikiAdmitted)
	if len(unoffered) == 0 {
		return catalog
	}
	catalog.UseCases = slices.DeleteFunc(slices.Clone(catalog.UseCases), func(useCase stackkitrelease.UseCase) bool {
		return slices.Contains(unoffered, useCase.ID)
	})
	return catalog
}

// managedWikiAdmission is server-owned recovery state, never an HTTP credential.
// Its digest names committed desired intent, not runtime execution history.
type managedWikiAdmission = controlplane.ManagedWikiCommitAdmission

// PrepareManagedWikiChange runs before a desired-spec commit. Existing Wiki is
// already admitted; only introducing it requires a current user decision.
func PrepareManagedWikiChange(ctx context.Context, tenantID, ownerID, stackID string, committed, candidate []byte, explicitRemoval bool) (string, error) {
	before, err := ManagedWikiSelectionPresent(committed)
	if err != nil {
		return "", err
	}
	after, err := ManagedWikiSelectionPresent(candidate)
	if err != nil {
		return "", err
	}
	if before && !after && !explicitRemoval {
		return "", ErrManagedWikiInstallNotAdmitted
	}
	if after && !before && !ManagedWikiInstallAdmitted(ctx, tenantID, ownerID) {
		return "", ErrManagedWikiInstallNotAdmitted
	}
	if !before && !after {
		return "", nil
	}
	kind := controlplane.ManagedWikiAdmissionExisting
	if !before && after {
		kind = controlplane.ManagedWikiAdmissionAddition
	}
	if before && !after {
		kind = controlplane.ManagedWikiAdmissionRemoval
	}
	return encodeManagedWikiAdmission(tenantID, ownerID, stackID, committed, candidate, "", kind)
}

func BindManagedWikiLifecycle(ctx context.Context, req *StackKitLifecycleRequest, committed []byte, canonicals ...[]byte) error {
	if req.DryRun {
		return nil
	}
	switch req.Operation {
	case StackKitLifecycleApply, StackKitLifecycleAdvancedChangeSet, StackKitLifecycleDriftReconcile, StackKitLifecycleRollback, StackKitLifecycleApplicationUpdate:
	default:
		return nil
	}
	if req.WikiInstallAdmission != "" {
		return ValidateManagedWikiInstallCheckpoint(committed, *req)
	}
	present, err := ManagedWikiSelectionPresent(committed)
	if err != nil {
		return err
	}
	if req.Operation == StackKitLifecycleRollback {
		// The orchestrator/store resolves an exact owner/tenant/stack-bound
		// committed target before calling this boundary, regardless of flag.
		if req.RollbackTargetRef == "" || len(committed) == 0 {
			return ErrManagedWikiInstallNotAdmitted
		}
		req.WikiInstallAdmission, err = encodeManagedWikiAdmission(req.TenantID, req.OwnerID, req.StackID, committed, committed, req.RollbackTargetRef, controlplane.ManagedWikiAdmissionExisting)
		return err
	}

	candidate := req.CandidateSpecJSON
	if len(candidate) == 0 {
		candidate = committed
	}
	selected, err := ManagedWikiSelectionPresent(candidate)
	if err != nil {
		return err
	}
	if !selected && !present {
		return nil
	}
	// Candidate changes are committed by the user-facing authoring/CAS path.
	// No detached candidate may turn an admitted stack into another selection.
	same, err := SameManagedWikiSelection(candidate, committed)
	if err != nil {
		return err
	}
	candidateDigest, err := managedWikiSpecDigest(candidate)
	if err != nil {
		return err
	}
	canonicalCandidate := false
	for _, raw := range canonicals {
		digest, err := managedWikiSpecDigest(raw)
		if err != nil {
			return err
		}
		if candidateDigest == digest {
			canonicalCandidate = true
			break
		}
	}
	if (!same && !canonicalCandidate) || !present {
		return ErrManagedWikiInstallNotAdmitted
	}
	req.WikiInstallAdmission, err = encodeManagedWikiAdmission(req.TenantID, req.OwnerID, req.StackID, committed, committed, "", controlplane.ManagedWikiAdmissionExisting)
	if err != nil {
		return err
	}
	if canonicalCandidate {
		var admission managedWikiAdmission
		if err := json.Unmarshal([]byte(req.WikiInstallAdmission), &admission); err != nil {
			return err
		}
		admission.CandidateSHA256 = candidateDigest
		raw, err := json.Marshal(admission)
		if err != nil {
			return err
		}
		req.WikiInstallAdmission = string(raw)
	}
	return ValidateManagedWikiInstallCheckpoint(committed, *req)
}

// ManagedWikiSelectionPresent identifies enabled Wiki in a canonical spec.
func ManagedWikiSelectionPresent(raw []byte) (bool, error) {
	digest, err := managedWikiInstallDigest(raw)
	return digest != "", err
}

func SameManagedWikiSelection(left, right []byte) (bool, error) {
	a, err := managedWikiInstallDigest(left)
	if err != nil {
		return false, err
	}
	b, err := managedWikiInstallDigest(right)
	return a == b, err
}

func managedWikiInstallDigest(raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var spec registrySelectionSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return "", err
	}
	definition := spec.Workloads[ManagedWikiWorkload]
	if len(definition) == 0 {
		return "", nil
	}
	var workload registryWorkload
	if err := json.Unmarshal(definition, &workload); err != nil {
		return "", err
	}
	if workload.Enabled != nil && !*workload.Enabled {
		return "", nil
	}
	var contract map[string]any
	if err := json.Unmarshal(definition, &contract); err != nil {
		return "", err
	}
	if contract == nil {
		return "", errors.New("managed Wiki selection must be a workload object")
	}
	var selectedNodes []registrySelectionNode
	for _, node := range spec.Nodes {
		selected := slices.Contains(workload.Placement.NodeRefs, node.ID)
		if len(workload.Placement.NodeRefs) == 0 {
			selected = (node.Enabled == nil || *node.Enabled) && (len(workload.Placement.SiteRefs) == 0 || slices.Contains(workload.Placement.SiteRefs, node.SiteRef))
		}
		if selected {
			selectedNodes = append(selectedNodes, node)
		}
	}
	sort.Slice(selectedNodes, func(i, j int) bool { return selectedNodes[i].ID < selectedNodes[j].ID })
	var document struct {
		Modules map[string]any   `json:"modules"`
		Nodes   []map[string]any `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return "", err
	}
	var selectedDefinitions []map[string]any
	for _, node := range selectedNodes {
		for _, definition := range document.Nodes {
			if definition["id"] == node.ID {
				selectedDefinitions = append(selectedDefinitions, definition)
			}
		}
	}
	canonical, err := json.Marshal(struct {
		Workload map[string]any
		Module   any
		Nodes    []map[string]any
	}{contract, document.Modules[ManagedWikiModule], selectedDefinitions})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

// Normalize only the managed generation target, which the existing native
// dispatcher sets to Terramate. Every other desired-spec field binds the job.
func managedWikiSpecDigest(raw []byte) (string, error) {
	return controlplane.CommittedStackSpecDigest(raw)
}

func encodeManagedWikiAdmission(tenantID, ownerID, stackID string, source, committed []byte, target, kind string) (string, error) {
	if tenantID == "" || ownerID == "" || stackID == "" {
		return "", ErrManagedWikiInstallNotAdmitted
	}
	digest, err := managedWikiSpecDigest(committed)
	if err != nil {
		return "", err
	}
	sourceDigest, err := managedWikiSpecDigest(source)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(managedWikiAdmission{TenantID: tenantID, OwnerID: ownerID, StackID: stackID, SpecSHA256: digest, SourceSHA256: sourceDigest, Kind: kind, RollbackTargetRef: target})
	return string(raw), err
}

// ValidateManagedWikiInstallCheckpoint must receive the current committed spec
// from the tenant-scoped stack store (the attributed target for rollback),
// never a historical job or desired candidate.
func ValidateManagedWikiInstallCheckpoint(committed []byte, req StackKitLifecycleRequest) error {
	present, err := ManagedWikiSelectionPresent(committed)
	if err != nil {
		return err
	}
	if req.WikiInstallAdmission == "" {
		selected, err := ManagedWikiSelectionPresent(req.CandidateSpecJSON)
		if err != nil {
			return err
		}
		if present || selected || req.Operation == StackKitLifecycleRollback {
			return ErrManagedWikiInstallNotAdmitted
		}
		return nil
	}
	var admission managedWikiAdmission
	if json.Unmarshal([]byte(req.WikiInstallAdmission), &admission) != nil || admission.TenantID == "" || admission.OwnerID == "" || admission.StackID == "" ||
		admission.TenantID != req.TenantID || admission.OwnerID != req.OwnerID || admission.StackID != req.StackID {
		return ErrManagedWikiInstallNotAdmitted
	}
	digest, err := managedWikiSpecDigest(committed)
	if err != nil {
		return err
	}
	if admission.SpecSHA256 != digest || admission.SourceSHA256 == "" {
		return ErrManagedWikiInstallNotAdmitted
	}
	switch admission.Kind {
	case controlplane.ManagedWikiAdmissionAddition:
		if !present {
			return ErrManagedWikiInstallNotAdmitted
		}
	case controlplane.ManagedWikiAdmissionRemoval:
		if present {
			return ErrManagedWikiInstallNotAdmitted
		}
	case controlplane.ManagedWikiAdmissionExisting:
		// An accepted existing-Wiki edit may have a different source digest;
		// execution binds its resulting committed candidate above.
	default:
		return ErrManagedWikiInstallNotAdmitted
	}
	if req.Operation == StackKitLifecycleRollback {
		if admission.Kind != controlplane.ManagedWikiAdmissionExisting || admission.SourceSHA256 != digest || admission.RollbackTargetRef == "" || admission.RollbackTargetRef != req.RollbackTargetRef {
			return ErrManagedWikiInstallNotAdmitted
		}
	} else if admission.RollbackTargetRef != "" {
		return ErrManagedWikiInstallNotAdmitted
	}
	if req.Operation != StackKitLifecycleRollback && len(req.CandidateSpecJSON) != 0 {
		candidateDigest, err := managedWikiSpecDigest(req.CandidateSpecJSON)
		if err != nil {
			return err
		}
		if candidateDigest != digest && candidateDigest != admission.CandidateSHA256 {
			return ErrManagedWikiInstallNotAdmitted
		}
	}
	return nil
}

// ValidateManagedWikiExecution reads the current committed authority at apply
// and renewal. A durable digest never becomes an admission for a newer spec.
func ValidateManagedWikiExecution(ctx context.Context, cfg StackKitLifecycleConfig, req StackKitLifecycleRequest) error {
	selected, err := ManagedWikiSelectionPresent(req.CandidateSpecJSON)
	if err != nil {
		return err
	}
	if cfg.CustomerRegistrySpec == nil {
		if selected || req.WikiInstallAdmission != "" || req.Operation == StackKitLifecycleRollback {
			return ErrManagedWikiInstallNotAdmitted
		}
		return nil
	}
	committed, err := cfg.CustomerRegistrySpec(ctx, req)
	if err != nil {
		return err
	}
	return ValidateManagedWikiInstallCheckpoint(committed, req)
}
