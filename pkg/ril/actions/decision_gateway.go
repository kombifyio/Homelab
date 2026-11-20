package actions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	jsoncanonicalizer "github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	"github.com/kombifyio/homelab/pkg/guardreach"
	rilaction "github.com/kombifyio/homelab/pkg/ril/actioncontract"
	"github.com/kombifyio/homelab/pkg/ril/primitives"
)

type RILDecisionGateway interface {
	RILAuthority(context.Context, string, string, string, string, string, string, string) (guardreach.RILAuthorityResult, error)
}

// prepareAndIssueDecision files the card's one Gateway Decision (`prepare`,
// answered at once by the acting agent's equipment) and then obtains the
// signed lease (`issue`). It is called before database locks; the returned
// result is checked again against the frozen proposal and current heads
// inside Begin's transaction.
func prepareAndIssueDecision(ctx context.Context, gateway RILDecisionGateway, in BeginExecution, profile primitives.Profile) (guardreach.RILAuthorityResult, *ApprovalRecord, error) {
	var empty guardreach.RILAuthorityResult
	if gateway == nil {
		return empty, nil, errors.New("RIL Gateway authority unavailable")
	}
	prepared, err := gateway.RILAuthority(ctx, in.OwnerSubjectID, in.TenantID, "prepare", in.CardID, "", "", "")
	if err != nil {
		return empty, nil, err
	}
	if prepared.DecisionID == "" {
		return empty, nil, errors.New("RIL Gateway decision unavailable")
	}
	result, err := gateway.RILAuthority(ctx, in.OwnerSubjectID, in.TenantID, "issue", in.CardID, "", "", "")
	if err != nil {
		return empty, nil, err
	}
	if !result.Allowed {
		return empty, nil, errors.New("RIL Gateway authority denied")
	}
	approval, err := equipmentApproval(result, in, profile, prepared.DecisionID)
	if err != nil {
		return empty, nil, err
	}
	return result, approval, nil
}

// equipmentApproval accepts only the equipment receipt of this primitive (the
// Decision answered by `equipment:<grant scope>` at assurance L0 on the
// equipment surface, allowed once) and a lease that binds exactly this
// primitive and grant scope.
func equipmentApproval(result guardreach.RILAuthorityResult, in BeginExecution, profile primitives.Profile, decisionID string) (*ApprovalRecord, error) {
	var receipt struct {
		ReceiptID  string    `json:"receiptId"`
		DecisionID string    `json:"decisionId"`
		IssuedAt   time.Time `json:"issuedAt"`
		Assurance  string    `json:"assurance"`
		Outcome    string    `json:"outcome"`
		OptionKind string    `json:"optionKind"`
		Surface    string    `json:"surface"`
		AnsweredBy struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"answeredBy"`
		Lifetime struct {
			Scope string `json:"scope"`
		} `json:"lifetime"`
		Grant json.RawMessage `json:"grant"`
	}
	if json.Unmarshal(result.Receipt, &receipt) != nil || receipt.ReceiptID == "" || receipt.DecisionID == "" || receipt.DecisionID != decisionID ||
		receipt.Assurance != "L0" || receipt.Outcome != "allowed" || receipt.OptionKind != "allow_once" || receipt.Surface != "equipment" ||
		receipt.AnsweredBy.Kind != "policy" || receipt.AnsweredBy.ID != profile.EquipmentPolicyID() || receipt.Lifetime.Scope != "once" ||
		(len(receipt.Grant) != 0 && !bytes.Equal(receipt.Grant, []byte("null"))) {
		return nil, errors.New("canonical RIL equipment receipt invalid")
	}
	parts := strings.Split(result.Lease, ".")
	if len(parts) != 3 {
		return nil, errors.New("RIL lease invalid")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var lease struct {
		DecisionID   string `json:"decisionId"`
		LeaseID      string `json:"leaseId"`
		TenantID     string `json:"tenantId"`
		OwnerSubject string `json:"ownerSubject"`
		CardID       string `json:"cardId"`
		Issuer       string `json:"iss"`
		Audience     string `json:"aud"`
		Purpose      string `json:"purpose"`
		Executor     string `json:"executor"`
		PrimitiveID  string `json:"primitiveId"`
		Scope        string `json:"scope"`
		IssuedAt     int64  `json:"iat"`
		Exp          int64  `json:"exp"`
	}
	if json.Unmarshal(payload, &lease) != nil || lease.DecisionID != receipt.DecisionID || lease.LeaseID == "" || lease.TenantID != in.TenantID || lease.OwnerSubject != in.OwnerSubjectID || lease.CardID != in.CardID || lease.Issuer != "kombify-gateway" || lease.Audience != "stackkits" || lease.Purpose != "ril-execution" || lease.Executor != "homelab" || lease.PrimitiveID != profile.ID || lease.Scope != profile.GrantScope || lease.IssuedAt <= 0 || lease.IssuedAt > time.Now().UTC().Unix() || lease.Exp <= in.Now.Unix() || lease.Exp-lease.IssuedAt > 900 {
		return nil, errors.New("RIL lease binding invalid")
	}
	canonical, err := jsoncanonicalizer.Transform(result.Receipt)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canonical)
	return &ApprovalRecord{ReceiptRef: "approval:" + receipt.ReceiptID, ReceiptHash: "sha256:" + hex.EncodeToString(sum[:]), ActorSubjectID: receipt.AnsweredBy.ID, Class: rilaction.ApprovalClassAgentEquipment, AuditCorrelation: receipt.DecisionID, ApprovedAt: receipt.IssuedAt.UTC(), ValidUntil: time.Unix(lease.Exp, 0).UTC()}, nil
}

// gatewayLeaseGrant is an opaque, non-bearer reference to the exact issued
// lease; the sealed lease itself remains in private database custody.
func gatewayLeaseGrant(result guardreach.RILAuthorityResult, profile primitives.Profile) (*rilaction.GrantBinding, error) {
	parts := strings.Split(result.Lease, ".")
	if len(parts) != 3 {
		return nil, errors.New("RIL lease invalid")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var l struct {
		LeaseID string `json:"leaseId"`
		Iat     int64  `json:"iat"`
		Exp     int64  `json:"exp"`
	}
	if json.Unmarshal(raw, &l) != nil || l.LeaseID == "" || l.Iat <= 0 || l.Exp <= l.Iat || l.Exp-l.Iat > 900 {
		return nil, errors.New("RIL lease binding invalid")
	}
	h := sha256.Sum256([]byte(result.Lease))
	return &rilaction.GrantBinding{BindingRef: "grant:" + l.LeaseID, BindingHash: "sha256:" + hex.EncodeToString(h[:]), Audience: "stackkits", Scopes: []string{profile.GrantScope}, GrantedAt: time.Unix(l.Iat, 0).UTC().Format(time.RFC3339Nano), ValidUntil: time.Unix(l.Exp, 0).UTC().Format(time.RFC3339Nano)}, nil
}
