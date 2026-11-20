package stackkitcommand

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/kombifyio/homelab/pkg/api/agentpb"
)

// The homelab identity agent link (HOMELAB-IDENTITY-ACCESS-TARGET section 7,
// Gateway ADR 0042 "Agent node envelope contract"). Homelab hands the node
// the Gateway's agent envelope verification key of the server's Connect
// registration and the D10 references; the pinned StackKits CLI
// (`stackkit identity agent-link install|remove`) decides whether the request
// is pinned. Core only bounds the public transport and checks that the node
// pinned exactly the bytes it dispatched.
const (
	// AgentLinkCapability marks an agent whose pinned StackKits release ships
	// `stackkit identity agent-link`.
	AgentLinkCapability = "stackkit.agent-link.v1"
	// MinAgentLinkRelease is the first StackKits release that ships
	// `stackkit identity agent-link install --expect-sha256` (pending its
	// publication and adoption; an older pin never advertises the capability).
	MinAgentLinkRelease = "v0.56.5"
	// AgentLinkRequestSchema is the request document the node pins.
	AgentLinkRequestSchema = "stackkit.home-agent-link-request/v1"
	// MaxAgentLinkRequestBytes is the StackKits request size limit.
	MaxAgentLinkRequestBytes = 64 << 10
	// AgentLinkOwnerWait is how long the node waits, per dispatch, for the
	// Owner's passkey step-up that a new key or owner needs; the request
	// stays open on the node for five minutes and a later dispatch picks up
	// the approval.
	AgentLinkOwnerWait = "20s"
	// AgentLinkPendingOwner is the node's answer while that step-up is open.
	AgentLinkPendingOwner = "owner_step_up"
)

// IsAgentLinkOperation reports the two agent link operations.
func IsAgentLinkOperation(operation agentpb.StackKitOperation) bool {
	switch operation {
	case agentpb.StackKitOperation_STACKKIT_OPERATION_AGENT_LINK_INSTALL,
		agentpb.StackKitOperation_STACKKIT_OPERATION_AGENT_LINK_REMOVE:
		return true
	}
	return false
}

// AgentLinkRequestDigest is the "sha256:<hex>" digest of the request bytes
// the node verifies with --expect-sha256 and reports back.
func AgentLinkRequestDigest(request []byte) string {
	digest := sha256.Sum256(request)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// validateAgentLinkCommand admits an install with Owner approval and one
// bounded JSON request of the agent link schema, and a remove with Owner
// approval and no request. They act on the identity workspace, never on a
// stack's workload, inventory or capability.
func validateAgentLinkCommand(command *agentpb.StackKitCommand) error {
	if !ReleaseAtLeast(command.Release.Version, MinAgentLinkRelease) {
		return fmt.Errorf("agent link operations require StackKits %s or newer; upgrade the configured release pin", MinAgentLinkRelease)
	}
	if !command.OwnerApproved {
		return fmt.Errorf("StackKit agent link operations require Owner approval")
	}
	if len(command.CandidateSpecJson) != 0 || len(command.AdvancedCapability) != 0 || len(command.InventoryJson) != 0 {
		return fmt.Errorf("StackKit agent link operations take no candidate, Inventory or capability")
	}
	if command.Operation == agentpb.StackKitOperation_STACKKIT_OPERATION_AGENT_LINK_REMOVE {
		if len(command.AgentLinkRequest) != 0 {
			return fmt.Errorf("StackKit agent link remove carries no request")
		}
		return nil
	}
	request := command.AgentLinkRequest
	if len(request) == 0 || len(request) > MaxAgentLinkRequestBytes || !json.Valid(request) {
		return fmt.Errorf("StackKit agent link install requires one JSON request within %d bytes", MaxAgentLinkRequestBytes)
	}
	var envelope struct {
		Schema string `json:"schema"`
	}
	decoder := json.NewDecoder(bytes.NewReader(request))
	if decoder.Decode(&envelope) != nil || decoder.Decode(new(any)) != io.EOF || envelope.Schema != AgentLinkRequestSchema {
		return fmt.Errorf("StackKit agent link install request must be one %s document", AgentLinkRequestSchema)
	}
	return nil
}

// AgentLinkEvidence is what the node reports after an agent link operation.
// Pending is set while the request waits for the Owner's passkey step-up.
type AgentLinkEvidence struct {
	Linked            bool
	Unchanged         bool
	Pending           bool
	NodeID            string
	KeyThumbprint     string
	BindingGeneration int64
	RequestSHA256     string
}

// ParseAgentLinkEvidence accepts an install only when the node pinned the exact
// request bytes the command carried, and a remove only when the node reports
// no link afterwards.
func ParseAgentLinkEvidence(command *agentpb.StackKitCommand, result *agentpb.StackKitResult) (AgentLinkEvidence, error) {
	if command == nil || result == nil || !result.Success || !IsAgentLinkOperation(command.Operation) {
		return AgentLinkEvidence{}, fmt.Errorf("successful StackKit agent link evidence is required")
	}
	var envelope struct {
		SchemaVersion string `json:"schemaVersion"`
		Command       string `json:"command"`
		Status        string `json:"status"`
		Data          struct {
			Linked            bool   `json:"linked"`
			Unchanged         bool   `json:"unchanged"`
			Pending           string `json:"pending"`
			NodeID            string `json:"nodeId"`
			KeyThumbprint     string `json:"keyThumbprint"`
			BindingGeneration int64  `json:"bindingGeneration"`
			RequestSHA256     string `json:"requestSHA256"`
		} `json:"data"`
	}
	if err := json.Unmarshal(result.CommandResultJson, &envelope); err != nil {
		return AgentLinkEvidence{}, fmt.Errorf("decode StackKit agent link evidence: %w", err)
	}
	evidence := AgentLinkEvidence{
		Linked: envelope.Data.Linked, Unchanged: envelope.Data.Unchanged, Pending: envelope.Data.Pending == AgentLinkPendingOwner,
		NodeID:        strings.TrimSpace(envelope.Data.NodeID),
		KeyThumbprint: strings.TrimSpace(envelope.Data.KeyThumbprint), BindingGeneration: envelope.Data.BindingGeneration,
		RequestSHA256: strings.TrimSpace(envelope.Data.RequestSHA256),
	}
	if envelope.SchemaVersion != CommandResultVersion || envelope.Command != ResultCommandName(command.Operation) || envelope.Status != commandResultStatusSuccess {
		return AgentLinkEvidence{}, fmt.Errorf("StackKit agent link evidence is not a successful %s result", ResultCommandName(command.Operation))
	}
	if command.Operation == agentpb.StackKitOperation_STACKKIT_OPERATION_AGENT_LINK_REMOVE {
		if evidence.Linked {
			return AgentLinkEvidence{}, fmt.Errorf("StackKit agent link remove left a link pinned")
		}
		return evidence, nil
	}
	if evidence.RequestSHA256 != AgentLinkRequestDigest(command.AgentLinkRequest) || (envelope.Data.Pending != "" && !evidence.Pending) {
		return AgentLinkEvidence{}, fmt.Errorf("StackKit agent link evidence does not answer the dispatched request")
	}
	if evidence.Pending {
		return evidence, nil
	}
	var dispatched struct {
		AgentEnvelope struct {
			Aud               string `json:"aud"`
			JKT               string `json:"jkt"`
			BindingGeneration int64  `json:"binding_generation"`
		} `json:"agentEnvelope"`
	}
	if json.Unmarshal(command.AgentLinkRequest, &dispatched) != nil || !evidence.Linked ||
		evidence.NodeID != dispatched.AgentEnvelope.Aud || evidence.KeyThumbprint != dispatched.AgentEnvelope.JKT ||
		evidence.BindingGeneration != dispatched.AgentEnvelope.BindingGeneration || evidence.BindingGeneration < 1 {
		return AgentLinkEvidence{}, fmt.Errorf("StackKit agent link evidence does not pin the dispatched key")
	}
	return evidence, nil
}

// AgentLinkResultPending reports a validated install result whose request
// waits for the Owner's passkey step-up on the node.
func AgentLinkResultPending(result *agentpb.StackKitResult) bool {
	if result == nil {
		return false
	}
	var envelope struct {
		Data struct {
			Pending string `json:"pending"`
		} `json:"data"`
	}
	return json.Unmarshal(result.CommandResultJson, &envelope) == nil && envelope.Data.Pending == AgentLinkPendingOwner
}
