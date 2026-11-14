package stackkitdecision

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// The ledger contract (kombify-Gateway, store stackkit_operation) and the
// consumer's binding definition (StackKits docs/DECISION-RECEIPTS.md).
const (
	// SubjectStore is the ledger store every StackKits decision is filed in.
	SubjectStore = "stackkit_operation"
	// RiskDestructive is the risk the ledger records for a StackKits operation.
	RiskDestructive = "destructive"
	// IntentDigestDomain separates the ledger's intent digest.
	IntentDigestDomain = "kombify.decision-intent.digest.v1"
	// ArgumentsDigestDomain separates the digest of one operation's canonical
	// arguments (the consumer's definition).
	ArgumentsDigestDomain = "kombify.stackkit-operation-arguments.digest.v1"
)

// The ledger's catalogue operation ids of the first slice.
const (
	OperationBackupRestore          = "backup_restore"
	OperationApplicationRemove      = "application_remove"
	OperationAdvancedChangeSetApply = "advanced_change_set_apply"
	OperationInstallationRemove     = "installation_remove"
)

// The principal kinds the ledger accepts.
const (
	PrincipalPerson        = "person"
	PrincipalAgentInstance = "agent_instance"
)

// The target kinds of the first slice; the installation has no kind.
const (
	TargetApplication    = "application"
	TargetOwnApplication = "own-application"
	TargetRestore        = "restore"
	TargetChangeSet      = "change-set"
)

var (
	serverIDPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,191}$`)
	hexDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
	prefixedDigest   = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// Principal is the acting principal a decision is bound to, spelled as the
// consumer's "Acting principal" section defines it: person:<platform subject>
// for the Owner, agent_instance:<platform agent instance id> for an agent.
type Principal struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Validate refuses a principal the ledger or the node would not admit.
func (p Principal) Validate() error {
	if (p.Kind != PrincipalPerson && p.Kind != PrincipalAgentInstance) || p.ID == "" ||
		p.ID != strings.TrimSpace(p.ID) || len(p.ID) > 256 || strings.ContainsFunc(p.ID, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return errors.New("the acting principal is invalid")
	}
	return nil
}

// Flag is the --decision-principal value the node command receives.
func (p Principal) Flag() string { return p.Kind + ":" + p.ID }

// Reference is the exact target reference the ledger signs as
// intent.resource: stackkit:<server id> for the installation and
// stackkit:<server id>:<kind>:<ref> for everything on it. The server id is
// the node's enrolled serverId, which the hosted enrollment pins from the
// control plane's own server record.
func Reference(serverID, kind, ref string) (string, error) {
	if !serverIDPattern.MatchString(serverID) {
		return "", errors.New("the node's server id is unavailable")
	}
	if kind == "" {
		if ref != "" {
			return "", errors.New("the installation target carries no reference")
		}
		return "stackkit:" + serverID, nil
	}
	switch kind {
	case TargetApplication, TargetOwnApplication, TargetRestore, TargetChangeSet:
	default:
		return "", fmt.Errorf("unknown target kind %q", kind)
	}
	if ref == "" || ref != strings.TrimSpace(ref) || len(ref) > 200 || strings.ContainsFunc(ref, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", errors.New("the target reference is invalid")
	}
	return "stackkit:" + serverID + ":" + kind + ":" + ref, nil
}

// BackupRestoreArguments is the canonical argument object of one restore
// activation. Every member is always present; an unused member is "".
func BackupRestoreArguments(resultID, workload, marketplaceService, operationID string) map[string]any {
	return map[string]any{
		"restore_result_id":   resultID,
		"workload":            workload,
		"marketplace_service": marketplaceService,
		"operation_id":        operationID,
	}
}

// AdvancedChangeSetApplyArguments is the canonical argument object of one
// change-set apply.
func AdvancedChangeSetApplyArguments(changeSetID string) map[string]any {
	return map[string]any{"change_set_id": changeSetID}
}

// ArgumentsDigest is the 64-hex SHA-256 over ArgumentsDigestDomain, a
// newline, the canonical JSON of the argument object and a newline.
func ArgumentsDigest(arguments map[string]any) (string, error) {
	if len(arguments) == 0 {
		return "", errors.New("the canonical arguments are empty")
	}
	canonical, err := canonicalJSON(arguments)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(ArgumentsDigestDomain + "\n" + canonical + "\n"))
	return hex.EncodeToString(sum[:]), nil
}

// PlanDigest turns one sha256:<hex> plan digest into the 64-hex planSha256.
func PlanDigest(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !prefixedDigest.MatchString(value) {
		return "", errors.New("the plan digest is not sha256:<64 lowercase hex>")
	}
	return strings.TrimPrefix(value, "sha256:"), nil
}

// Facts are exactly the members of the intent the ledger signs and the node
// recomputes.
type Facts struct {
	Operation       string
	Resource        string
	ArgumentsSHA256 string
	// PlanSHA256 is empty for an operation without a plan.
	PlanSHA256 string
	Revision   string
	Principal  Principal
}

// Validate refuses facts the node could never recompute.
func (f Facts) Validate() error {
	switch f.Operation {
	case OperationBackupRestore, OperationApplicationRemove, OperationAdvancedChangeSetApply, OperationInstallationRemove:
	default:
		return fmt.Errorf("operation %q is not in the first slice", f.Operation)
	}
	if !strings.HasPrefix(f.Resource, "stackkit:") || len(f.Resource) > 512 {
		return errors.New("the target reference is invalid")
	}
	if !hexDigestPattern.MatchString(f.ArgumentsSHA256) || (f.PlanSHA256 != "" && !hexDigestPattern.MatchString(f.PlanSHA256)) {
		return errors.New("the arguments or plan digest is invalid")
	}
	if !prefixedDigest.MatchString(f.Revision) {
		return errors.New("the revision is not the node's sha256:<hex> spec digest")
	}
	return f.Principal.Validate()
}

// Intent is the intent object the ledger signs: operation, resource, risk and
// the parameters binding the digests, the revision and the acting principal.
// planSha256 is a member only when the operation has a plan.
func (f Facts) Intent() map[string]any {
	parameters := map[string]any{
		"argumentsSha256":     f.ArgumentsSHA256,
		"revision":            f.Revision,
		"actingPrincipalKind": f.Principal.Kind,
		"actingPrincipalId":   f.Principal.ID,
	}
	if f.PlanSHA256 != "" {
		parameters["planSha256"] = f.PlanSHA256
	}
	return map[string]any{
		"operation":  f.Operation,
		"resource":   f.Resource,
		"risk":       RiskDestructive,
		"parameters": parameters,
	}
}

// IntentDigest is the ledger's intent digest: sha256: + hex SHA-256 of the
// domain, a newline, the canonical JSON of the intent and a newline.
func IntentDigest(f Facts) (string, error) {
	canonical, err := canonicalJSON(f.Intent())
	if err != nil {
		return "", fmt.Errorf("intent canonical form: %w", err)
	}
	sum := sha256.Sum256([]byte(IntentDigestDomain + "\n" + canonical + "\n"))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
