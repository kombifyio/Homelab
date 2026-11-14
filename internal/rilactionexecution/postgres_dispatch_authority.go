package rilactionexecution

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/kombifyio/techstack/pkg/db/tenantcontext"
	"strings"
	"time"

	"github.com/kombifyio/techstack/pkg/api/agentpb"
	"github.com/kombifyio/techstack/pkg/guardreach"
	"github.com/kombifyio/techstack/pkg/ril/actioncontract"
	"github.com/kombifyio/techstack/pkg/ril/actions"
	"github.com/kombifyio/techstack/pkg/ril/nodeproof"
)

// PostgresDispatchAuthority re-checks and binds the admitted card, server and
// lifecycle heads under row locks immediately before the typed command is
// sent, so the worker cannot be selected from a stale inventory projection or
// from the provider-free stack target. The locks are released before the send:
// a send can last the command's own timeout (an upgrade runs up to 25
// minutes), and holding the server row through it would block the worker's
// heartbeats. The bound authority is then the one-use Gateway claim of this
// execution (fenced to the admission digest), which the node verifies again
// immediately before it starts the process.
type PostgresDispatchAuthority struct{ DB *sql.DB }

func (a PostgresDispatchAuthority) OwnerFor(ctx context.Context, request rilaction.Request, admission string) (string, error) {
	if a.DB == nil {
		return "", fmt.Errorf("RIL authority unavailable")
	}
	tx, err := tenantcontext.Database(ctx, a.DB).BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err = tenantcontext.ApplyTenant(ctx, tx, request.TenantID); err != nil {
		return "", err
	}
	var owner string
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT owner_subject_id, execution_request_json FROM ril_action_cards WHERE tenant_id=$1 AND id=$2 AND execution_id=$3 AND execution_admission_digest=$4 AND status='executing'`, request.TenantID, request.ActionCardID, request.ExecutionID, admission).Scan(&owner, &raw)
	if err != nil {
		return "", err
	}
	var stored rilaction.Request
	if json.Unmarshal(raw, &stored) != nil {
		return "", fmt.Errorf("invalid admitted request")
	}
	want, err := rilaction.ComputeRequestDigest(request)
	if err != nil {
		return "", err
	}
	got, err := rilaction.ComputeRequestDigest(stored)
	if err != nil || got != want || owner == "" {
		return "", fmt.Errorf("admitted owner mismatch")
	}
	return owner, nil
}

func (a PostgresDispatchAuthority) WithWorker(ctx context.Context, request rilaction.Request, admission string, proof *agentpb.RILExecutionProof, dispatch func(context.Context, string) error) error {
	if a.DB == nil || dispatch == nil {
		return fmt.Errorf("RIL dispatch authority unavailable")
	}
	tx, err := tenantcontext.Database(ctx, a.DB).BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = tenantcontext.ApplyTenant(ctx, tx, request.TenantID); err != nil {
		return err
	}
	var owner, server, lifecycleID, generationID string
	var saved, proposalRaw []byte
	var cardRevision, inventoryRevision, serverRevision, serverGeneration, lifecycleRevision int64
	err = tx.QueryRowContext(ctx, `
 SELECT card.execution_request_json, card.owner_subject_id, card.server_id, card.decision_proposal_json, card.decision_card_revision,
        card.admission_inventory_revision, card.admission_server_revision, card.admission_server_generation,
        card.admission_lease_id, card.admission_lease_revision, card.admission_resource_generation_id::text
 FROM ril_action_cards card
 WHERE card.tenant_id=$1 AND card.id=$2 AND card.execution_id=$3
   AND card.execution_admission_digest=$4 AND card.status='executing'
 FOR UPDATE`, request.TenantID, request.ActionCardID, request.ExecutionID, admission).Scan(&saved, &owner, &server, &proposalRaw, &cardRevision, &inventoryRevision, &serverRevision, &serverGeneration, &lifecycleID, &lifecycleRevision, &generationID)
	if err != nil {
		return fmt.Errorf("resolve admitted RIL action: %w", err)
	}
	// The same lifecycle authority model that admitted the execution must
	// still hold exactly: the VM lease of a leased server, or the owner's own
	// admitted server, at the admitted revisions and generation.
	lifecycle, err := actions.LockLifecycleAuthorityTx(ctx, tx, request.TenantID, owner, server, request.StackID, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("resolve admitted RIL worker: %w", err)
	}
	if !lifecycle.MatchesAdmission(actions.ExecutionAdmission{InventoryRevision: inventoryRevision, ServerRevision: serverRevision, ServerGeneration: serverGeneration, LifecycleAuthorityID: lifecycleID, LifecycleRevision: lifecycleRevision, LifecycleGenerationID: generationID}) {
		return fmt.Errorf("RIL lifecycle authority differs from the admitted execution")
	}
	worker, validUntil := lifecycle.WorkerID, lifecycle.ValidUntil
	var persisted rilaction.Request
	if err := json.Unmarshal(saved, &persisted); err != nil {
		return fmt.Errorf("decode admitted RIL request: %w", err)
	}
	expected, err := rilaction.ComputeRequestDigest(request)
	if err != nil {
		return err
	}
	actual, err := rilaction.ComputeRequestDigest(persisted)
	if err != nil || actual != expected {
		return fmt.Errorf("RIL request differs from admitted authority")
	}
	var proposal struct {
		EffectDigest       string `json:"effectDigest"`
		LifecycleRevision  int64  `json:"lifecycleRevision"`
		LifecycleExpiresAt int64  `json:"lifecycleExpiresAt"`
		ServerRevision     int64  `json:"serverRevision"`
		Effect             struct {
			CardRevision int64 `json:"cardRevision"`
		} `json:"effect"`
	}
	if json.Unmarshal(proposalRaw, &proposal) != nil {
		return fmt.Errorf("RIL proposal unavailable")
	}
	facts, err := nodeproof.InspectTransport(proof)
	if err != nil {
		return err
	}
	if facts.ExecutionID != request.ExecutionID || facts.Fence != admission || facts.TenantID != request.TenantID || facts.OwnerSubject != owner || facts.ServerID != server || facts.StackID != request.StackID || facts.EffectDigest != proposal.EffectDigest || facts.ReceiptDigest != request.Approval.ReceiptHash || proposal.Effect.CardRevision != cardRevision || proposal.LifecycleRevision != lifecycle.Revision || facts.LifecycleRevision != lifecycle.Revision || proposal.ServerRevision != lifecycle.ServerRevision || facts.RuntimeLeaseExpiresAt != proposal.LifecycleExpiresAt || facts.RuntimeLeaseExpiresAt > validUntil.Unix() || facts.Exp > facts.RuntimeLeaseExpiresAt || facts.Exp <= time.Now().Unix() {
		return fmt.Errorf("RIL proof differs from locked proposal and lifecycle heads")
	}
	deadline, err := time.Parse(time.RFC3339Nano, request.ValidUntil)
	if err != nil {
		return err
	}
	if validUntil.Before(deadline) {
		deadline = validUntil
	}
	if !time.Now().Before(deadline) || strings.TrimSpace(worker) == "" {
		return fmt.Errorf("RIL dispatch authority expired")
	}
	// The authority is bound; release the card, server and lifecycle locks
	// before the send instead of holding them for the command's duration.
	if err := tx.Commit(); err != nil {
		return err
	}
	bounded, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	return dispatch(bounded, worker)
}

// IssuedLease opens only the authority sealed in the same transaction that
// admitted this exact execution. It is never returned through workflow state.
func (a PostgresDispatchAuthority) IssuedLease(ctx context.Context, request rilaction.Request, admission string) (guardreach.RILAuthorityResult, error) {
	var empty guardreach.RILAuthorityResult
	if a.DB == nil {
		return empty, fmt.Errorf("RIL authority unavailable")
	}
	tx, err := tenantcontext.Database(ctx, a.DB).BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	if err = tenantcontext.ApplyTenant(ctx, tx, request.TenantID); err != nil {
		return empty, err
	}
	var sealed []byte
	var saved []byte
	err = tx.QueryRowContext(ctx, `SELECT issued_lease_ciphertext, execution_request_json FROM ril_action_cards WHERE tenant_id=$1 AND id=$2 AND execution_id=$3 AND execution_admission_digest=$4 AND status='executing' AND issued_lease_expires_at>clock_timestamp()`, request.TenantID, request.ActionCardID, request.ExecutionID, admission).Scan(&sealed, &saved)
	if err != nil {
		return empty, err
	}
	var persisted rilaction.Request
	if json.Unmarshal(saved, &persisted) != nil {
		return empty, fmt.Errorf("invalid admitted request")
	}
	expected, err := rilaction.ComputeRequestDigest(request)
	if err != nil {
		return empty, err
	}
	actual, err := rilaction.ComputeRequestDigest(persisted)
	if err != nil || actual != expected {
		return empty, fmt.Errorf("admitted request differs")
	}
	issued, err := actions.OpenRILIssuedLease(request.TenantID, request.ActionCardID, request.ExecutionID, request.Grant.BindingHash, sealed)
	if err != nil {
		return empty, err
	}
	sum := sha256.Sum256([]byte(issued.Lease))
	if "sha256:"+hex.EncodeToString(sum[:]) != request.Grant.BindingHash {
		return empty, fmt.Errorf("RIL issued lease mismatch")
	}
	return issued, nil
}
