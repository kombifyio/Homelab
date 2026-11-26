package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/jobs"
	"github.com/kombifyio/homelab/pkg/kombifywallet"
	"github.com/kombifyio/homelab/pkg/specv2"
)

func (o *Orchestrator) ConfigureCustomerRegistryIssuer(issuer jobs.CustomerRegistryIssuer) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cfg.CustomerRegistryIssuer = issuer
}

func (o *Orchestrator) customerRegistrySpec(ctx context.Context, req jobs.StackKitLifecycleRequest) ([]byte, error) {
	stack, err := o.findStackForJob(ctx, req.StackID, ProvisionStackOptions{RequestContext: ctx, TenantID: req.TenantID, OwnerID: req.OwnerID})
	if err != nil {
		return nil, err
	}
	if stack.ownerID != req.OwnerID || stack.tenantID != req.TenantID || stack.stackKitInstanceID != req.StackKitInstanceID {
		return nil, ErrStackKitLifecycleUnavailable
	}
	if req.Operation == jobs.StackKitLifecycleRollback {
		store, ok := o.effectiveStackStore().(controlplane.CommittedStackSpecStore)
		if !ok {
			return nil, jobs.ErrManagedWikiInstallNotAdmitted
		}
		target := committedRollbackTarget(req, req.RollbackTargetRef)
		if req.WikiInstallAdmission != "" {
			var admission controlplane.ManagedWikiCommitAdmission
			if json.Unmarshal([]byte(req.WikiInstallAdmission), &admission) != nil || admission.SpecSHA256 == "" {
				return nil, jobs.ErrManagedWikiInstallNotAdmitted
			}
			target.SpecSHA256 = admission.SpecSHA256
		}
		raw, err := store.ReadCommittedStackSpecTarget(ctx, target)
		if errors.Is(err, controlplane.ErrNotFound) || errors.Is(err, controlplane.ErrConflict) {
			return nil, jobs.ErrManagedWikiInstallNotAdmitted
		}
		return raw, err
	}
	return registrySpecJSON(stack.config["stack_spec_v2"])
}

func registrySpecJSON(spec any) ([]byte, error) {
	if spec == nil {
		return nil, nil
	}
	return json.Marshal(spec)
}

func (o *Orchestrator) prepareManagedWikiRollout(ctx context.Context, stack *orchestratorStack, spec any) (string, error) {
	committed, err := registrySpecJSON(stack.config["stack_spec_v2"])
	if err != nil {
		return "", err
	}
	candidate, err := registrySpecJSON(spec)
	if err != nil {
		return "", err
	}
	// Provision/deploy consumes committed intent; it never changes that intent.
	req := jobs.StackKitLifecycleRequest{TenantID: stack.tenantID, OwnerID: stack.ownerID, StackID: stack.id, Operation: jobs.StackKitLifecycleApply, CandidateSpecJSON: candidate}
	canonicals, err := controlplane.CommittedStackSpecCanonicals(stack.config, committed)
	if err != nil {
		return "", err
	}
	if err := jobs.BindManagedWikiLifecycle(ctx, &req, committed, canonicals...); err != nil {
		return "", err
	}
	return req.WikiInstallAdmission, nil
}

func (o *Orchestrator) bindManagedWikiLifecycle(ctx context.Context, req *jobs.StackKitLifecycleRequest, committed []byte) error {
	if req.Operation == jobs.StackKitLifecycleRollback {
		snapshot, err := o.customerRegistrySpec(ctx, *req)
		if err != nil {
			return err
		}
		return jobs.BindManagedWikiLifecycle(ctx, req, snapshot)
	}
	stack, err := o.findStackForJob(ctx, req.StackID, ProvisionStackOptions{RequestContext: ctx, TenantID: req.TenantID, OwnerID: req.OwnerID})
	if err != nil {
		return err
	}
	canonicals, err := controlplane.CommittedStackSpecCanonicals(stack.config, committed)
	if err != nil {
		return err
	}
	return jobs.BindManagedWikiLifecycle(ctx, req, committed, canonicals...)
}

func customerRegistryRenewal(issuer jobs.CustomerRegistryIssuer, wallet *kombifywallet.Client) jobs.CustomerRegistryRenewal {
	if issuer == nil || !wallet.Enabled() {
		return nil
	}
	return func(ctx context.Context, req jobs.StackKitLifecycleRequest, product string) (string, error) {
		scope, ok := kombifywallet.ProjectScope(req.StackID)
		if !ok {
			return "", errors.New("customer registry renewal requires the deployment's Wallet scope")
		}
		grant, err := issuer(ctx, req.TenantID, req.OwnerID, req.StackID, req.AgentID, product)
		if err != nil {
			return "", err
		}
		defer clear(grant)
		workerDigest := sha256.Sum256([]byte(req.AgentID))
		item, err := wallet.Ensure(ctx, req.OwnerID, kombifywallet.ItemSpec{
			Type: kombifywallet.ItemTypeGenericSecret, Scope: scope,
			DisplayName: registryGrantDisplayName(product) + " image pull " + hex.EncodeToString(workerDigest[:8]),
			Description: "Short-lived registry pull grant for this deployment's connected server. Allow host delivery only to that server.",
			Value:       grant,
		})
		if err != nil {
			return "", err
		}
		return item.ItemID, nil
	}
}

func registryGrantDisplayName(product string) string {
	if product == "paperwork" {
		return "Paperwork"
	}
	return "Wiki"
}

func (o *Orchestrator) commitManagedWikiLifecycleCandidate(ctx context.Context, req *jobs.StackKitLifecycleRequest, stack *orchestratorStack) error {
	switch req.Operation {
	case jobs.StackKitLifecycleApply, jobs.StackKitLifecycleAdvancedChangeSet, jobs.StackKitLifecycleDriftReconcile:
	default:
		return nil
	}
	if req.DryRun || req.WizardExtend != nil || len(req.CandidateSpecJSON) == 0 {
		return nil
	}
	committed, err := registrySpecJSON(stack.config["stack_spec_v2"])
	if err != nil {
		return err
	}
	if req.WikiInstallAdmission != "" {
		return jobs.ValidateManagedWikiInstallCheckpoint(committed, *req)
	}
	var candidate map[string]any
	if err := json.Unmarshal(req.CandidateSpecJSON, &candidate); err != nil {
		return err
	}
	if candidate == nil {
		return errors.New("StackSpec candidate must be an object")
	}
	normalizationBase := committed
	if len(normalizationBase) == 0 {
		normalizationBase = []byte(`{}`)
	}
	currentIntent, err := jobs.TerramateGenerationCandidate(normalizationBase)
	if err != nil {
		return err
	}
	candidateIntent, err := jobs.TerramateGenerationCandidate(req.CandidateSpecJSON)
	if err != nil {
		return err
	}
	if string(currentIntent) == string(candidateIntent) {
		return nil
	}
	canonicals, err := controlplane.CommittedStackSpecCanonicals(stack.config, committed)
	if err != nil {
		return err
	}
	candidateDigest, err := controlplane.CommittedStackSpecDigest(req.CandidateSpecJSON)
	if err != nil {
		return err
	}
	for _, canonical := range canonicals {
		digest, err := controlplane.CommittedStackSpecDigest(canonical)
		if err != nil {
			return err
		}
		if digest == candidateDigest {
			return nil
		}
	}
	admission, err := jobs.PrepareManagedWikiChange(ctx, req.TenantID, req.OwnerID, req.StackID, committed, req.CandidateSpecJSON, false)
	if err != nil {
		return err
	}
	if !req.OwnerApproved || specv2.StackKitInstanceID(candidate) != stack.stackKitInstanceID {
		return ErrStackKitLifecycleUnavailable
	}
	validator := o.cfg.RolloutRetrySpecValidator
	if validator == nil {
		cli, err := specv2.NewCLIValidatorFromEnv()
		if err != nil {
			return err
		}
		if cli == nil {
			return fmt.Errorf("%w: pinned candidate validator is unavailable", ErrStackKitLifecycleUnavailable)
		}
		validator = cli
	}
	if err := validator.ValidateSpec(ctx, candidate); err != nil {
		return fmt.Errorf("invalid StackSpec candidate: %w", err)
	}
	store := o.effectiveStackStore()
	if store == nil {
		return ErrStackKitLifecycleUnavailable
	}
	current, err := store.GetStack(ctx, req.TenantID, req.StackID)
	if err != nil {
		return err
	}
	currentSpec, err := registrySpecJSON(current.Config["stack_spec_v2"])
	if err != nil {
		return err
	}
	if current.OwnerSubjectID != req.OwnerID || current.TenantID != req.TenantID || string(currentSpec) != string(committed) {
		return controlplane.ErrConflict
	}
	config := cloneProvisionSpec(current.Config)
	config["stack_spec_v2"] = candidate
	updated, err := store.CompareAndSwapStackConfig(ctx, controlplane.StackConfigCAS{TenantID: req.TenantID, StackID: req.StackID, ExpectedUpdatedAt: current.UpdatedAt, Config: config, ManagedWikiCommit: controlplane.ManagedWikiCommitProof{Admission: admission}})
	if err != nil {
		return err
	}
	stack.config, req.WikiInstallAdmission = updated.Config, admission
	return nil
}

func committedRollbackTarget(req jobs.StackKitLifecycleRequest, target string) controlplane.CommittedStackSpecTarget {
	return controlplane.CommittedStackSpecTarget{TenantID: req.TenantID, OwnerID: req.OwnerID, StackID: req.StackID, AgentID: req.AgentID, NodeRef: req.NodeRef, TargetRef: target}
}

func (o *Orchestrator) hasCommittedSpec(ctx context.Context, req jobs.StackKitLifecycleRequest, raw []byte) (bool, error) {
	store := o.effectiveStackStore()
	if store == nil {
		return false, nil
	}
	stack, err := store.GetStack(ctx, req.TenantID, req.StackID)
	if err != nil {
		return false, err
	}
	if stack.OwnerSubjectID != req.OwnerID {
		return false, jobs.ErrManagedWikiInstallNotAdmitted
	}
	known, err := controlplane.HasCommittedStackSpec(stack.Config, raw)
	if errors.Is(err, controlplane.ErrConflict) {
		return false, jobs.ErrManagedWikiInstallNotAdmitted
	}
	return known, err
}

func (o *Orchestrator) commitRollbackTarget(ctx context.Context, req jobs.StackKitLifecycleRequest, target string, baseline []byte) error {
	if len(baseline) == 0 {
		return nil
	}
	store, ok := o.effectiveStackStore().(controlplane.CommittedStackSpecStore)
	if !ok {
		return nil
	}
	binding := committedRollbackTarget(req, target)
	digest, err := controlplane.CommittedStackSpecDigest(baseline)
	if err != nil {
		return err
	}
	_, err = store.ReadCommittedStackSpecTarget(ctx, binding)
	// Optional provenance capture never replaces an existing alias or fails an
	// unrelated change set. Target consumers enforce the saved digest on use.
	if err == nil || errors.Is(err, controlplane.ErrConflict) {
		return nil
	}
	if !errors.Is(err, controlplane.ErrNotFound) {
		return err
	}
	known, err := o.hasCommittedSpec(ctx, req, baseline)
	if err != nil {
		return err
	}
	if !known {
		return nil
	}
	var document map[string]any
	if err := json.Unmarshal(baseline, &document); err != nil {
		return err
	}
	if specv2.StackKitInstanceID(document) != req.StackKitInstanceID {
		return jobs.ErrManagedWikiInstallNotAdmitted
	}
	binding.SpecSHA256 = digest
	if err := store.BindCommittedStackSpecTarget(ctx, binding); err != nil {
		if errors.Is(err, controlplane.ErrNotFound) || errors.Is(err, controlplane.ErrConflict) {
			return nil
		}
		return err
	}
	return nil
}

// The authenticated native command compiled this committed candidate. Retain
// its CUE-normalized representation so defaults are not mistaken for new intent.
func (o *Orchestrator) commitSpecCanonical(ctx context.Context, req jobs.StackKitLifecycleRequest, canonical []byte) error {
	if len(canonical) == 0 || len(req.CandidateSpecJSON) == 0 {
		return nil
	}
	known, err := o.hasCommittedSpec(ctx, req, req.CandidateSpecJSON)
	if err != nil {
		return err
	}
	if !known {
		return nil
	}
	store, ok := o.effectiveStackStore().(controlplane.CommittedStackSpecStore)
	if !ok {
		return nil
	}
	if _, err := jobs.PrepareManagedWikiChange(ctx, req.TenantID, req.OwnerID, req.StackID, req.CandidateSpecJSON, canonical, false); err != nil {
		return err
	}
	var document map[string]any
	if err := json.Unmarshal(canonical, &document); err != nil {
		return err
	}
	if specv2.StackKitInstanceID(document) != req.StackKitInstanceID {
		return jobs.ErrManagedWikiInstallNotAdmitted
	}
	digest, err := controlplane.CommittedStackSpecDigest(req.CandidateSpecJSON)
	if err != nil {
		return err
	}
	err = store.BindCommittedStackSpecCanonical(ctx, req.TenantID, req.OwnerID, req.StackID, controlplane.CommittedStackSpecCanonical{SourceSHA256: digest, SpecJSON: canonical})
	if errors.Is(err, controlplane.ErrNotFound) {
		return nil
	}
	return err
}
