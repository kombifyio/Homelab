package actions

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	jsoncanonicalizer "github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	"github.com/google/uuid"
	"github.com/kombifyio/homelab/pkg/ril/primitives"
)

type DecisionRequester struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}
type DecisionProposal struct {
	OperationID        string            `json:"operationId"`
	Requester          DecisionRequester `json:"requester"`
	Effect             map[string]any    `json:"effect"`
	EffectDigest       string            `json:"effectDigest"`
	LifecycleRevision  int64             `json:"lifecycleRevision"`
	LifecycleExpiresAt int64             `json:"lifecycleExpiresAt"`
	ServerRevision     int64             `json:"serverRevision"`
}

// PrepareDecision freezes a server-derived effect and the current lifecycle
// authority heads (LifecycleAuthority) before the card's one Gateway Decision.
// An owner-operated server's lifecycle window is bounded, not a lease, so a
// re-prepared proposal of an unchanged server and generation carries the
// fresh bound; a VM lease's window is the lease's and stays frozen.
func (s *PostgresAuthority) PrepareDecision(ctx context.Context, tenant, owner, cardID string, requester DecisionRequester) (*DecisionProposal, error) {
	if tenant == "" || owner == "" || cardID == "" || requester.ID == "" || (requester.Kind != "service" && requester.Kind != "agent_instance") {
		return nil, ErrExecutionAdmission
	}
	var result *DecisionProposal
	err := s.withTenant(ctx, tenant, func(tx *sql.Tx) error {
		card, err := scanGovernedCard(tx.QueryRowContext(ctx, `SELECT `+governedCardColumns+` FROM ril_action_cards WHERE tenant_id=$1 AND owner_subject_id=$2 AND id=$3 AND card_kind='governed-action' FOR UPDATE`, tenant, owner, cardID))
		if err != nil {
			return err
		}
		profile, ok := primitives.For(card.Template.Primitive)
		if card.Status != "awaiting_approval" || !ok || !profile.Admits(card.Template.Target, card.Template.Inputs) {
			return ErrExecutionAdmission
		}
		var revision int64
		var stored string
		if err = tx.QueryRowContext(ctx, `SELECT decision_card_revision, COALESCE(decision_proposal_json::text,'') FROM ril_action_cards WHERE tenant_id=$1 AND owner_subject_id=$2 AND id=$3`, tenant, owner, cardID).Scan(&revision, &stored); err != nil {
			return err
		}
		authority, err := LockLifecycleAuthorityTx(ctx, tx, tenant, owner, card.ServerID, card.Template.StackID, time.Now().UTC())
		if err != nil || revision < 1 {
			return fmt.Errorf("%w: current lifecycle unavailable", ErrExecutionAdmission)
		}
		expires := authority.ValidUntil.Unix()
		if stored != "" {
			var existing DecisionProposal
			if json.Unmarshal([]byte(stored), &existing) != nil || existing.Requester != requester || existing.LifecycleRevision != authority.Revision || existing.ServerRevision != authority.ServerRevision || existing.LifecycleExpiresAt > expires {
				return ErrCardConflict
			}
			if authority.Kind == LifecycleAuthorityOwnerServer && existing.LifecycleExpiresAt != expires {
				existing.LifecycleExpiresAt = expires
				raw, err := json.Marshal(existing)
				if err != nil {
					return err
				}
				if _, err = tx.ExecContext(ctx, `UPDATE ril_action_cards SET decision_proposal_json=$4::jsonb WHERE tenant_id=$1 AND owner_subject_id=$2 AND id=$3 AND status='awaiting_approval'`, tenant, owner, cardID, raw); err != nil {
					return err
				}
			}
			result = &existing
			return nil
		}
		targetRaw, err := json.Marshal(card.Template.Target)
		if err != nil {
			return err
		}
		var source map[string]any
		if err = json.Unmarshal(targetRaw, &source); err != nil {
			return err
		}
		target := map[string]any{}
		for from, to := range map[string]string{"scope": "scope", "site_ref": "siteRef", "node_ref": "nodeRef", "module_instance_ref": "moduleInstanceRef", "runtime_instance_ref": "runtimeInstanceRef", "execution_channel_ref": "executionChannelRef"} {
			if v, ok := source[from]; ok {
				target[to] = v
			}
		}
		// Inputs are exactly the profile's sorted, required opaque references.
		inputs := make([]any, 0, len(card.Template.Inputs))
		for _, input := range card.Template.Inputs {
			inputs = append(inputs, map[string]any{"id": input.ID, "type": string(input.Type), "opaqueRef": input.OpaqueRef})
		}
		op := "op-" + strings.ReplaceAll(uuid.NewString(), "-", "")
		effect := map[string]any{"schemaVersion": "kombify.ril-effect.v1", "operationId": op, "audience": "stackkits", "tenantId": tenant, "ownerSubject": owner, "cardId": card.ID, "cardRevision": revision, "serverId": card.ServerID, "stackId": card.Template.StackID, "primitive": map[string]any{"id": card.Template.Primitive.ID, "contractHash": card.Template.Primitive.ContractHash, "operationClass": card.Template.Primitive.OperationClass}, "resolvedPlanHash": card.Template.ResolvedPlanHash, "target": target, "inputs": inputs}
		raw, err := json.Marshal(effect)
		if err != nil {
			return err
		}
		canonical, err := jsoncanonicalizer.Transform(raw)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(append(append([]byte("kombify.ril-effect.v1\n"), canonical...), '\n'))
		proposal := DecisionProposal{OperationID: op, Requester: requester, Effect: effect, EffectDigest: "sha256:" + hex.EncodeToString(digest[:]), LifecycleRevision: authority.Revision, LifecycleExpiresAt: expires, ServerRevision: authority.ServerRevision}
		raw, err = json.Marshal(proposal)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE ril_action_cards SET decision_proposal_json=$4::jsonb WHERE tenant_id=$1 AND owner_subject_id=$2 AND id=$3 AND decision_proposal_json IS NULL`, tenant, owner, cardID, raw)
		if err != nil {
			return err
		}
		result = &proposal
		return nil
	})
	return result, err
}
