package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
)

const committedStackSpecsKey = "committed_stack_spec_snapshots"
const maxUnreferencedCommittedSpecs = 64
const maxCommittedStackSpecTargets = 128

const (
	ManagedWikiAdmissionAddition = "addition"
	ManagedWikiAdmissionExisting = "existing"
	ManagedWikiAdmissionRemoval  = "removal"
)

var ErrManagedWikiInstallNotAdmitted = errors.New("managed Wiki installation does not have an admission for the selected workload")

// This proof travels only between trusted authoring/checkpoint code and the store.
// It is never decoded from a public HTTP request.
type ManagedWikiCommitProof struct {
	Admission       string `json:"-"`
	ExplicitRemoval bool   `json:"-"`
}

type ManagedWikiCommitAdmission struct {
	TenantID          string `json:"tenant_id"`
	OwnerID           string `json:"owner_id"`
	StackID           string `json:"stack_id"`
	SpecSHA256        string `json:"spec_sha256"`
	SourceSHA256      string `json:"source_sha256"`
	Kind              string `json:"kind"`
	CandidateSHA256   string `json:"candidate_sha256,omitempty"`
	RollbackTargetRef string `json:"rollback_target_ref,omitempty"`
}

func committedWikiPresent(config map[string]any) (bool, error) {
	raw, err := json.Marshal(config["stack_spec_v2"])
	if err != nil {
		return false, err
	}
	var spec struct {
		Workloads map[string]json.RawMessage `json:"workloads"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return false, err
	}
	workload := spec.Workloads["wiki"]
	if len(workload) == 0 {
		return false, nil
	}
	var selected struct {
		Enabled *bool `json:"enabled"`
	}
	if bytes.Equal(bytes.TrimSpace(workload), []byte("null")) {
		return false, errors.New("Wiki workload must be an object")
	}
	if err := json.Unmarshal(workload, &selected); err != nil {
		return false, err
	}
	return selected.Enabled == nil || *selected.Enabled, nil
}

func validateManagedWikiCommit(stack Stack, proposed map[string]any, proof ManagedWikiCommitProof) error {
	before, err := committedWikiPresent(stack.Config)
	if err != nil {
		return err
	}
	after, err := committedWikiPresent(proposed)
	if err != nil {
		return err
	}
	if before == after {
		return nil
	}
	var admission ManagedWikiCommitAdmission
	if json.Unmarshal([]byte(proof.Admission), &admission) != nil || admission.TenantID == "" || admission.OwnerID == "" || admission.StackID == "" ||
		admission.TenantID != stack.TenantID || admission.OwnerID != stack.OwnerSubjectID || admission.StackID != stack.ID || admission.RollbackTargetRef != "" || admission.CandidateSHA256 != "" {
		return ErrManagedWikiInstallNotAdmitted
	}
	raw, err := json.Marshal(proposed["stack_spec_v2"])
	if err != nil {
		return err
	}
	digest, err := CommittedStackSpecDigest(raw)
	if err != nil {
		return err
	}
	source, err := json.Marshal(stack.Config["stack_spec_v2"])
	if err != nil {
		return err
	}
	sourceDigest, err := CommittedStackSpecDigest(source)
	if err != nil {
		return err
	}
	kind := ManagedWikiAdmissionAddition
	if before {
		kind = ManagedWikiAdmissionRemoval
	}
	if digest != admission.SpecSHA256 || admission.SourceSHA256 != sourceDigest || admission.Kind != kind || (before && !proof.ExplicitRemoval) {
		return ErrManagedWikiInstallNotAdmitted
	}
	return nil
}

// Native target aliases are accepted only from the authenticated change-set
// result, after its baseline matches a spec committed in this stack's row.
type CommittedStackSpecTarget struct {
	TenantID   string `json:"tenant_id"`
	OwnerID    string `json:"owner_id"`
	StackID    string `json:"stack_id"`
	AgentID    string `json:"agent_id"`
	NodeRef    string `json:"node_ref"`
	TargetRef  string `json:"target_ref"`
	SpecSHA256 string `json:"spec_sha256"`
}

type committedStackSpecs struct {
	Specs       map[string]json.RawMessage          `json:"specs"`
	Targets     map[string]CommittedStackSpecTarget `json:"targets"`
	Order       []string                            `json:"order,omitempty"`
	Canonicals  map[string][]string                 `json:"canonicals,omitempty"`
	TargetOrder []string                            `json:"target_order,omitempty"`
}

type CommittedStackSpecCanonical struct {
	SourceSHA256 string
	SpecJSON     json.RawMessage
}

type CommittedStackSpecStore interface {
	BindCommittedStackSpecCanonical(context.Context, string, string, string, CommittedStackSpecCanonical) error
	BindCommittedStackSpecTarget(context.Context, CommittedStackSpecTarget) error
	ReadCommittedStackSpecTarget(context.Context, CommittedStackSpecTarget) ([]byte, error)
}

// Only the native dispatcher's mandatory generation target is normalized.
func CommittedStackSpecDigest(raw []byte) (string, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		raw = []byte(`{}`)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		return "", err
	}
	if spec == nil {
		return "", errors.New("committed StackSpec must be an object")
	}
	generation, _ := spec["generation"].(map[string]any)
	if generation == nil {
		generation = map[string]any{}
	}
	generation["target"] = "terramate"
	spec["generation"] = generation
	canonical, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func readCommittedStackSpecs(config map[string]any) (committedStackSpecs, error) {
	out := committedStackSpecs{Specs: map[string]json.RawMessage{}, Targets: map[string]CommittedStackSpecTarget{}}
	if raw := config[committedStackSpecsKey]; raw != nil {
		encoded, err := json.Marshal(raw)
		if err != nil {
			return out, err
		}
		if err = json.Unmarshal(encoded, &out); err != nil {
			return out, err
		}
		if out.Specs == nil || out.Targets == nil {
			return out, ErrConflict
		}
	}
	if out.Canonicals == nil {
		out.Canonicals = map[string][]string{}
	}
	if len(out.Order) == 0 {
		for digest := range out.Specs {
			out.Order = append(out.Order, digest)
		}
		sort.Strings(out.Order)
	}
	if len(out.TargetOrder) == 0 {
		for key := range out.Targets {
			out.TargetOrder = append(out.TargetOrder, key)
		}
		sort.Strings(out.TargetOrder)
	}
	return out, nil
}

func committedTargetKey(target CommittedStackSpecTarget) string {
	raw, _ := json.Marshal([]string{target.AgentID, target.NodeRef, target.TargetRef})
	return string(raw)
}

// Called inside the same create/CAS transaction as the desired-state write.
// The caller's replacement config cannot replace or fabricate this catalog.
func retainCommittedStackSpecs(stack Stack, proposed map[string]any, target *CommittedStackSpecTarget, canonical *CommittedStackSpecCanonical, proofs ...ManagedWikiCommitProof) (map[string]any, error) {
	var proof ManagedWikiCommitProof
	if len(proofs) > 0 {
		proof = proofs[0]
	}
	if err := validateManagedWikiCommit(stack, proposed, proof); err != nil {
		return nil, err
	}
	catalog, err := readCommittedStackSpecs(stack.Config)
	if err != nil {
		return nil, err
	}
	for _, config := range []map[string]any{stack.Config, proposed} {
		if spec := config["stack_spec_v2"]; spec != nil {
			raw, err := json.Marshal(spec)
			if err != nil {
				return nil, err
			}
			digest, err := CommittedStackSpecDigest(raw)
			if err != nil {
				return nil, err
			}
			catalog.retain(digest, raw)
		}
	}
	if canonical != nil {
		if _, ok := catalog.Specs[canonical.SourceSHA256]; !ok {
			return nil, ErrNotFound
		}
		digest, err := CommittedStackSpecDigest(canonical.SpecJSON)
		if err != nil {
			return nil, err
		}
		source := Stack{Config: map[string]any{"stack_spec_v2": json.RawMessage(catalog.Specs[canonical.SourceSHA256])}}
		if err := validateManagedWikiCommit(source, map[string]any{"stack_spec_v2": canonical.SpecJSON}, ManagedWikiCommitProof{}); err != nil {
			return nil, err
		}
		catalog.retain(digest, append(json.RawMessage(nil), canonical.SpecJSON...))
		if digest != canonical.SourceSHA256 {
			found := false
			for _, old := range catalog.Canonicals[canonical.SourceSHA256] {
				if old == digest {
					found = true
				}
			}
			if !found {
				catalog.Canonicals[canonical.SourceSHA256] = append(catalog.Canonicals[canonical.SourceSHA256], digest)
			}
		}
	}
	if target != nil {
		if target.TenantID != stack.TenantID || target.OwnerID != stack.OwnerSubjectID || target.StackID != stack.ID || target.AgentID == "" || target.NodeRef == "" || target.TargetRef == "" {
			return nil, ErrConflict
		}
		key := committedTargetKey(*target)
		if old, ok := catalog.Targets[key]; ok && old != *target {
			return nil, ErrConflict
		}
		if _, ok := catalog.Specs[target.SpecSHA256]; !ok {
			return nil, ErrNotFound
		}
		if _, exists := catalog.Targets[key]; !exists {
			catalog.TargetOrder = append(catalog.TargetOrder, key)
		}
		catalog.Targets[key] = *target
	}
	catalog.prune()
	config := cloneMap(proposed)
	if config == nil {
		config = map[string]any{}
	}
	if len(catalog.Specs) > 0 || len(catalog.Targets) > 0 {
		config[committedStackSpecsKey] = catalog
	} else {
		delete(config, committedStackSpecsKey)
	}
	return config, nil
}

func (catalog *committedStackSpecs) retain(digest string, raw json.RawMessage) {
	order := catalog.Order[:0]
	for _, old := range catalog.Order {
		if old != digest {
			order = append(order, old)
		}
	}
	catalog.Order = append(order, digest)
	catalog.Specs[digest] = raw
}

func (catalog *committedStackSpecs) prune() {
	if excess := len(catalog.TargetOrder) - maxCommittedStackSpecTargets; excess > 0 {
		for _, key := range catalog.TargetOrder[:excess] {
			delete(catalog.Targets, key)
		}
		catalog.TargetOrder = append([]string(nil), catalog.TargetOrder[excess:]...)
	}
	keep := map[string]bool{}
	for _, target := range catalog.Targets {
		keep[target.SpecSHA256] = true
	}
	for i, n := len(catalog.Order)-1, 0; i >= 0 && n < maxUnreferencedCommittedSpecs; i-- {
		digest := catalog.Order[i]
		if !keep[digest] {
			keep[digest] = true
			n++
		}
	}
	order := catalog.Order[:0]
	for _, digest := range catalog.Order {
		if keep[digest] {
			order = append(order, digest)
		} else {
			delete(catalog.Specs, digest)
		}
	}
	catalog.Order = order
	for source, canonicals := range catalog.Canonicals {
		if !keep[source] {
			delete(catalog.Canonicals, source)
			continue
		}
		retained := canonicals[:0]
		for _, digest := range canonicals {
			if keep[digest] {
				retained = append(retained, digest)
			}
		}
		catalog.Canonicals[source] = retained
	}
}

// A missing source does not prevent deployment; it cannot grant attribution.
func HasCommittedStackSpec(config map[string]any, source []byte) (bool, error) {
	catalog, err := readCommittedStackSpecs(config)
	if err != nil {
		return false, err
	}
	digest, err := CommittedStackSpecDigest(source)
	if err != nil {
		return false, err
	}
	raw, ok := catalog.Specs[digest]
	if !ok {
		return false, nil
	}
	actual, err := CommittedStackSpecDigest(raw)
	if err != nil {
		return false, err
	}
	if actual != digest {
		return false, ErrConflict
	}
	return true, nil
}

// Only native representations explicitly bound to this exact committed source.
func CommittedStackSpecCanonicals(config map[string]any, source []byte) ([][]byte, error) {
	catalog, err := readCommittedStackSpecs(config)
	if err != nil {
		return nil, err
	}
	digest, err := CommittedStackSpecDigest(source)
	if err != nil {
		return nil, err
	}
	var out [][]byte
	for _, canonical := range catalog.Canonicals[digest] {
		raw, ok := catalog.Specs[canonical]
		if !ok {
			return nil, ErrConflict
		}
		actual, err := CommittedStackSpecDigest(raw)
		if err != nil {
			return nil, err
		}
		if actual != canonical {
			return nil, ErrConflict
		}
		out = append(out, append([]byte(nil), raw...))
	}
	return out, nil
}

func bindCommittedStackSpecTarget(ctx context.Context, store StackStore, target CommittedStackSpecTarget) error {
	for attempt := 0; attempt < 3; attempt++ {
		stack, err := store.GetStack(ctx, target.TenantID, target.StackID)
		if err != nil {
			return err
		}
		if stack.OwnerSubjectID != target.OwnerID {
			return ErrNotFound
		}
		_, err = store.CompareAndSwapStackConfig(ctx, StackConfigCAS{TenantID: target.TenantID, StackID: target.StackID, ExpectedUpdatedAt: stack.UpdatedAt, Config: stack.Config, CommittedSpecTarget: &target})
		if !errors.Is(err, ErrConflict) {
			return err
		}
	}
	return ErrConflict
}

func readCommittedStackSpecTarget(ctx context.Context, store StackStore, target CommittedStackSpecTarget) ([]byte, error) {
	stack, err := store.GetStack(ctx, target.TenantID, target.StackID)
	if err != nil {
		return nil, err
	}
	if stack.OwnerSubjectID != target.OwnerID {
		return nil, ErrNotFound
	}
	return ResolveCommittedStackSpecTarget(*stack, target)
}

func ResolveCommittedStackSpecTarget(stack Stack, target CommittedStackSpecTarget) ([]byte, error) {
	if stack.TenantID != target.TenantID || stack.OwnerSubjectID != target.OwnerID || stack.ID != target.StackID {
		return nil, ErrNotFound
	}
	catalog, err := readCommittedStackSpecs(stack.Config)
	if err != nil {
		return nil, err
	}
	found, ok := catalog.Targets[committedTargetKey(target)]
	if !ok || found.TenantID != target.TenantID || found.OwnerID != target.OwnerID || found.StackID != target.StackID {
		return nil, ErrNotFound
	}
	if target.SpecSHA256 != "" && target.SpecSHA256 != found.SpecSHA256 {
		return nil, ErrConflict
	}
	raw, ok := catalog.Specs[found.SpecSHA256]
	if !ok {
		return nil, ErrNotFound
	}
	digest, err := CommittedStackSpecDigest(raw)
	if err != nil {
		return nil, err
	}
	if digest != found.SpecSHA256 {
		return nil, ErrConflict
	}
	return append([]byte(nil), raw...), nil
}

func (s *PostgresStore) BindCommittedStackSpecTarget(ctx context.Context, target CommittedStackSpecTarget) error {
	return bindCommittedStackSpecTarget(ctx, s, target)
}
func (s *MemoryStore) BindCommittedStackSpecTarget(ctx context.Context, target CommittedStackSpecTarget) error {
	return bindCommittedStackSpecTarget(ctx, s, target)
}
func (s *PostgresStore) ReadCommittedStackSpecTarget(ctx context.Context, target CommittedStackSpecTarget) ([]byte, error) {
	return readCommittedStackSpecTarget(ctx, s, target)
}
func (s *MemoryStore) ReadCommittedStackSpecTarget(ctx context.Context, target CommittedStackSpecTarget) ([]byte, error) {
	return readCommittedStackSpecTarget(ctx, s, target)
}

func bindCommittedStackSpecCanonical(ctx context.Context, store StackStore, tenant, owner, stackID string, canonical CommittedStackSpecCanonical) error {
	for attempt := 0; attempt < 3; attempt++ {
		stack, err := store.GetStack(ctx, tenant, stackID)
		if err != nil {
			return err
		}
		if stack.OwnerSubjectID != owner {
			return ErrNotFound
		}
		_, err = store.CompareAndSwapStackConfig(ctx, StackConfigCAS{TenantID: tenant, StackID: stackID, ExpectedUpdatedAt: stack.UpdatedAt, Config: stack.Config, CommittedSpecCanonical: &canonical})
		if !errors.Is(err, ErrConflict) {
			return err
		}
	}
	return ErrConflict
}
func (s *PostgresStore) BindCommittedStackSpecCanonical(ctx context.Context, tenant, owner, stack string, canonical CommittedStackSpecCanonical) error {
	return bindCommittedStackSpecCanonical(ctx, s, tenant, owner, stack, canonical)
}
func (s *MemoryStore) BindCommittedStackSpecCanonical(ctx context.Context, tenant, owner, stack string, canonical CommittedStackSpecCanonical) error {
	return bindCommittedStackSpecCanonical(ctx, s, tenant, owner, stack, canonical)
}
