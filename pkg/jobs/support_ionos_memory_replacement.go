package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/kombifyio/homelab/internal/advancedissuer"
	"github.com/kombifyio/homelab/internal/runtimeproduct/runtimeaction"
	"github.com/kombifyio/homelab/internal/runtimeproduct/vmlease"
	"github.com/kombifyio/homelab/internal/stackkitrelease"
	agentpb "github.com/kombifyio/homelab/pkg/api/agentpb"
	"github.com/kombifyio/homelab/pkg/db/tenantcontext"
	"github.com/kombifyio/homelab/pkg/grpcserver"
	"github.com/kombifyio/homelab/pkg/kombifyme"
	"github.com/kombifyio/homelab/pkg/originca"
	"github.com/kombifyio/homelab/pkg/stackkitcommand"
	"github.com/kombifyio/homelab/pkg/vmleases"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// JobTypeSupportIONOSMemoryReplacement orchestrates the free support-authorized
	// replacement from IONOS Basic Cube S to IONOS Memory Cube S.
	JobTypeSupportIONOSMemoryReplacement JobType = "support_ionos_memory_replacement"
)

var (
	// ErrSupportReplacementAckLost marks an ambiguous / unanswered destination
	// write or activation result that must fence both Cubes instead of rolling back.
	ErrSupportReplacementAckLost = errors.New("jobs: support replacement destination acknowledgement lost")
	// ErrSupportReplacementFencedConflict marks a conflict state requiring manual
	// operator reconciliation before any further mutation.
	ErrSupportReplacementFencedConflict = errors.New("jobs: support replacement entered fenced_conflict")

	supportReplacementActivationSanitizer = regexp.MustCompile(`[^a-z0-9._-]+`)
)

type SupportReplacementCheckpoint struct {
	StackKit                       string   `json:"stackkit,omitempty"`
	StackName                      string   `json:"stack_name,omitempty"`
	StackKitInstanceID             string   `json:"stackkit_instance_id,omitempty"`
	SpecPath                       string   `json:"spec_path,omitempty"`
	BoundSpecPath                  string   `json:"bound_spec_path,omitempty"`
	LocalSiteRef                   string   `json:"local_site_ref,omitempty"`
	LocalNodeRef                   string   `json:"local_node_ref,omitempty"`
	LocalExecutionChannelRef       string   `json:"local_execution_channel_ref,omitempty"`
	RouteHosts                     []string `json:"route_hosts,omitempty"`
	DestOriginCertID               string   `json:"dest_origin_cert_id,omitempty"`
	DestOriginCertHosts            []string `json:"dest_origin_cert_hosts,omitempty"`
	DestOriginCertSHA256           string   `json:"dest_origin_cert_sha256,omitempty"`
	DestOriginCertNotAfter         string   `json:"dest_origin_cert_not_after,omitempty"`
	SourcePlanHash                 string   `json:"source_plan_hash,omitempty"`
	DestinationPlanHash            string   `json:"destination_plan_hash,omitempty"`
	DestinationApplied             bool     `json:"destination_applied,omitempty"`
	SnapshotAnchorID               string   `json:"snapshot_anchor_id,omitempty"`
	RestoreResultID                string   `json:"restore_result_id,omitempty"`
	FinalSnapshotAnchorID          string   `json:"final_snapshot_anchor_id,omitempty"`
	FinalRestoreResultID           string   `json:"final_restore_result_id,omitempty"`
	ActivationOperationID          string   `json:"activation_operation_id,omitempty"`
	ActivationNotStartedReasonCode string   `json:"activation_not_started_reason_code,omitempty"`
	WalletCustodyDelivered         bool     `json:"wallet_custody_delivered,omitempty"`
	StoppableServices              []string `json:"stoppable_services,omitempty"`
	FencedServices                 []string `json:"fenced_services,omitempty"`
	FenceVerified                  *bool    `json:"fence_verified,omitempty"`
	FenceError                     string   `json:"fence_error,omitempty"`
}

type SupportReplacementState struct {
	TenantID                      string          `json:"tenant_id"`
	ReplacementID                 string          `json:"replacement_id"`
	OwnerSubjectID                string          `json:"owner_subject_id"`
	StackID                       string          `json:"stack_id"`
	RuntimeSlotID                 string          `json:"runtime_slot_id"`
	SourceLeaseID                 string          `json:"source_lease_id"`
	SourceServerID                string          `json:"source_server_id"`
	SourceResourceGenerationID    string          `json:"source_resource_generation_id"`
	SourceGenerationOrdinal       int64           `json:"source_generation_ordinal"`
	DestinationLeaseID            string          `json:"destination_lease_id,omitempty"`
	DestinationServerID           string          `json:"destination_server_id,omitempty"`
	DestinationResourceGeneration string          `json:"destination_resource_generation_id,omitempty"`
	DestinationGenerationOrdinal  *int64          `json:"destination_generation_ordinal,omitempty"`
	DestinationOperationID        string          `json:"destination_operation_id,omitempty"`
	DestinationWritesObserved     bool            `json:"destination_writes_observed"`
	CutoverVerifiedAt             *time.Time      `json:"cutover_verified_at,omitempty"`
	SourceTeardownOperationID     string          `json:"source_teardown_operation_id,omitempty"`
	RollbackReason                string          `json:"rollback_reason,omitempty"`
	FencedConflictReason          string          `json:"fenced_conflict_reason,omitempty"`
	CheckpointJSON                json.RawMessage `json:"checkpoint_json,omitempty"`
	Phase                         string          `json:"phase"`
	NoChargeConfirmed             bool            `json:"no_charge_confirmed"`
	CustomerAcceptedAt            *time.Time      `json:"customer_accepted_at,omitempty"`
}

type SupportReplacementTransition struct {
	TenantID                  string
	ReplacementID             string
	Phase                     string
	DestinationWritesObserved *bool
	CutoverVerifiedAt         *time.Time
	SourceTeardownOperationID string
	RollbackReason            string
	FencedConflictReason      string
	CheckpointPatchJSON       json.RawMessage
}

type SupportReplacementStore interface {
	GetSupportReplacement(ctx context.Context, tenantID, replacementID string) (SupportReplacementState, error)
	AdmitSupportReplacementDestination(ctx context.Context, tenantID, replacementID, idempotencyKey string) (SupportReplacementState, error)
	TransitionSupportReplacement(ctx context.Context, req SupportReplacementTransition) (SupportReplacementState, error)
}

type SupportReplacementStepOps interface {
	EnsureDestinationGuardEnrolled(ctx context.Context, state SupportReplacementState) (string, error)
	ProveAndDeliverWalletCustody(ctx context.Context, state SupportReplacementState, destAgentID string) error
	StageAndRestoreData(ctx context.Context, state SupportReplacementState, destAgentID string) error
	VerifyIsolatedDestination(ctx context.Context, state SupportReplacementState, destAgentID string) error
	FenceSourceWriters(ctx context.Context, state SupportReplacementState) error
	FinalTransferAndConfirmDestinationWrites(ctx context.Context, state SupportReplacementState, destAgentID string) (wrote bool, ackLost bool, err error)
	SwitchAddressesAndIdentityAuthority(ctx context.Context, state SupportReplacementState) error
	VerifyLiveCutover(ctx context.Context, state SupportReplacementState, destAgentID string) error
	FinalizeCutoverCustody(ctx context.Context, state SupportReplacementState) error
	ResumeDestinationWriters(ctx context.Context, state SupportReplacementState) error
	VerifyNoDestinationWrites(ctx context.Context, state SupportReplacementState) (bool, error)
	ResumeSourceWriters(ctx context.Context, state SupportReplacementState) error
	DecommissionCube(ctx context.Context, tenantID, ownerID, stackID, leaseID, generationID string) (string, error)
	VerifyAbsenceAndRelease(ctx context.Context, tenantID, leaseID, generationID string) error
	DiscardReplacementBackupCustody(ctx context.Context, state SupportReplacementState) error
	RevokeStagedOriginCertificate(ctx context.Context, state SupportReplacementState) error
	FenceBothCubes(ctx context.Context, state SupportReplacementState, reason string) error
}

// SupportReplacementStackFenceChecker checks whether a stack is currently
// protected by an active or fenced_conflict support replacement.
type SupportReplacementStackFenceChecker interface {
	EnsureNoActiveSupportReplacementForStack(ctx context.Context, tenantID, stackID string) error
}

type SupportReplacementConfig struct {
	Store SupportReplacementStore
	Ops   SupportReplacementStepOps
}

type SupportReplacementRunOptions struct {
	ForcePreWriteRollback bool
	RollbackReason        string
}

type SupportReplacementWorkflow struct {
	Store SupportReplacementStore
	Ops   SupportReplacementStepOps
	Now   func() time.Time
}

func SupportIONOSMemoryReplacementHandler(cfg *ProvisionConfig) JobHandler {
	return func(ctx context.Context, job *Job, q *Queue) error {
		if cfg == nil || cfg.SupportReplacement == nil || cfg.SupportReplacement.Store == nil || cfg.SupportReplacement.Ops == nil {
			return NewPermanentError(fmt.Errorf("support replacement workflow is not configured"))
		}
		tenantID, _ := job.Payload["tenant_id"].(string)
		replacementID, _ := job.Payload["replacement_id"].(string)
		idempotencyKey, _ := job.Payload["idempotency_key"].(string)
		forceRollback, _ := job.Payload["force_pre_write_rollback"].(bool)
		rollbackReason, _ := job.Payload["rollback_reason"].(string)
		if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(replacementID) == "" {
			return NewPermanentError(fmt.Errorf("support replacement job requires tenant_id and replacement_id"))
		}
		if strings.TrimSpace(idempotencyKey) == "" {
			return NewPermanentError(fmt.Errorf("support replacement job requires idempotency_key"))
		}
		q.UpdateProgress(job.ID, 10, "Running fenced Memory Cube S support replacement...")
		ops := cfg.SupportReplacement.Ops
		if defaultOps, ok := ops.(*DefaultSupportReplacementStepOps); ok && defaultOps != nil {
			bound := *defaultOps
			if bound.Sender == nil {
				bound.Sender = cfg.StackKitCommander
			}
			if bound.Inventory == nil {
				bound.Inventory = cfg.ManagedStackKitInventory
			}
			if bound.AdvancedIssuer == nil {
				if cfg.AdvancedIssuer != nil {
					bound.AdvancedIssuer = cfg.AdvancedIssuer
				} else {
					bound.AdvancedIssuer = cfg.StackKitLifecycle.AdvancedIssuer
				}
			}
			if bound.BackupDelivery == nil {
				bound.BackupDelivery = managedBackupDeliveryOf(bound.Inventory)
			}
			if bound.OriginCertificates == nil {
				bound.OriginCertificates = cfg.OriginCertificates
			}
			if bound.StackKitLifecycle.Sender == nil {
				bound.StackKitLifecycle = cfg.StackKitLifecycle
			}
			ops = &bound
		}
		wf := &SupportReplacementWorkflow{
			Store: cfg.SupportReplacement.Store,
			Ops:   ops,
			Now:   time.Now,
		}
		state, err := wf.Run(ctx, strings.TrimSpace(tenantID), strings.TrimSpace(replacementID), job.ID, SupportReplacementRunOptions{
			ForcePreWriteRollback: forceRollback,
			RollbackReason:        rollbackReason,
		})
		job.mu.Lock()
		if job.Result == nil {
			job.Result = map[string]interface{}{}
		}
		job.Result["replacement_id"] = state.ReplacementID
		job.Result["phase"] = state.Phase
		job.Result["destination_lease_id"] = state.DestinationLeaseID
		job.Result["destination_writes_observed"] = state.DestinationWritesObserved
		job.mu.Unlock()
		if err != nil {
			if errors.Is(err, ErrSupportReplacementFencedConflict) {
				return NewPermanentError(err)
			}
			return err
		}
		q.UpdateProgress(job.ID, 100, "Support replacement settled in phase "+state.Phase)
		return nil
	}
}

func (w *SupportReplacementWorkflow) Run(
	ctx context.Context,
	tenantID, replacementID, jobID string,
	opts SupportReplacementRunOptions,
) (SupportReplacementState, error) {
	if w == nil || w.Store == nil || w.Ops == nil {
		return SupportReplacementState{}, NewPermanentError(fmt.Errorf("support replacement dependencies are nil"))
	}
	state, err := w.Store.GetSupportReplacement(ctx, tenantID, replacementID)
	if err != nil {
		return SupportReplacementState{}, err
	}
	if !state.NoChargeConfirmed || state.CustomerAcceptedAt == nil {
		return state, NewPermanentError(fmt.Errorf("support replacement %q requires confirmed zero-charge staff authorization and customer acceptance", replacementID))
	}
	switch state.Phase {
	case "completed", "rolled_back", "cancelled":
		return state, nil
	case "fenced_conflict":
		return state, fmt.Errorf("%w: %s", ErrSupportReplacementFencedConflict, state.FencedConflictReason)
	case "rolling_back":
		if strings.HasPrefix(state.FencedConflictReason, "reconciled_rollback:") {
			return w.resumeReconciledRollback(ctx, state)
		}
		return w.runRollingBackPhase(ctx, state)
	}

	if opts.ForcePreWriteRollback && !state.DestinationWritesObserved &&
		state.Phase != "cutover_verified" && state.Phase != "source_decommissioning" {
		reason := strings.TrimSpace(opts.RollbackReason)
		if reason == "" {
			reason = "operator forced pre-write rollback"
		}
		return w.rollbackPreWrite(ctx, state, reason)
	}

	if state.Phase == "accepted" {
		next, admitErr := w.Store.AdmitSupportReplacementDestination(ctx, tenantID, replacementID, "sr-admit:"+replacementID)
		if admitErr != nil {
			return state, admitErr
		}
		state = next
	}

	if state.Phase == "destination_admitted" {
		next, tErr := w.Store.TransitionSupportReplacement(ctx, SupportReplacementTransition{
			TenantID:      state.TenantID,
			ReplacementID: state.ReplacementID,
			Phase:         "staging",
		})
		if tErr != nil {
			return state, tErr
		}
		state = next
	}

	var destAgentID string
	if state.Phase == "staging" {
		next, agentID, sErr := w.runStagingPhase(ctx, state)
		if sErr != nil {
			return next, sErr
		}
		state = next
		destAgentID = agentID
	}

	if state.Phase == "cutover_in_progress" {
		next, cErr := w.runCutoverPhase(ctx, state, destAgentID)
		if cErr != nil {
			return next, cErr
		}
		state = next
	}

	if state.Phase == "cutover_verified" {
		if state.CutoverVerifiedAt == nil {
			return w.enterFencedConflict(ctx, state, "cutover_verified missing cutover_verified_at timestamp")
		}
		opID, decommErr := w.Ops.DecommissionCube(
			ctx, state.TenantID, state.OwnerSubjectID, state.StackID,
			state.SourceLeaseID, state.SourceResourceGenerationID,
		)
		if decommErr != nil {
			return state, fmt.Errorf("source decommission start failed: %w", decommErr)
		}
		next, tErr := w.Store.TransitionSupportReplacement(ctx, SupportReplacementTransition{
			TenantID:                  state.TenantID,
			ReplacementID:             state.ReplacementID,
			Phase:                     "source_decommissioning",
			SourceTeardownOperationID: opID,
		})
		if tErr != nil {
			return state, tErr
		}
		state = next
	}

	if state.Phase == "source_decommissioning" {
		if state.CutoverVerifiedAt == nil {
			return w.enterFencedConflict(ctx, state, "source_decommissioning missing cutover_verified_at timestamp")
		}
		if err := w.Ops.VerifyAbsenceAndRelease(ctx, state.TenantID, state.SourceLeaseID, state.SourceResourceGenerationID); err != nil {
			return state, &JobWaitError{
				Reason:      WaitReasonManagedRuntimeProvider,
				Message:     fmt.Sprintf("source release verification pending: %v", err),
				ResumeAfter: 5 * time.Second,
				Cause:       err,
			}
		}
		next, tErr := w.Store.TransitionSupportReplacement(ctx, SupportReplacementTransition{
			TenantID:      state.TenantID,
			ReplacementID: state.ReplacementID,
			Phase:         "completed",
		})
		if tErr != nil {
			return state, tErr
		}
		state = next
	}

	return state, nil
}

func (w *SupportReplacementWorkflow) runStagingPhase(
	ctx context.Context,
	state SupportReplacementState,
) (SupportReplacementState, string, error) {
	agentID, err := w.Ops.EnsureDestinationGuardEnrolled(ctx, state)
	if err != nil {
		if isJobWaitError(err) {
			return state, "", err
		}
		rolled, rbErr := w.rollbackPreWrite(ctx, state, "destination Guard enrollment failed: "+err.Error())
		if rbErr != nil {
			return rolled, "", rbErr
		}
		return rolled, "", err
	}
	if err := w.Ops.ProveAndDeliverWalletCustody(ctx, state, agentID); err != nil {
		rolled, rbErr := w.rollbackPreWrite(ctx, state, "destination Wallet recipient proof failed: "+err.Error())
		if rbErr != nil {
			return rolled, "", rbErr
		}
		return rolled, "", err
	}
	if refreshed, getErr := w.Store.GetSupportReplacement(ctx, state.TenantID, state.ReplacementID); getErr == nil {
		state = refreshed
	}
	if err := w.Ops.StageAndRestoreData(ctx, state, agentID); err != nil {
		rolled, rbErr := w.rollbackPreWrite(ctx, state, "staging restore failed: "+err.Error())
		if rbErr != nil {
			return rolled, "", rbErr
		}
		return rolled, "", err
	}
	if refreshed, getErr := w.Store.GetSupportReplacement(ctx, state.TenantID, state.ReplacementID); getErr == nil {
		state = refreshed
	}
	if err := w.Ops.VerifyIsolatedDestination(ctx, state, agentID); err != nil {
		rolled, rbErr := w.rollbackPreWrite(ctx, state, "isolated destination verification failed: "+err.Error())
		if rbErr != nil {
			return rolled, "", rbErr
		}
		return rolled, "", err
	}
	next, tErr := w.Store.TransitionSupportReplacement(ctx, SupportReplacementTransition{
		TenantID:      state.TenantID,
		ReplacementID: state.ReplacementID,
		Phase:         "cutover_in_progress",
	})
	if tErr != nil {
		return state, agentID, tErr
	}
	return next, agentID, nil
}

func (w *SupportReplacementWorkflow) runCutoverPhase(
	ctx context.Context,
	state SupportReplacementState,
	destAgentID string,
) (SupportReplacementState, error) {
	if strings.TrimSpace(destAgentID) == "" {
		var err error
		destAgentID, err = w.Ops.EnsureDestinationGuardEnrolled(ctx, state)
		if err != nil {
			if isJobWaitError(err) && !state.DestinationWritesObserved {
				return state, err
			}
			if state.DestinationWritesObserved {
				return w.enterFencedConflict(ctx, state, "destination Guard unreachable after writes observed: "+err.Error())
			}
			return w.rollbackPreWrite(ctx, state, "destination Guard unreachable before cutover writes: "+err.Error())
		}
	}

	if state.DestinationWritesObserved {
		// Resuming after writes were observed (operator reconcile from
		// fenced_conflict or a restarted job): FenceBothCubes may have stopped
		// the destination writers, so restart them before routes move to it.
		// The source stays fenced.
		if err := w.Ops.ResumeDestinationWriters(ctx, state); err != nil {
			return w.enterFencedConflict(ctx, state, "destination writer resume failed after writes observed: "+err.Error())
		}
	} else {
		if err := w.Ops.FenceSourceWriters(ctx, state); err != nil {
			return w.rollbackPreWrite(ctx, state, "source writer fence failed: "+err.Error())
		}
		wrote, ackLost, err := w.Ops.FinalTransferAndConfirmDestinationWrites(ctx, state, destAgentID)
		if ackLost || (err != nil && wrote) {
			reason := "destination final transfer acknowledgement lost"
			if err != nil {
				reason = "destination write state uncertain: " + err.Error()
			}
			if wrote {
				tVal := true
				next, tErr := w.Store.TransitionSupportReplacement(ctx, SupportReplacementTransition{
					TenantID:                  state.TenantID,
					ReplacementID:             state.ReplacementID,
					Phase:                     "cutover_in_progress",
					DestinationWritesObserved: &tVal,
				})
				if tErr == nil {
					state = next
				} else {
					reason = fmt.Sprintf("%s (persist destination_writes_observed failed: %v)", reason, tErr)
				}
			}
			return w.enterFencedConflict(ctx, state, reason)
		}
		if err != nil {
			rolled, rbErr := w.rollbackPreWrite(ctx, state, "pre-write final transfer failed: "+err.Error())
			if rbErr != nil {
				return rolled, rbErr
			}
			return rolled, err
		}
		if !wrote {
			return w.rollbackPreWrite(ctx, state, "final transfer reported zero destination writes")
		}
		tVal := true
		next, tErr := w.Store.TransitionSupportReplacement(ctx, SupportReplacementTransition{
			TenantID:                  state.TenantID,
			ReplacementID:             state.ReplacementID,
			Phase:                     "cutover_in_progress",
			DestinationWritesObserved: &tVal,
		})
		if tErr != nil {
			return w.enterFencedConflict(ctx, state, "destination writes occurred but failed to persist destination_writes_observed: "+tErr.Error())
		}
		state = next
	}

	if err := w.Ops.SwitchAddressesAndIdentityAuthority(ctx, state); err != nil {
		return w.enterFencedConflict(ctx, state, "address/identity switch failed after destination writes: "+err.Error())
	}
	if err := w.Ops.VerifyLiveCutover(ctx, state, destAgentID); err != nil {
		return w.enterFencedConflict(ctx, state, "live cutover verification failed after destination writes: "+err.Error())
	}
	if err := w.Ops.FinalizeCutoverCustody(ctx, state); err != nil {
		return w.enterFencedConflict(ctx, state, "cutover custody finalization failed after live verification: "+err.Error())
	}
	now := time.Now().UTC()
	if w.Now != nil {
		now = w.Now().UTC()
	}
	next, tErr := w.Store.TransitionSupportReplacement(ctx, SupportReplacementTransition{
		TenantID:          state.TenantID,
		ReplacementID:     state.ReplacementID,
		Phase:             "cutover_verified",
		CutoverVerifiedAt: &now,
	})
	if tErr != nil {
		return w.enterFencedConflict(ctx, state, "cutover verified on live nodes but failed to persist cutover_verified: "+tErr.Error())
	}
	return next, nil
}

func (w *SupportReplacementWorkflow) rollbackPreWrite(
	ctx context.Context,
	state SupportReplacementState,
	reason string,
) (SupportReplacementState, error) {
	if state.DestinationWritesObserved {
		return w.enterFencedConflict(ctx, state, "rollback forbidden after destination_writes_observed: "+reason)
	}
	if state.DestinationLeaseID != "" {
		clean, err := w.Ops.VerifyNoDestinationWrites(ctx, state)
		if err != nil || !clean {
			msg := "unable to verify absence of destination writes before rollback"
			if err != nil {
				msg += ": " + err.Error()
			}
			return w.enterFencedConflict(ctx, state, msg)
		}
	}
	if err := w.Ops.ResumeSourceWriters(ctx, state); err != nil {
		return w.enterFencedConflict(ctx, state, "failed to resume source writers during rollback: "+err.Error())
	}
	if state.DestinationLeaseID == "" {
		if err := w.Ops.DiscardReplacementBackupCustody(ctx, state); err != nil {
			return state, fmt.Errorf("rollback replacement backup custody cleanup failed: %w", err)
		}
		if err := w.Ops.RevokeStagedOriginCertificate(ctx, state); err != nil {
			return state, fmt.Errorf("rollback staged origin certificate revocation failed: %w", err)
		}
		next, tErr := w.Store.TransitionSupportReplacement(ctx, SupportReplacementTransition{
			TenantID:       state.TenantID,
			ReplacementID:  state.ReplacementID,
			Phase:          "rolled_back",
			RollbackReason: reason,
		})
		if tErr != nil {
			return state, tErr
		}
		return next, nil
	}
	if state.Phase != "rolling_back" {
		next, tErr := w.Store.TransitionSupportReplacement(ctx, SupportReplacementTransition{
			TenantID:       state.TenantID,
			ReplacementID:  state.ReplacementID,
			Phase:          "rolling_back",
			RollbackReason: reason,
		})
		if tErr != nil {
			return state, tErr
		}
		state = next
	}
	return w.runRollingBackPhase(ctx, state)
}

// resumeReconciledRollback drives an operator reconcile from fenced_conflict to
// rolling_back. FenceBothCubes stopped both Cubes and the conflict may stem
// from an uncertain activation, so destination write absence is proved again
// and the source writers are restarted before the destination is destroyed.
func (w *SupportReplacementWorkflow) resumeReconciledRollback(
	ctx context.Context,
	state SupportReplacementState,
) (SupportReplacementState, error) {
	if state.DestinationWritesObserved {
		return w.enterFencedConflict(ctx, state, "destination writes observed while rolling_back")
	}
	if state.DestinationLeaseID != "" {
		clean, err := w.Ops.VerifyNoDestinationWrites(ctx, state)
		if err != nil || !clean {
			msg := "unable to verify absence of destination writes before reconciled rollback"
			if err != nil {
				msg += ": " + err.Error()
			}
			return w.enterFencedConflict(ctx, state, msg)
		}
	}
	if err := w.Ops.ResumeSourceWriters(ctx, state); err != nil {
		return w.enterFencedConflict(ctx, state, "failed to resume source writers during reconciled rollback: "+err.Error())
	}
	return w.runRollingBackPhase(ctx, state)
}

func (w *SupportReplacementWorkflow) runRollingBackPhase(
	ctx context.Context,
	state SupportReplacementState,
) (SupportReplacementState, error) {
	if state.DestinationWritesObserved {
		return w.enterFencedConflict(ctx, state, "destination writes observed while rolling_back")
	}
	if state.DestinationLeaseID != "" {
		if _, err := w.Ops.DecommissionCube(
			ctx, state.TenantID, state.OwnerSubjectID, state.StackID,
			state.DestinationLeaseID, state.DestinationResourceGeneration,
		); err != nil {
			return state, fmt.Errorf("rollback destination decommission failed: %w", err)
		}
		if err := w.Ops.VerifyAbsenceAndRelease(ctx, state.TenantID, state.DestinationLeaseID, state.DestinationResourceGeneration); err != nil {
			return state, &JobWaitError{
				Reason:      WaitReasonManagedRuntimeProvider,
				Message:     fmt.Sprintf("rollback destination absence verification pending: %v", err),
				ResumeAfter: 5 * time.Second,
				Cause:       err,
			}
		}
	}
	if err := w.Ops.DiscardReplacementBackupCustody(ctx, state); err != nil {
		return state, fmt.Errorf("rollback replacement backup custody cleanup failed: %w", err)
	}
	if err := w.Ops.RevokeStagedOriginCertificate(ctx, state); err != nil {
		return state, fmt.Errorf("rollback staged origin certificate revocation failed: %w", err)
	}
	next, tErr := w.Store.TransitionSupportReplacement(ctx, SupportReplacementTransition{
		TenantID:       state.TenantID,
		ReplacementID:  state.ReplacementID,
		Phase:          "rolled_back",
		RollbackReason: state.RollbackReason,
	})
	if tErr != nil {
		return state, tErr
	}
	return next, nil
}

func (w *SupportReplacementWorkflow) enterFencedConflict(
	ctx context.Context,
	state SupportReplacementState,
	reason string,
) (SupportReplacementState, error) {
	fenceErr := w.Ops.FenceBothCubes(ctx, state, reason)
	fullReason := reason
	if fenceErr != nil {
		fullReason = fmt.Sprintf("%s [fence_incomplete: %v]", reason, fenceErr)
	}
	patchPayload := map[string]any{
		"fence_verified": fenceErr == nil,
	}
	if fenceErr != nil {
		patchPayload["fence_error"] = fenceErr.Error()
	}
	patchBytes, _ := json.Marshal(patchPayload)
	updated, err := w.Store.TransitionSupportReplacement(ctx, SupportReplacementTransition{
		TenantID:             state.TenantID,
		ReplacementID:        state.ReplacementID,
		Phase:                "fenced_conflict",
		FencedConflictReason: fullReason,
		CheckpointPatchJSON:  patchBytes,
	})
	if err != nil {
		return state, NewPermanentError(fmt.Errorf("%w: %s (persist failed: %v)", ErrSupportReplacementFencedConflict, fullReason, errors.Join(fenceErr, err)))
	}
	return updated, NewPermanentError(fmt.Errorf("%w: %s", ErrSupportReplacementFencedConflict, fullReason))
}

// DefaultSupportReplacementStepOps composes Homelab's real Postgres, Guard/Agent,
// Wallet delivery, address registration, homelab identity authority, and native
// lease decommission subsystems without fake fallbacks.
type DefaultSupportReplacementStepOps struct {
	DB                 *sql.DB
	ControlPlaneDB     *sql.DB
	Sender             StackKitCommandSender
	Decommissioner     ManagedLeaseDecommissioner
	BackupDelivery     ManagedBackupTargetDelivery
	Inventory          ManagedStackKitInventoryBuilder
	AdvancedIssuer     AdvancedIssuer
	OriginCertificates *originca.Service
	StackKitLifecycle  StackKitLifecycleConfig
	ReleaseResolver    func(ctx context.Context, tenantID, stackID string) (*stackkitrelease.Release, error)
	EnrollmentTimeout  time.Duration
}

type supportReplacementExecContext struct {
	Release              stackkitrelease.Release
	ReleasePin           *agentpb.StackKitReleasePin
	StackKit             string
	StackName            string
	StackKitInstanceID   string
	SpecPath             string
	BoundSpecPath        string
	AddressPrefix        string
	RouteHosts           []string
	UsesManagedKombifyMe bool
	CandidateSpecJSON    []byte
	StoppableServices    []string
	LocalBinding         localExecutionBinding
	SourceAgentID        string
	SourcePublicIP       string
	DestPublicIP         string
}

func (o *DefaultSupportReplacementStepOps) withProviderTenantTx(
	ctx context.Context,
	tenantID string,
	readOnly bool,
	fn func(tx *sql.Tx) error,
) error {
	if o == nil || o.DB == nil {
		return fmt.Errorf("support replacement providerControl DB is not configured")
	}
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return fmt.Errorf("tenant_id is required for provider-control transaction")
	}
	tx, err := o.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: readOnly})
	if err != nil {
		return fmt.Errorf("begin provider-control transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		return fmt.Errorf("set provider-control app.tenant_id: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit provider-control transaction: %w", err)
	}
	committed = true
	return nil
}

func (o *DefaultSupportReplacementStepOps) withControlPlaneTenantTx(
	ctx context.Context,
	tenantID string,
	readOnly bool,
	fn func(tx *sql.Tx) error,
) error {
	if o == nil || o.ControlPlaneDB == nil {
		return fmt.Errorf("support replacement control-plane DB is not configured")
	}
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return fmt.Errorf("tenant_id is required for control-plane transaction")
	}
	tx, err := tenantcontext.Begin(ctx, o.ControlPlaneDB, &sql.TxOptions{ReadOnly: readOnly}, tenantID)
	if err != nil {
		return fmt.Errorf("begin control-plane tenant transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit control-plane tenant transaction: %w", err)
	}
	committed = true
	return nil
}

func (o *DefaultSupportReplacementStepOps) loadCheckpoint(ctx context.Context, state SupportReplacementState) (SupportReplacementCheckpoint, error) {
	var cp SupportReplacementCheckpoint
	if len(state.CheckpointJSON) > 0 && string(state.CheckpointJSON) != "{}" {
		_ = json.Unmarshal(state.CheckpointJSON, &cp)
	}
	if o == nil || o.DB == nil {
		return cp, nil
	}
	var raw []byte
	err := o.withProviderTenantTx(ctx, state.TenantID, true, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT COALESCE(checkpoint_json, '{}'::jsonb)
			FROM managed_runtime_support_replacements
			WHERE tenant_id = $1 AND replacement_id = $2
		`, state.TenantID, state.ReplacementID).Scan(&raw)
	})
	if err != nil {
		return cp, err
	}
	if len(raw) > 0 {
		var dbCP SupportReplacementCheckpoint
		if err := json.Unmarshal(raw, &dbCP); err == nil {
			cp = dbCP
		}
	}
	return cp, nil
}

func mergeSupportCheckpoint(dst *SupportReplacementCheckpoint, src SupportReplacementCheckpoint) {
	if dst == nil {
		return
	}
	if src.StackKit != "" {
		dst.StackKit = src.StackKit
	}
	if src.StackName != "" {
		dst.StackName = src.StackName
	}
	if src.StackKitInstanceID != "" {
		dst.StackKitInstanceID = src.StackKitInstanceID
	}
	if src.SpecPath != "" {
		dst.SpecPath = src.SpecPath
	}
	if src.BoundSpecPath != "" {
		dst.BoundSpecPath = src.BoundSpecPath
	}
	if src.LocalSiteRef != "" {
		dst.LocalSiteRef = src.LocalSiteRef
	}
	if src.LocalNodeRef != "" {
		dst.LocalNodeRef = src.LocalNodeRef
	}
	if src.LocalExecutionChannelRef != "" {
		dst.LocalExecutionChannelRef = src.LocalExecutionChannelRef
	}
	if len(src.RouteHosts) > 0 {
		dst.RouteHosts = append([]string(nil), src.RouteHosts...)
	}
	if src.DestOriginCertID != "" {
		dst.DestOriginCertID = src.DestOriginCertID
	}
	if len(src.DestOriginCertHosts) > 0 {
		dst.DestOriginCertHosts = append([]string(nil), src.DestOriginCertHosts...)
	}
	if src.DestOriginCertSHA256 != "" {
		dst.DestOriginCertSHA256 = src.DestOriginCertSHA256
	}
	if src.DestOriginCertNotAfter != "" {
		dst.DestOriginCertNotAfter = src.DestOriginCertNotAfter
	}
	if src.SourcePlanHash != "" {
		dst.SourcePlanHash = src.SourcePlanHash
	}
	if src.DestinationPlanHash != "" {
		dst.DestinationPlanHash = src.DestinationPlanHash
	}
	if src.DestinationApplied {
		dst.DestinationApplied = true
	}
	if src.SnapshotAnchorID != "" {
		dst.SnapshotAnchorID = src.SnapshotAnchorID
	}
	if src.RestoreResultID != "" {
		dst.RestoreResultID = src.RestoreResultID
	}
	if src.FinalSnapshotAnchorID != "" {
		dst.FinalSnapshotAnchorID = src.FinalSnapshotAnchorID
	}
	if src.FinalRestoreResultID != "" {
		dst.FinalRestoreResultID = src.FinalRestoreResultID
	}
	if src.ActivationOperationID != "" {
		dst.ActivationOperationID = src.ActivationOperationID
	}
	if src.ActivationNotStartedReasonCode != "" {
		dst.ActivationNotStartedReasonCode = src.ActivationNotStartedReasonCode
	}
	if src.WalletCustodyDelivered {
		dst.WalletCustodyDelivered = true
	}
	if len(src.StoppableServices) > 0 {
		dst.StoppableServices = append([]string(nil), src.StoppableServices...)
	}
	if len(src.FencedServices) > 0 {
		dst.FencedServices = append([]string(nil), src.FencedServices...)
	}
	if src.FenceVerified != nil {
		v := *src.FenceVerified
		dst.FenceVerified = &v
	}
	if src.FenceError != "" {
		dst.FenceError = src.FenceError
	}
}

func (o *DefaultSupportReplacementStepOps) persistCheckpoint(ctx context.Context, state SupportReplacementState, cp SupportReplacementCheckpoint) error {
	if o == nil || o.DB == nil {
		return nil
	}
	encoded, err := json.Marshal(cp)
	if err != nil {
		return fmt.Errorf("encode support replacement checkpoint: %w", err)
	}
	return o.withProviderTenantTx(ctx, state.TenantID, false, func(tx *sql.Tx) error {
		_, execErr := tx.ExecContext(ctx, `
			UPDATE managed_runtime_support_replacements
			SET checkpoint_json = COALESCE(checkpoint_json, '{}'::jsonb) || $3::jsonb
			WHERE tenant_id = $1 AND replacement_id = $2
		`, state.TenantID, state.ReplacementID, string(encoded))
		return execErr
	})
}

func (o *DefaultSupportReplacementStepOps) resolveRelease(ctx context.Context, tenantID, stackID string) (*stackkitrelease.Release, error) {
	if o != nil && o.ReleaseResolver != nil {
		return o.ReleaseResolver(ctx, tenantID, stackID)
	}
	return TargetStackKitReleaseForStack(ctx, tenantID, stackID)
}

func (o *DefaultSupportReplacementStepOps) loadExecContext(ctx context.Context, state SupportReplacementState, needSourceAgent bool) (supportReplacementExecContext, SupportReplacementCheckpoint, error) {
	cp, err := o.loadCheckpoint(ctx, state)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return supportReplacementExecContext{}, cp, fmt.Errorf("load support replacement checkpoint: %w", err)
	}
	release, err := o.resolveRelease(ctx, state.TenantID, state.StackID)
	if err != nil {
		return supportReplacementExecContext{}, cp, fmt.Errorf("resolve pinned StackKits release: %w", err)
	}
	if release == nil {
		return supportReplacementExecContext{}, cp, fmt.Errorf("pinned StackKits release is not configured")
	}
	pin := grpcserver.StackKitReleasePinFor(*release)
	if err := stackkitcommand.ValidateReleasePin(pin); err != nil {
		return supportReplacementExecContext{}, cp, fmt.Errorf("invalid StackKits release pin: %w", err)
	}

	execCtx := supportReplacementExecContext{
		Release:            *release,
		ReleasePin:         pin,
		StackKit:           firstNonEmpty(cp.StackKit, "cloud-kit"),
		StackName:          firstNonEmpty(cp.StackName, state.StackID),
		StackKitInstanceID: cp.StackKitInstanceID,
		SpecPath:           firstNonEmpty(cp.SpecPath, defaultStackKitSpecPath),
		BoundSpecPath:      cp.BoundSpecPath,
		RouteHosts:         append([]string(nil), cp.RouteHosts...),
		StoppableServices:  append([]string(nil), cp.StoppableServices...),
	}

	if o.DB != nil {
		if err := o.withProviderTenantTx(ctx, state.TenantID, true, func(tx *sql.Tx) error {
			var srcWorker sql.NullString
			var srcMetaRaw, srcLeaseRaw []byte
			if err := tx.QueryRowContext(ctx, `
				SELECT s.worker_id, COALESCE(s.metadata_json, '{}'::jsonb), COALESCE(l.lease_json, '{}'::jsonb)
				FROM techstack_vm_leases l
				LEFT JOIN servers s ON s.tenant_id = l.tenant_id AND s.id = $3
				WHERE l.tenant_id = $1 AND l.id = $2
			`, state.TenantID, state.SourceLeaseID, state.SourceServerID).Scan(&srcWorker, &srcMetaRaw, &srcLeaseRaw); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("load source server and lease context: %w", err)
			}
			if srcWorker.Valid {
				execCtx.SourceAgentID = strings.TrimSpace(srcWorker.String)
			}
			execCtx.SourcePublicIP = extractRuntimePublicIP(srcMetaRaw, srcLeaseRaw)
			if len(srcLeaseRaw) > 0 {
				var leaseDoc struct {
					Metadata map[string]string `json:"metadata"`
				}
				if json.Unmarshal(srcLeaseRaw, &leaseDoc) == nil {
					if v := NormalizeStackKitRef(leaseDoc.Metadata["stackkit"]); v != "" {
						execCtx.StackKit = v
					}
					if v := strings.TrimSpace(leaseDoc.Metadata["stack_name"]); v != "" {
						execCtx.StackName = v
					}
					if len(execCtx.StoppableServices) == 0 {
						execCtx.StoppableServices = filterStoppableServiceKeys(strings.Split(leaseDoc.Metadata["requested_services"], ","))
					}
				}
			}
			if strings.TrimSpace(state.DestinationServerID) != "" {
				var dstMetaRaw, dstLeaseRaw []byte
				if err := tx.QueryRowContext(ctx, `
					SELECT COALESCE(s.metadata_json, '{}'::jsonb), COALESCE(l.lease_json, '{}'::jsonb)
					FROM servers s
					LEFT JOIN techstack_vm_leases l ON l.tenant_id = s.tenant_id AND l.id = s.lease_id
					WHERE s.tenant_id = $1 AND s.id = $2
				`, state.TenantID, state.DestinationServerID).Scan(&dstMetaRaw, &dstLeaseRaw); err != nil && !errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("load destination server metadata: %w", err)
				}
				execCtx.DestPublicIP = extractRuntimePublicIP(dstMetaRaw, dstLeaseRaw)
			}
			return nil
		}); err != nil {
			return supportReplacementExecContext{}, cp, err
		}
	}

	if o.ControlPlaneDB != nil {
		if err := o.withControlPlaneTenantTx(ctx, state.TenantID, true, func(tx *sql.Tx) error {
			var stackName, instanceID string
			var configRaw, servicesRaw []byte
			if err := tx.QueryRowContext(ctx, `
				SELECT COALESCE(name, ''), COALESCE(stackkit_instance_id, ''),
				       COALESCE(config_json, '{}'::jsonb), COALESCE(services_json, '[]'::jsonb)
				FROM stacks
				WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL
			`, state.TenantID, state.StackID).Scan(&stackName, &instanceID, &configRaw, &servicesRaw); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return nil
				}
				return fmt.Errorf("load control-plane stack metadata: %w", err)
			}
			if strings.TrimSpace(stackName) != "" {
				execCtx.StackName = strings.TrimSpace(stackName)
			}
			if strings.TrimSpace(instanceID) != "" {
				execCtx.StackKitInstanceID = strings.TrimSpace(instanceID)
			}
			var configDoc map[string]interface{}
			var explicitDomain string
			var hasKombifyMeBinding bool
			if json.Unmarshal(configRaw, &configDoc) == nil && configDoc != nil {
				if rawSpec, ok := configDoc["stack_spec_v2"]; ok && rawSpec != nil {
					if specMap, ok := rawSpec.(map[string]interface{}); ok && specMap != nil {
						if d, dErr := stackSpecDomainBase(specMap); dErr == nil {
							explicitDomain = strings.ToLower(strings.TrimSpace(d))
						}
					}
					if specBytes, mErr := json.Marshal(rawSpec); mErr == nil {
						execCtx.CandidateSpecJSON = specBytes
					}
				}
				if binding, ok := DecodeStackKitRolloutBinding(configDoc[StackKitRolloutBindingResultField]); ok {
					if binding.StackKit != "" {
						execCtx.StackKit = NormalizeStackKitRef(binding.StackKit)
					}
					if binding.SpecPath != "" {
						execCtx.SpecPath = binding.SpecPath
					}
					if binding.BoundSpecPath != "" {
						execCtx.BoundSpecPath = binding.BoundSpecPath
					}
				}
				if prefix := strings.TrimSpace(stringFromInterface(configDoc[metadataKeyKombifyMeAddressPrefix])); prefix != "" {
					execCtx.AddressPrefix = prefix
					hasKombifyMeBinding = true
				}
				addrMode := strings.TrimSpace(firstNonEmpty(
					stringFromInterface(configDoc[metadataKeyAddressMode]),
					stringFromInterface(configDoc[metadataKeyRequestedAddressMode]),
				))
				kmStatus := strings.TrimSpace(stringFromInterface(configDoc[metadataKeyKombifyMeAddressStatus]))
				addrZone := strings.TrimSpace(stringFromInterface(configDoc[metadataKeyKombifyMeAddressZone]))
				if strings.EqualFold(addrMode, addressModeKombifyMe) || strings.EqualFold(kmStatus, "registered") || addrZone != "" {
					hasKombifyMeBinding = true
				}
				if len(execCtx.RouteHosts) == 0 {
					var rawHosts []string
					if rawList := stringListFromInterface(configDoc[metadataKeyKombifyMeRouteHosts]); len(rawList) > 0 {
						rawHosts = rawList
					} else if rawStr := strings.TrimSpace(stringFromInterface(configDoc[metadataKeyKombifyMeRouteHosts])); rawStr != "" {
						rawHosts = splitMetadataList(rawStr)
					}
					if len(rawHosts) > 0 {
						if normalized, normErr := originca.NormalizeHosts(rawHosts); normErr == nil {
							execCtx.RouteHosts = normalized
						}
					}
				}
			}
			if explicitDomain == addressModeKombifyMeDomain || strings.HasSuffix(explicitDomain, "."+addressModeKombifyMeDomain) ||
				hasKombifyMeBinding || len(execCtx.RouteHosts) > 0 || len(cp.DestOriginCertHosts) > 0 || explicitDomain == "" {
				execCtx.UsesManagedKombifyMe = true
			}
			if len(execCtx.StoppableServices) == 0 && len(servicesRaw) > 0 {
				var servicesList []string
				if json.Unmarshal(servicesRaw, &servicesList) == nil {
					execCtx.StoppableServices = filterStoppableServiceKeys(servicesList)
				}
			}
			return nil
		}); err != nil {
			return supportReplacementExecContext{}, cp, err
		}
	}

	if execCtx.StackKitInstanceID == "" {
		execCtx.StackKitInstanceID = execCtx.StackName
	}
	if cp.LocalSiteRef != "" && cp.LocalNodeRef != "" && cp.LocalExecutionChannelRef != "" {
		execCtx.LocalBinding = localExecutionBinding{
			SiteRef:             cp.LocalSiteRef,
			NodeRef:             cp.LocalNodeRef,
			ExecutionChannelRef: cp.LocalExecutionChannelRef,
		}
	} else {
		binding, bindErr := localExecutionBindingFor(execCtx.StackKit)
		if bindErr != nil {
			return supportReplacementExecContext{}, cp, bindErr
		}
		execCtx.LocalBinding = binding
	}
	if needSourceAgent && execCtx.SourceAgentID == "" {
		return supportReplacementExecContext{}, cp, fmt.Errorf("source server %q has no enrolled worker_id", state.SourceServerID)
	}
	if len(cp.RouteHosts) > 0 || len(cp.DestOriginCertHosts) > 0 || execCtx.AddressPrefix != "" {
		execCtx.UsesManagedKombifyMe = true
	}
	if o.ControlPlaneDB == nil && !execCtx.UsesManagedKombifyMe {
		// Without ControlPlaneDB metadata, managed cloud runtimes default to kombify.me.
		execCtx.UsesManagedKombifyMe = true
	}
	if execCtx.UsesManagedKombifyMe && len(execCtx.RouteHosts) == 0 {
		if cfg := kombifyme.DefaultConfigFromEnv(); strings.TrimSpace(cfg.APIKey) != "" {
			hosts, kmErr := resolveKombifyMeRouteHosts(ctx, state, cfg)
			if kmErr != nil {
				return supportReplacementExecContext{}, cp, fmt.Errorf("resolve kombify.me route hosts: %w", kmErr)
			}
			execCtx.RouteHosts = hosts
		}
	}
	return execCtx, cp, nil
}

func resolveKombifyMeRouteHosts(ctx context.Context, state SupportReplacementState, cfg kombifyme.Config) ([]string, error) {
	address, err := kombifyme.NewManagedAddress(state.TenantID, state.OwnerSubjectID, state.StackID)
	if err != nil {
		return nil, err
	}
	client := kombifyme.NewClient(cfg)
	readback, err := client.Forward(ctx, http.MethodGet, "subdomains?binding_stack_ref="+url.QueryEscape(address.StackRef), nil, "")
	if err != nil {
		return nil, fmt.Errorf("read existing kombify.me routes: %w", withUpstreamErrorCode(err))
	}
	routes, ok := readback.Body.([]any)
	if !ok || len(routes) == 0 {
		return nil, nil
	}
	var rawHosts []string
	for _, raw := range routes {
		route, _ := raw.(map[string]any)
		if route == nil || route["subdomain_kind"] != "service" {
			continue
		}
		if fqdn := strings.TrimSpace(stringFromInterface(route["fqdn"])); fqdn != "" {
			rawHosts = append(rawHosts, fqdn)
		}
	}
	if len(rawHosts) == 0 {
		return nil, nil
	}
	return originca.NormalizeHosts(rawHosts)
}

func extractRuntimePublicIP(metaRaw, leaseRaw []byte) string {
	var meta map[string]any
	if len(metaRaw) > 0 && json.Unmarshal(metaRaw, &meta) == nil && meta != nil {
		for _, key := range []string{"runtime_public_ip", "public_ip", "node_public_ip"} {
			if ip := strings.TrimSpace(stringFromInterface(meta[key])); ip != "" {
				return ip
			}
		}
		if host, ok := meta["host"].(map[string]any); ok {
			if ip := strings.TrimSpace(stringFromInterface(host["public_ip"])); ip != "" {
				return ip
			}
		}
	}
	var leaseDoc struct {
		Metadata map[string]string `json:"metadata"`
		Resource struct {
			PublicIP string `json:"public_ip"`
		} `json:"resource"`
	}
	if len(leaseRaw) > 0 && json.Unmarshal(leaseRaw, &leaseDoc) == nil {
		for _, cand := range []string{leaseDoc.Resource.PublicIP, leaseDoc.Metadata["runtime_public_ip"], leaseDoc.Metadata["public_ip"]} {
			if strings.TrimSpace(cand) != "" {
				return strings.TrimSpace(cand)
			}
		}
	}
	return ""
}

func filterStoppableServiceKeys(keys []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range keys {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" || seen[k] {
			continue
		}
		// Per cmd/stackkit/commands/service.go:26-32, base, id (PocketID), and auth (TinyAuth) cannot be stopped.
		if k == "base" || k == "id" || k == "auth" {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

func supportReplacementActivationID(replacementID string) string {
	cleaned := strings.ToLower(strings.TrimSpace(replacementID))
	cleaned = supportReplacementActivationSanitizer.ReplaceAllString(cleaned, "-")
	cleaned = strings.Trim(cleaned, "._-")
	if cleaned == "" {
		cleaned = "default"
	}
	candidate := "sr-" + cleaned + "-activate"
	if len(candidate) > 120 {
		candidate = candidate[:120]
	}
	return candidate
}

func (o *DefaultSupportReplacementStepOps) EnsureDestinationGuardEnrolled(ctx context.Context, state SupportReplacementState) (string, error) {
	if o == nil || o.DB == nil {
		return "", fmt.Errorf("support replacement DB is not configured")
	}
	var workerID sql.NullString
	var connState, lifecycle string
	var createdAt time.Time
	var opStatus, opPhase, opErrCode, opErrMsg string
	err := o.withProviderTenantTx(ctx, state.TenantID, true, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `
			SELECT worker_id, connection_state, lifecycle_state, created_at
			FROM servers
			WHERE tenant_id = $1 AND id = $2
		`, state.TenantID, state.DestinationServerID).Scan(&workerID, &connState, &lifecycle, &createdAt); err != nil {
			return fmt.Errorf("inspect destination server Guard enrollment: %w", err)
		}
		if strings.TrimSpace(state.DestinationOperationID) != "" {
			if opErr := tx.QueryRowContext(ctx, `
				SELECT
					op.status,
					op.phase,
					COALESCE(head.receipt_json #>> '{reason,code}', ''),
					COALESCE(head.receipt_json #>> '{reason,message}', '')
				FROM provider_operations AS op
				LEFT JOIN provider_operation_receipts AS head
				  ON head.tenant_id = op.tenant_id
				 AND head.operation_id = op.operation_id
				 AND head.sequence = op.head_sequence
				WHERE op.tenant_id = $1 AND op.operation_id = $2
			`, state.TenantID, state.DestinationOperationID).Scan(&opStatus, &opPhase, &opErrCode, &opErrMsg); opErr != nil && !errors.Is(opErr, sql.ErrNoRows) {
				return fmt.Errorf("inspect destination provision operation: %w", opErr)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if opStatus == "failed" || opStatus == "denied" || opPhase == "failed" || opPhase == "denied" {
		return "", fmt.Errorf("destination provision operation %q terminated (%s/%s %s: %s)", state.DestinationOperationID, opStatus, opPhase, opErrCode, opErrMsg)
	}
	switch lifecycle {
	case "failed", "decommissioning", "decommissioned":
		return "", fmt.Errorf("destination server %q entered terminal lifecycle_state %q", state.DestinationServerID, lifecycle)
	}
	if !workerID.Valid || strings.TrimSpace(workerID.String) == "" || connState != "connected" {
		timeout := o.EnrollmentTimeout
		if timeout <= 0 {
			timeout = 15 * time.Minute
		}
		if !createdAt.IsZero() && time.Since(createdAt) > timeout {
			return "", fmt.Errorf("destination server %q timed out awaiting Guard enrollment (state=%q, op_status=%q)", state.DestinationServerID, connState, opStatus)
		}
		return "", &JobWaitError{
			Reason:      WaitReasonManagedRuntimeEnrollment,
			Message:     fmt.Sprintf("destination server %q is awaiting Guard enrollment (state=%q, op_status=%q)", state.DestinationServerID, connState, opStatus),
			ResumeAfter: 15 * time.Second,
			Cause:       fmt.Errorf("destination server %q is not yet enrolled/connected on Guard (state=%q)", state.DestinationServerID, connState),
		}
	}
	return strings.TrimSpace(workerID.String), nil
}

func (o *DefaultSupportReplacementStepOps) ProveAndDeliverWalletCustody(ctx context.Context, state SupportReplacementState, destAgentID string) error {
	if o == nil || o.Sender == nil {
		return fmt.Errorf("StackKitCommandSender is not configured")
	}
	if o.BackupDelivery == nil {
		return fmt.Errorf("managed backup Wallet delivery authority is not configured")
	}
	if _, ok := o.BackupDelivery.(ManagedBackupReplacementDelivery); !ok {
		return fmt.Errorf("managed backup Wallet delivery authority does not support replacement recipient handoff")
	}
	if strings.TrimSpace(state.DestinationLeaseID) == "" ||
		strings.TrimSpace(state.DestinationServerID) == "" ||
		strings.TrimSpace(state.DestinationResourceGeneration) == "" {
		return fmt.Errorf("support replacement %q requires complete destination identity before backup custody handoff", state.ReplacementID)
	}
	execCtx, cp, err := o.loadExecContext(ctx, state, true)
	if err != nil {
		return err
	}
	if len(execCtx.RouteHosts) == 0 {
		if cfg := kombifyme.DefaultConfigFromEnv(); strings.TrimSpace(cfg.APIKey) != "" {
			hosts, kmErr := resolveKombifyMeRouteHosts(ctx, state, cfg)
			if kmErr != nil {
				return fmt.Errorf("resolve kombify.me route hosts: %w", kmErr)
			}
			execCtx.RouteHosts = hosts
		}
	}
	if o.AdvancedIssuer == nil {
		return fmt.Errorf("advanced capability issuer is not configured: %w", advancedissuer.ErrUnavailable)
	}
	trustBundle := o.AdvancedIssuer.TrustBundle()
	if len(trustBundle) == 0 {
		return fmt.Errorf("advanced capability issuer returned an empty trust bundle: %w", advancedissuer.ErrUnavailable)
	}
	if len(execCtx.CandidateSpecJSON) == 0 {
		return fmt.Errorf("support replacement requires committed stack_spec_v2 candidate for stack %q", state.StackID)
	}

	initSpecPath := execCtx.SpecPath
	boundSpecPath := execCtx.BoundSpecPath
	if execCtx.AddressPrefix != "" && boundSpecPath != "" {
		initSpecPath = defaultStackKitSpecPath
	}
	baseRequest := StackKitLifecycleRequest{
		StackID:               state.StackID,
		StackKitInstanceID:    execCtx.StackKitInstanceID,
		TenantID:              state.TenantID,
		OwnerID:               state.OwnerSubjectID,
		AgentID:               destAgentID,
		NodeRef:               execCtx.LocalBinding.NodeRef,
		OwnerApproved:         true,
		WorkingDirectory:      defaultStackKitNodeWorkspace,
		SpecPath:              initSpecPath,
		BoundSpecPath:         boundSpecPath,
		AddressPrefix:         execCtx.AddressPrefix,
		StackKit:              execCtx.StackKit,
		StackName:             execCtx.StackName,
		CandidateSpecJSON:     execCtx.CandidateSpecJSON,
		AdvancedTrustBundle:   trustBundle,
		managedBackupDelivery: o.BackupDelivery,
		replacementBackupCustody: &ManagedBackupReplacementScope{
			TenantID:                      state.TenantID,
			StackID:                       state.StackID,
			OwnerID:                       state.OwnerSubjectID,
			ReplacementID:                 state.ReplacementID,
			DestinationLeaseID:            state.DestinationLeaseID,
			DestinationServerID:           state.DestinationServerID,
			DestinationResourceGeneration: state.DestinationResourceGeneration,
		},
	}

	destPlanHash := cp.DestinationPlanHash
	if !cp.WalletCustodyDelivered || destPlanHash == "" {
		// 1. Inspect source node's active plan to obtain sourcePlanHash and resolved backup requirements.
		srcPlanCmd := &agentpb.StackKitCommand{
			CommandId:                state.ReplacementID + "-source-plan",
			Operation:                agentpb.StackKitOperation_STACKKIT_OPERATION_PLAN,
			WorkingDirectory:         defaultStackKitNodeWorkspace,
			SpecPath:                 execCtx.SpecPath,
			OutputDirectory:          "deploy",
			TimeoutSeconds:           int32(stackKitReadCommandTimeout / time.Second),
			Release:                  execCtx.ReleasePin,
			LocalSiteRef:             execCtx.LocalBinding.SiteRef,
			LocalNodeRef:             execCtx.LocalBinding.NodeRef,
			LocalExecutionChannelRef: execCtx.LocalBinding.ExecutionChannelRef,
			Stackkit:                 execCtx.StackKit,
			StackName:                execCtx.StackName,
			StackkitInstanceId:       execCtx.StackKitInstanceID,
		}
		srcPlanRes, err := o.validateAndSend(ctx, state.TenantID, execCtx.SourceAgentID, srcPlanCmd)
		if err != nil {
			return fmt.Errorf("inspect source plan: %w", err)
		}
		sourcePlanHash, err := typedStackKitPlanHash(srcPlanRes)
		if err != nil {
			return fmt.Errorf("parse source plan hash: %w", err)
		}
		cp.SourcePlanHash = sourcePlanHash
		cp.StackKit = execCtx.StackKit
		cp.StackName = execCtx.StackName
		cp.StackKitInstanceID = execCtx.StackKitInstanceID
		cp.SpecPath = execCtx.SpecPath
		cp.BoundSpecPath = execCtx.BoundSpecPath
		cp.LocalSiteRef = execCtx.LocalBinding.SiteRef
		cp.LocalNodeRef = execCtx.LocalBinding.NodeRef
		cp.LocalExecutionChannelRef = execCtx.LocalBinding.ExecutionChannelRef
		cp.StoppableServices = execCtx.StoppableServices
		if len(execCtx.RouteHosts) > 0 {
			cp.RouteHosts = append([]string(nil), execCtx.RouteHosts...)
		}

		var inventoryJSON []byte
		if o.Inventory != nil {
			if resolvedPlan, _, planErr := stackkitcommand.ManagedBackupPlan(srcPlanRes, execCtx.StackKitInstanceID); planErr == nil && len(resolvedPlan) > 0 {
				inventoryJSON, err = o.Inventory.Build(ctx, ManagedStackKitInventoryRequest{
					TenantID:     state.TenantID,
					StackID:      state.StackID,
					ResolvedPlan: resolvedPlan,
				})
				if err != nil {
					return fmt.Errorf("build destination StackKits Inventory: %w", err)
				}
			}
		}
		baseRequest.InventoryJSON = inventoryJSON

		// 2. Initialize destination workspace, import Advanced trust, bind addresses,
		// stage replacement Wallet backup custody, generate, and plan.
		admitTrust := func(ctx context.Context, command *agentpb.StackKitCommand, result *agentpb.StackKitResult) error {
			_, admitErr := admitAdvancedTrustImport(ctx, o.AdvancedIssuer, baseRequest, execCtx.StackName, command, result)
			return admitErr
		}
		destPlanHash, err = runTypedStackKitPreApplySequence(ctx, StackKitLifecycleConfig{}, o.Sender, state.ReplacementID, baseRequest, execCtx.Release, admitTrust)
		if err != nil {
			return fmt.Errorf("destination pre-apply and Wallet custody delivery failed: %w", err)
		}
		if boundSpecPath != "" {
			cp.SpecPath = boundSpecPath
		}
		cp.DestinationPlanHash = destPlanHash
		cp.WalletCustodyDelivered = true
	} else if len(execCtx.RouteHosts) > 0 {
		cp.RouteHosts = append([]string(nil), execCtx.RouteHosts...)
	}

	if !cp.DestinationApplied {
		// 3. Stage destination Origin CA certificate in isolation (without calling
		// Track/CompleteInstall so the source Cube's live cert stays active until cutover).
		if len(execCtx.RouteHosts) > 0 && releaseRendersOriginCertificateRouter(execCtx.Release) {
			if o.OriginCertificates == nil {
				return &OriginCertificateError{Stage: "configuration", Err: originca.ErrUnavailable}
			}
			destHosts, err := managedOriginCertificateHosts(execCtx.RouteHosts, execCtx.DestPublicIP)
			if err != nil {
				return &OriginCertificateError{Stage: "hostnames", Err: err}
			}
			if cp.DestOriginCertID == "" || cp.DestOriginCertSHA256 == "" {
				if cp.DestOriginCertID != "" {
					if revokeErr := o.OriginCertificates.RevokeCertificate(ctx, cp.DestOriginCertID); revokeErr != nil {
						return &OriginCertificateError{Stage: "custody", Err: revokeErr}
					}
					cp.DestOriginCertID = ""
					cp.DestOriginCertHosts = nil
					cp.DestOriginCertNotAfter = ""
				}
				ocaReq := baseRequest
				ocaReq.Operation = StackKitLifecycleGenerate
				ocaReq.OwnerApproved = true
				ocaReq.InventoryJSON = nil
				ocaReq.CandidateSpecJSON = nil
				ocaReq.ExpectedSpecHash = ""
				ocaReq.SpecPath = cp.SpecPath
				ocaReq, err = NormalizeStackKitLifecycleRequest(ocaReq)
				if err != nil {
					return &OriginCertificateError{Stage: "request", Err: err}
				}
				dispatcher, err := newAdvancedDispatcher(ctx, o.Sender, o.AdvancedIssuer, execCtx.Release, ocaReq)
				if err != nil {
					return &OriginCertificateError{Stage: "capability", Err: err}
				}
				operations, err := dispatcher.require(stackKitStepOriginCertificateRequest, stackKitStepOriginCertificateInstall)
				if err != nil {
					return err
				}
				capability, err := dispatcher.issue(ctx, operations)
				if err != nil {
					return err
				}
				requested, _, err := dispatcher.dispatch(ctx, state.ReplacementID+"-dest-oca-request", stackKitStepOriginCertificateRequest, capability, func(command *agentpb.StackKitCommand) {
					command.OriginHosts = append([]string(nil), destHosts...)
				})
				if err != nil {
					return err
				}
				if requested.OriginRequest == nil {
					return &OriginCertificateError{Stage: "request", Err: fmt.Errorf("the destination node returned no CSR")}
				}
				cert, err := o.OriginCertificates.Issue(ctx, destHosts, requested.OriginRequest.CSRPEM)
				if err != nil {
					return &OriginCertificateError{Stage: "issue", Err: err}
				}
				// Persist DestOriginCertID BEFORE dispatching install so rollback can never leak an untracked cert.
				cp.DestOriginCertID = cert.ID
				cp.DestOriginCertHosts = append([]string(nil), cert.Hosts...)
				cp.DestOriginCertSHA256 = ""
				cp.DestOriginCertNotAfter = cert.ExpiresOn.UTC().Format(time.RFC3339)
				if err := o.persistCheckpoint(ctx, state, cp); err != nil {
					_ = o.OriginCertificates.RevokeCertificate(ctx, cert.ID)
					return fmt.Errorf("persist staged destination origin certificate id: %w", err)
				}
				installed, _, err := dispatcher.dispatch(ctx, state.ReplacementID+"-dest-oca-install", stackKitStepOriginCertificateInstall, capability, func(command *agentpb.StackKitCommand) {
					command.OriginCertificatePem = []byte(cert.PEM)
				})
				if err != nil {
					return err
				}
				evidence := installed.OriginInstall
				if evidence == nil || !originca.SameHosts(evidence.Hosts, destHosts) || !evidence.NotAfter.Equal(cert.ExpiresOn) {
					return &OriginCertificateError{Stage: "install", Err: fmt.Errorf("the destination node did not confirm the issued certificate")}
				}
				cp.DestOriginCertSHA256 = evidence.CertificateSHA256
			}
		}

		// 4. Execute isolated destination APPLY so the node persists a signed
		// Apply result required by BACKUP_RESTORE, VERIFY, and BACKUP_RESTORE_ACTIVATE.
		applyReq := baseRequest
		applyReq.Operation = StackKitLifecycleApply
		applyReq.ExpectedSpecHash = ""
		applyReq.InventoryJSON = nil
		applyReq.SpecPath = cp.SpecPath
		applyReq.OwnerApproved = true
		applyReq, err = NormalizeStackKitLifecycleRequest(applyReq)
		if err != nil {
			return fmt.Errorf("normalize isolated destination apply request: %w", err)
		}
		if err := renewCustomerRegistryPull(ctx, o.StackKitLifecycle, state.ReplacementID+"-dest-apply", applyReq, execCtx.Release, map[string]interface{}{}); err != nil {
			return fmt.Errorf("renew customer registry pull for destination apply: %w", err)
		}
		applyRes, _, applyErr := dispatchTypedStackKitOperation(ctx, o.Sender, state.ReplacementID+"-dest-apply", applyReq, execCtx.Release, func(command *agentpb.StackKitCommand) {
			command.ExpectedPlanHash = destPlanHash
		})
		if applyErr != nil {
			return fmt.Errorf("isolated destination apply failed: %w", applyErr)
		}
		if applyRes == nil || !applyRes.Success {
			return fmt.Errorf("isolated destination apply did not succeed cleanly")
		}
		normalizedApply, normErr := normalizeStackKitLifecycleResult(applyReq, applyRes)
		if normErr != nil {
			return fmt.Errorf("normalize isolated destination apply result: %w", normErr)
		}
		if status := strings.TrimSpace(stringFromInterface(normalizedApply["status"])); status == string(runtimeaction.StatusCompletedDegraded) || status == string(runtimeaction.StatusFailed) {
			return fmt.Errorf("isolated destination apply returned status %q", status)
		}
		if degraded := appendApplyDegradedComponents(nil, normalizedApply, execCtx.RouteHosts); len(degraded) > 0 {
			return fmt.Errorf("isolated destination apply completed degraded (%d incomplete units)", len(degraded))
		}
		cp.DestinationApplied = true
	}

	return o.persistCheckpoint(ctx, state, cp)
}

func (o *DefaultSupportReplacementStepOps) StageAndRestoreData(ctx context.Context, state SupportReplacementState, destAgentID string) error {
	execCtx, cp, err := o.loadExecContext(ctx, state, true)
	if err != nil {
		return err
	}
	if cp.SourcePlanHash == "" || cp.DestinationPlanHash == "" {
		return fmt.Errorf("staging requires source and destination plan hashes from custody step")
	}

	runCmd := &agentpb.StackKitCommand{
		CommandId:                state.ReplacementID + "-stage-backup",
		Operation:                agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_RUN,
		WorkingDirectory:         defaultStackKitNodeWorkspace,
		SpecPath:                 execCtx.SpecPath,
		TimeoutSeconds:           int32(stackKitBackupCommandTimeout / time.Second),
		Release:                  execCtx.ReleasePin,
		OwnerApproved:            true,
		LocalSiteRef:             execCtx.LocalBinding.SiteRef,
		LocalNodeRef:             execCtx.LocalBinding.NodeRef,
		LocalExecutionChannelRef: execCtx.LocalBinding.ExecutionChannelRef,
		Stackkit:                 execCtx.StackKit,
		StackName:                execCtx.StackName,
		StackkitInstanceId:       execCtx.StackKitInstanceID,
		ExpectedPlanHash:         cp.SourcePlanHash,
	}
	runRes, err := o.validateAndSend(ctx, state.TenantID, execCtx.SourceAgentID, runCmd)
	if err != nil {
		return fmt.Errorf("source backup run failed: %w", err)
	}
	runEvidence, err := stackkitcommand.ParseBackupRunEvidence(runCmd, runRes)
	if err != nil {
		return fmt.Errorf("admit source backup run evidence: %w", err)
	}
	cp.SnapshotAnchorID = runEvidence.SnapshotAnchorID
	if err := o.persistCheckpoint(ctx, state, cp); err != nil {
		return err
	}

	restoreCmd := &agentpb.StackKitCommand{
		CommandId:                state.ReplacementID + "-stage-restore",
		Operation:                agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_RESTORE,
		WorkingDirectory:         defaultStackKitNodeWorkspace,
		SpecPath:                 execCtx.SpecPath,
		TimeoutSeconds:           int32(stackKitBackupCommandTimeout / time.Second),
		Release:                  execCtx.ReleasePin,
		OwnerApproved:            true,
		LocalSiteRef:             execCtx.LocalBinding.SiteRef,
		LocalNodeRef:             execCtx.LocalBinding.NodeRef,
		LocalExecutionChannelRef: execCtx.LocalBinding.ExecutionChannelRef,
		Stackkit:                 execCtx.StackKit,
		StackName:                execCtx.StackName,
		StackkitInstanceId:       execCtx.StackKitInstanceID,
		ExpectedPlanHash:         cp.DestinationPlanHash,
		SnapshotAnchorId:         runEvidence.SnapshotAnchorID,
	}
	restoreRes, err := o.validateAndSend(ctx, state.TenantID, destAgentID, restoreCmd)
	if err != nil {
		if isAckAmbiguousError(err) {
			return fmt.Errorf("%w: %v", ErrSupportReplacementAckLost, err)
		}
		return fmt.Errorf("destination staged backup restore failed: %w", err)
	}
	restoreEvidence, err := stackkitcommand.ParseBackupRestoreEvidence(restoreCmd, restoreRes)
	if err != nil {
		return fmt.Errorf("admit destination staged restore evidence: %w", err)
	}
	cp.RestoreResultID = restoreEvidence.RestoreResultID
	return o.persistCheckpoint(ctx, state, cp)
}

func (o *DefaultSupportReplacementStepOps) VerifyIsolatedDestination(ctx context.Context, state SupportReplacementState, destAgentID string) error {
	execCtx, cp, err := o.loadExecContext(ctx, state, false)
	if err != nil {
		return err
	}
	if cp.DestinationPlanHash == "" {
		return fmt.Errorf("isolated destination verify requires destination plan hash")
	}
	verifyCmd := &agentpb.StackKitCommand{
		CommandId:                state.ReplacementID + "-verify-isolated",
		Operation:                agentpb.StackKitOperation_STACKKIT_OPERATION_VERIFY,
		WorkingDirectory:         defaultStackKitNodeWorkspace,
		SpecPath:                 execCtx.SpecPath,
		TimeoutSeconds:           int32(stackKitReadCommandTimeout / time.Second),
		Release:                  execCtx.ReleasePin,
		Offline:                  true,
		LocalSiteRef:             execCtx.LocalBinding.SiteRef,
		LocalNodeRef:             execCtx.LocalBinding.NodeRef,
		LocalExecutionChannelRef: execCtx.LocalBinding.ExecutionChannelRef,
		Stackkit:                 execCtx.StackKit,
		StackName:                execCtx.StackName,
		StackkitInstanceId:       execCtx.StackKitInstanceID,
		ExpectedPlanHash:         cp.DestinationPlanHash,
	}
	res, err := o.validateAndSend(ctx, state.TenantID, destAgentID, verifyCmd)
	if err != nil {
		return err
	}
	if _, err := stackkitcommand.ParseVerifyPlanEvidence(verifyCmd, res); err != nil {
		return fmt.Errorf("admit isolated verify plan evidence: %w", err)
	}
	return nil
}

func (o *DefaultSupportReplacementStepOps) FenceSourceWriters(ctx context.Context, state SupportReplacementState) error {
	execCtx, cp, err := o.loadExecContext(ctx, state, true)
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(cp.FencedServices)+len(execCtx.StoppableServices))
	fenced := make([]string, 0, len(cp.FencedServices)+len(execCtx.StoppableServices))
	for _, svc := range filterStoppableServiceKeys(cp.FencedServices) {
		if !seen[svc] {
			seen[svc] = true
			fenced = append(fenced, svc)
		}
	}
	for _, svc := range execCtx.StoppableServices {
		if !seen[svc] {
			seen[svc] = true
			fenced = append(fenced, svc)
			cp.FencedServices = append([]string(nil), fenced...)
			if persistErr := o.persistCheckpoint(ctx, state, cp); persistErr != nil {
				return fmt.Errorf("persist fenced source service %q before stop: %w", svc, persistErr)
			}
		}
		cmd := &agentpb.StackKitCommand{
			CommandId:                state.ReplacementID + "-fence-" + svc,
			Operation:                agentpb.StackKitOperation_STACKKIT_OPERATION_SERVICE_STOP,
			WorkingDirectory:         defaultStackKitNodeWorkspace,
			SpecPath:                 execCtx.SpecPath,
			TimeoutSeconds:           int32(stackKitWriteCommandTimeout / time.Second),
			Release:                  execCtx.ReleasePin,
			OwnerApproved:            true,
			ServiceKey:               svc,
			LocalSiteRef:             execCtx.LocalBinding.SiteRef,
			LocalNodeRef:             execCtx.LocalBinding.NodeRef,
			LocalExecutionChannelRef: execCtx.LocalBinding.ExecutionChannelRef,
			Stackkit:                 execCtx.StackKit,
			StackName:                execCtx.StackName,
			StackkitInstanceId:       execCtx.StackKitInstanceID,
		}
		if _, err := o.validateAndSend(ctx, state.TenantID, execCtx.SourceAgentID, cmd); err != nil {
			return fmt.Errorf("fence source service %q: %w", svc, err)
		}
	}
	return nil
}

func (o *DefaultSupportReplacementStepOps) FinalTransferAndConfirmDestinationWrites(ctx context.Context, state SupportReplacementState, destAgentID string) (bool, bool, error) {
	execCtx, cp, err := o.loadExecContext(ctx, state, true)
	if err != nil {
		return false, false, err
	}
	if cp.SourcePlanHash == "" || cp.DestinationPlanHash == "" || !cp.DestinationApplied {
		return false, false, fmt.Errorf("post-fence final transfer requires applied destination and source/destination plan hashes")
	}

	// Step A: Post-fence final snapshot on fenced source Cube so zero writes
	// between initial staging and FenceSourceWriters can be lost.
	if cp.FinalSnapshotAnchorID == "" {
		finalRunCmd := &agentpb.StackKitCommand{
			CommandId:                state.ReplacementID + "-final-backup",
			Operation:                agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_RUN,
			WorkingDirectory:         defaultStackKitNodeWorkspace,
			SpecPath:                 execCtx.SpecPath,
			TimeoutSeconds:           int32(stackKitBackupCommandTimeout / time.Second),
			Release:                  execCtx.ReleasePin,
			OwnerApproved:            true,
			LocalSiteRef:             execCtx.LocalBinding.SiteRef,
			LocalNodeRef:             execCtx.LocalBinding.NodeRef,
			LocalExecutionChannelRef: execCtx.LocalBinding.ExecutionChannelRef,
			Stackkit:                 execCtx.StackKit,
			StackName:                execCtx.StackName,
			StackkitInstanceId:       execCtx.StackKitInstanceID,
			ExpectedPlanHash:         cp.SourcePlanHash,
		}
		finalRunRes, err := o.validateAndSend(ctx, state.TenantID, execCtx.SourceAgentID, finalRunCmd)
		if err != nil {
			return false, false, fmt.Errorf("post-fence source backup run failed: %w", err)
		}
		runEvidence, err := stackkitcommand.ParseBackupRunEvidence(finalRunCmd, finalRunRes)
		if err != nil {
			return false, false, fmt.Errorf("admit post-fence source backup run evidence: %w", err)
		}
		cp.FinalSnapshotAnchorID = runEvidence.SnapshotAnchorID
		if err := o.persistCheckpoint(ctx, state, cp); err != nil {
			return false, false, err
		}
	}

	// Step B: Post-fence staged restore onto isolated destination Cube.
	if cp.FinalRestoreResultID == "" {
		finalRestoreCmd := &agentpb.StackKitCommand{
			CommandId:                state.ReplacementID + "-final-restore",
			Operation:                agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_RESTORE,
			WorkingDirectory:         defaultStackKitNodeWorkspace,
			SpecPath:                 execCtx.SpecPath,
			TimeoutSeconds:           int32(stackKitBackupCommandTimeout / time.Second),
			Release:                  execCtx.ReleasePin,
			OwnerApproved:            true,
			LocalSiteRef:             execCtx.LocalBinding.SiteRef,
			LocalNodeRef:             execCtx.LocalBinding.NodeRef,
			LocalExecutionChannelRef: execCtx.LocalBinding.ExecutionChannelRef,
			Stackkit:                 execCtx.StackKit,
			StackName:                execCtx.StackName,
			StackkitInstanceId:       execCtx.StackKitInstanceID,
			ExpectedPlanHash:         cp.DestinationPlanHash,
			SnapshotAnchorId:         cp.FinalSnapshotAnchorID,
		}
		finalRestoreRes, err := o.validateAndSend(ctx, state.TenantID, destAgentID, finalRestoreCmd)
		if err != nil {
			if isAckAmbiguousError(err) {
				return false, true, fmt.Errorf("%w: %v", ErrSupportReplacementAckLost, err)
			}
			return false, false, fmt.Errorf("post-fence destination backup restore failed: %w", err)
		}
		restoreEvidence, err := stackkitcommand.ParseBackupRestoreEvidence(finalRestoreCmd, finalRestoreRes)
		if err != nil {
			return false, false, fmt.Errorf("admit post-fence destination restore evidence: %w", err)
		}
		cp.FinalRestoreResultID = restoreEvidence.RestoreResultID
		if err := o.persistCheckpoint(ctx, state, cp); err != nil {
			return false, false, err
		}
	}

	// Step C: Activate the post-fence FinalRestoreResultID on destination Cube.
	if cp.ActivationOperationID != "" {
		return false, true, fmt.Errorf("%w: activation operation %q was previously initiated; refusing repeat copy", ErrSupportReplacementAckLost, cp.ActivationOperationID)
	}
	activationOpID := supportReplacementActivationID(state.ReplacementID)
	actCmd := &agentpb.StackKitCommand{
		CommandId:                state.ReplacementID + "-final-sync",
		Operation:                agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_RESTORE_ACTIVATE,
		WorkingDirectory:         defaultStackKitNodeWorkspace,
		SpecPath:                 execCtx.SpecPath,
		TimeoutSeconds:           int32(stackKitBackupCommandTimeout / time.Second),
		Release:                  execCtx.ReleasePin,
		OwnerApproved:            true,
		LocalSiteRef:             execCtx.LocalBinding.SiteRef,
		LocalNodeRef:             execCtx.LocalBinding.NodeRef,
		LocalExecutionChannelRef: execCtx.LocalBinding.ExecutionChannelRef,
		Stackkit:                 execCtx.StackKit,
		StackName:                execCtx.StackName,
		StackkitInstanceId:       execCtx.StackKitInstanceID,
		ExpectedPlanHash:         cp.DestinationPlanHash,
		RestoreResultId:          cp.FinalRestoreResultID,
		ActivationOperationId:    activationOpID,
	}
	if o == nil || o.Sender == nil {
		return false, false, &preDispatchValidationError{Operation: actCmd.Operation, Err: fmt.Errorf("StackKitCommandSender is not configured")}
	}
	if err := stackkitcommand.ValidateCommand(actCmd); err != nil {
		return false, false, &preDispatchValidationError{Operation: actCmd.Operation, Err: err}
	}
	cp.ActivationOperationID = activationOpID
	if err := o.persistCheckpoint(ctx, state, cp); err != nil {
		return false, false, err
	}
	res, err := sendStackKitCommandBoundedForTenant(ctx, o.Sender, state.TenantID, destAgentID, actCmd)
	if err != nil {
		if isAckAmbiguousError(err) {
			return false, true, fmt.Errorf("%w: %v", ErrSupportReplacementAckLost, err)
		}
		return false, false, err
	}
	if res == nil {
		return false, true, fmt.Errorf("%w: empty restore activation result", ErrSupportReplacementAckLost)
	}
	if !res.Success {
		reasonCode := strings.TrimSpace(extractRestoreActivationFailureReasonCode(res))
		if strings.HasPrefix(reasonCode, "restore_activation_not_started_at_") {
			cp.ActivationNotStartedReasonCode = reasonCode
			if pErr := o.persistCheckpoint(ctx, state, cp); pErr != nil {
				return false, true, fmt.Errorf("%w: restore activation did not start (%s) and checkpoint persist failed: %v", ErrSupportReplacementAckLost, reasonCode, pErr)
			}
			return false, false, fmt.Errorf("restore activation did not start (%s): %s", reasonCode, strings.TrimSpace(res.Stderr))
		}
		return true, true, fmt.Errorf("%w: restore activation failed post-journal (exit %d, reason=%q): %s",
			ErrSupportReplacementAckLost, res.ExitCode, reasonCode, strings.TrimSpace(res.Stderr))
	}
	if _, err := stackkitcommand.ParseRestoreActivationEvidence(actCmd, res); err != nil {
		return true, true, fmt.Errorf("%w: %v", ErrSupportReplacementAckLost, err)
	}
	return true, false, nil
}

func extractRestoreActivationFailureReasonCode(res *agentpb.StackKitResult) string {
	if res == nil || len(res.CommandResultJson) == 0 {
		return ""
	}
	var envelope struct {
		ReasonCode string `json:"reason_code"`
		Data       struct {
			ReasonCode      string `json:"reason_code"`
			CamelReasonCode string `json:"reasonCode"`
		} `json:"data"`
		Error struct {
			ReasonCode      string `json:"reason_code"`
			CamelReasonCode string `json:"reasonCode"`
		} `json:"error"`
	}
	if err := json.Unmarshal(res.CommandResultJson, &envelope); err != nil {
		return ""
	}
	return firstNonEmpty(
		envelope.Data.ReasonCode,
		envelope.Data.CamelReasonCode,
		envelope.Error.ReasonCode,
		envelope.Error.CamelReasonCode,
		envelope.ReasonCode,
	)
}

func (o *DefaultSupportReplacementStepOps) SwitchAddressesAndIdentityAuthority(ctx context.Context, state SupportReplacementState) error {
	if o == nil || o.ControlPlaneDB == nil {
		return fmt.Errorf("support replacement control-plane DB is not configured")
	}
	execCtx, _, err := o.loadExecContext(ctx, state, false)
	if err != nil {
		return err
	}

	if execCtx.UsesManagedKombifyMe {
		cfg := kombifyme.DefaultConfigFromEnv()
		if strings.TrimSpace(cfg.APIKey) == "" {
			return fmt.Errorf("kombify.me cutover requires KOMBIFYME API key for managed stack %q", state.StackID)
		}
		if err := o.switchManagedKombifyMeRoutes(ctx, state, execCtx, cfg); err != nil {
			return err
		}
	}

	return o.withControlPlaneTenantTx(ctx, state.TenantID, false, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE homelabs
			SET identity_authority_server_id = $3,
			    identity_authority_stack_id = COALESCE(NULLIF($4, ''), identity_authority_stack_id),
			    identity_authority_observed_at = now(),
			    updated_at = now()
			WHERE tenant_id = $1
			  AND identity_authority_server_id = $2
			  AND deleted_at IS NULL
		`, state.TenantID, state.SourceServerID, state.DestinationServerID, state.StackID); err != nil {
			return fmt.Errorf("update homelab identity authority server pointer: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE stack_routing_desired
			SET server_id = $4,
			    lease_id = $5,
			    revision = revision + 1,
			    updated_at = now()
			WHERE tenant_id = $1
			  AND stack_id = $2
			  AND server_id = $3
		`, state.TenantID, state.StackID, state.SourceServerID, state.DestinationServerID, state.DestinationLeaseID); err != nil {
			return fmt.Errorf("update stack_routing_desired target server/lease: %w", err)
		}
		return nil
	})
}

func (o *DefaultSupportReplacementStepOps) switchManagedKombifyMeRoutes(
	ctx context.Context,
	state SupportReplacementState,
	execCtx supportReplacementExecContext,
	cfg kombifyme.Config,
) error {
	address, err := kombifyme.NewManagedAddress(state.TenantID, state.OwnerSubjectID, state.StackID)
	if err != nil {
		return err
	}
	client := kombifyme.NewClient(cfg)
	readback, err := client.Forward(ctx, http.MethodGet, "subdomains?binding_stack_ref="+url.QueryEscape(address.StackRef), nil, "")
	if err != nil {
		return fmt.Errorf("read existing kombify.me routes: %w", withUpstreamErrorCode(err))
	}
	routes, ok := readback.Body.([]any)
	if !ok || len(routes) == 0 {
		return fmt.Errorf("kombify.me route cutover expected registered routes for stack %q, found none", state.StackID)
	}
	destIP := strings.TrimSpace(execCtx.DestPublicIP)
	parsedIP, ipErr := netip.ParseAddr(strings.Trim(destIP, "[]"))
	if ipErr != nil || !parsedIP.IsGlobalUnicast() || parsedIP.IsPrivate() || parsedIP.IsLoopback() || parsedIP.IsLinkLocalUnicast() || parsedIP.IsMulticast() {
		return fmt.Errorf("kombify.me cutover requires a public destination server IP (got %q)", destIP)
	}
	destTarget := &ManagedRuntimeTarget{PublicIP: parsedIP.String()}
	destOrigin := "https://" + net.JoinHostPort(parsedIP.String(), "443")

	var baseName, layout string
	for _, raw := range routes {
		route, _ := raw.(map[string]any)
		if route == nil || route["subdomain_kind"] != "base" {
			continue
		}
		baseName, _ = route["name"].(string)
		layout, _ = route["layout"].(string)
		if baseName != "" {
			break
		}
	}
	if baseName == "" {
		return fmt.Errorf("kombify.me route cutover found no base route for stack %q", state.StackID)
	}
	var services []stackKitAddressPlanService
	for _, raw := range routes {
		route, _ := raw.(map[string]any)
		if route == nil || route["subdomain_kind"] != "service" {
			continue
		}
		name, _ := route["name"].(string)
		fqdn, _ := route["fqdn"].(string)
		svcKey := strings.TrimPrefix(name, baseName+"-")
		if layout == managedAddressLayoutZone || strings.Contains(name, ".") {
			svcKey = strings.SplitN(name, ".", 2)[0]
		}
		if svcKey != "" && fqdn != "" {
			services = append(services, stackKitAddressPlanService{ServiceKey: svcKey, Host: fqdn})
		}
	}
	if len(services) == 0 {
		return fmt.Errorf("kombify.me route cutover found no service routes for stack %q", state.StackID)
	}
	plan := stackKitAddressPlan{
		APIVersion:      stackKitAddressPlanV1,
		Provider:        addressModeKombifyMe,
		StackName:       execCtx.StackName,
		Domain:          addressModeKombifyMeDomain,
		SubdomainPrefix: baseName,
		Services:        services,
	}
	if layout == managedAddressLayoutZone {
		plan.RegisteredZone = baseName + "." + addressModeKombifyMeDomain
	}
	if _, err := allocateManagedAddressPlan(ctx, plan, destTarget, address); err != nil {
		return fmt.Errorf("switch kombify.me addresses to destination %s: %w", destOrigin, err)
	}
	return verifyKombifyMeRoutesTarget(ctx, client, address, destOrigin)
}

func verifyKombifyMeRoutesTarget(ctx context.Context, client *kombifyme.Client, address kombifyme.ManagedAddress, destOrigin string) error {
	wantOrigin := normalizedHTTPOrigin(destOrigin)
	verifyResp, err := client.Forward(ctx, http.MethodGet, "subdomains?binding_stack_ref="+url.QueryEscape(address.StackRef), nil, "")
	if err != nil {
		return fmt.Errorf("verify kombify.me route cutover readback: %w", withUpstreamErrorCode(err))
	}
	routes, ok := verifyResp.Body.([]any)
	if !ok {
		return fmt.Errorf("kombify.me route readback returned invalid payload")
	}
	serviceCount := 0
	for _, raw := range routes {
		route, _ := raw.(map[string]any)
		if route == nil || route["subdomain_kind"] != "service" {
			continue
		}
		serviceCount++
		targetAddr, _ := route["target_addr"].(string)
		if normalizedHTTPOrigin(targetAddr) != wantOrigin {
			name, _ := route["name"].(string)
			return fmt.Errorf("kombify.me route %q still points to %q instead of %q", name, targetAddr, destOrigin)
		}
	}
	if serviceCount == 0 {
		return fmt.Errorf("kombify.me route verification found zero service routes for stack %q", address.StackRef)
	}
	return nil
}

func (o *DefaultSupportReplacementStepOps) VerifyLiveCutover(ctx context.Context, state SupportReplacementState, destAgentID string) error {
	execCtx, cp, err := o.loadExecContext(ctx, state, false)
	if err != nil {
		return err
	}
	if cp.DestinationPlanHash == "" {
		return fmt.Errorf("live cutover verify requires destination plan hash")
	}
	verifyCmd := &agentpb.StackKitCommand{
		CommandId:                state.ReplacementID + "-verify-live",
		Operation:                agentpb.StackKitOperation_STACKKIT_OPERATION_VERIFY,
		WorkingDirectory:         defaultStackKitNodeWorkspace,
		SpecPath:                 execCtx.SpecPath,
		TimeoutSeconds:           int32(stackKitReadCommandTimeout / time.Second),
		Release:                  execCtx.ReleasePin,
		Offline:                  false,
		LocalSiteRef:             execCtx.LocalBinding.SiteRef,
		LocalNodeRef:             execCtx.LocalBinding.NodeRef,
		LocalExecutionChannelRef: execCtx.LocalBinding.ExecutionChannelRef,
		Stackkit:                 execCtx.StackKit,
		StackName:                execCtx.StackName,
		StackkitInstanceId:       execCtx.StackKitInstanceID,
		ExpectedPlanHash:         cp.DestinationPlanHash,
	}
	res, err := o.validateAndSend(ctx, state.TenantID, destAgentID, verifyCmd)
	if err != nil {
		return err
	}
	if _, err := stackkitcommand.ParseVerifyPlanEvidence(verifyCmd, res); err != nil {
		return fmt.Errorf("admit live verify plan evidence: %w", err)
	}
	if o.BackupDelivery != nil {
		var binding ManagedBackupNodeBinding
		var bindErr error
		if repDelivery, ok := o.BackupDelivery.(ManagedBackupReplacementDelivery); ok {
			binding, bindErr = repDelivery.ReplacementNodeBinding(ctx, state.TenantID, state.StackID, state.ReplacementID)
			if bindErr != nil {
				binding, bindErr = o.BackupDelivery.NodeBinding(ctx, state.TenantID, state.StackID)
			}
		} else {
			binding, bindErr = o.BackupDelivery.NodeBinding(ctx, state.TenantID, state.StackID)
		}
		if bindErr != nil || strings.TrimSpace(binding.BackupTargetRef) == "" || strings.TrimSpace(binding.CustodyAttestationRef) == "" {
			return fmt.Errorf("live cutover backup custody binding check failed: %v", bindErr)
		}
		if _, err := o.BackupDelivery.AttestTarget(ctx, state.TenantID, state.StackID); err != nil {
			return fmt.Errorf("live cutover backup target attestation failed: %w", err)
		}
	}
	if execCtx.UsesManagedKombifyMe {
		cfg := kombifyme.DefaultConfigFromEnv()
		if strings.TrimSpace(cfg.APIKey) == "" {
			return fmt.Errorf("live cutover verify requires KOMBIFYME API key for managed stack %q", state.StackID)
		}
		destIP := strings.TrimSpace(execCtx.DestPublicIP)
		parsedIP, ipErr := netip.ParseAddr(strings.Trim(destIP, "[]"))
		if ipErr != nil || !parsedIP.IsGlobalUnicast() || parsedIP.IsPrivate() || parsedIP.IsLoopback() || parsedIP.IsLinkLocalUnicast() || parsedIP.IsMulticast() {
			return fmt.Errorf("live cutover verify requires a valid public destination IP (got %q)", execCtx.DestPublicIP)
		}
		addr, err := kombifyme.NewManagedAddress(state.TenantID, state.OwnerSubjectID, state.StackID)
		if err != nil {
			return err
		}
		if err := verifyKombifyMeRoutesTarget(ctx, kombifyme.NewClient(cfg), addr, "https://"+net.JoinHostPort(parsedIP.String(), "443")); err != nil {
			return err
		}
	}
	return nil
}

func (o *DefaultSupportReplacementStepOps) FinalizeCutoverCustody(ctx context.Context, state SupportReplacementState) error {
	cp, err := o.loadCheckpoint(ctx, state)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("load checkpoint for cutover finalization: %w", err)
	}
	if cp.DestOriginCertID != "" && o.OriginCertificates != nil {
		expiresOn, parseErr := time.Parse(time.RFC3339, cp.DestOriginCertNotAfter)
		if parseErr != nil {
			expiresOn = time.Now().UTC().Add(365 * 24 * time.Hour)
		}
		destCert := originca.Certificate{
			ID:        cp.DestOriginCertID,
			Hosts:     append([]string(nil), cp.DestOriginCertHosts...),
			ExpiresOn: expiresOn,
		}
		if err := o.OriginCertificates.Track(ctx, state.TenantID, state.StackID, destCert); err != nil {
			return fmt.Errorf("promote destination origin certificate custody: %w", err)
		}
		if err := o.OriginCertificates.CompleteInstall(ctx, state.TenantID, state.StackID, cp.DestOriginCertID, cp.DestOriginCertSHA256, time.Now().UTC()); err != nil {
			return fmt.Errorf("complete destination origin certificate cutover: %w", err)
		}
	}
	if o.BackupDelivery != nil {
		repDelivery, ok := o.BackupDelivery.(ManagedBackupReplacementDelivery)
		if !ok {
			return fmt.Errorf("backup delivery authority does not support replacement custody commit")
		}
		if err := repDelivery.CommitReplacementCustody(ctx, state.TenantID, state.StackID, state.ReplacementID); err != nil {
			return fmt.Errorf("commit replacement backup custody: %w", err)
		}
		binding, bindErr := o.BackupDelivery.NodeBinding(ctx, state.TenantID, state.StackID)
		if bindErr != nil || strings.TrimSpace(binding.BackupTargetRef) == "" || strings.TrimSpace(binding.CustodyAttestationRef) == "" {
			return fmt.Errorf("verify committed backup custody binding: %v", bindErr)
		}
	}
	return nil
}

func (o *DefaultSupportReplacementStepOps) VerifyNoDestinationWrites(ctx context.Context, state SupportReplacementState) (bool, error) {
	if o == nil || o.DB == nil {
		return false, fmt.Errorf("support replacement DB is not configured")
	}
	var observed bool
	var phase string
	var cutoverAt sql.NullTime
	var destAgentID, destConnState string
	err := o.withProviderTenantTx(ctx, state.TenantID, true, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `
			SELECT destination_writes_observed, phase, cutover_verified_at
			FROM managed_runtime_support_replacements
			WHERE tenant_id = $1 AND replacement_id = $2
		`, state.TenantID, state.ReplacementID).Scan(&observed, &phase, &cutoverAt); err != nil {
			return err
		}
		if strings.TrimSpace(state.DestinationServerID) != "" {
			if srvErr := tx.QueryRowContext(ctx, `
				SELECT COALESCE(worker_id, ''), COALESCE(connection_state, '')
				FROM servers
				WHERE tenant_id = $1 AND id = $2
			`, state.TenantID, state.DestinationServerID).Scan(&destAgentID, &destConnState); srvErr != nil && !errors.Is(srvErr, sql.ErrNoRows) {
				return fmt.Errorf("inspect destination server state: %w", srvErr)
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if observed || cutoverAt.Valid || phase == "cutover_verified" || phase == "source_decommissioning" || phase == "completed" {
		return false, nil
	}

	cp, cpErr := o.loadCheckpoint(ctx, state)
	if cpErr != nil && !errors.Is(cpErr, sql.ErrNoRows) {
		return false, fmt.Errorf("load replacement checkpoint: %w", cpErr)
	}

	activationCommandAttempted := false
	activationNotStartedProved := strings.HasPrefix(strings.TrimSpace(cp.ActivationNotStartedReasonCode), "restore_activation_not_started_at_")
	if o.ControlPlaneDB != nil {
		var observedWrite bool
		err = o.withControlPlaneTenantTx(ctx, state.TenantID, true, func(tx *sql.Tx) error {
			var handoffStatus string
			var stagedTargetRef sql.NullString
			hErr := tx.QueryRowContext(ctx, `
				SELECT status, node_backup_target_ref
				FROM stack_backup_replacement_handoffs
				WHERE tenant_id = $1 AND stack_id = $2 AND replacement_id = $3
			`, state.TenantID, state.StackID, state.ReplacementID).Scan(&handoffStatus, &stagedTargetRef)
			if hErr != nil && !errors.Is(hErr, sql.ErrNoRows) {
				return fmt.Errorf("inspect backup replacement handoff: %w", hErr)
			}
			if hErr == nil {
				if handoffStatus == "committed" {
					observedWrite = true
					return nil
				}
				if stagedTargetRef.Valid && strings.TrimSpace(stagedTargetRef.String) != "" {
					var activeTargetRef sql.NullString
					if bErr := tx.QueryRowContext(ctx, `
						SELECT node_backup_target_ref
						FROM stack_backup_stores
						WHERE tenant_id = $1 AND stack_id = $2 AND token_revoked_at IS NULL
					`, state.TenantID, state.StackID).Scan(&activeTargetRef); bErr != nil && !errors.Is(bErr, sql.ErrNoRows) {
						return fmt.Errorf("inspect active backup store binding: %w", bErr)
					}
					if activeTargetRef.Valid && activeTargetRef.String == stagedTargetRef.String {
						observedWrite = true
						return nil
					}
				}
			}
			if strings.TrimSpace(cp.DestOriginCertID) != "" {
				var activeCertID sql.NullString
				if cErr := tx.QueryRowContext(ctx, `
					SELECT certificate_id
					FROM stack_origin_certificates
					WHERE tenant_id = $1 AND stack_id = $2
				`, state.TenantID, state.StackID).Scan(&activeCertID); cErr != nil && !errors.Is(cErr, sql.ErrNoRows) {
					return fmt.Errorf("inspect active origin certificate: %w", cErr)
				}
				if activeCertID.Valid && activeCertID.String == cp.DestOriginCertID {
					observedWrite = true
					return nil
				}
			}
			if strings.TrimSpace(state.DestinationServerID) != "" {
				var homelabActive bool
				if hlErr := tx.QueryRowContext(ctx, `
					SELECT EXISTS (
						SELECT 1 FROM homelabs
						WHERE tenant_id = $1 AND identity_authority_server_id = $2 AND deleted_at IS NULL
					)
				`, state.TenantID, state.DestinationServerID).Scan(&homelabActive); hlErr != nil {
					return fmt.Errorf("inspect homelab identity authority: %w", hlErr)
				}
				if homelabActive {
					observedWrite = true
					return nil
				}
			}
			var routingCount int
			if rErr := tx.QueryRowContext(ctx, `
				SELECT count(*)
				FROM stack_routing_desired
				WHERE tenant_id = $1 AND stack_id = $2 AND (server_id = $3 OR lease_id = $4)
			`, state.TenantID, state.StackID, state.DestinationServerID, state.DestinationLeaseID).Scan(&routingCount); rErr != nil {
				return fmt.Errorf("inspect desired routing authority: %w", rErr)
			}
			if routingCount > 0 {
				observedWrite = true
				return nil
			}
			var cmdState, cmdResultText string
			cmdErr := tx.QueryRowContext(ctx, `
				SELECT state, COALESCE(result_json::text, '{}')
				FROM typed_agent_commands
				WHERE tenant_id = $1 AND command_id = $2
			`, state.TenantID, state.ReplacementID+"-final-sync").Scan(&cmdState, &cmdResultText)
			if cmdErr != nil && !errors.Is(cmdErr, sql.ErrNoRows) {
				return fmt.Errorf("inspect typed_agent_commands final-sync: %w", cmdErr)
			}
			if cmdErr == nil {
				if cmdState == "completed" {
					var resDoc struct {
						Success bool `json:"success"`
					}
					if json.Unmarshal([]byte(cmdResultText), &resDoc) == nil && resDoc.Success {
						observedWrite = true
						return nil
					}
					activationCommandAttempted = true
				} else if cmdState != "failed" {
					activationCommandAttempted = true
				} else if !strings.Contains(cmdResultText, "restore_activation_not_started_at_") {
					activationCommandAttempted = true
				} else {
					activationNotStartedProved = true
				}
			} else if cp.ActivationOperationID != "" && !activationNotStartedProved {
				activationCommandAttempted = true
			}
			return nil
		})
		if err != nil {
			return false, err
		}
		if observedWrite {
			return false, nil
		}
	}

	execCtx, _, err := o.loadExecContext(ctx, state, false)
	if err != nil {
		return false, err
	}
	if execCtx.UsesManagedKombifyMe && strings.TrimSpace(execCtx.DestPublicIP) != "" {
		cfg := kombifyme.DefaultConfigFromEnv()
		if strings.TrimSpace(cfg.APIKey) == "" {
			return false, fmt.Errorf("kombify.me readback requires KOMBIFYME API key before rollback")
		}
		destIP := strings.TrimSpace(execCtx.DestPublicIP)
		parsedIP, ipErr := netip.ParseAddr(strings.Trim(destIP, "[]"))
		if ipErr != nil {
			return false, fmt.Errorf("invalid destination public IP %q during write-absence verification: %w", destIP, ipErr)
		}
		wantDestOrigin := normalizedHTTPOrigin("https://" + net.JoinHostPort(parsedIP.String(), "443"))
		addr, addrErr := kombifyme.NewManagedAddress(state.TenantID, state.OwnerSubjectID, state.StackID)
		if addrErr != nil {
			return false, addrErr
		}
		client := kombifyme.NewClient(cfg)
		readback, kmErr := client.Forward(ctx, http.MethodGet, "subdomains?binding_stack_ref="+url.QueryEscape(addr.StackRef), nil, "")
		if kmErr != nil {
			return false, fmt.Errorf("verify kombify.me routes absent for destination: %w", withUpstreamErrorCode(kmErr))
		}
		routes, ok := readback.Body.([]any)
		if !ok {
			return false, fmt.Errorf("kombify.me route readback returned invalid payload")
		}
		for _, raw := range routes {
			route, _ := raw.(map[string]any)
			if route == nil || route["subdomain_kind"] != "service" {
				continue
			}
			targetAddr, _ := route["target_addr"].(string)
			if normalizedHTTPOrigin(targetAddr) == wantDestOrigin {
				return false, nil
			}
		}
	}

	needsNodeVerification := (cp.ActivationOperationID != "" && !activationNotStartedProved) || activationCommandAttempted
	if needsNodeVerification || (destAgentID != "" && destConnState == "connected" && cp.DestinationApplied) {
		if needsNodeVerification && (o.Sender == nil || destAgentID == "" || destConnState != "connected") {
			return false, fmt.Errorf("destination Guard unreachable to verify absence of activation writes")
		}
		if o.Sender != nil && destAgentID != "" && destConnState == "connected" {
			statusCmd := &agentpb.StackKitCommand{
				CommandId:                state.ReplacementID + "-verify-no-writes-backup-status",
				Operation:                agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_STATUS,
				WorkingDirectory:         defaultStackKitNodeWorkspace,
				SpecPath:                 execCtx.SpecPath,
				TimeoutSeconds:           int32(stackKitReadCommandTimeout / time.Second),
				Release:                  execCtx.ReleasePin,
				LocalSiteRef:             execCtx.LocalBinding.SiteRef,
				LocalNodeRef:             execCtx.LocalBinding.NodeRef,
				LocalExecutionChannelRef: execCtx.LocalBinding.ExecutionChannelRef,
				Stackkit:                 execCtx.StackKit,
				StackName:                execCtx.StackName,
				StackkitInstanceId:       execCtx.StackKitInstanceID,
			}
			statusRes, statusErr := o.validateAndSend(ctx, state.TenantID, destAgentID, statusCmd)
			if statusErr != nil {
				return false, fmt.Errorf("destination node backup status verification failed: %w", statusErr)
			}
			if _, parseErr := stackkitcommand.ParseBackupStatusEvidence(statusCmd, statusRes); parseErr != nil {
				return false, fmt.Errorf("parse destination node backup status evidence: %w", parseErr)
			}
		}
		if needsNodeVerification {
			if execCtx.StackKit == DefaultBasementKitRef {
				recoverCmd := &agentpb.StackKitCommand{
					CommandId:                state.ReplacementID + "-verify-no-writes-recover",
					Operation:                agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_RESTORE_RECOVER,
					WorkingDirectory:         defaultStackKitNodeWorkspace,
					SpecPath:                 execCtx.SpecPath,
					TimeoutSeconds:           int32(stackKitBackupCommandTimeout / time.Second),
					Release:                  execCtx.ReleasePin,
					OwnerApproved:            true,
					LocalSiteRef:             execCtx.LocalBinding.SiteRef,
					LocalNodeRef:             execCtx.LocalBinding.NodeRef,
					LocalExecutionChannelRef: execCtx.LocalBinding.ExecutionChannelRef,
					Stackkit:                 execCtx.StackKit,
					StackName:                execCtx.StackName,
					StackkitInstanceId:       execCtx.StackKitInstanceID,
					ExpectedPlanHash:         cp.DestinationPlanHash,
					RestoreResultId:          firstNonEmpty(cp.FinalRestoreResultID, cp.RestoreResultID),
					ActivationOperationId:    firstNonEmpty(cp.ActivationOperationID, supportReplacementActivationID(state.ReplacementID)),
				}
				recRes, recErr := o.validateAndSend(ctx, state.TenantID, destAgentID, recoverCmd)
				if recErr != nil {
					if !strings.Contains(strings.ToLower(recErr.Error()), "exact lifecycle mutation is required for explicit recovery") {
						return false, fmt.Errorf("destination activation recovery probe failed: %w", recErr)
					}
				} else {
					ev, evErr := stackkitcommand.ParseRestoreActivationEvidence(recoverCmd, recRes)
					if evErr != nil {
						return false, fmt.Errorf("destination activation recovery probe returned unparseable evidence: %w", evErr)
					}
					return false, fmt.Errorf("destination node has restore activation journal %q (status=%s)", ev.OperationID, ev.Status)
				}
			} else {
				return false, fmt.Errorf("destination activation operation %q outcome cannot be proved absent before cutover", firstNonEmpty(cp.ActivationOperationID, state.ReplacementID+"-final-sync"))
			}
		}
	}

	return true, nil
}

func (o *DefaultSupportReplacementStepOps) resolveServicesToResume(ctx context.Context, state SupportReplacementState) ([]string, supportReplacementExecContext, error) {
	cp, err := o.loadCheckpoint(ctx, state)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, supportReplacementExecContext{}, fmt.Errorf("load support replacement checkpoint: %w", err)
	}
	services := filterStoppableServiceKeys(cp.FencedServices)
	if len(services) == 0 && cp.FenceVerified == nil {
		return nil, supportReplacementExecContext{}, nil
	}
	execCtx, cp, err := o.loadExecContext(ctx, state, false)
	if err != nil {
		return nil, supportReplacementExecContext{}, err
	}
	services = filterStoppableServiceKeys(cp.FencedServices)
	if len(services) == 0 && cp.FenceVerified != nil {
		services = filterStoppableServiceKeys(execCtx.StoppableServices)
	}
	return services, execCtx, nil
}

// PreflightResumeSourceWriters verifies fail-closed that fenced source writers
// can be resumed before any irreversible rollback state is committed.
func (o *DefaultSupportReplacementStepOps) PreflightResumeSourceWriters(ctx context.Context, state SupportReplacementState) error {
	services, execCtx, err := o.resolveServicesToResume(ctx, state)
	if err != nil {
		return err
	}
	if len(services) == 0 {
		return nil
	}
	if o == nil || o.Sender == nil {
		return &preDispatchValidationError{
			Operation: agentpb.StackKitOperation_STACKKIT_OPERATION_SERVICE_START,
			Err:       fmt.Errorf("StackKitCommandSender is not configured to resume fenced source services (%s)", strings.Join(services, ",")),
		}
	}
	if execCtx.SourceAgentID == "" {
		return fmt.Errorf("source server %q has no enrolled worker_id to resume services", state.SourceServerID)
	}
	return nil
}

// PreflightResumeDestinationWriters verifies fail-closed that fenced destination
// writers can be resumed before any irreversible cutover custody or terminal phase is committed.
func (o *DefaultSupportReplacementStepOps) PreflightResumeDestinationWriters(ctx context.Context, state SupportReplacementState) error {
	if strings.TrimSpace(state.DestinationServerID) == "" {
		return nil
	}
	services, _, err := o.resolveServicesToResume(ctx, state)
	if err != nil {
		return err
	}
	if len(services) == 0 {
		return nil
	}
	if o == nil || o.Sender == nil {
		return &preDispatchValidationError{
			Operation: agentpb.StackKitOperation_STACKKIT_OPERATION_SERVICE_START,
			Err:       fmt.Errorf("StackKitCommandSender is not configured to resume fenced destination services (%s)", strings.Join(services, ",")),
		}
	}
	if _, err := o.resolveServerAgentID(ctx, state.TenantID, state.DestinationServerID); err != nil {
		return fmt.Errorf("resolve destination server %q for service resume: %w", state.DestinationServerID, err)
	}
	return nil
}

func (o *DefaultSupportReplacementStepOps) resumeCubeServices(
	ctx context.Context,
	state SupportReplacementState,
	role string,
	agentID string,
	cmdPrefix string,
	services []string,
	execCtx supportReplacementExecContext,
) error {
	var errs []error
	for _, svc := range services {
		cmd := &agentpb.StackKitCommand{
			CommandId:                state.ReplacementID + cmdPrefix + svc,
			Operation:                agentpb.StackKitOperation_STACKKIT_OPERATION_SERVICE_START,
			WorkingDirectory:         defaultStackKitNodeWorkspace,
			SpecPath:                 execCtx.SpecPath,
			TimeoutSeconds:           int32(stackKitWriteCommandTimeout / time.Second),
			Release:                  execCtx.ReleasePin,
			OwnerApproved:            true,
			ServiceKey:               svc,
			LocalSiteRef:             execCtx.LocalBinding.SiteRef,
			LocalNodeRef:             execCtx.LocalBinding.NodeRef,
			LocalExecutionChannelRef: execCtx.LocalBinding.ExecutionChannelRef,
			Stackkit:                 execCtx.StackKit,
			StackName:                execCtx.StackName,
			StackkitInstanceId:       execCtx.StackKitInstanceID,
		}
		if _, err := o.validateAndSend(ctx, state.TenantID, agentID, cmd); err != nil {
			errs = append(errs, fmt.Errorf("resume %s service %q: %w", role, svc, err))
		}
	}
	return errors.Join(errs...)
}

func (o *DefaultSupportReplacementStepOps) ResumeSourceWriters(ctx context.Context, state SupportReplacementState) error {
	services, execCtx, err := o.resolveServicesToResume(ctx, state)
	if err != nil {
		return err
	}
	if len(services) == 0 {
		return nil
	}
	if execCtx.SourceAgentID == "" {
		return fmt.Errorf("source server %q has no enrolled worker_id to resume services", state.SourceServerID)
	}
	return o.resumeCubeServices(ctx, state, "source", execCtx.SourceAgentID, "-resume-", services, execCtx)
}

// ResumeDestinationWriters starts services on the destination Cube after a
// fenced_conflict is reconciled forward to completed.
func (o *DefaultSupportReplacementStepOps) ResumeDestinationWriters(ctx context.Context, state SupportReplacementState) error {
	if strings.TrimSpace(state.DestinationServerID) == "" {
		return nil
	}
	services, execCtx, err := o.resolveServicesToResume(ctx, state)
	if err != nil {
		return err
	}
	if len(services) == 0 {
		return nil
	}
	destAgentID, err := o.resolveServerAgentID(ctx, state.TenantID, state.DestinationServerID)
	if err != nil {
		return fmt.Errorf("resolve destination server %q for service resume: %w", state.DestinationServerID, err)
	}
	return o.resumeCubeServices(ctx, state, "destination", destAgentID, "-resume-dest-", services, execCtx)
}

func (o *DefaultSupportReplacementStepOps) DecommissionCube(ctx context.Context, tenantID, ownerID, stackID, leaseID, generationID string) (string, error) {
	if o == nil || o.Decommissioner == nil {
		return "", fmt.Errorf("managed lease decommissioner is not configured")
	}
	if o.DB == nil {
		return "", fmt.Errorf("support replacement DB is not configured")
	}
	var leaseRaw []byte
	var existingOpID, existingStatus string
	err := o.withProviderTenantTx(ctx, tenantID, true, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `
			SELECT lease_json
			FROM techstack_vm_leases
			WHERE tenant_id = $1 AND id = $2
		`, tenantID, leaseID).Scan(&leaseRaw); err != nil {
			return fmt.Errorf("load lease %q for decommission digest: %w", leaseID, err)
		}
		_ = tx.QueryRowContext(ctx, `
			SELECT operation_id, status
			FROM provider_operations
			WHERE tenant_id = $1
			  AND lease_id = $2
			  AND operation = 'decommission'
			  AND command_json #>> '{command,resource_generation_id}' = $3
			ORDER BY updated_at DESC
			LIMIT 1
		`, tenantID, leaseID, generationID).Scan(&existingOpID, &existingStatus)
		return nil
	})
	if err != nil {
		return "", err
	}
	if existingOpID != "" && (existingStatus == "pending" || existingStatus == "running" || existingStatus == "succeeded") {
		return existingOpID, nil
	}
	var lease vmlease.Lease
	if err := json.Unmarshal(leaseRaw, &lease); err != nil {
		return "", fmt.Errorf("decode lease %q for decommission digest: %w", leaseID, err)
	}
	digest, err := vmleases.ResourceGenerationDigest(tenantID, lease)
	if err != nil {
		return "", fmt.Errorf("compute resource generation digest for lease %q: %w", leaseID, err)
	}
	res, err := o.Decommissioner.DecommissionManagedLeases(ctx, ManagedLeaseDecommissionRequest{
		StackID:                  stackID,
		TenantID:                 tenantID,
		OwnerID:                  ownerID,
		LeaseID:                  leaseID,
		ResourceGenerationDigest: digest,
	})
	if err != nil {
		return "", err
	}
	if res != nil && len(res.Proofs) > 0 && strings.TrimSpace(res.Proofs[0].ReceiptRef) != "" {
		return res.Proofs[0].ReceiptRef, nil
	}
	return "decommission:" + leaseID, nil
}

func (o *DefaultSupportReplacementStepOps) VerifyAbsenceAndRelease(ctx context.Context, tenantID, leaseID, generationID string) error {
	if o == nil || o.DB == nil {
		return fmt.Errorf("support replacement DB is not configured")
	}
	trimmedGenID := strings.TrimSpace(generationID)
	if _, parseErr := uuid.Parse(trimmedGenID); parseErr != nil {
		return fmt.Errorf("verify absence and release evidence: invalid resource_generation_id %q: %w", generationID, parseErr)
	}
	var released, absent bool
	err := o.withProviderTenantTx(ctx, tenantID, true, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT
				EXISTS (
					SELECT 1
					FROM managed_runtime_capacity_release_facts
					WHERE tenant_id = $1
					  AND lease_id = $2
					  AND resource_generation_id = $3::uuid
				),
				(
					EXISTS (
						SELECT 1
						FROM provider_absence_observations AS obs
						JOIN provider_operations AS op
						  ON op.tenant_id = obs.tenant_id
						 AND op.operation_id = obs.operation_id
						WHERE obs.tenant_id = $1
						  AND op.lease_id = $2
						  AND (op.command_json #>> '{command,resource_generation_id}')::uuid = $3::uuid
						  AND obs.observation = 'absent'
						  AND obs.source = 'provider_api'
					)
					OR EXISTS (
						SELECT 1
						FROM managed_runtime_capacity_release_facts AS fact
						WHERE fact.tenant_id = $1
						  AND fact.lease_id = $2
						  AND fact.resource_generation_id = $3::uuid
						  AND fact.release_authority = 'resource_free_teardown'
					)
				)
		`, tenantID, leaseID, trimmedGenID).Scan(&released, &absent)
	})
	if err != nil {
		return fmt.Errorf("verify absence and release evidence: %w", err)
	}
	if !released || !absent {
		return fmt.Errorf("lease %q generation %q awaiting provider absence (%v) and capacity release (%v)", leaseID, generationID, absent, released)
	}
	return nil
}

func (o *DefaultSupportReplacementStepOps) DiscardReplacementBackupCustody(ctx context.Context, state SupportReplacementState) error {
	if o == nil || o.BackupDelivery == nil {
		return nil
	}
	repDelivery, ok := o.BackupDelivery.(ManagedBackupReplacementDelivery)
	if !ok {
		return fmt.Errorf("backup delivery authority does not support replacement custody discard")
	}
	return repDelivery.DiscardReplacementCustody(ctx, state.TenantID, state.StackID, state.ReplacementID)
}

func (o *DefaultSupportReplacementStepOps) RevokeStagedOriginCertificate(ctx context.Context, state SupportReplacementState) error {
	if o == nil {
		return nil
	}
	cp, err := o.loadCheckpoint(ctx, state)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if strings.TrimSpace(cp.DestOriginCertID) == "" {
		return nil
	}
	if o.OriginCertificates == nil {
		return originca.ErrUnavailable
	}
	if err := o.OriginCertificates.RevokeCertificate(ctx, cp.DestOriginCertID); err != nil {
		return err
	}
	if o.DB != nil {
		return o.withProviderTenantTx(ctx, state.TenantID, false, func(tx *sql.Tx) error {
			_, execErr := tx.ExecContext(ctx, `
				UPDATE managed_runtime_support_replacements
				SET checkpoint_json = COALESCE(checkpoint_json, '{}'::jsonb)
				    - 'dest_origin_cert_id'
				    - 'dest_origin_cert_hosts'
				    - 'dest_origin_cert_sha256'
				    - 'dest_origin_cert_not_after'
				WHERE tenant_id = $1 AND replacement_id = $2
			`, state.TenantID, state.ReplacementID)
			return execErr
		})
	}
	return nil
}

func (o *DefaultSupportReplacementStepOps) FenceBothCubes(ctx context.Context, state SupportReplacementState, reason string) error {
	if o == nil || o.Sender == nil {
		return fmt.Errorf("fence both cubes: StackKitCommandSender is not configured")
	}
	fenceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	execCtx, _, err := o.loadExecContext(fenceCtx, state, false)
	if err != nil {
		return fmt.Errorf("fence both cubes: %w", err)
	}
	services := filterStoppableServiceKeys(execCtx.StoppableServices)
	if len(services) == 0 {
		return fmt.Errorf("fence both cubes: no stoppable services resolved for stack %q", state.StackID)
	}
	type target struct {
		role     string
		serverID string
	}
	targets := []target{{role: "source", serverID: strings.TrimSpace(state.SourceServerID)}}
	if dst := strings.TrimSpace(state.DestinationServerID); dst != "" {
		targets = append(targets, target{role: "destination", serverID: dst})
	}
	var errs []error
	for _, tgt := range targets {
		if tgt.serverID == "" {
			errs = append(errs, fmt.Errorf("fence %s: server id is missing", tgt.role))
			continue
		}
		agentID, resolveErr := o.resolveServerAgentID(fenceCtx, state.TenantID, tgt.serverID)
		if resolveErr != nil {
			errs = append(errs, fmt.Errorf("fence %s (%s): %w", tgt.role, tgt.serverID, resolveErr))
			continue
		}
		for _, svc := range services {
			cmd := &agentpb.StackKitCommand{
				CommandId:                state.ReplacementID + "-fence-conflict-" + tgt.serverID + "-" + svc,
				Operation:                agentpb.StackKitOperation_STACKKIT_OPERATION_SERVICE_STOP,
				WorkingDirectory:         defaultStackKitNodeWorkspace,
				SpecPath:                 execCtx.SpecPath,
				TimeoutSeconds:           int32(stackKitWriteCommandTimeout / time.Second),
				Release:                  execCtx.ReleasePin,
				OwnerApproved:            true,
				ServiceKey:               svc,
				LocalSiteRef:             execCtx.LocalBinding.SiteRef,
				LocalNodeRef:             execCtx.LocalBinding.NodeRef,
				LocalExecutionChannelRef: execCtx.LocalBinding.ExecutionChannelRef,
				Stackkit:                 execCtx.StackKit,
				StackName:                execCtx.StackName,
				StackkitInstanceId:       execCtx.StackKitInstanceID,
			}
			if _, sendErr := o.validateAndSend(fenceCtx, state.TenantID, agentID, cmd); sendErr != nil {
				errs = append(errs, fmt.Errorf("fence %s (%s) service %q: %w", tgt.role, tgt.serverID, svc, sendErr))
			}
		}
	}
	return errors.Join(errs...)
}

func (o *DefaultSupportReplacementStepOps) resolveServerAgentID(ctx context.Context, tenantID, serverID string) (string, error) {
	if o == nil || o.DB == nil {
		return "", fmt.Errorf("support replacement DB is not configured")
	}
	var workerID sql.NullString
	err := o.withProviderTenantTx(ctx, tenantID, true, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT worker_id FROM servers WHERE tenant_id = $1 AND id = $2
		`, tenantID, serverID).Scan(&workerID)
	})
	if err != nil {
		return "", fmt.Errorf("resolve server worker_id: %w", err)
	}
	if !workerID.Valid || strings.TrimSpace(workerID.String) == "" {
		return "", fmt.Errorf("server %q has no enrolled worker_id", serverID)
	}
	return strings.TrimSpace(workerID.String), nil
}

type preDispatchValidationError struct {
	Operation agentpb.StackKitOperation
	Err       error
}

func (e *preDispatchValidationError) Error() string {
	return fmt.Sprintf("validate StackKit command %v: %v", e.Operation, e.Err)
}

func (e *preDispatchValidationError) Unwrap() error {
	return e.Err
}

type definiteStepCommandError struct {
	Operation agentpb.StackKitOperation
	ExitCode  int32
	Stderr    string
}

func (e *definiteStepCommandError) Error() string {
	return fmt.Sprintf("command %v failed (exit %d): %s", e.Operation, e.ExitCode, e.Stderr)
}

func (o *DefaultSupportReplacementStepOps) validateAndSend(ctx context.Context, tenantID, agentID string, cmd *agentpb.StackKitCommand) (*agentpb.StackKitResult, error) {
	if o == nil || o.Sender == nil {
		return nil, &preDispatchValidationError{Err: fmt.Errorf("StackKitCommandSender is not configured")}
	}
	if cmd == nil {
		return nil, &preDispatchValidationError{Err: fmt.Errorf("command is required")}
	}
	if err := stackkitcommand.ValidateCommand(cmd); err != nil {
		return nil, &preDispatchValidationError{Operation: cmd.Operation, Err: err}
	}
	res, err := sendStackKitCommandBoundedForTenant(ctx, o.Sender, tenantID, agentID, cmd)
	if err != nil {
		return nil, err
	}
	if res != nil && !res.Success {
		return res, &definiteStepCommandError{
			Operation: cmd.Operation,
			ExitCode:  res.ExitCode,
			Stderr:    strings.TrimSpace(res.Stderr),
		}
	}
	return res, nil
}

func isAckAmbiguousError(err error) bool {
	if err == nil {
		return false
	}
	var defErr *definiteStepCommandError
	var preErr *preDispatchValidationError
	if errors.As(err, &defErr) || errors.As(err, &preErr) {
		return false
	}
	if errors.Is(err, ErrSupportReplacementAckLost) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.DeadlineExceeded, codes.Canceled, codes.Unavailable, codes.Aborted, codes.Unknown:
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"timeout",
		"timed out",
		"deadline exceeded",
		"context canceled",
		"connection reset",
		"broken pipe",
		"transport is closing",
		"use of closed network connection",
		"stream closed",
		"stream terminated",
		"agent is disconnected",
		"eof",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
