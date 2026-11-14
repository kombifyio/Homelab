package guardreach

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"

	"github.com/kombifyio/techstack/internal/gocommon/servicecall"
)

// The decision ledger's StackKits store (kombify-Gateway #3129). Techstack is
// its only admitted filer: it files with its service identity, acting for the
// Owner, and reads the answer back with a fresh one-use token per read.
const (
	stackKitDecisionsPath     = "/v1/internal/stackkit-decisions"
	stackKitDecisionFileScope = "stackkit-decision:file"
	stackKitDecisionReadScope = "stackkit-decision:read"
)

var stackKitDecisionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{3,127}$`)

// StackKitDecisionFiling is one decision Techstack files: the operation id
// the ledger replays the same decision for, and the intent it signs.
type StackKitDecisionFiling struct {
	// OperationID is stable for one operator request, so filing it again
	// with the same intent returns the same decision.
	OperationID string
	// Intent is {operation, resource, risk, parameters} exactly as the
	// StackKits consumer recomputes it (pkg/auth/stackkitdecision).
	Intent map[string]any
}

// StackKitDecision is the ledger's answer to a filing or a read.
type StackKitDecision struct {
	// Status is the HTTP status of the read: 200 allowed, 202 pending.
	Status       int
	DecisionID   string          `json:"decisionId"`
	State        string          `json:"state"`
	IntentDigest string          `json:"intentDigest"`
	ExpiresAt    string          `json:"expiresAt"`
	Receipt      json.RawMessage `json:"receipt"`
	Key          json.RawMessage `json:"key"`
}

// FileStackKitDecision files one decision in the stackkit_operation store
// for the Owner: single use, default deny, the options allow_once and deny,
// expiring 900 seconds after filing.
func (g *Gateway) FileStackKitDecision(ctx context.Context, owner, tenant string, filing StackKitDecisionFiling) (StackKitDecision, error) {
	var decision StackKitDecision
	if g == nil || owner == "" || tenant == "" || !stackKitDecisionIDPattern.MatchString(filing.OperationID) || len(filing.Intent) == 0 {
		return decision, errors.New("StackKits decision filing input invalid")
	}
	body, err := json.Marshal(map[string]any{
		"store":         "stackkit_operation",
		"operationId":   filing.OperationID,
		"intent":        filing.Intent,
		"options":       []string{"allow_once", "deny"},
		"defaultOption": "deny",
		"singleUse":     true,
		"expiresIn":     900,
	})
	if err != nil {
		return decision, err
	}
	status, err := g.do(ctx, stackKitDecisionFileScope, http.MethodPost, stackKitDecisionsPath, body, &servicecall.OnBehalfOf{Sub: owner, OrgID: tenant}, &decision)
	if err != nil {
		return decision, err
	}
	decision.Status = status
	if !stackKitDecisionIDPattern.MatchString(decision.DecisionID) {
		return decision, errors.New("Gateway filed no readable StackKits decision")
	}
	return decision, nil
}

// ReadStackKitDecision reads one decision: 200 allowed with the signed
// receipt and its key, 202 still pending. A 403 (invalid or target mismatch)
// and a 410 (expired) come back as a *GatewayError with that status.
func (g *Gateway) ReadStackKitDecision(ctx context.Context, owner, tenant, decisionID string) (StackKitDecision, error) {
	var decision StackKitDecision
	if g == nil || owner == "" || tenant == "" || !stackKitDecisionIDPattern.MatchString(decisionID) {
		return decision, errors.New("StackKits decision read input invalid")
	}
	status, err := g.do(ctx, stackKitDecisionReadScope, http.MethodGet, stackKitDecisionsPath+"/"+url.PathEscape(decisionID), nil, &servicecall.OnBehalfOf{Sub: owner, OrgID: tenant}, &decision)
	if err != nil {
		return decision, err
	}
	decision.Status = status
	if status != http.StatusOK && status != http.StatusAccepted {
		return decision, errors.New("Gateway answered the StackKits decision read with an unexpected status")
	}
	return decision, nil
}
