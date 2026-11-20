package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kombifyio/homelab/internal/stackkitrelease"
	"github.com/kombifyio/homelab/pkg/agentcontrol"
	agentpb "github.com/kombifyio/homelab/pkg/api/agentpb"
	"github.com/kombifyio/homelab/pkg/grpcserver"
	"github.com/kombifyio/homelab/pkg/stackkitcommand"
)

// Guard host operation budgets. The Guard gets headroom on top of each.
const (
	hostListCommandTimeout   = time.Minute
	HostActionCommandTimeout = 2 * time.Minute
	hostOperationGrace       = 30 * time.Second
)

// ErrHostOperationsUnavailable is returned while no typed command channel is
// configured; the routes then stay inert.
var ErrHostOperationsUnavailable = errors.New("guard host operations are unavailable")

// HostOperationRefusal is the Guard's refusal before any change: the node is
// unchanged. Code is a stable snake_case token.
type HostOperationRefusal struct{ Code, Message string }

func (r *HostOperationRefusal) Error() string { return r.Code + ": " + r.Message }

// HostOperationFailure is a failure after the Guard started work; the node may
// have changed.
type HostOperationFailure struct{ Code, Message string }

func (f *HostOperationFailure) Error() string { return f.Code + ": " + f.Message }

// HostOperationTarget names the node and, for an action, the stable command
// identity that makes a retry reattach to the first attempt.
type HostOperationTarget struct {
	TenantID  string
	AgentID   string
	CommandID string
}

// HostOperationsDispatcher sends the Guard host operations to a node's agent
// through the typed StackKit command channel. The hub refuses an agent that
// does not advertise the capability the operation needs.
type HostOperationsDispatcher struct {
	sender          StackKitCommandSender
	releaseResolver func() (*stackkitrelease.Release, error)
}

// NewHostOperationsDispatcher returns nil without a sender, which keeps the
// host operation routes inert.
func NewHostOperationsDispatcher(sender StackKitCommandSender) *HostOperationsDispatcher {
	if sender == nil {
		return nil
	}
	return &HostOperationsDispatcher{sender: sender, releaseResolver: configuredTargetStackKitRelease}
}

func (d *HostOperationsDispatcher) ListProcesses(ctx context.Context, target HostOperationTarget, limit int32) (stackkitcommand.HostOperationData, error) {
	return d.run(ctx, target, &agentpb.StackKitCommand{
		Operation: agentpb.StackKitOperation_STACKKIT_OPERATION_HOST_LIST_PROCESSES, HostListLimit: limit,
		TimeoutSeconds: int32(hostListCommandTimeout / time.Second),
	})
}

func (d *HostOperationsDispatcher) ListContainers(ctx context.Context, target HostOperationTarget, limit int32) (stackkitcommand.HostOperationData, error) {
	return d.run(ctx, target, &agentpb.StackKitCommand{
		Operation: agentpb.StackKitOperation_STACKKIT_OPERATION_HOST_LIST_CONTAINERS, HostListLimit: limit,
		TimeoutSeconds: int32(hostListCommandTimeout / time.Second),
	})
}

// run sends one command and returns its host-operations document. A Guard
// refusal is a *HostOperationRefusal and a failure a *HostOperationFailure.
func (d *HostOperationsDispatcher) run(ctx context.Context, target HostOperationTarget, command *agentpb.StackKitCommand) (stackkitcommand.HostOperationData, error) {
	if d == nil || d.sender == nil {
		return stackkitcommand.HostOperationData{}, ErrHostOperationsUnavailable
	}
	release, err := d.releaseResolver()
	if err != nil {
		return stackkitcommand.HostOperationData{}, fmt.Errorf("resolve pinned StackKits release: %w", err)
	}
	if release == nil {
		return stackkitcommand.HostOperationData{}, errors.New("pinned published StackKits release is not configured")
	}
	command.CommandId = strings.TrimSpace(target.CommandID)
	if command.CommandId == "" {
		command.CommandId, err = newHostOperationCommandID()
		if err != nil {
			return stackkitcommand.HostOperationData{}, err
		}
	}
	command.Release = grpcserver.StackKitReleasePinFor(*release)
	if err := stackkitcommand.ValidateCommand(command); err != nil {
		return stackkitcommand.HostOperationData{}, err
	}
	result, err := sendStackKitCommandBoundedWithGraceForTenant(ctx, d.sender, target.TenantID, target.AgentID, command, hostOperationGrace)
	if errors.Is(err, agentcontrol.ErrCommandExists) {
		// A retry of the same request: reattach to the first attempt's outcome
		// instead of running the action twice.
		awaiter, ok := d.sender.(stackKitCommandAwaiter)
		if !ok {
			return stackkitcommand.HostOperationData{}, err
		}
		result, err = awaiter.AwaitStackKitCommandForTenant(ctx, target.TenantID, command.CommandId)
	}
	if err != nil {
		return stackkitcommand.HostOperationData{}, err
	}
	if result == nil {
		return stackkitcommand.HostOperationData{}, errors.New("host operation returned no result")
	}
	return parseHostOperationResult(command, result)
}

// ParseHostOperationResult reads a Guard host operation's result: the typed
// document of a success, a *HostOperationRefusal for a Guard refusal (nothing
// changed) or a *HostOperationFailure. The governed host primitives and the
// read dispatcher share it.
func ParseHostOperationResult(command *agentpb.StackKitCommand, result *agentpb.StackKitResult) (stackkitcommand.HostOperationData, error) {
	return parseHostOperationResult(command, result)
}

func parseHostOperationResult(command *agentpb.StackKitCommand, result *agentpb.StackKitResult) (stackkitcommand.HostOperationData, error) {
	var envelope struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(result.CommandResultJson, &envelope); err != nil {
		return stackkitcommand.HostOperationData{}, fmt.Errorf("decode host operation result: %w", err)
	}
	if envelope.Status == "success" {
		return stackkitcommand.ParseHostOperationResult(command, result)
	}
	var problem struct {
		Refusal *HostOperationFailure `json:"refusal"`
		Failure *HostOperationFailure `json:"failure"`
	}
	_ = json.Unmarshal(envelope.Data, &problem)
	switch {
	case envelope.Status == "denied" && problem.Refusal != nil:
		return stackkitcommand.HostOperationData{}, &HostOperationRefusal{Code: safeHostOperationCode(problem.Refusal.Code), Message: boundedHostOperationMessage(problem.Refusal.Message)}
	case problem.Failure != nil:
		return stackkitcommand.HostOperationData{}, &HostOperationFailure{Code: safeHostOperationCode(problem.Failure.Code), Message: boundedHostOperationMessage(problem.Failure.Message)}
	default:
		return stackkitcommand.HostOperationData{}, &HostOperationFailure{Code: "host_operation_failed", Message: "the Guard reported a failure without a reason"}
	}
}

// safeHostOperationCode keeps a bounded snake_case token and replaces anything
// else, so a node cannot inject free text into a stable code field.
func safeHostOperationCode(code string) string {
	code = strings.TrimSpace(code)
	if code == "" || len(code) > 48 {
		return "host_operation_failed"
	}
	for _, r := range code {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return "host_operation_failed"
		}
	}
	return code
}

// boundedHostOperationMessage caps the node's free text before it reaches an
// error message.
func boundedHostOperationMessage(message string) string {
	message = strings.TrimSpace(message)
	if runes := []rune(message); len(runes) > 240 {
		return string(runes[:240])
	}
	return message
}

func newHostOperationCommandID() (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("crypto/rand failed: %w", err)
	}
	return "hostop-" + hex.EncodeToString(raw), nil
}
