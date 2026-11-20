package actions

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	rilaction "github.com/kombifyio/homelab/pkg/ril/actioncontract"
)

// StepReceiptReader reads the typed step receipt of one terminal card.
type StepReceiptReader interface {
	StepReceipt(ctx context.Context, tenantID, ownerID, cardID string) (*rilaction.StepReceipt, error)
}

// StepReceipt projects the persisted card, its approval and evidence, and the
// append-only transition audit into the typed step receipt. A card that has
// not reached a terminal state with evidence is a conflict; a card without a
// persisted approval (a card executed before the approval was persisted on
// Begin) has no receipt.
func (s *PostgresAuthority) StepReceipt(ctx context.Context, tenantID, ownerID, cardID string) (*rilaction.StepReceipt, error) {
	tenantID, ownerID, cardID = strings.TrimSpace(tenantID), strings.TrimSpace(ownerID), strings.TrimSpace(cardID)
	var out *rilaction.StepReceipt
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		card, err := scanGovernedCard(tx.QueryRowContext(ctx, `SELECT `+governedCardColumns+` FROM ril_action_cards WHERE tenant_id=$1 AND owner_subject_id=$2 AND id=$3`, tenantID, ownerID, cardID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCardNotFound
		}
		if err != nil {
			return err
		}
		if (card.Status != string(StatusCompleted) && card.Status != string(StatusFailed)) || card.Evidence == nil || card.Approval == nil {
			return ErrCardConflict
		}
		var executions, completions int64
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FILTER (WHERE to_status = 'executing'),
			       COUNT(*) FILTER (WHERE to_status IN ('completed','failed'))
			FROM ril_action_transition_audit
			WHERE tenant_id = $1 AND action_card_id = $2
		`, tenantID, cardID).Scan(&executions, &completions); err != nil {
			return err
		}
		receipt, err := rilaction.NewStepReceipt(rilaction.StepReceiptInput{
			ActionCardID: card.ID, ExecutionID: card.ExecutionID, TraceID: card.TraceID,
			Approval: rilaction.StepReceiptApproval{
				ReceiptRef: card.Approval.ReceiptRef, ReceiptHash: card.Approval.ReceiptHash,
				DecisionID: card.Approval.AuditCorrelation, Class: card.Approval.Class,
				ApprovedAt: card.Approval.ApprovedAt.UTC().Format(time.RFC3339Nano),
			},
			Evidence: *card.Evidence, Executions: executions, Completions: completions,
			StartedAt: formatReceiptTime(card.ExecutionStartedAt), CompletedAt: formatReceiptTime(card.CompletedAt),
		})
		if err != nil {
			return ErrCardConflict
		}
		out = &receipt
		return nil
	})
	return out, err
}

func formatReceiptTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

var _ StepReceiptReader = (*PostgresAuthority)(nil)
