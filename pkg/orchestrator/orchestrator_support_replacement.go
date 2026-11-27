package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kombifyio/homelab/internal/providercontrol"
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/jobs"
)

// ConfigureSupportReplacement late-binds the Memory Cube S support replacement
// coordinator and step ops onto the orchestrator and re-registers job handlers.
func (o *Orchestrator) ConfigureSupportReplacement(cfg *jobs.SupportReplacementConfig) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cfg.SupportReplacement = cfg
	jobs.RegisterDefaultHandlers(o.queue, o.provisionConfig(o.cfg.RuntimeActions))
}

// EnqueueSupportIONOSMemoryReplacement submits a background job for the free
// staff-authorized Memory Cube S replacement workflow with durable single-flight
// and idempotency_key enforcement.
func (o *Orchestrator) EnqueueSupportIONOSMemoryReplacement(
	ctx context.Context,
	tenantID, ownerID, stackID, replacementID, idempotencyKey string,
	forcePreWriteRollback bool,
	rollbackReason string,
) (string, error) {
	if o == nil || o.queue == nil {
		return "", fmt.Errorf("orchestrator queue is not configured")
	}
	if ctx == nil {
		ctx = o.ctx
	}
	tenantID = strings.TrimSpace(tenantID)
	ownerID = strings.TrimSpace(ownerID)
	stackID = strings.TrimSpace(stackID)
	replacementID = strings.TrimSpace(replacementID)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	rollbackReason = strings.TrimSpace(rollbackReason)
	if tenantID == "" || stackID == "" || replacementID == "" {
		return "", fmt.Errorf("%w: tenant_id, stack_id, and replacement_id are required", providercontrol.ErrInvalidRequest)
	}
	if len(idempotencyKey) == 0 || len(idempotencyKey) > 128 || strings.ContainsAny(idempotencyKey, "\r\n\x00") {
		return "", fmt.Errorf("%w: idempotency_key is required", providercontrol.ErrInvalidRequest)
	}

	intentBytes, err := json.Marshal(struct {
		TenantID              string `json:"tenant_id"`
		OwnerID               string `json:"owner_id"`
		StackID               string `json:"stack_id"`
		ReplacementID         string `json:"replacement_id"`
		ForcePreWriteRollback bool   `json:"force_pre_write_rollback"`
		RollbackReason        string `json:"rollback_reason"`
	}{
		TenantID:              tenantID,
		OwnerID:               ownerID,
		StackID:               stackID,
		ReplacementID:         replacementID,
		ForcePreWriteRollback: forcePreWriteRollback,
		RollbackReason:        rollbackReason,
	})
	if err != nil {
		return "", fmt.Errorf("encode support replacement intent: %w", err)
	}
	intentDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(intentBytes))
	keySum := sha256.Sum256([]byte(tenantID + "\x00" + replacementID + "\x00" + idempotencyKey))
	jobID := fmt.Sprintf("sr-run-%x", keySum[:16])

	unlockStack := o.stackLocks.lock(stackID)
	defer unlockStack()
	o.mu.RLock()
	defer o.mu.RUnlock()

	job := &jobs.Job{
		ID:          jobID,
		Type:        jobs.JobTypeSupportIONOSMemoryReplacement,
		TargetType:  targetTypeStack,
		TargetID:    stackID,
		MaxAttempts: 1,
		Payload: map[string]interface{}{
			"tenant_id":                tenantID,
			"owner_id":                 ownerID,
			"stack_id":                 stackID,
			"replacement_id":           replacementID,
			"idempotency_key":          idempotencyKey,
			"force_pre_write_rollback": forcePreWriteRollback,
			"rollback_reason":          rollbackReason,
		},
		Result: map[string]interface{}{
			"support_replacement_request_sha256": intentDigest,
		},
	}

	if o.jobStore == nil {
		id, _, err := o.queue.EnqueueSupportReplacement(job, tenantID, replacementID, intentDigest)
		if err != nil {
			if errors.Is(err, jobs.ErrSupportReplacementConflict) {
				return "", fmt.Errorf("%w: %v", providercontrol.ErrNativeAdmissionConflict, err)
			}
			return "", err
		}
		return id, nil
	}

	if existing, getErr := o.jobStore.GetJob(ctx, tenantID, jobID); getErr == nil {
		existingDigest, _ := existing.Result["support_replacement_request_sha256"].(string)
		if existing.Type != string(jobs.JobTypeSupportIONOSMemoryReplacement) || existing.StackID != stackID || existingDigest != intentDigest {
			return "", fmt.Errorf("%w: idempotency key %q already used with different support replacement run intent", providercontrol.ErrNativeAdmissionConflict, idempotencyKey)
		}
		return existing.ID, nil
	} else if !errors.Is(getErr, controlplane.ErrNotFound) {
		return "", fmt.Errorf("check durable support replacement job: %w", getErr)
	}

	recent, listErr := o.jobStore.ListJobsByStack(ctx, tenantID, stackID, 50)
	if listErr != nil && !errors.Is(listErr, controlplane.ErrNotFound) {
		return "", fmt.Errorf("list active support replacement jobs: %w", listErr)
	}
	for _, j := range recent {
		if j.Type == string(jobs.JobTypeSupportIONOSMemoryReplacement) && (j.State == persistentStatePending || j.State == persistentStateRunning) {
			return "", fmt.Errorf("%w: active support replacement job %q already in flight", providercontrol.ErrNativeAdmissionConflict, j.ID)
		}
	}

	if err := o.enqueueWithSync(job, tenantID); err != nil {
		if existing, getErr := o.jobStore.GetJob(ctx, tenantID, jobID); getErr == nil {
			existingDigest, _ := existing.Result["support_replacement_request_sha256"].(string)
			if existing.Type == string(jobs.JobTypeSupportIONOSMemoryReplacement) && existing.StackID == stackID && existingDigest == intentDigest {
				return existing.ID, nil
			}
			return "", fmt.Errorf("%w: idempotency key %q already used with different support replacement run intent", providercontrol.ErrNativeAdmissionConflict, idempotencyKey)
		}
		if errors.Is(err, controlplane.ErrConflict) || errors.Is(err, jobs.ErrSupportReplacementConflict) {
			return "", fmt.Errorf("%w: %v", providercontrol.ErrNativeAdmissionConflict, err)
		}
		return "", err
	}
	return job.ID, nil
}
