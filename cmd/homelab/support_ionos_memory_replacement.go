package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/kombifyio/homelab/internal/providercontrol"
	"github.com/kombifyio/homelab/internal/routes"
	"github.com/kombifyio/homelab/pkg/db"
	"github.com/kombifyio/homelab/pkg/jobs"
	"github.com/kombifyio/homelab/pkg/logger"
	"github.com/kombifyio/homelab/pkg/orchestrator"
)

type providerControlSupportReplacementStore struct {
	admission *providercontrol.NativeAdmission
}

func (s providerControlSupportReplacementStore) GetSupportReplacement(ctx context.Context, tenantID, replacementID string) (jobs.SupportReplacementState, error) {
	rec, err := s.admission.GetSupportReplacement(ctx, tenantID, replacementID)
	if err != nil {
		return jobs.SupportReplacementState{}, err
	}
	return supportReplacementRecordToJobState(rec), nil
}

func (s providerControlSupportReplacementStore) AdmitSupportReplacementDestination(ctx context.Context, tenantID, replacementID, idempotencyKey string) (jobs.SupportReplacementState, error) {
	_, rec, err := s.admission.AdmitSupportReplacementProvision(ctx, providercontrol.SupportReplacementProvisionRequest{
		TenantID:       tenantID,
		ReplacementID:  replacementID,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return jobs.SupportReplacementState{}, err
	}
	return supportReplacementRecordToJobState(rec), nil
}

func (s providerControlSupportReplacementStore) TransitionSupportReplacement(ctx context.Context, req jobs.SupportReplacementTransition) (jobs.SupportReplacementState, error) {
	rec, err := s.admission.TransitionSupportReplacement(ctx, providercontrol.SupportReplacementTransitionRequest{
		TenantID:                  req.TenantID,
		ReplacementID:             req.ReplacementID,
		Phase:                     req.Phase,
		DestinationWritesObserved: req.DestinationWritesObserved,
		CutoverVerifiedAt:         req.CutoverVerifiedAt,
		SourceTeardownOperationID: req.SourceTeardownOperationID,
		RollbackReason:            req.RollbackReason,
		FencedConflictReason:      req.FencedConflictReason,
		CheckpointPatchJSON:       req.CheckpointPatchJSON,
	})
	if err != nil {
		return jobs.SupportReplacementState{}, err
	}
	return supportReplacementRecordToJobState(rec), nil
}

func (s providerControlSupportReplacementStore) EnsureNoActiveSupportReplacementForStack(ctx context.Context, tenantID, stackID string) error {
	if s.admission == nil {
		return nil
	}
	return s.admission.EnsureNoActiveSupportReplacementForStack(ctx, tenantID, stackID)
}

func supportReplacementRecordToJobState(rec providercontrol.SupportReplacementRecord) jobs.SupportReplacementState {
	return jobs.SupportReplacementState{
		TenantID:                      rec.TenantID,
		ReplacementID:                 rec.ReplacementID,
		OwnerSubjectID:                rec.OwnerSubjectID,
		StackID:                       rec.StackID,
		RuntimeSlotID:                 rec.RuntimeSlotID,
		SourceLeaseID:                 rec.SourceLeaseID,
		SourceServerID:                rec.SourceServerID,
		SourceResourceGenerationID:    rec.SourceResourceGenerationID,
		SourceGenerationOrdinal:       rec.SourceGenerationOrdinal,
		DestinationLeaseID:            rec.DestinationLeaseID,
		DestinationServerID:           rec.DestinationServerID,
		DestinationResourceGeneration: rec.DestinationResourceGeneration,
		DestinationGenerationOrdinal:  rec.DestinationGenerationOrdinal,
		DestinationOperationID:        rec.DestinationOperationID,
		DestinationWritesObserved:     rec.DestinationWritesObserved,
		CutoverVerifiedAt:             rec.CutoverVerifiedAt,
		SourceTeardownOperationID:     rec.SourceTeardownOperationID,
		RollbackReason:                rec.RollbackReason,
		FencedConflictReason:          rec.FencedConflictReason,
		CheckpointJSON:                rec.CheckpointJSON,
		Phase:                         rec.Phase,
		NoChargeConfirmed:             rec.NoChargeConfirmed,
		CustomerAcceptedAt:            rec.CustomerAcceptedAt,
	}
}

type routeSupportReplacementController struct {
	admission *providercontrol.NativeAdmission
	orch      *orchestrator.Orchestrator
	ops       *jobs.DefaultSupportReplacementStepOps
}

func (c routeSupportReplacementController) Authorize(ctx context.Context, req providercontrol.AuthorizeSupportReplacementRequest) (providercontrol.SupportReplacementRecord, error) {
	return c.admission.AuthorizeSupportReplacement(ctx, req)
}

func (c routeSupportReplacementController) Accept(ctx context.Context, req providercontrol.AcceptSupportReplacementRequest) (providercontrol.SupportReplacementRecord, error) {
	return c.admission.AcceptSupportReplacement(ctx, req)
}

func (c routeSupportReplacementController) Get(ctx context.Context, tenantID, replacementID string) (providercontrol.SupportReplacementRecord, error) {
	return c.admission.GetSupportReplacement(ctx, tenantID, replacementID)
}

func (c routeSupportReplacementController) List(ctx context.Context, tenantID, phase string) ([]providercontrol.SupportReplacementRecord, error) {
	return c.admission.ListSupportReplacements(ctx, tenantID, phase)
}

func (c routeSupportReplacementController) EnqueueRun(
	ctx context.Context,
	tenantID, ownerID, stackID, replacementID, idempotencyKey string,
	forcePreWriteRollback bool,
	reason string,
) (string, error) {
	if c.orch == nil {
		return "", fmt.Errorf("%w: orchestrator unavailable", providercontrol.ErrProfileUnavailable)
	}
	return c.orch.EnqueueSupportIONOSMemoryReplacement(ctx, tenantID, ownerID, stackID, replacementID, idempotencyKey, forcePreWriteRollback, reason)
}

func (c routeSupportReplacementController) ReconcileConflict(ctx context.Context, tenantID, replacementID, targetPhase, reason string) (providercontrol.SupportReplacementRecord, error) {
	return reconcileSupportReplacementConflict(ctx, c.admission, c.ops, c.orch, tenantID, replacementID, targetPhase, reason)
}

var _ routes.SupportReplacementController = routeSupportReplacementController{}

func runSupportIONOSMemoryReplacement(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: support-ionos-memory-replacement <authorize|accept|status|list|rollback|reconcile> [flags]")
	}
	action := strings.TrimSpace(args[0])
	rest := args[1:]

	fs := flag.NewFlagSet("support-ionos-memory-replacement "+action, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		tenantID, replacementID, ownerID, stackID, sourceLeaseID string
		caseRef, reason, authorizedBy, evidenceRef, acceptedBy   string
		idempotencyKey, phase, targetPhase                       string
		noChargeConfirmed                                        bool
	)
	fs.StringVar(&tenantID, "tenant-id", "", "tenant id")
	fs.StringVar(&replacementID, "replacement-id", "", "replacement id")
	fs.StringVar(&ownerID, "owner-id", "", "owner subject id")
	fs.StringVar(&stackID, "stack-id", "", "stack id")
	fs.StringVar(&sourceLeaseID, "source-lease-id", "", "source Basic Cube S lease id")
	fs.StringVar(&caseRef, "case-ref", "", "support case reference")
	fs.StringVar(&reason, "reason", "", "authorization / rollback / reconciliation reason")
	fs.StringVar(&authorizedBy, "authorized-by", "", "staff principal authorizing replacement")
	fs.StringVar(&evidenceRef, "evidence-ref", "", "support evidence reference")
	fs.StringVar(&acceptedBy, "accepted-by", "", "customer acceptance principal")
	fs.StringVar(&idempotencyKey, "idempotency-key", "", "idempotency key")
	fs.StringVar(&phase, "phase", "", "phase filter for list")
	fs.StringVar(&targetPhase, "target-phase", "", "reconcile target phase (completed|rolled_back|cutover_in_progress|rolling_back)")
	fs.BoolVar(&noChargeConfirmed, "no-charge-confirmed", false, "confirm zero customer charge")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	database, release, err := openManagedRuntimeRetenantDatabase(ctx)
	if err != nil {
		return err
	}
	defer release()
	composed, err := composeManagedRuntimeRetenantControl(database)
	if err != nil {
		return err
	}
	admission := composed.admission
	cliOps := composeSupportReplacementCLIOps(database)
	switch action {
	case "authorize":
		rec, err := admission.AuthorizeSupportReplacement(ctx, providercontrol.AuthorizeSupportReplacementRequest{
			TenantID:          tenantID,
			ReplacementID:     replacementID,
			OwnerSubjectID:    ownerID,
			StackID:           stackID,
			SourceLeaseID:     sourceLeaseID,
			CaseRef:           caseRef,
			Reason:            reason,
			AuthorizedBy:      authorizedBy,
			EvidenceRef:       evidenceRef,
			IdempotencyKey:    idempotencyKey,
			NoChargeConfirmed: noChargeConfirmed,
		})
		if err != nil {
			return err
		}
		return printSupportReplacementJSON(os.Stdout, rec)
	case "accept":
		rec, err := admission.AcceptSupportReplacement(ctx, providercontrol.AcceptSupportReplacementRequest{
			TenantID:       tenantID,
			ReplacementID:  replacementID,
			OwnerSubjectID: ownerID,
			AcceptedBy:     acceptedBy,
		})
		if err != nil {
			return err
		}
		return printSupportReplacementJSON(os.Stdout, rec)
	case "status":
		rec, err := admission.GetSupportReplacement(ctx, tenantID, replacementID)
		if err != nil {
			return err
		}
		snap, err := admission.InspectSupportReplacementCapacity(ctx, tenantID, replacementID)
		if err != nil {
			return err
		}
		return printSupportReplacementJSON(os.Stdout, map[string]any{"replacement": rec, "capacity": snap})
	case "list":
		items, err := admission.ListSupportReplacements(ctx, tenantID, phase)
		if err != nil {
			return err
		}
		return printSupportReplacementJSON(os.Stdout, items)
	case "rollback":
		rec, err := rollbackSupportReplacementFromCLI(ctx, admission, cliOps, tenantID, replacementID, reason)
		if err != nil {
			return err
		}
		return printSupportReplacementJSON(os.Stdout, rec)
	case "reconcile":
		rec, err := reconcileSupportReplacementConflict(ctx, admission, cliOps, nil, tenantID, replacementID, targetPhase, reason)
		if err != nil {
			return err
		}
		return printSupportReplacementJSON(os.Stdout, rec)
	default:
		return fmt.Errorf("unknown support-ionos-memory-replacement subcommand %q", action)
	}
}

func composeSupportReplacementCLIOps(database *db.DB) *jobs.DefaultSupportReplacementStepOps {
	if database == nil || database.DB == nil {
		return nil
	}
	deps := routeDeps{
		v2:  &v2Boot{db: database},
		log: logger.Get(),
	}
	inv := managedStackKitInventoryBuilder(deps)
	var backupDelivery jobs.ManagedBackupTargetDelivery
	if provider, ok := inv.(interface {
		BackupDelivery() jobs.ManagedBackupTargetDelivery
	}); ok {
		backupDelivery = provider.BackupDelivery()
	}
	return &jobs.DefaultSupportReplacementStepOps{
		DB:                 database.DB,
		ControlPlaneDB:     database.DB,
		BackupDelivery:     backupDelivery,
		Inventory:          inv,
		OriginCertificates: composeOriginCertificates(deps.v2, deps.log),
	}
}

func reconcileSupportReplacementConflict(
	ctx context.Context,
	admission *providercontrol.NativeAdmission,
	ops *jobs.DefaultSupportReplacementStepOps,
	orch *orchestrator.Orchestrator,
	tenantID, replacementID, targetPhase, reason string,
) (providercontrol.SupportReplacementRecord, error) {
	if admission == nil {
		return providercontrol.SupportReplacementRecord{}, fmt.Errorf("%w: native admission is not configured", providercontrol.ErrInvalidRequest)
	}
	target := strings.TrimSpace(targetPhase)
	switch target {
	case providercontrol.SupportReplacementPhaseCompleted, providercontrol.SupportReplacementPhaseRolledBack:
		rec, err := admission.ValidateSupportReplacementConflictReconcile(ctx, tenantID, replacementID, target, reason)
		if err != nil {
			return rec, err
		}
		if prepErr := prepareSupportReplacementTerminalOutcome(ctx, ops, rec, target); prepErr != nil {
			return rec, fmt.Errorf("pre-%s cleanup failed before committing immutable terminal phase: %w", target, prepErr)
		}
		return admission.ReconcileSupportReplacementConflict(ctx, tenantID, replacementID, target, reason)
	default:
		rec, err := admission.ReconcileSupportReplacementConflict(ctx, tenantID, replacementID, target, reason)
		if err != nil {
			return rec, err
		}
		if cleanupErr := enqueueReconciledSupportReplacementWorkflow(ctx, orch, rec, reason); cleanupErr != nil {
			return rec, fmt.Errorf("conflict reconciled to %s, but post-reconcile enqueue failed: %w", rec.Phase, cleanupErr)
		}
		return rec, nil
	}
}

func rollbackSupportReplacementFromCLI(
	ctx context.Context,
	admission *providercontrol.NativeAdmission,
	ops *jobs.DefaultSupportReplacementStepOps,
	tenantID, replacementID, reason string,
) (providercontrol.SupportReplacementRecord, error) {
	if admission == nil {
		return providercontrol.SupportReplacementRecord{}, fmt.Errorf("%w: native admission is not configured", providercontrol.ErrInvalidRequest)
	}
	rec, err := admission.ValidateSupportReplacementCLIRollback(ctx, tenantID, replacementID, reason)
	if err != nil {
		return rec, err
	}
	if rec.Phase == providercontrol.SupportReplacementPhaseRolledBack {
		return rec, nil
	}
	if prepErr := prepareSupportReplacementRollback(ctx, ops, rec, true); prepErr != nil {
		return rec, fmt.Errorf("pre-rollback verification/cleanup failed before committing rolled_back: %w", prepErr)
	}
	return admission.RollbackSupportReplacementFromCLI(ctx, tenantID, replacementID, reason)
}

func prepareSupportReplacementTerminalOutcome(
	ctx context.Context,
	ops *jobs.DefaultSupportReplacementStepOps,
	rec providercontrol.SupportReplacementRecord,
	targetPhase string,
) error {
	switch targetPhase {
	case providercontrol.SupportReplacementPhaseRolledBack:
		return prepareSupportReplacementRollback(ctx, ops, rec, false)
	case providercontrol.SupportReplacementPhaseCompleted:
		if err := requireSupportReplacementCleanupDeps(ctx, ops, rec); err != nil {
			return err
		}
		state := supportReplacementRecordToJobState(rec)
		if err := ops.PreflightResumeDestinationWriters(ctx, state); err != nil {
			return fmt.Errorf("resume destination writers preflight: %w", err)
		}
		if err := ops.FinalizeCutoverCustody(ctx, state); err != nil {
			return fmt.Errorf("finalize cutover custody: %w", err)
		}
		if err := ops.ResumeDestinationWriters(ctx, state); err != nil {
			return fmt.Errorf("resume destination writers: %w", err)
		}
		return nil
	default:
		return nil
	}
}

func prepareSupportReplacementRollback(
	ctx context.Context,
	ops *jobs.DefaultSupportReplacementStepOps,
	rec providercontrol.SupportReplacementRecord,
	verifyNoDestinationWrites bool,
) error {
	if err := requireSupportReplacementCleanupDeps(ctx, ops, rec); err != nil {
		return err
	}
	state := supportReplacementRecordToJobState(rec)
	if err := ops.PreflightResumeSourceWriters(ctx, state); err != nil {
		return fmt.Errorf("resume source writers preflight: %w", err)
	}
	if verifyNoDestinationWrites && strings.TrimSpace(state.DestinationLeaseID) != "" {
		clean, err := ops.VerifyNoDestinationWrites(ctx, state)
		if err != nil {
			return fmt.Errorf("verify no destination writes: %w", err)
		}
		if !clean {
			return fmt.Errorf("destination writes observed; refusing pre-write rollback")
		}
	}
	if ops.BackupDelivery != nil {
		if err := ops.DiscardReplacementBackupCustody(ctx, state); err != nil {
			return fmt.Errorf("discard replacement backup custody: %w", err)
		}
	}
	if err := ops.RevokeStagedOriginCertificate(ctx, state); err != nil {
		return fmt.Errorf("revoke staged origin certificate: %w", err)
	}
	if err := ops.ResumeSourceWriters(ctx, state); err != nil {
		return fmt.Errorf("resume source writers: %w", err)
	}
	return nil
}

func requireSupportReplacementCleanupDeps(
	ctx context.Context,
	ops *jobs.DefaultSupportReplacementStepOps,
	rec providercontrol.SupportReplacementRecord,
) error {
	if ops == nil {
		return fmt.Errorf("support replacement cleanup dependencies are not configured")
	}
	if ops.BackupDelivery == nil && hasStagedReplacementHandoff(ctx, ops.ControlPlaneDB, rec.TenantID, rec.StackID, rec.ReplacementID) {
		return fmt.Errorf("staged backup replacement handoff exists for %q but BackupDelivery is not configured in this context", rec.ReplacementID)
	}
	return nil
}

func enqueueReconciledSupportReplacementWorkflow(
	ctx context.Context,
	orch *orchestrator.Orchestrator,
	rec providercontrol.SupportReplacementRecord,
	reason string,
) error {
	switch rec.Phase {
	case providercontrol.SupportReplacementPhaseCutoverInProgress, providercontrol.SupportReplacementPhaseRollingBack:
		if orch == nil {
			return fmt.Errorf("reconciled to %s; start or run the replacement workflow via the running Homelab server (/run) to drive remaining steps", rec.Phase)
		}
		idem := fmt.Sprintf("reconcile:%s:%s:%s", rec.Phase, rec.ReplacementID, rec.UpdatedAt.UTC().Format(time.RFC3339Nano))
		if _, err := orch.EnqueueSupportIONOSMemoryReplacement(
			ctx, rec.TenantID, rec.OwnerSubjectID, rec.StackID, rec.ReplacementID, idem, false, reason,
		); err != nil {
			return fmt.Errorf("enqueue resumed %s workflow: %w", rec.Phase, err)
		}
		return nil
	default:
		return nil
	}
}

func hasStagedReplacementHandoff(ctx context.Context, controlPlaneDB *sql.DB, tenantID, stackID, replacementID string) bool {
	if controlPlaneDB == nil {
		return false
	}
	tx, err := controlPlaneDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return true
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		return true
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM stack_backup_replacement_handoffs
			WHERE tenant_id = $1 AND stack_id = $2 AND replacement_id = $3 AND status = 'staged'
		)
	`, tenantID, stackID, replacementID).Scan(&exists); err != nil {
		return true
	}
	return exists
}

func printSupportReplacementJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
