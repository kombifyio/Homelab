package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kombifyio/homelab/pkg/api/agentpb"
)

// A decision receipt (StackKits docs/DECISION-RECEIPTS.md) is the platform's
// signed confirmation of one destructive operation. Core sends it only for
// the two first-slice operations it dispatches; the pinned StackKits CLI
// verifies and spends it before the first effect. The Agent only carries it.

var (
	stackKitDecisionReceiptShape   = regexp.MustCompile(`^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`)
	stackKitDecisionPrincipalShape = regexp.MustCompile(`^(person|agent_instance):[^\s\x00-\x1f\x7f]{1,256}$`)
)

const (
	maxStackKitDecisionReceiptBytes = 16384
	maxStackKitWorkspaceSpecBytes   = 1 << 20
)

// stackKitDecisionReceiptArgs replaces the self-asserted --owner-approve of a
// first-slice command with the receipt carrier. The receipt goes to a 0600
// file below the workspace's .stackkit directory (never argv, so it is not in
// the process list); the caller removes the returned file after the command.
// A receipt on any other operation is refused, never ignored.
func stackKitDecisionReceiptArgs(workDir string, command *agentpb.StackKitCommand, operationArgs []string) ([]string, []string, error) {
	receipt := strings.TrimSpace(command.GetDecisionReceipt())
	principal := strings.TrimSpace(command.GetDecisionPrincipal())
	if receipt == "" && principal == "" {
		return operationArgs, nil, nil
	}
	switch command.Operation {
	case agentpb.StackKitOperation_STACKKIT_OPERATION_BACKUP_RESTORE_ACTIVATE,
		agentpb.StackKitOperation_STACKKIT_OPERATION_ADVANCED_CHANGE_SET_APPLY:
	default:
		return nil, nil, fmt.Errorf("a decision receipt covers only restore activation and change-set apply, not %s", command.Operation)
	}
	if command.DryRun || len(receipt) > maxStackKitDecisionReceiptBytes || !stackKitDecisionReceiptShape.MatchString(receipt) ||
		!stackKitDecisionPrincipalShape.MatchString(principal) {
		return nil, nil, fmt.Errorf("the decision receipt or its acting principal is malformed")
	}
	path, err := writeStackKitPrivateFile(workDir, ".homelab-decision-receipt-*.jws", []byte(receipt))
	if err != nil {
		return nil, nil, fmt.Errorf("write decision receipt: %w", err)
	}
	args := make([]string, 0, len(operationArgs)+4)
	for _, arg := range operationArgs {
		if arg == "--owner-approve" {
			continue
		}
		args = append(args, arg)
	}
	args = append(args, "--decision-receipt", "@"+path, "--decision-principal", principal)
	return args, []string{path}, nil
}

// stackKitWorkspaceRevision is sha256:<hex> of the workspace StackSpec bytes
// the command ran against (working_directory/spec_path, the --chdir and
// --spec the Agent passes): the revision a decision receipt binds. It is
// empty when the file cannot be read as one bounded regular file, and Core
// then files no decision.
func stackKitWorkspaceRevision(workDir string, command *agentpb.StackKitCommand) string {
	specPath, err := cleanStackKitRelativePath(command.SpecPath, "stack-spec.yaml", "spec_path")
	if err != nil || workDir == "" {
		return ""
	}
	path := filepath.Join(workDir, filepath.FromSlash(specPath))
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxStackKitWorkspaceSpecBytes {
		return ""
	}
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	hash := sha256.New()
	if written, err := io.Copy(hash, io.LimitReader(file, maxStackKitWorkspaceSpecBytes+1)); err != nil || written != info.Size() {
		return ""
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}
