package orchestrator

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/jobs"
)

func (o *Orchestrator) rolloutRetryIntentForRequest(ctx context.Context, req RolloutRetryRequest, source controlplane.Job, serverID, candidateSHA string) (map[string]any, error) {
	failure, _ := providerIdentityMap(source.Result["typed_stackkit_rollout"])
	partialInit := stringFromAny(failure["operation"]) == jobs.StackKitLifecycleInit &&
		stringFromAny(source.Result[rolloutRetryKindField]) == rolloutRetryKindExact &&
		source.Result[jobs.RolloutRetryCurrentIntentField] != nil
	if req.CurrentSpecHash == "" {
		if partialInit {
			if intent := movedReleaseInspectIntent(req.StackID, source, failure, req.LeaseID, serverID); intent != nil {
				return intent, nil
			}
			return nil, fmt.Errorf("%w: partial INIT recovery requires its current normalized spec hash", ErrRolloutRetryInvalid)
		}
		intent, err := rolloutRetryCurrentIntent(source, req.LeaseID, serverID)
		if err != nil || intent != nil {
			return intent, err
		}
		return o.inheritedRolloutRetryIntent(ctx, req, source, serverID)
	}
	if !partialInit || candidateSHA == "" {
		return nil, fmt.Errorf("%w: current spec hash is only admitted for exact partial INIT recovery", ErrRolloutRetryInvalid)
	}

	// Historical job records supply the proof. The request supplies only the
	// node's CAS precondition; it cannot author a lineage or a runtime binding.
	seen := map[string]bool{}
	chain := []controlplane.Job{}
	current := source
	var intent map[string]any
	var applyID string
	// The walk covers what inheritance accepts: up to rolloutRetryLineageDepth
	// inherited hops, plus the failed INIT that is its source and the Apply.
	for depth := 0; depth < rolloutRetryLineageDepth+2; depth++ {
		if current.TenantID != req.TenantID || seen[current.ID] {
			return nil, fmt.Errorf("%w: native intent lineage has another tenant or a cycle", ErrRolloutRetryInvalid)
		}
		seen[current.ID] = true
		if err := validateRolloutRetrySource(current, req.StackID, req.LeaseID, serverID); err != nil {
			return nil, err
		}
		failure, _ = providerIdentityMap(current.Result["typed_stackkit_rollout"])
		if stringFromAny(failure["operation"]) == jobs.StackKitLifecycleApply {
			var err error
			intent, err = rolloutRetryCurrentIntent(current, req.LeaseID, serverID)
			if err != nil || intent == nil {
				return nil, fmt.Errorf("%w: native intent lineage has no exact failed APPLY proof", ErrRolloutRetryInvalid)
			}
			applyID = current.ID
			break
		}
		priorID := stringFromAny(current.Result[rolloutRetrySourceField])
		key := exactRolloutRetryKey(req.StackID, priorID, req.LeaseID)
		if _, typed := current.Result["typed_stackkit_rollout"]; !typed && depth > 0 {
			// An inherited hop: a replacement that shows no typed node failure and
			// whose dispatch recorded the intent it inherited. It is stepped over with
			// the same cross-checks inheritance applies, and its stored intent is
			// replayed below.
			if storedRolloutRetryIntent(req.StackID, current, req.LeaseID, serverID) == nil ||
				priorID == "" || stringFromAny(current.Result[rolloutRetryKeyField]) != key ||
				stringFromAny(current.Result[rolloutRetryLeaseField]) != req.LeaseID ||
				stringFromAny(current.Result[rolloutRetryServerField]) != serverID {
				return nil, fmt.Errorf("%w: native intent lineage crosses a retry that is not an exact inherited dispatch", ErrRolloutRetryInvalid)
			}
			chain = append(chain, current)
			previous, err := o.jobStore.GetJob(ctx, req.TenantID, priorID)
			if err != nil || previous == nil {
				return nil, fmt.Errorf("%w: native intent lineage job is unavailable", ErrRolloutRetryInvalid)
			}
			current = *previous
			continue
		}
		if stringFromAny(failure["operation"]) != jobs.StackKitLifecycleInit ||
			stringFromAny(failure["status"]) != "failed" ||
			stringFromAny(failure["command_id"]) != current.ID+"-"+jobs.StackKitLifecycleInit ||
			priorID == "" || stringFromAny(current.Result[rolloutRetryKindField]) != rolloutRetryKindExact ||
			stringFromAny(current.Result[rolloutRetryKeyField]) != key || current.ID != exactRolloutRetryJobID(key) ||
			stringFromAny(current.Result[rolloutRetryReplacementField]) != current.ID ||
			stringFromAny(current.Result[rolloutRetryLeaseField]) != req.LeaseID ||
			stringFromAny(current.Result[rolloutRetryServerField]) != serverID ||
			stringFromAny(current.Result[rolloutRetryCandidateField]) != candidateSHA {
			return nil, fmt.Errorf("%w: failed INIT is not this candidate's exact runtime dispatch", ErrRolloutRetryInvalid)
		}
		chain = append(chain, current)
		previous, err := o.jobStore.GetJob(ctx, req.TenantID, priorID)
		if err != nil || previous == nil {
			return nil, fmt.Errorf("%w: native intent lineage job is unavailable", ErrRolloutRetryInvalid)
		}
		current = *previous
	}
	if intent == nil {
		return nil, fmt.Errorf("%w: native intent lineage exceeds its bounded history", ErrRolloutRetryInvalid)
	}
	// Reconstruct each sealed dispatch from its previously verified producer.
	// Missing or changed proof refuses recovery even if its caller hash is valid.
	for index := len(chain) - 1; index >= 0; index-- {
		init := chain[index]
		stored, _ := providerIdentityMap(init.Result[jobs.RolloutRetryCurrentIntentField])
		failure, _ := providerIdentityMap(init.Result["typed_stackkit_rollout"])
		if _, typed := init.Result["typed_stackkit_rollout"]; !typed {
			if !reflect.DeepEqual(stored, intent) {
				return nil, fmt.Errorf("%w: inherited retry native intent proof changed", ErrRolloutRetryInvalid)
			}
			intent = inheritRolloutRetryIntent(intent, init.ID)
			continue
		}
		if !reflect.DeepEqual(stored, intent) {
			return nil, fmt.Errorf("%w: failed INIT native intent proof changed", ErrRolloutRetryInvalid)
		}
		intent = cloneProvisionSpec(intent)
		// The release is not the document's identity: StackKits compares the
		// caller's hash with the node's actual intent at INIT, so the CAS stays
		// meaningful across a moved pin. Each hop records the release it ran under,
		// and the next hop's stored intent was written from it.
		if recorded := strings.TrimSpace(stringFromAny(failure["release"])); recorded != "" {
			intent["release"] = recorded
		}
		intent["source_job_id"] = init.ID
		intent["apply_source_job_id"] = applyID
	}
	return intent, nil
}

// Boot recovery compares the sealed receipt and revalidates historical proof
// before re-enqueuing this optional CAS path. Ordinary pending jobs keep their
// existing recovery policy.
func (o *Orchestrator) validatePendingPartialInitRetry(ctx context.Context, row controlplane.Job) error {
	_, payloadHash := row.Payload[jobs.RolloutRetryCurrentSpecHashField]
	_, receiptHash := row.Result[jobs.RolloutRetryCurrentSpecHashField]
	intent, _ := providerIdentityMap(row.Result[jobs.RolloutRetryCurrentIntentField])
	if !payloadHash && !receiptHash && stringFromAny(intent["apply_source_job_id"]) == "" {
		return nil
	}
	exact, err := exactRolloutRetryReceipt(row.Payload, row.Result)
	if err != nil || !exact || stringFromAny(row.Result[rolloutRetryReplacementField]) != row.ID {
		return fmt.Errorf("%w: pending native retry payload differs from its receipt", ErrRolloutRetryInvalid)
	}
	candidate, _ := providerIdentityMap(row.Payload["stack_spec_v2"])
	req := RolloutRetryRequest{RequestContext: ctx, StackID: row.StackID, TenantID: row.TenantID,
		OwnerID: stringFromAny(row.Payload["owner_id"]), SourceJobID: stringFromAny(row.Result[rolloutRetrySourceField]),
		LeaseID: stringFromAny(row.Result[rolloutRetryLeaseField]), CandidateSpec: candidate, OwnerApproved: true,
		CurrentSpecHash: stringFromAny(row.Result[jobs.RolloutRetryCurrentSpecHashField])}
	if err := normalizeRolloutRetryRequest(&req); err != nil {
		return err
	}
	stack, _, opts, serverID, err := o.prepareRolloutRetry(ctx, req)
	if err != nil {
		return err
	}
	exact, err = exactRolloutRetryReceipt(row.Result, rolloutRetryDispatchResult(opts))
	if err != nil || !exact || serverID != stringFromAny(row.Result[rolloutRetryServerField]) {
		return fmt.Errorf("%w: pending native retry lineage changed", ErrRolloutRetryInvalid)
	}
	if err := o.validateExactRetryTarget(ctx, stack, req.LeaseID); err != nil {
		return err
	}
	return o.requireExactManagedRuntimeDeploy(ctx, stack, req.LeaseID, serverID)
}
