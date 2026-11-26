package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kombifyio/homelab/internal/stackkitrelease"

	"github.com/kombifyio/homelab/pkg/specv2"
	"github.com/kombifyio/homelab/pkg/unifier"
)

// RolloutRetryCandidateSHA256Field is the existing exact recovery receipt's
// native candidate digest, shared by dispatch and its execution boundary.
const RolloutRetryCandidateSHA256Field = "rollout_retry_candidate_sha256"

// RolloutRetryCurrentIntentField carries the validated failed Apply's node
// intent binding so recovery can obtain that document's normalized CAS hash.
const RolloutRetryCurrentIntentField = "rollout_retry_current_intent"

// RolloutRetryCurrentSpecHashField is a sealed caller CAS precondition, never
// authorization. StackKits compares it with the actual node intent at INIT.
const RolloutRetryCurrentSpecHashField = "rollout_retry_current_spec_hash"

func ValidRolloutRetryCurrentSpecHash(value string) bool {
	return stackKitSnapshotAnchorPattern.MatchString(value)
}

// rolloutRetryNodeInitSpecFiles are the only node-side names INIT writes a
// single canonical document to. A retry addresses one of them when its
// controller-side snapshot name differs. The address-bound and projected
// documents are never renamed into: INIT must not replace the ADDRESS_BIND
// document, and a retry that lost its bound mode is refused instead.
var rolloutRetryNodeInitSpecFiles = map[string]bool{"stack-spec.yaml": true, "stack-spec.v2.json": true}

// bindRolloutRetryCurrentIntent rechecks, before PLAN, that this dispatch
// recovers exactly the node intent a failed Apply proved. "Exact" binds the
// stack, source, lease, server, agent, StackKit and the node-side file; the
// document that is applied is the stack's current admitted candidate (the
// owner-approved repair, verified by its digest), not the failed document. A refusal names every binding that failed.
func bindRolloutRetryCurrentIntent(job *Job, request *StackKitLifecycleRequest, leaseID, serverID string) error {
	value, present := job.Payload[RolloutRetryCurrentIntentField]
	if !present {
		if _, hasHash := job.Payload[RolloutRetryCurrentSpecHashField]; hasHash {
			return fmt.Errorf("native retry CAS precondition has no admitted intent lineage")
		}
		return nil
	}
	prior := mapFromInterface(value)
	// The failed Apply recorded the controller's executed document. A split
	// prefix binding executes its bound document, not the node's init file.
	bound := request.BoundSpecPath != ""
	priorSpecBase := filepath.Base(stringFromMap(prior, "spec_path"))
	if stringFromMap(prior, "spec_path") == "" {
		priorSpecBase = ""
	}
	// The candidate snapshot was already verified during deploy preparation.
	// Recheck the exact source/lease/runtime binding before PLAN. Ids are named,
	// never printed; release tags, StackKit and file basenames are not secret.
	var failed []string
	check := func(ok bool, detail string) {
		if !ok {
			failed = append(failed, detail)
		}
	}
	check(stringFromMap(job.Payload, "rollout_retry_kind") == "exact-existing-runtime", "rollout_retry_kind")
	check(stringFromMap(job.Payload, RolloutRetryCandidateSHA256Field) != "", "candidate_sha256 missing")
	check(stringFromMap(job.Payload, "rollout_retry_replacement_job_id") == job.ID, "replacement_job_id")
	check(stringFromMap(prior, "source_job_id") == stringFromMap(job.Payload, "rollout_retry_source_job_id"), "source_job_id")
	check(stringFromMap(prior, "lease_id") == stringFromMap(job.Payload, "rollout_retry_lease_id") && stringFromMap(prior, "lease_id") == leaseID, "lease_id")
	check(stringFromMap(prior, "server_id") == stringFromMap(job.Payload, "rollout_retry_server_id") && stringFromMap(prior, "server_id") == serverID, "server_id")
	check(stringFromMap(prior, "agent_id") == request.AgentID, "agent_id")
	check(stringFromMap(prior, "stackkit") == request.StackKit, fmt.Sprintf("stackkit %s != %s", stringFromMap(prior, "stackkit"), request.StackKit))
	rebaseNodeSpec := false
	switch {
	case priorSpecBase == "":
		failed = append(failed, "spec_path missing")
	case strings.HasPrefix(priorSpecBase, "stack-spec.snapshot-"):
		failed = append(failed, fmt.Sprintf("spec_path %s is a controller snapshot name that does not exist on the node", priorSpecBase))
	case bound:
		// The address-bound document keeps a fixed basename on the node.
		check(priorSpecBase == request.BoundSpecPath, fmt.Sprintf("spec_path %s != %s", priorSpecBase, request.BoundSpecPath))
	case priorSpecBase != request.SpecPath:
		// A retry executes its content-addressed snapshot, so the controller-side
		// file name legitimately differs from the failed Apply's. The document is
		// bound by the admitted candidate digest; the node-side file is the one the
		// failed Apply's intent lives in, so the retry addresses that name.
		if rolloutRetryNodeInitSpecFiles[priorSpecBase] {
			rebaseNodeSpec = true
		} else {
			failed = append(failed, fmt.Sprintf("spec_path %s != %s (the recorded node document is not a single init file; it cannot be replaced by INIT)", priorSpecBase, request.SpecPath))
		}
	}
	// A moved release is not the identity of the document: the admitted
	// candidate digest is. A failed-Apply lineage recovers by the hash PLAN reads
	// from the node; a caller-hash lineage by the hash the caller read, which
	// StackKits compares with the node's actual intent at INIT. Both stay
	// compare-and-swap, so the release the lineage recorded is informational.
	hash, hasHash := job.Payload[RolloutRetryCurrentSpecHashField]
	if len(failed) > 0 {
		return fmt.Errorf("native retry current intent does not match its exact dispatch binding: %s", strings.Join(failed, "; "))
	}
	if rebaseNodeSpec {
		request.SpecPath = priorSpecBase
	}
	// A split prefix binding has two independently persisted documents. Its
	// bound generation hash must never authorize replacement of the init file,
	// so its retry sends no CAS precondition: StackKits accepts the exact init
	// candidate as already applied and refuses any differing replacement.
	if request.AddressPrefix != "" || request.BoundSpecPath != "" {
		if hasHash || stringFromMap(prior, "apply_source_job_id") != "" {
			return fmt.Errorf("native retry current intent requires one canonical node document")
		}
		return nil
	}
	if hasHash {
		currentHash, valid := hash.(string)
		if !valid || !ValidRolloutRetryCurrentSpecHash(currentHash) ||
			stringFromMap(prior, "apply_source_job_id") == "" ||
			stringFromMap(prior, "apply_source_job_id") == stringFromMap(prior, "source_job_id") {
			return fmt.Errorf("native retry CAS precondition has no validated partial INIT lineage")
		}
		request.ExpectedSpecHash = currentHash
	} else {
		if stringFromMap(prior, "apply_source_job_id") != "" {
			return fmt.Errorf("partial INIT recovery requires its current normalized spec hash")
		}
		request.inspectCurrentIntent = true
	}
	return nil
}

func materializeRolloutRetryProjection(persister *unifier.SpecPersister, job *Job) (string, error) {
	if persister == nil || job == nil || stringFromMap(job.Payload, "rollout_retry_kind") == "" {
		return "", nil
	}
	projection := mapFromInterface(job.Payload[payloadKeyStackSpecV2])
	if len(projection) == 0 {
		if stringFromMap(job.Payload, RolloutRetryCandidateSHA256Field) != "" {
			return "", fmt.Errorf("native retry candidate snapshot is missing")
		}
		return "", nil
	}
	if stringFromMap(job.Payload, "rollout_retry_kind") != "exact-existing-runtime" ||
		stringFromMap(job.Payload, "rollout_retry_source_job_id") == "" ||
		stringFromMap(job.Payload, "rollout_retry_key") == "" ||
		stringFromMap(job.Payload, "rollout_retry_lease_id") == "" ||
		stringFromMap(job.Payload, "rollout_retry_server_id") == "" ||
		stringFromMap(job.Payload, "rollout_retry_replacement_job_id") != job.ID {
		return "", fmt.Errorf("native retry candidate has no exact dispatch context")
	}
	if err := specv2.RequireCanonicalV2(projection); err != nil {
		return "", fmt.Errorf("native retry candidate: %w", err)
	}
	raw, err := json.Marshal(projection)
	if err != nil {
		return "", fmt.Errorf("serialize native retry candidate: %w", err)
	}
	want := strings.TrimSpace(stringFromMap(job.Payload, RolloutRetryCandidateSHA256Field))
	if want == "" || want != fmt.Sprintf("sha256:%x", sha256.Sum256(raw)) {
		return "", fmt.Errorf("native retry candidate does not match its dispatch digest")
	}
	return persister.SaveStackSpecSnapshotBytes(raw)
}

func managedRolloutRetryProjectionForStack(ctx context.Context, tenant, stack string, persister *unifier.SpecPersister, snapshotPath, dispatchID string) (string, error) {
	return managedRolloutRetryProjectionWithRelease(persister, snapshotPath, dispatchID, func() (*stackkitrelease.Release, error) { return TargetStackKitReleaseForStack(ctx, tenant, stack) })
}
func managedRolloutRetryProjectionWithRelease(persister *unifier.SpecPersister, snapshotPath, dispatchID string, resolve func() (*stackkitrelease.Release, error)) (string, error) {
	release, err := resolve()
	if err != nil {
		return "", err
	}
	if release == nil {
		return "", fmt.Errorf("native retry requires its pinned StackKits release")
	}
	raw, err := os.ReadFile(snapshotPath) // #nosec G304 -- content-addressed path from the confined SpecPersister.
	if err != nil {
		return "", err
	}
	managed, err := advancedGenerationCandidate(raw, *release)
	if err != nil {
		return "", err
	}
	return persister.SaveStackSpecSnapshotForDispatch(managed, dispatchID)
}
