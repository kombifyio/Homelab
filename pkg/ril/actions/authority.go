package actions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kombifyio/techstack/pkg/db/tenantcontext"
	"github.com/kombifyio/techstack/pkg/guardreach"
	jsoncanonicalizer "github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	"strings"
	"time"

	"github.com/kombifyio/techstack/pkg/ril/actioncontract"
	"github.com/kombifyio/techstack/pkg/ril/primitives"
	"github.com/google/uuid"
)

var (
	ErrCardNotFound               = errors.New("RIL action card not found")
	ErrCardConflict               = errors.New("RIL action card state conflict")
	ErrApprovalRequired           = errors.New("RIL action card approval required")
	ErrGrantRequired              = errors.New("RIL action card delegated grant required")
	ErrConnectorBindingRequired   = errors.New("RIL action card exact connector binding required")
	ErrConnectorGrantInsufficient = errors.New("RIL action card connector grant insufficient")
	ErrExecutionAdmission         = errors.New("RIL action execution admission rejected")
	ErrExecutionInProgress        = errors.New("RIL action card execution in progress")
)

type ActionTemplate struct {
	StackID          string                       `json:"stack_id"`
	Primitive        rilaction.PrimitiveBinding   `json:"primitive"`
	ResolvedPlanHash string                       `json:"resolved_plan_hash"`
	Grant            *rilaction.GrantBinding      `json:"grant,omitempty"`
	ConnectorBinding *ConnectorBindingExpectation `json:"connector_binding,omitempty"`
	Target           rilaction.TargetBinding      `json:"target"`
	Inputs           []rilaction.Input            `json:"inputs,omitempty"`
	EvidenceSinkRef  string                       `json:"evidence_sink_ref"`
}

type GovernedCard struct {
	ID                 string              `json:"id"`
	TenantID           string              `json:"-"`
	OwnerSubjectID     string              `json:"-"`
	ServerID           string              `json:"server_id"`
	Title              string              `json:"title"`
	Severity           string              `json:"severity"`
	Status             string              `json:"status"`
	Template           ActionTemplate      `json:"action"`
	Approval           *ApprovalRecord     `json:"approval,omitempty"`
	ExecutionID        string              `json:"execution_id,omitempty"`
	TraceID            string              `json:"trace_id,omitempty"`
	IdempotencyKey     string              `json:"-"`
	Evidence           *rilaction.Evidence `json:"evidence,omitempty"`
	ExecutionAdmission *ExecutionAdmission `json:"-"`
	ErrorCode          string              `json:"error_code,omitempty"`
	CreatedAt          time.Time           `json:"created_at"`
	UpdatedAt          time.Time           `json:"updated_at"`
	ApprovedAt         *time.Time          `json:"approved_at,omitempty"`
	DeniedAt           *time.Time          `json:"denied_at,omitempty"`
	ExecutionStartedAt *time.Time          `json:"execution_started_at,omitempty"`
	CompletedAt        *time.Time          `json:"completed_at,omitempty"`
}

type ApprovalRecord struct {
	ReceiptRef       string                  `json:"receipt_ref"`
	ReceiptHash      string                  `json:"receipt_hash"`
	ActorSubjectID   string                  `json:"actor_subject_id"`
	Class            rilaction.ApprovalClass `json:"class"`
	AuditCorrelation string                  `json:"audit_correlation_id"`
	ApprovedAt       time.Time               `json:"approved_at"`
	ValidUntil       time.Time               `json:"valid_until"`
}

type CreateGovernedCard struct {
	ID             string
	TenantID       string
	OwnerSubjectID string
	ServerID       string
	Title          string
	Severity       string
	Template       ActionTemplate
}

type BeginExecution struct {
	TenantID            string
	OwnerSubjectID      string
	CardID              string
	ExecutionID         string
	TraceID             string
	IdempotencyKey      string
	Now                 time.Time
	ConnectorProjection *ConnectorBindingProjection
}

type BeginDisposition string

const (
	BeginAcquired BeginDisposition = "acquired"
	BeginReplay   BeginDisposition = "replay"
)

type BeginResult struct {
	Disposition BeginDisposition
	Card        *GovernedCard
	Request     rilaction.Request
	Admission   ExecutionAdmission
}

type Authority interface {
	Create(context.Context, CreateGovernedCard) (*GovernedCard, error)
	Get(context.Context, string, string, string) (*GovernedCard, error)
	List(context.Context, string, string) ([]GovernedCard, error)
	Approve(context.Context, string, string, string, string, time.Time) (*GovernedCard, error)
	Deny(context.Context, string, string, string, string, time.Time) (*GovernedCard, error)
	Begin(context.Context, BeginExecution) (BeginResult, error)
	Complete(context.Context, string, string, rilaction.Evidence, string, time.Time) (*GovernedCard, error)
	Fail(context.Context, string, string, string, string, time.Time) (*GovernedCard, error)
}

type PostgresAuthority struct {
	db              *sql.DB
	executorBound   ExecutorBoundAdmission
	decisionGateway RILDecisionGateway
}

// ExecutorBoundTarget is one executor-bound card's binding as created: its
// server, stack, catalog profile, target and inputs.
type ExecutorBoundTarget struct {
	// CardID is the admitted card ("" while it is created).
	CardID         string
	TenantID       string
	OwnerSubjectID string
	ServerID       string
	Template       ActionTemplate
	Profile        primitives.Profile
}

// ExecutorBoundAdmission admits an executor-bound card when it is created and
// again when it begins. ContractHash returns the authenticated release
// capability hash of exactly this primitive on the stack's pinned StackKits
// release ("" when the release does not qualify it). Derive returns the
// server-derived target and inputs of a new card from fresh inventory: the
// caller names only the target scope and the inputs that select the target
// (the service key); every ref is derived, and a caller-supplied ref that
// differs refuses. Target validates the target and inputs against fresh
// inventory and refuses a protected, locked or maintained target.
type ExecutorBoundAdmission struct {
	ContractHash func(ctx context.Context, tenantID, stackID, primitiveID string) (string, error)
	Derive       func(ctx context.Context, target ExecutorBoundTarget) (rilaction.TargetBinding, []rilaction.Input, error)
	Target       func(ctx context.Context, target ExecutorBoundTarget) error
}

// ConfigureExecutorBoundAdmission installs the release and inventory authority
// of the executor-bound primitives.
func (s *PostgresAuthority) ConfigureExecutorBoundAdmission(admission ExecutorBoundAdmission) {
	s.executorBound = admission
}

// admitExecutorBound returns the primitive's authenticated contract hash after
// the target and inputs passed fresh inventory admission.
func (s *PostgresAuthority) admitExecutorBound(ctx context.Context, target ExecutorBoundTarget) (string, error) {
	if s.executorBound.ContractHash == nil || s.executorBound.Target == nil {
		return "", fmt.Errorf("%w: executor-bound admission unavailable", ErrExecutionAdmission)
	}
	if !target.Profile.Admits(target.Template.Target, target.Template.Inputs) {
		return "", fmt.Errorf("%w: target or inputs differ from the primitive profile", ErrExecutionAdmission)
	}
	hash, err := s.executorBound.ContractHash(ctx, target.TenantID, target.Template.StackID, target.Profile.ID)
	if err != nil || hash == "" {
		return "", fmt.Errorf("%w: pinned StackKits release lacks the authenticated primitive capability", ErrExecutionAdmission)
	}
	if err := s.executorBound.Target(ctx, target); err != nil {
		return "", fmt.Errorf("%w: %w", ErrExecutionAdmission, err)
	}
	return hash, nil
}

func (s *PostgresAuthority) ConfigureRILDecisionGateway(gateway RILDecisionGateway) {
	s.decisionGateway = gateway
}

func NewPostgresAuthority(db *sql.DB) *PostgresAuthority { return &PostgresAuthority{db: db} }

func (s *PostgresAuthority) Create(ctx context.Context, input CreateGovernedCard) (*GovernedCard, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("ril actions: database not configured")
	}
	if primitives.IsExecutorBound(input.Template.Primitive.ID) {
		profile, ok := primitives.For(input.Template.Primitive)
		if !ok {
			return nil, ErrExecutionAdmission
		}
		if s.executorBound.Derive == nil {
			return nil, fmt.Errorf("%w: executor-bound admission unavailable", ErrExecutionAdmission)
		}
		target, inputs, err := s.executorBound.Derive(ctx, ExecutorBoundTarget{
			TenantID: strings.TrimSpace(input.TenantID), OwnerSubjectID: strings.TrimSpace(input.OwnerSubjectID),
			ServerID: strings.TrimSpace(input.ServerID), Template: input.Template, Profile: profile,
		})
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrExecutionAdmission, err)
		}
		if !callerRefsMatchDerived(input.Template, target, inputs) {
			return nil, fmt.Errorf("%w: caller-supplied target or input differs from the server-derived one", ErrExecutionAdmission)
		}
		input.Template.Target, input.Template.Inputs = target, inputs
		hash, err := s.admitExecutorBound(ctx, ExecutorBoundTarget{
			TenantID: strings.TrimSpace(input.TenantID), OwnerSubjectID: strings.TrimSpace(input.OwnerSubjectID),
			ServerID: strings.TrimSpace(input.ServerID), Template: input.Template, Profile: profile,
		})
		if err != nil {
			return nil, err
		}
		if input.Template.Primitive.ContractHash != "" && input.Template.Primitive.ContractHash != hash {
			return nil, ErrExecutionAdmission
		}
		input.Template.Primitive.ContractHash = hash
	}
	var out *GovernedCard
	err := s.withTenant(ctx, input.TenantID, func(tx *sql.Tx) error {
		card, err := CreateInTx(ctx, tx, input)
		out = card
		return err
	})
	return out, err
}

// callerRefsMatchDerived reports whether every target field and input the
// caller named equals the server-derived one. The caller must name the
// derived scope; it may omit any ref, but never supply a different or an
// additional one.
func callerRefsMatchDerived(template ActionTemplate, target rilaction.TargetBinding, inputs []rilaction.Input) bool {
	caller := template.Target
	if caller.Scope != target.Scope {
		return false
	}
	for _, pair := range [][2]string{
		{caller.SiteRef, target.SiteRef}, {caller.NodeRef, target.NodeRef}, {caller.ModuleInstanceRef, target.ModuleInstanceRef},
		{caller.RuntimeInstanceRef, target.RuntimeInstanceRef}, {caller.ExecutionChannelRef, target.ExecutionChannelRef},
	} {
		if pair[0] != "" && pair[0] != pair[1] {
			return false
		}
	}
	seen := map[string]bool{}
	for _, supplied := range template.Inputs {
		if seen[supplied.ID] {
			return false
		}
		seen[supplied.ID] = true
		matched := false
		for _, derived := range inputs {
			if derived.ID == supplied.ID {
				matched = supplied.Type == derived.Type && supplied.OpaqueRef == derived.OpaqueRef &&
					supplied.Boolean == nil && supplied.Integer == nil && supplied.Enum == ""
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// ActiveServiceMutationCard returns an executing or verifying executor-bound
// card that mutates this service: a start-service, stop-service or
// restart-service card for the same service key, or an update-service,
// run-backup or host-primitive card anywhere on the stack (an update rewrites
// every service of the stack; a backup snapshots all of them; a host action
// changes the server under all of them, and two host actions never run side
// by side). An empty serviceKey (a stack-wide update or backup) matches every
// mutating service card of the stack. excludeCardID is the card being admitted itself. It is the
// action-card half of the managed service concurrency guard; the job half is
// FindUnresolvedServiceJob.
func (s *PostgresAuthority) ActiveServiceMutationCard(ctx context.Context, tenantID, stackID, serviceKey, excludeCardID string) (string, error) {
	if s == nil || s.db == nil {
		return "", fmt.Errorf("ril actions: database not configured")
	}
	serviceRef := ""
	if serviceKey != "" {
		serviceRef = primitives.Input(primitives.InputRuntimeInstanceRef, serviceKey).OpaqueRef
	}
	var id string
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			SELECT id FROM ril_action_cards
			WHERE tenant_id=$1 AND stack_id=$2 AND id<>$4 AND status IN ('executing','verifying')
			  AND (action_template_json->'primitive'->>'id' IN ('update-service','run-backup',`+hostPrimitiveList()+`)
			    OR (action_template_json->'primitive'->>'id' IN ('start-service','stop-service','restart-service')
			      AND ($3 = '' OR EXISTS (
			        SELECT 1 FROM jsonb_array_elements(COALESCE(action_template_json->'inputs','[]'::jsonb)) AS input
			        WHERE input->>'id' = 'runtime-instance-ref' AND input->>'opaque_ref' = $3))))
			ORDER BY created_at ASC LIMIT 1`, tenantID, stackID, serviceRef, excludeCardID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	return id, err
}

// CreateInTx inserts one governed card and its creation audit inside a caller
// transaction that already carries the card tenant's app.tenant_id scope. It
// lets another tenant-scoped authority (the RIL signal outbox) commit the card
// atomically with its own write.
func CreateInTx(ctx context.Context, tx *sql.Tx, input CreateGovernedCard) (*GovernedCard, error) {
	if tx == nil {
		return nil, fmt.Errorf("ril actions: transaction required")
	}
	input.ID = strings.TrimSpace(input.ID)
	if input.ID == "" {
		input.ID = uuid.NewString()
	}
	input.TenantID, input.OwnerSubjectID, input.ServerID = strings.TrimSpace(input.TenantID), strings.TrimSpace(input.OwnerSubjectID), strings.TrimSpace(input.ServerID)
	input.Title, input.Severity = strings.TrimSpace(input.Title), strings.TrimSpace(input.Severity)
	if input.TenantID == "" || input.OwnerSubjectID == "" || input.ServerID == "" || input.Title == "" || strings.TrimSpace(input.Template.StackID) == "" {
		return nil, fmt.Errorf("ril actions: tenant, owner, server, stack, and title required")
	}
	if input.Severity == "" {
		input.Severity = "info"
	}
	if err := validateConnectorBindingExpectation(input.Template.ConnectorBinding); err != nil {
		return nil, err
	}
	templateJSON, err := json.Marshal(input.Template)
	if err != nil {
		return nil, err
	}
	// An executor-bound card waits for its one Gateway Decision, which the
	// acting agent's equipment answers on Begin; it never carries a grant.
	status := "awaiting_approval"
	if primitives.IsExecutorBound(input.Template.Primitive.ID) {
		profile, ok := primitives.For(input.Template.Primitive)
		if !ok || input.Template.Grant != nil || !profile.Admits(input.Template.Target, input.Template.Inputs) {
			return nil, ErrExecutionAdmission
		}
	} else if input.Template.Grant == nil {
		status = "awaiting_grant"
	}
	card, err := scanGovernedCard(tx.QueryRowContext(ctx, `
		INSERT INTO ril_action_cards (
			id, tenant_id, owner_subject_id, server_id, stack_id, title,
			status, severity, action_json, decision_json, action_template_json
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'{}'::jsonb,'{}'::jsonb,$9::jsonb)
		RETURNING `+governedCardColumns,
		input.ID, input.TenantID, input.OwnerSubjectID, input.ServerID,
		input.Template.StackID, input.Title, status, input.Severity, templateJSON))
	if err != nil {
		return nil, err
	}
	if err := appendActionTransitionAudit(ctx, tx, actionTransitionAudit{
		TenantID: input.TenantID, CardID: input.ID, ToStatus: status,
		CorrelationID: newAuditCorrelation(""), ActorSubjectID: input.OwnerSubjectID,
		OccurredAt: card.CreatedAt,
	}); err != nil {
		return nil, err
	}
	return card, nil
}

func (s *PostgresAuthority) Get(ctx context.Context, tenantID, ownerID, cardID string) (*GovernedCard, error) {
	var out *GovernedCard
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		card, err := scanGovernedCard(tx.QueryRowContext(ctx, `SELECT `+governedCardColumns+` FROM ril_action_cards WHERE tenant_id=$1 AND owner_subject_id=$2 AND id=$3 AND card_kind='governed-action'`, tenantID, ownerID, cardID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCardNotFound
		}
		out = card
		return err
	})
	return out, err
}

func (s *PostgresAuthority) List(ctx context.Context, tenantID, ownerID string) ([]GovernedCard, error) {
	var out []GovernedCard
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+governedCardColumns+` FROM ril_action_cards WHERE tenant_id=$1 AND owner_subject_id=$2 AND card_kind='governed-action' ORDER BY created_at DESC`, tenantID, ownerID)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			card, err := scanGovernedCard(rows)
			if err != nil {
				return err
			}
			out = append(out, *card)
		}
		return rows.Err()
	})
	return out, err
}

func (s *PostgresAuthority) Approve(ctx context.Context, tenantID, ownerID, cardID, correlationID string, now time.Time) (*GovernedCard, error) {
	correlationID = strings.TrimSpace(correlationID)
	if correlationID == "" {
		correlationID = "audit-" + uuid.NewString()
	}
	receiptRef := "approval:" + cardID
	receiptHash := digestText(receiptRef + "\x00" + ownerID + "\x00" + correlationID + "\x00" + now.Format(time.RFC3339Nano))
	record := ApprovalRecord{ReceiptRef: receiptRef, ReceiptHash: receiptHash, ActorSubjectID: ownerID, Class: rilaction.ApprovalClassOwnerStepUp, AuditCorrelation: correlationID, ApprovedAt: now, ValidUntil: now.Add(rilaction.MaxAuthorityValidity)}
	data, _ := json.Marshal(record)
	return s.transition(ctx, tenantID, ownerID, cardID, `status='awaiting_approval' AND COALESCE(action_template_json->'primitive'->>'id','') NOT IN (`+executorBoundPrimitiveList()+`)`, "approved", `approval_json=$5::jsonb, approved_at=$6`, data, now)
}

// hostPrimitiveList is the SQL literal list of the host primitive ids, which
// serialize with every other mutating card of their stack.
func hostPrimitiveList() string {
	ids := []string{}
	for _, profile := range primitives.All() {
		if profile.IsHost() {
			ids = append(ids, "'"+profile.ID+"'")
		}
	}
	return strings.Join(ids, ",")
}

// executorBoundPrimitiveList is the SQL literal list of the closed
// executor-bound primitive ids; a person approval never admits one of them.
func executorBoundPrimitiveList() string {
	ids := make([]string, 0, len(primitives.All()))
	for _, profile := range primitives.All() {
		ids = append(ids, "'"+profile.ID+"'")
	}
	return strings.Join(ids, ",")
}

func (s *PostgresAuthority) Deny(ctx context.Context, tenantID, ownerID, cardID, correlationID string, now time.Time) (*GovernedCard, error) {
	correlationID = newAuditCorrelation(correlationID)
	data, _ := json.Marshal(map[string]any{"decision": "denied", "actor_subject_id": ownerID, "audit_correlation_id": correlationID, "denied_at": now})
	return s.transition(ctx, tenantID, ownerID, cardID, `status IN ('awaiting_grant','awaiting_approval')`, "denied", `approval_json=$5::jsonb, denied_at=$6`, data, now)
}

func (s *PostgresAuthority) transition(ctx context.Context, tenantID, ownerID, cardID, statusPredicate, to, set string, data []byte, now time.Time) (*GovernedCard, error) {
	var out *GovernedCard
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var from string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM ril_action_cards WHERE tenant_id=$1 AND owner_subject_id=$2 AND id=$3 FOR UPDATE`, tenantID, ownerID, cardID).Scan(&from); errors.Is(err, sql.ErrNoRows) {
			return ErrCardConflict
		} else if err != nil {
			return err
		}
		card, err := scanGovernedCard(tx.QueryRowContext(ctx, `UPDATE ril_action_cards SET status=$4, `+set+`, updated_at=now() WHERE tenant_id=$1 AND owner_subject_id=$2 AND id=$3 AND `+statusPredicate+` RETURNING `+governedCardColumns, tenantID, ownerID, cardID, to, data, now))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCardConflict
		}
		if err != nil {
			return err
		}
		correlationID := auditCorrelationFromDecision(data)
		if err := appendActionTransitionAudit(ctx, tx, actionTransitionAudit{
			TenantID: tenantID, CardID: cardID, FromStatus: from, ToStatus: to,
			CorrelationID: correlationID, ActorSubjectID: ownerID, OccurredAt: now,
		}); err != nil {
			return err
		}
		out = card
		return nil
	})
	return out, err
}

//nolint:gocyclo // Admission keeps decision, grant, replay, and transactional fencing in one fail-closed path.
func (s *PostgresAuthority) Begin(ctx context.Context, input BeginExecution) (BeginResult, error) {
	if input.Now.Location() != time.UTC {
		return BeginResult{}, fmt.Errorf("ril actions: UTC clock required")
	}
	var gatewayIssue guardreach.RILAuthorityResult
	var gatewayApproval *ApprovalRecord
	current, err := s.Get(ctx, input.TenantID, input.OwnerSubjectID, input.CardID)
	if err != nil {
		return BeginResult{}, err
	}
	currentProfile, currentExecutorBound := primitives.For(current.Template.Primitive)
	if primitives.IsExecutorBound(current.Template.Primitive.ID) && !currentExecutorBound {
		return BeginResult{}, ErrExecutionAdmission
	}
	if currentExecutorBound && current.Status == "awaiting_approval" {
		// The card's connector binding (when it has one) must carry this
		// primitive's grant scope before any Decision or lease is issued.
		if err := validateConnectorBinding(current, input); err != nil {
			return BeginResult{}, err
		}
		// Fresh inventory and the exact release capability are admitted again
		// before the card's one Decision is filed.
		hash, admitErr := s.admitExecutorBound(ctx, ExecutorBoundTarget{CardID: input.CardID, TenantID: input.TenantID, OwnerSubjectID: input.OwnerSubjectID, ServerID: current.ServerID, Template: current.Template, Profile: currentProfile})
		if admitErr != nil {
			return BeginResult{}, admitErr
		}
		if hash != current.Template.Primitive.ContractHash {
			return BeginResult{}, fmt.Errorf("%w: release capability or primitive changed", ErrExecutionAdmission)
		}
		gatewayIssue, gatewayApproval, err = prepareAndIssueDecision(ctx, s.decisionGateway, input, currentProfile)
		if err != nil {
			var denial *guardreach.GatewayError
			if errors.As(err, &denial) && denial.Status == 403 && denial.ReasonCode == "ril_decision_not_allowed" {
				return BeginResult{}, ErrApprovalRequired
			}
			if errors.As(err, &denial) && denial.Status == 409 {
				return BeginResult{}, ErrCardConflict
			}
			return BeginResult{}, fmt.Errorf("%w: gateway decision unavailable: %v", ErrExecutionAdmission, err)
		}
		input.Now = time.Now().UTC()
	}
	var out BeginResult
	err = s.withTenant(ctx, input.TenantID, func(tx *sql.Tx) error {
		card, err := scanGovernedCard(tx.QueryRowContext(ctx, `SELECT `+governedCardColumns+` FROM ril_action_cards WHERE tenant_id=$1 AND owner_subject_id=$2 AND id=$3 FOR UPDATE`, input.TenantID, input.OwnerSubjectID, input.CardID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCardNotFound
		}
		if err != nil {
			return err
		}
		profile, executorBound := primitives.For(card.Template.Primitive)
		// Only the configured Gateway can admit an executor-bound card. Never
		// fall back to a person approval on a changed or missing card.
		if executorBound && gatewayApproval == nil && card.Status != string(StatusCompleted) && card.Status != string(StatusFailed) && card.Status != "executing" && card.Status != "verifying" {
			return fmt.Errorf("%w: gateway RIL authority required", ErrExecutionAdmission)
		}
		if gatewayApproval != nil && (!executorBound || profile.ID != currentProfile.ID) {
			return ErrExecutionAdmission
		}
		if card.Status == string(StatusCompleted) || card.Status == string(StatusFailed) {
			if card.IdempotencyKey != input.IdempotencyKey || card.Evidence == nil {
				return ErrCardConflict
			}
			out = BeginResult{Disposition: BeginReplay, Card: card}
			return nil
		}
		if card.Status == "executing" || card.Status == "verifying" {
			if card.IdempotencyKey == input.IdempotencyKey && card.ExecutionID == input.ExecutionID && card.TraceID == input.TraceID {
				var requestJSON string
				if err := tx.QueryRowContext(ctx, `SELECT COALESCE(execution_request_json::text, '') FROM ril_action_cards WHERE tenant_id=$1 AND owner_subject_id=$2 AND id=$3`, input.TenantID, input.OwnerSubjectID, input.CardID).Scan(&requestJSON); err != nil {
					return err
				}
				var request rilaction.Request
				if requestJSON == "" || json.Unmarshal([]byte(requestJSON), &request) != nil || card.ExecutionAdmission == nil {
					return ErrCardConflict
				}
				if _, admitted := primitives.For(request.Primitive); admitted {
					// The frozen proposal's revision is advanced and its body
					// cleared on any template/owner/server change. Together
					// with the bound projection hash this identifies the
					// original admission before a workflow can be resumed.
					if card.Template.Primitive != request.Primitive {
						return ErrCardConflict
					}
					var raw string
					var revision int64
					if err := tx.QueryRowContext(ctx, `SELECT COALESCE(decision_proposal_json::text,''),decision_card_revision FROM ril_action_cards WHERE tenant_id=$1 AND owner_subject_id=$2 AND id=$3`, input.TenantID, input.OwnerSubjectID, input.CardID).Scan(&raw, &revision); err != nil {
						return err
					}
					var proposal struct {
						Effect struct {
							CardRevision int64 `json:"cardRevision"`
						} `json:"effect"`
					}
					if raw == "" || json.Unmarshal([]byte(raw), &proposal) != nil || revision < 1 || proposal.Effect.CardRevision != revision {
						return ErrCardConflict
					}
					if err := validateConnectorBinding(card, input); err != nil {
						return err
					}
				}
				out = BeginResult{Disposition: BeginAcquired, Card: card, Request: request, Admission: *card.ExecutionAdmission}
				return nil
			}
			return ErrCardConflict
		}
		statusBefore := card.Status
		var decisionProposal DecisionProposal
		if gatewayApproval != nil {
			if card.Status != "awaiting_approval" {
				return ErrApprovalRequired
			}
			var raw string
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(decision_proposal_json::text,'') FROM ril_action_cards WHERE tenant_id=$1 AND owner_subject_id=$2 AND id=$3`, input.TenantID, input.OwnerSubjectID, input.CardID).Scan(&raw); err != nil {
				return err
			}
			if json.Unmarshal([]byte(raw), &decisionProposal) != nil {
				return ErrExecutionAdmission
			}
			storedEffect, err := json.Marshal(decisionProposal.Effect)
			if err != nil {
				return err
			}
			left, err := jsoncanonicalizer.Transform(storedEffect)
			if err != nil {
				return err
			}
			right, err := jsoncanonicalizer.Transform(gatewayIssue.Effect)
			if err != nil || !bytes.Equal(left, right) {
				return ErrExecutionAdmission
			}
			card.Approval = gatewayApproval
			if card.Template.Grant != nil {
				return ErrExecutionAdmission
			}
			grant, err := gatewayLeaseGrant(gatewayIssue, profile)
			if err != nil {
				return ErrExecutionAdmission
			}
			card.Template.Grant = grant
		} else if card.Status != string(StatusApproved) || card.Approval == nil {
			return ErrApprovalRequired
		}
		if err := RefuseUnentitledStart(card, input); err != nil {
			return err
		}
		validUntil := grantValidUntil(card, input.Now)
		request := rilaction.Request{APIVersion: rilaction.APIVersionV1Alpha1, ActionCardID: card.ID, ExecutionID: input.ExecutionID, TraceID: input.TraceID, TenantID: input.TenantID, StackID: card.Template.StackID, Primitive: card.Template.Primitive, ResolvedPlanHash: card.Template.ResolvedPlanHash, Approval: rilaction.ApprovalBinding{ReceiptRef: card.Approval.ReceiptRef, ReceiptHash: card.Approval.ReceiptHash, Decision: string(StatusApproved), Class: card.Approval.Class, ApprovedAt: card.Approval.ApprovedAt.Format(time.RFC3339Nano), ValidUntil: card.Approval.ValidUntil.Format(time.RFC3339Nano)}, Grant: *card.Template.Grant, Target: card.Template.Target, Inputs: card.Template.Inputs, EvidenceSinkRef: card.Template.EvidenceSinkRef, IssuedAt: input.Now.Format(time.RFC3339Nano), ValidUntil: validUntil.Format(time.RFC3339Nano), Nonce: "nonce-" + uuid.NewString(), IdempotencyKey: input.IdempotencyKey}
		admission, admittedRequest, admissionErr := admitExecutionTx(ctx, tx, card, request, input.Now)
		if admissionErr != nil {
			return admissionErr
		}
		if gatewayApproval != nil && (admission.ServerRevision != decisionProposal.ServerRevision || admission.LifecycleRevision != decisionProposal.LifecycleRevision || admission.LifecycleRevision == 0) {
			return ErrExecutionAdmission
		}
		request = admittedRequest
		requestJSON, _ := json.Marshal(request)
		var sealed []byte
		if gatewayApproval != nil {
			sealed, err = SealRILIssuedLease(input.TenantID, input.CardID, input.ExecutionID, request.Grant.BindingHash, gatewayIssue)
			if err != nil {
				return err
			}
		}
		card, err = scanGovernedCard(tx.QueryRowContext(ctx, `UPDATE ril_action_cards SET status='executing', execution_request_json=$4::jsonb, execution_id=$5, trace_id=$6, idempotency_key=$7, execution_started_at=$8, admission_inventory_revision=$9, admission_server_revision=$10, admission_server_generation=$11, admission_lease_id=$12, admission_lease_revision=$13, admission_resource_generation_id=$14::uuid, execution_admission_digest=$15, updated_at=now() WHERE tenant_id=$1 AND owner_subject_id=$2 AND id=$3 AND status=$16 RETURNING `+governedCardColumns, input.TenantID, input.OwnerSubjectID, input.CardID, requestJSON, input.ExecutionID, input.TraceID, input.IdempotencyKey, input.Now, admission.InventoryRevision, admission.ServerRevision, admission.ServerGeneration, admission.LifecycleAuthorityID, admission.LifecycleRevision, admission.LifecycleGenerationID, admission.Digest, statusBefore))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCardConflict
		}
		if err != nil {
			return err
		}
		if gatewayApproval != nil {
			// The Gateway approval (receipt and Decision id) is persisted with
			// the execution so the typed step receipt carries the AI Platform
			// approval it ran under.
			approvalJSON, marshalErr := json.Marshal(gatewayApproval)
			if marshalErr != nil {
				return marshalErr
			}
			if _, err = tx.ExecContext(ctx, `UPDATE ril_action_cards SET issued_lease_ciphertext=$4,issued_lease_expires_at=$5,approval_json=$6::jsonb WHERE tenant_id=$1 AND id=$2 AND execution_id=$3`, input.TenantID, input.CardID, input.ExecutionID, sealed, request.ValidUntil, approvalJSON); err != nil {
				return err
			}
		}
		if err := appendActionTransitionAudit(ctx, tx, actionTransitionAudit{
			TenantID: input.TenantID, CardID: input.CardID, FromStatus: statusBefore,
			ToStatus: "executing", CorrelationID: newAuditCorrelation(input.TraceID),
			ActorSubjectID: input.OwnerSubjectID, ExecutionID: input.ExecutionID,
			TraceID: input.TraceID, OccurredAt: input.Now,
		}); err != nil {
			return err
		}
		card.ExecutionAdmission = &admission
		out = BeginResult{Disposition: BeginAcquired, Card: card, Request: request, Admission: admission}
		return nil
	})
	return out, err
}

func (s *PostgresAuthority) Complete(ctx context.Context, tenantID, cardID string, evidence rilaction.Evidence, errorCode string, now time.Time) (*GovernedCard, error) {
	data, err := json.Marshal(evidence)
	if err != nil {
		return nil, err
	}
	status := "completed"
	if evidence.Status == "failed" {
		status = "failed"
	}
	errorCode = strings.TrimSpace(errorCode)
	var out *GovernedCard
	err = s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var from, ownerID string
		if err := tx.QueryRowContext(ctx, `SELECT status, owner_subject_id FROM ril_action_cards WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenantID, cardID).Scan(&from, &ownerID); errors.Is(err, sql.ErrNoRows) {
			return ErrCardConflict
		} else if err != nil {
			return err
		}
		if from == "completed" || from == "failed" {
			card, scanErr := scanGovernedCard(tx.QueryRowContext(ctx, `SELECT `+governedCardColumns+` FROM ril_action_cards WHERE tenant_id=$1 AND id=$2`, tenantID, cardID))
			if scanErr != nil {
				return scanErr
			}
			persisted, _ := json.Marshal(card.Evidence)
			if card.ExecutionID != evidence.ExecutionID || string(persisted) != string(data) || card.ErrorCode != errorCode {
				return ErrCardConflict
			}
			out = card
			return nil
		}
		card, scanErr := scanGovernedCard(tx.QueryRowContext(ctx, `UPDATE ril_action_cards SET status=$3,evidence_json=$4::jsonb,error_code=NULLIF($5,''),completed_at=$6,issued_lease_ciphertext=NULL,issued_lease_expires_at=NULL,updated_at=now() WHERE tenant_id=$1 AND id=$2 AND status IN ('executing','verifying') AND execution_id=$7 RETURNING `+governedCardColumns, tenantID, cardID, status, data, errorCode, now, evidence.ExecutionID))
		if errors.Is(scanErr, sql.ErrNoRows) {
			return ErrCardConflict
		}
		if scanErr != nil {
			return scanErr
		}
		if err := appendActionTransitionAudit(ctx, tx, actionTransitionAudit{
			TenantID: tenantID, CardID: cardID, FromStatus: from, ToStatus: status,
			CorrelationID: newAuditCorrelation(evidence.TraceID), ActorSubjectID: ownerID,
			ExecutionID: evidence.ExecutionID, TraceID: evidence.TraceID, OccurredAt: now,
		}); err != nil {
			return err
		}
		out = card
		return nil
	})
	return out, err
}

// Fail terminalizes an admitted execution when no contract-valid public
// evidence exists (for example, transport failure before StackKits can return
// a result). It is idempotent for the same execution identity and records only
// a bounded public error code; raw transport or provider details never enter
// the action card.
func (s *PostgresAuthority) Fail(ctx context.Context, tenantID, cardID, executionID, errorCode string, now time.Time) (*GovernedCard, error) {
	errorCode = strings.TrimSpace(errorCode)
	switch errorCode {
	case "execution_unavailable", "stackkit_execution_failed", "workflow_execution_unavailable":
	default:
		errorCode = "execution_unavailable"
	}
	var out *GovernedCard
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var from, ownerID, persistedExecutionID, persistedErrorCode string
		if err := tx.QueryRowContext(ctx, `SELECT status, owner_subject_id, COALESCE(execution_id,''), COALESCE(error_code,'') FROM ril_action_cards WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenantID, cardID).Scan(&from, &ownerID, &persistedExecutionID, &persistedErrorCode); errors.Is(err, sql.ErrNoRows) {
			return ErrCardConflict
		} else if err != nil {
			return err
		}
		if from == "failed" {
			if persistedExecutionID != executionID || persistedErrorCode != errorCode {
				return ErrCardConflict
			}
			card, scanErr := scanGovernedCard(tx.QueryRowContext(ctx, `SELECT `+governedCardColumns+` FROM ril_action_cards WHERE tenant_id=$1 AND id=$2`, tenantID, cardID))
			if scanErr != nil {
				return scanErr
			}
			out = card
			return nil
		}
		card, scanErr := scanGovernedCard(tx.QueryRowContext(ctx, `UPDATE ril_action_cards SET status='failed',error_code=$4,completed_at=$5,issued_lease_ciphertext=NULL,issued_lease_expires_at=NULL,updated_at=now() WHERE tenant_id=$1 AND id=$2 AND status IN ('executing','verifying') AND execution_id=$3 RETURNING `+governedCardColumns, tenantID, cardID, executionID, errorCode, now))
		if errors.Is(scanErr, sql.ErrNoRows) {
			return ErrCardConflict
		}
		if scanErr != nil {
			return scanErr
		}
		if err := appendActionTransitionAudit(ctx, tx, actionTransitionAudit{
			TenantID: tenantID, CardID: cardID, FromStatus: from, ToStatus: "failed",
			CorrelationID: newAuditCorrelation(card.TraceID), ActorSubjectID: ownerID,
			ExecutionID: executionID, TraceID: card.TraceID, OccurredAt: now,
		}); err != nil {
			return err
		}
		out = card
		return nil
	})
	return out, err
}

const cardStatusBeforeExecution = "approved"

type actionTransitionAudit struct {
	TenantID, CardID, FromStatus, ToStatus, CorrelationID, ActorSubjectID string
	ExecutionID, TraceID                                                  string
	OccurredAt                                                            time.Time
}

func appendActionTransitionAudit(ctx context.Context, tx *sql.Tx, audit actionTransitionAudit) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO ril_action_transition_audit (
			tenant_id, action_card_id, from_status, to_status,
			audit_correlation_id, actor_subject_id, execution_id, trace_id, occurred_at
		) VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, NULLIF($7, ''), NULLIF($8, ''), $9)
	`, audit.TenantID, audit.CardID, audit.FromStatus, audit.ToStatus,
		audit.CorrelationID, audit.ActorSubjectID, audit.ExecutionID, audit.TraceID, audit.OccurredAt)
	return err
}

func newAuditCorrelation(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return "audit-" + uuid.NewString()
}

func auditCorrelationFromDecision(data []byte) string {
	var decision struct {
		AuditCorrelationID string `json:"audit_correlation_id"`
	}
	if json.Unmarshal(data, &decision) == nil {
		return newAuditCorrelation(decision.AuditCorrelationID)
	}
	return newAuditCorrelation("")
}

const governedCardColumns = `id,tenant_id,owner_subject_id,COALESCE(server_id,''),title,severity,status,action_template_json::text,COALESCE(approval_json::text,''),COALESCE(execution_id,''),COALESCE(idempotency_key,''),COALESCE(trace_id,''),COALESCE(evidence_json::text,''),COALESCE(error_code,''),created_at,updated_at,approved_at,denied_at,execution_started_at,completed_at,admission_inventory_revision,admission_server_revision,admission_server_generation,COALESCE(admission_lease_id,''),admission_lease_revision,COALESCE(admission_resource_generation_id::text,''),COALESCE(execution_admission_digest,'')`

type scanner interface{ Scan(...any) error }

func scanGovernedCard(row scanner) (*GovernedCard, error) {
	var c GovernedCard
	var templateJSON, approvalJSON, evidenceJSON, admissionLeaseID, admissionGenerationID, admissionDigest string
	var admissionInventoryRevision, admissionServerRevision, admissionServerGeneration, admissionLeaseRevision sql.NullInt64
	err := row.Scan(&c.ID, &c.TenantID, &c.OwnerSubjectID, &c.ServerID, &c.Title, &c.Severity, &c.Status, &templateJSON, &approvalJSON, &c.ExecutionID, &c.IdempotencyKey, &c.TraceID, &evidenceJSON, &c.ErrorCode, &c.CreatedAt, &c.UpdatedAt, &c.ApprovedAt, &c.DeniedAt, &c.ExecutionStartedAt, &c.CompletedAt, &admissionInventoryRevision, &admissionServerRevision, &admissionServerGeneration, &admissionLeaseID, &admissionLeaseRevision, &admissionGenerationID, &admissionDigest)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(templateJSON), &c.Template); err != nil {
		return nil, err
	}
	if approvalJSON != "" {
		c.Approval = &ApprovalRecord{}
		if err = json.Unmarshal([]byte(approvalJSON), c.Approval); err != nil {
			return nil, err
		}
	}
	if evidenceJSON != "" {
		c.Evidence = &rilaction.Evidence{}
		if err = json.Unmarshal([]byte(evidenceJSON), c.Evidence); err != nil {
			return nil, err
		}
	}
	if admissionInventoryRevision.Valid || admissionServerRevision.Valid || admissionServerGeneration.Valid || admissionLeaseID != "" || admissionLeaseRevision.Valid || admissionGenerationID != "" || admissionDigest != "" {
		if !admissionInventoryRevision.Valid || !admissionServerRevision.Valid || !admissionServerGeneration.Valid || !admissionLeaseRevision.Valid || admissionInventoryRevision.Int64 < 1 || admissionServerRevision.Int64 < 1 || admissionServerGeneration.Int64 < 1 || admissionLeaseRevision.Int64 < 1 || admissionLeaseID == "" || admissionGenerationID == "" || admissionDigest == "" {
			return nil, fmt.Errorf("ril actions: incomplete persisted execution admission")
		}
		c.ExecutionAdmission = &ExecutionAdmission{
			InventoryRevision: admissionInventoryRevision.Int64,
			ServerRevision:    admissionServerRevision.Int64, ServerGeneration: admissionServerGeneration.Int64,
			LifecycleAuthorityID: admissionLeaseID,
			LifecycleRevision:    admissionLeaseRevision.Int64, LifecycleGenerationID: admissionGenerationID,
			Digest: admissionDigest,
		}
	}
	return &c, nil
}
func (s *PostgresAuthority) withTenant(ctx context.Context, tenantID string, fn func(*sql.Tx) error) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("ril actions: database not configured")
	}
	tx, err := tenantcontext.Database(ctx, s.db).BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = tenantcontext.ApplyTenant(ctx, tx, strings.TrimSpace(tenantID)); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
func digestText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

var _ Authority = (*PostgresAuthority)(nil)
