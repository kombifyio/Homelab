package jobs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kombifyio/homelab/internal/stackkitrelease"
	"github.com/kombifyio/homelab/pkg/auth/stackkitdecision"
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/guardreach"
	"github.com/kombifyio/homelab/pkg/stackkitcommand"
)

// The producer of the platform's signed decision receipt (platform-8f0n2.20)
// for the destructive operations of the first slice this route dispatches:
// backup_restore_activate (ledger backup_restore) and advanced_change_set
// (ledger advanced_change_set_apply). Behind the feature flag
// stackkit_decision_receipt the route marks such a request with the acting
// principal it authenticated; the job then resolves the binding on the node,
// files the decision with the ledger, waits for the person's answer, checks
// the receipt and hands it to the node command. Nothing is dispatched to the
// node's destructive step before a checked allowed receipt for exactly this
// intent is in hand; a deny, an expiry or any refusal executes nothing.

// DecisionResultField is the job result member that shows the decision: its
// id, its state (pending, allowed, denied, expired, refused) and the reason.
const DecisionResultField = "decision_receipt"

// decisionPollInterval is how often the job reads a pending decision. Each
// read carries a fresh one-use token.
const decisionPollInterval = 5 * time.Second

// StackKitDecisionLedger is the decision ledger client (guardreach.Gateway,
// the same service identity and key the RIL decisions use).
type StackKitDecisionLedger interface {
	FileStackKitDecision(ctx context.Context, owner, tenant string, filing guardreach.StackKitDecisionFiling) (guardreach.StackKitDecision, error)
	ReadStackKitDecision(ctx context.Context, owner, tenant, decisionID string) (guardreach.StackKitDecision, error)
}

// StackKitDecisionServers resolves the server an agent runs: the node's
// enrolled serverId every target reference carries.
type StackKitDecisionServers interface {
	ListServerRuntimesByTenant(ctx context.Context, tenantID, stackID string) ([]controlplane.ServerRuntime, error)
}

// DecisionReceiptRefusedError is a decision that executes nothing.
type DecisionReceiptRefusedError struct {
	// State is denied, expired or refused.
	State string
	// ReasonCode is the platform's refusal name or the producer's own reason.
	ReasonCode string
	DecisionID string
	Message    string
}

func (e *DecisionReceiptRefusedError) Error() string {
	return fmt.Sprintf("the decision receipt for this operation is %s (%s): %s; nothing was executed", e.State, e.ReasonCode, e.Message)
}

func decisionRefused(state, reason, decisionID, format string, args ...any) *DecisionReceiptRefusedError {
	return &DecisionReceiptRefusedError{State: state, ReasonCode: reason, DecisionID: decisionID, Message: fmt.Sprintf(format, args...)}
}

// decisionReceiptRequired reports whether this request runs on the receipt
// path: the route set the acting principal only with the flag on and only for
// a first-slice operation.
func decisionReceiptRequired(req StackKitLifecycleRequest) bool {
	return strings.TrimSpace(req.DecisionPrincipal) != ""
}

// parseDecisionPrincipal reads the <kind>:<id> principal the route filed.
func parseDecisionPrincipal(value string) (stackkitdecision.Principal, error) {
	kind, id, found := strings.Cut(strings.TrimSpace(value), ":")
	principal := stackkitdecision.Principal{Kind: kind, ID: id}
	if !found {
		return principal, errors.New("the acting principal is invalid")
	}
	return principal, principal.Validate()
}

// decisionServerID is the server the request's agent runs, from the control
// plane's own server record (the hosted enrollment pins the same id on the
// node). For a person principal it also checks that the person is that
// server's Owner, which the node checks again against its enrolled ownerId.
func decisionServerID(ctx context.Context, servers StackKitDecisionServers, req StackKitLifecycleRequest, principal stackkitdecision.Principal) (string, error) {
	if servers == nil {
		return "", decisionRefused("refused", "stackkit_decision_server_unavailable", "", "the server registry is unavailable")
	}
	rows, err := servers.ListServerRuntimesByTenant(ctx, req.TenantID, req.StackID)
	if err != nil {
		return "", decisionRefused("refused", "stackkit_decision_server_unavailable", "", "the server registry could not be read")
	}
	match := ""
	for i := range rows {
		if strings.TrimSpace(rows[i].WorkerID) != req.AgentID {
			continue
		}
		if match != "" {
			return "", decisionRefused("refused", "stackkit_decision_server_ambiguous", "", "more than one server names this agent")
		}
		match = strings.TrimSpace(rows[i].ID)
		if principal.Kind == stackkitdecision.PrincipalPerson && strings.TrimSpace(rows[i].OwnerSubjectID) != "" && strings.TrimSpace(rows[i].OwnerSubjectID) != principal.ID {
			return "", decisionRefused("refused", stackkitdecision.RefusalTargetMismatch, "", "the confirming person is not this server's Owner")
		}
	}
	if match == "" {
		return "", decisionRefused("refused", "stackkit_decision_server_unavailable", "", "no server of this stack runs the addressed agent")
	}
	return match, nil
}

// obtainStackKitDecisionReceipt files the decision for exactly these facts
// and returns the checked receipt once the person allowed it. The ledger
// replays the same decision for the same operation id and intent, so a
// retried job continues the same decision instead of filing a second one.
func obtainStackKitDecisionReceipt(ctx context.Context, cfg StackKitLifecycleConfig, track decisionTracker, req StackKitLifecycleRequest, facts stackkitdecision.Facts) (*stackkitdecision.Admitted, error) {
	if err := facts.Validate(); err != nil {
		return nil, decisionRefused("refused", "stackkit_decision_binding_unavailable", "", "%v", err)
	}
	digest, err := stackkitdecision.IntentDigest(facts)
	if err != nil {
		return nil, decisionRefused("refused", "stackkit_decision_binding_unavailable", "", "%v", err)
	}
	ledger := cfg.decisionLedger()
	if ledger == nil {
		return nil, decisionRefused("refused", "stackkit_decision_ledger_unavailable", "", "the decision ledger is not configured")
	}
	// The wait ends before whatever the step needs besides the receipt
	// lapses (the Advanced capability): a receipt that arrives after that
	// could never be used, so the person is not asked when the wait cannot
	// fit.
	deadline := time.Now().Add(stackkitdecision.MaxLifetime + time.Minute)
	if !track.notAfter.IsZero() {
		if bound := track.notAfter.Add(-stackkitdecision.DispatchMargin); bound.Before(deadline) {
			deadline = bound
		}
		if time.Until(deadline) < minimumDecisionWait {
			return nil, decisionRefused("refused", "stackkit_decision_window_too_short", "",
				"the operation's authority expires before the Owner could answer; start the operation again")
		}
	}
	operationID := "homelab-" + track.jobID + "-" + facts.Operation
	filed, err := ledger.FileStackKitDecision(ctx, req.OwnerID, req.TenantID, guardreach.StackKitDecisionFiling{OperationID: operationID, Intent: facts.Intent()})
	if err != nil {
		return nil, decisionLedgerRefusal(err, "")
	}
	if filed.IntentDigest != "" && filed.IntentDigest != digest {
		return nil, decisionRefused("refused", stackkitdecision.RefusalTargetMismatch, filed.DecisionID, "the ledger filed another intent than the one requested")
	}
	track.record(filed.DecisionID, "pending", "", facts, digest)
	track.progress("Waiting for the Owner's confirmation in the kombify Companion or Workbench (decision " + filed.DecisionID + ")")
	for {
		read, readErr := ledger.ReadStackKitDecision(ctx, req.OwnerID, req.TenantID, filed.DecisionID)
		switch {
		case readErr != nil:
			refusal := decisionLedgerRefusal(readErr, filed.DecisionID)
			if refusal.State != "unavailable" {
				track.record(filed.DecisionID, refusal.State, refusal.ReasonCode, facts, digest)
				return nil, refusal
			}
		case read.Status == http.StatusOK:
			admitted, checkErr := stackkitdecision.Check(read.Receipt, filed.DecisionID, digest, time.Now())
			if checkErr != nil {
				var named *stackkitdecision.Refusal
				reason := stackkitdecision.RefusalInvalid
				if errors.As(checkErr, &named) {
					reason = named.Name
				}
				state := "refused"
				if reason == stackkitdecision.RefusalExpired {
					state = "expired"
				}
				track.record(filed.DecisionID, state, reason, facts, digest)
				return nil, decisionRefused(state, reason, filed.DecisionID, "%v", checkErr)
			}
			track.record(filed.DecisionID, "allowed", "", facts, digest)
			return admitted, nil
		}
		if !time.Now().Before(deadline) {
			track.record(filed.DecisionID, "expired", stackkitdecision.RefusalExpired, facts, digest)
			return nil, decisionRefused("expired", stackkitdecision.RefusalExpired, filed.DecisionID, "the Owner did not answer before the decision or the operation's authority expired")
		}
		timer := time.NewTimer(decisionPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, decisionRefused("refused", "stackkit_decision_wait_cancelled", filed.DecisionID, "the job stopped while it waited for the Owner")
		case <-timer.C:
		}
	}
}

// decisionLedgerRefusal maps a ledger answer to the decision's state: 403 is
// a deny or an invalid filing, 410 an expiry; a transport failure or a 5xx is
// "unavailable" and the wait goes on until the decision's deadline.
func decisionLedgerRefusal(err error, decisionID string) *DecisionReceiptRefusedError {
	var denial *guardreach.GatewayError
	if errors.As(err, &denial) {
		switch {
		case denial.Status == http.StatusGone:
			return decisionRefused("expired", stackkitdecision.RefusalExpired, decisionID, "the decision expired")
		case denial.Status == http.StatusForbidden && strings.Contains(denial.ReasonCode, "denied"):
			return decisionRefused("denied", denial.ReasonCode, decisionID, "the Owner denied this operation")
		case denial.Status == http.StatusForbidden:
			return decisionRefused("refused", firstNonEmpty(denial.ReasonCode, stackkitdecision.RefusalInvalid), decisionID, "the ledger refused the decision")
		case denial.Status < 500 && denial.Status != http.StatusTooManyRequests:
			return decisionRefused("refused", firstNonEmpty(denial.ReasonCode, "stackkit_decision_ledger_refused"), decisionID, "the ledger refused the request")
		}
	}
	if decisionID == "" {
		return decisionRefused("refused", "stackkit_decision_ledger_unavailable", "", "the decision could not be filed")
	}
	return decisionRefused("unavailable", "stackkit_decision_ledger_unavailable", decisionID, "the decision could not be read")
}

// decisionTracker is where one decision's state is shown: the job id the
// ledger operation id derives from, the job result entry and the progress
// line the console already renders.
type decisionTracker struct {
	jobID   string
	put     func(entry map[string]interface{})
	message func(string)
	// notAfter, when set, is when the step's other authority lapses (the
	// Advanced capability's expiry); the wait ends DispatchMargin before it.
	notAfter time.Time
}

// minimumDecisionWait is the least time a person is given to answer; a wait
// that cannot be this long is refused before the decision is filed.
const minimumDecisionWait = 2 * time.Minute

func jobDecisionTracker(job *Job, q *Queue) decisionTracker {
	return decisionTracker{
		jobID: job.ID,
		put: func(entry map[string]interface{}) {
			job.mutateResult(func(result map[string]interface{}) { result[DecisionResultField] = entry })
		},
		message: func(message string) {
			if q != nil {
				q.UpdateProgress(job.ID, 20, message)
			}
		},
	}
}

func (t decisionTracker) record(decisionID, state, reason string, facts stackkitdecision.Facts, digest string) {
	if t.put == nil {
		return
	}
	entry := map[string]interface{}{
		"decision_id": decisionID, "state": state, "operation": facts.Operation,
		"resource": facts.Resource, "intent_digest": digest, "acting_principal": facts.Principal.Flag(),
	}
	if reason != "" {
		entry["reason_code"] = reason
	}
	t.put(entry)
}

func (t decisionTracker) progress(message string) {
	if t.message != nil {
		t.message(message)
	}
}

// restoreActivationDecisionFacts is the binding of one restore activation as
// the node recomputes it (StackKits docs/DECISION-RECEIPTS.md, backup_restore):
// target stackkit:<server>:restore:<result id>, the canonical arguments with
// the durable activation operation id and no workload or Marketplace service
// (the Agent passes neither), the admitted node plan hash and the workspace
// revision the same plan admission reported.
func restoreActivationDecisionFacts(serverID string, req StackKitLifecycleRequest, planHash, revision string, principal stackkitdecision.Principal) (stackkitdecision.Facts, error) {
	resource, err := stackkitdecision.Reference(serverID, stackkitdecision.TargetRestore, req.RestoreResultID)
	if err != nil {
		return stackkitdecision.Facts{}, err
	}
	arguments, err := stackkitdecision.ArgumentsDigest(stackkitdecision.BackupRestoreArguments(req.RestoreResultID, "", "", req.ActivationOperationID))
	if err != nil {
		return stackkitdecision.Facts{}, err
	}
	plan, err := stackkitdecision.PlanDigest(planHash)
	if err != nil {
		return stackkitdecision.Facts{}, err
	}
	return stackkitdecision.Facts{
		Operation: stackkitdecision.OperationBackupRestore, Resource: resource, ArgumentsSHA256: arguments,
		PlanSHA256: plan, Revision: revision, Principal: principal,
	}, nil
}

// changeSetApplyDecisionFacts is the binding of one change-set apply
// (advanced_change_set_apply): target stackkit:<server>:change-set:<id>, the
// arguments {change_set_id}, the stored change-set digest the apply pins with
// --expect-sha256 as its plan, and the workspace revision the create reported.
func changeSetApplyDecisionFacts(serverID, changeSetID, changeSetSHA256, revision string, principal stackkitdecision.Principal) (stackkitdecision.Facts, error) {
	resource, err := stackkitdecision.Reference(serverID, stackkitdecision.TargetChangeSet, changeSetID)
	if err != nil {
		return stackkitdecision.Facts{}, err
	}
	arguments, err := stackkitdecision.ArgumentsDigest(stackkitdecision.AdvancedChangeSetApplyArguments(changeSetID))
	if err != nil {
		return stackkitdecision.Facts{}, err
	}
	plan, err := stackkitdecision.PlanDigest(changeSetSHA256)
	if err != nil {
		return stackkitdecision.Facts{}, err
	}
	return stackkitdecision.Facts{
		Operation: stackkitdecision.OperationAdvancedChangeSetApply, Resource: resource, ArgumentsSHA256: arguments,
		PlanSHA256: plan, Revision: revision, Principal: principal,
	}, nil
}

// requireDecisionReceipt resolves, files and awaits the receipt of one
// first-slice step. The revision is the Agent's digest of the workspace
// StackSpec from the node read that preceded the step; without it no
// decision is filed.
func requireDecisionReceipt(ctx context.Context, cfg StackKitLifecycleConfig, track decisionTracker, req StackKitLifecycleRequest, release stackkitrelease.Release, build func(serverID string, principal stackkitdecision.Principal) (stackkitdecision.Facts, error)) (*stackkitdecision.Admitted, error) {
	if err := requireDecisionReceiptNode(ctx, req, release); err != nil {
		return nil, err
	}
	principal, err := parseDecisionPrincipal(req.DecisionPrincipal)
	if err != nil {
		return nil, decisionRefused("refused", stackkitdecision.RefusalInvalid, "", "%v", err)
	}
	serverID, err := decisionServerID(ctx, cfg.DecisionServers, req, principal)
	if err != nil {
		return nil, err
	}
	facts, err := build(serverID, principal)
	if err != nil {
		return nil, decisionRefused("refused", "stackkit_decision_binding_unavailable", "", "%v", err)
	}
	if !stackKitDecisionIDSafe(track.jobID) {
		return nil, decisionRefused("refused", "stackkit_decision_binding_unavailable", "", "the job id cannot name a ledger operation")
	}
	return obtainStackKitDecisionReceipt(ctx, cfg, track, req, facts)
}

// stackKitDecisionIDSafe keeps the ledger operation id inside the ledger's
// id shape ([A-Za-z0-9][A-Za-z0-9._:-]{3,127}).
func stackKitDecisionIDSafe(jobID string) bool {
	if jobID == "" || len("homelab-"+jobID+"-"+stackkitdecision.OperationAdvancedChangeSetApply) > 128 {
		return false
	}
	for _, r := range jobID {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == ':' || r == '-') {
			return false
		}
	}
	return true
}

// DecisionReceiptMinimumStackKitsRelease is the first StackKits release whose
// CLI takes --decision-receipt and --decision-principal (the consumer,
// StackKits #3244 and this change's consumer points). An older node is
// refused before anything is filed or sent, instead of receiving a flag it
// does not know. The coordinating session sets it to the release that ships
// the consumer before the flag is turned on.
const DecisionReceiptMinimumStackKitsRelease = "v0.58.0"

// ErrDecisionReceiptNodeUnsupported refuses the receipt path on a node whose
// StackKits CLI predates the receipt carrier.
var ErrDecisionReceiptNodeUnsupported = errors.New("this server's StackKits release does not take a decision receipt yet")

// DecisionReceiptNodeRelease is the StackKits release a command for this
// agent runs: the node's installed release while it withholds a published
// update (AddressNodeStackKitRelease), otherwise the stack's target release.
func DecisionReceiptNodeRelease(ctx context.Context, tenantID, stackID, agentID string, target *stackkitrelease.Release) (string, error) {
	if target == nil {
		resolved, err := TargetStackKitReleaseForStack(ctx, tenantID, stackID)
		if err != nil {
			return "", err
		}
		if resolved == nil {
			return "", errors.New("pinned published StackKits release is not configured")
		}
		target = resolved
	}
	if node, ok := ReadNodeStackKitRelease(ctx, tenantID, agentID); ok && node.Installed != nil {
		return stackKitReleaseTag(node.Installed.Version), nil
	}
	return stackKitReleaseTag(target.Receipt().Version), nil
}

// DecisionReceiptSupported reports whether a release takes the carrier.
func DecisionReceiptSupported(version string) bool {
	return stackkitcommand.ReleaseAtLeast(stackKitReleaseTag(version), DecisionReceiptMinimumStackKitsRelease)
}

// requireDecisionReceiptNode refuses the receipt path on an older node.
func requireDecisionReceiptNode(ctx context.Context, req StackKitLifecycleRequest, release stackkitrelease.Release) error {
	version, err := DecisionReceiptNodeRelease(ctx, req.TenantID, req.StackID, req.AgentID, &release)
	if err != nil {
		return decisionRefused("refused", "stackkit_decision_node_release_unavailable", "", "%v", err)
	}
	if !DecisionReceiptSupported(version) {
		return decisionRefused("refused", "stackkit_decision_node_unsupported", "", "%v (runs %s, needs %s or later)", ErrDecisionReceiptNodeUnsupported, version, DecisionReceiptMinimumStackKitsRelease)
	}
	return nil
}

// stackKitDecisionLedger is the process-wide ledger client, installed at
// startup with the same Gateway service identity and key the RIL decisions
// use (cmd/homelab). A config's own DecisionLedger takes precedence.
var stackKitDecisionLedger atomic.Pointer[StackKitDecisionLedger]

// SetStackKitDecisionLedger installs the decision ledger client; nil removes
// it, which refuses the decision receipt path.
func SetStackKitDecisionLedger(ledger StackKitDecisionLedger) {
	if ledger == nil {
		stackKitDecisionLedger.Store(nil)
		return
	}
	stackKitDecisionLedger.Store(&ledger)
}

func (cfg StackKitLifecycleConfig) decisionLedger() StackKitDecisionLedger {
	if cfg.DecisionLedger != nil {
		return cfg.DecisionLedger
	}
	if ledger := stackKitDecisionLedger.Load(); ledger != nil {
		return *ledger
	}
	return nil
}
