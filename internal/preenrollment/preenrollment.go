// Package preenrollment binds SSH to a machine that is not enrolled yet to a
// target the person authorised (platform-x21l30).
//
// Before a server is in the inventory, several routes open SSH with a
// credential from the person's Wallet to a host named in the request: the
// wizard connection test, the discovery probe and test, device readiness, and
// the connect-remote enrollment the wizard start and its retry queue. A call
// that reaches one of them through the MCP surface, or that the Gateway signed
// for an agent, may connect only to a target the person authorised: the
// enrollment target the person entered themselves, or a device record from a
// scan the person started. A stored target or a planned inventory row is no
// such proof, and no approval a route carries replaces it.
package preenrollment

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	ksapi "github.com/kombifyio/homelab/pkg/api"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/middleware"
)

// ReasonUnauthorisedTarget is the refusal of an agent call whose target the
// person did not authorise.
const ReasonUnauthorisedTarget = "pre_enrollment_target_not_authorised"

// StackConfigKey holds, on a connect-remote stack, the target the person
// entered themselves. It is written only by the person's own request and kept
// only while the stored target stays that one.
const StackConfigKey = "server_remote_target_person_entered"

type agentCallKey struct{}

// MarkAgentCall marks a request that entered through the MCP surface. Only the
// MCP endpoint calls it, on the context the tool calls inherit.
func MarkAgentCall(ctx context.Context) context.Context {
	return context.WithValue(ctx, agentCallKey{}, true)
}

// AgentCall reports a call that is not the person's own: it entered through
// the MCP surface, or it came through the Gateway without the signed principal
// type of a person (an agent, an on-behalf-of exchange, a machine, or an
// envelope that signs no principal type at all). Only a request that passed no
// Gateway envelope (the local session of a self-hosted Homelab) and was not
// marked is the person's without a signed principal.
func AgentCall(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	if marked, _ := ctx.Value(agentCallKey{}).(bool); marked {
		return true
	}
	if claims, verified := middleware.VerifiedStepUpFromContext(ctx); verified {
		return strings.TrimSpace(claims.PrincipalType) != middleware.PrincipalTypeUser
	}
	return middleware.IsEdgeAuthenticated(ctx)
}

// Target is one SSH destination: host, port and login.
type Target struct {
	Host string
	Port int
	User string
}

// Key is the comparable form of a target; port 0 means 22.
func (t Target) Key() string {
	port := t.Port
	if port == 0 {
		port = 22
	}
	host := strings.ToLower(strings.TrimSpace(t.Host))
	user := strings.TrimSpace(t.User)
	if host == "" || user == "" || port < 1 || port > 65535 {
		return ""
	}
	return fmt.Sprintf("%s|%d|%s", host, port, user)
}

// StoredTarget reads the connect-remote target persisted on a stack.
func StoredTarget(config map[string]any) Target {
	target := Target{Host: stringValue(config["server_remote_host"]), User: stringValue(config["server_remote_user"])}
	switch port := config["server_remote_port"].(type) {
	case int:
		target.Port = port
	case int64:
		target.Port = int(port)
	case float64:
		target.Port = int(port)
	}
	return target
}

// PersonEntered reports whether the stack's stored target is the one the
// person entered themselves.
func PersonEntered(config map[string]any) bool {
	key := StoredTarget(config).Key()
	return key != "" && stringValue(config[StackConfigKey]) == key
}

// Record updates the person-entered marker of a stack config whose stored
// target is being set to target. entered says the target came from this
// request, not from the stack's storage. The person's own request that entered
// the target records it; every other write keeps the record only while the
// target stays the one already recorded.
func Record(ctx context.Context, config map[string]any, target Target, entered bool) {
	key := target.Key()
	if key != "" && entered && !AgentCall(ctx) {
		config[StackConfigKey] = key
		return
	}
	if key == "" || stringValue(config[StackConfigKey]) != key {
		delete(config, StackConfigKey)
	}
}

// JobPayloadKey binds a queued connect-remote enrollment to the target it was
// admitted for; the executor dials nothing else.
const JobPayloadKey = "authorised_remote_target"

// AdmittedTarget is the target a connect-remote enrollment may be queued for:
// target itself when the person asked for it in their own request, or when it
// is the person-entered target stored on the stack; empty otherwise.
func AdmittedTarget(ctx context.Context, config map[string]any, target Target) string {
	key := target.Key()
	if key == "" {
		return ""
	}
	if !AgentCall(ctx) || (PersonEntered(config) && StoredTarget(config).Key() == key) {
		return key
	}
	return ""
}

// Refuse answers an agent call whose target the person did not authorise.
// Nothing was connected and no credential was read.
func Refuse(e *httpx.Event) error {
	return httpx.Error(e, http.StatusForbidden, ksapi.ErrCodeForbidden,
		"This server is not one the person chose to connect", map[string]any{
			"reason_code": ReasonUnauthorisedTarget,
			"retryable":   false,
			"user_guidance": "An agent connects only to a server the person entered in the Homelab wizard or " +
				"found with their own network scan. Ask the person to add this server in the wizard.",
		})
}

func stringValue(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}
