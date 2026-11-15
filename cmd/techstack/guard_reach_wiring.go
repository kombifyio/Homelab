package main

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kombifyio/techstack/pkg/api/agentpb"
	"github.com/kombifyio/techstack/pkg/config"
	"github.com/kombifyio/techstack/pkg/controlplane"
	"github.com/kombifyio/techstack/pkg/guardreach"
	"github.com/kombifyio/techstack/pkg/kombifyme"
	"github.com/kombifyio/techstack/pkg/logger"
	"github.com/kombifyio/techstack/pkg/orchestrator"
	"github.com/kombifyio/techstack/pkg/serviceauth"
	"github.com/kombifyio/techstack/pkg/stackkitcommand"
)

const (
	// guardReachEnabledEnv switches kombify Guard reach on for a connected
	// (SaaS) deployment. It is off by default: reach publishes a server at a
	// kombify.me address, so a rollout is an explicit operator decision.
	guardReachEnabledEnv = "TECHSTACK_GUARD_REACH_ENABLED"
	guardReachGatewayEnv = "TECHSTACK_GUARD_REACH_GATEWAY_URL"
	guardReachSecretEnv  = "TECHSTACK_GATEWAY_CONNECT_SERVICE_SECRET"
	guardReachRelayEnv   = "TECHSTACK_GUARD_REACH_RELAY_URL"
	guardReachPollEnv    = "TECHSTACK_GUARD_REACH_REVOCATION_POLL"
	// guardReachAnnounceMCPEnv registers the server's kombify.me route as its product MCP host with the Connect node
	// (Gateway ADR 0042). Off by default.
	guardReachAnnounceMCPEnv = "TECHSTACK_GUARD_REACH_ANNOUNCE_MCP_HOST"
)

// Maintenance authority is independent of optional tunnel publication and its
// kombify.me credential. Missing dedicated custody keeps delegated tools closed.
func bootMaintenanceOriginGateway(log *logger.Logger) *guardreach.Gateway {
	secret := strings.TrimSpace(os.Getenv(guardReachSecretEnv))
	if (secret == "" && !serviceauth.CanSign("")) || (secret != "" && secret == strings.TrimSpace(os.Getenv("SERVICE_AUTH_SECRET"))) {
		log.Warn("maintenance_origin_disabled", "reason", "dedicated_service_custody_unavailable")
		return nil
	}
	gateway, err := guardreach.NewGateway(guardreach.GatewayConfig{BaseURL: os.Getenv(guardReachGatewayEnv), Secret: secret})
	if err != nil {
		log.Warn("maintenance_origin_disabled", "reason", "gateway_configuration_invalid")
		return nil
	}
	return gateway
}

// bootGuardReach composes the kombify Guard reach service for a connected
// edition. A self-managed installation, a deployment without the explicit
// enable flag and one missing either credential (the service-auth secret for
// Connect, the platform kombify.me key for the relay) run no reach at all;
// Guard enrollment and execution are unaffected either way.
func bootGuardReach(v2 *v2Boot, edition config.Edition, orch *orchestrator.Orchestrator, log *logger.Logger) *guardreach.Service {
	if !edition.IsSaaS() || !envBoolDefault(guardReachEnabledEnv, false) {
		return nil
	}
	store := controlPlanePostgresStore(v2)
	if store == nil {
		return nil
	}
	// The Connect routes accept only Techstack's own secret; the shared
	// SERVICE_AUTH_SECRET is never used here, not even as a fallback.
	secret := strings.TrimSpace(os.Getenv(guardReachSecretEnv))
	meConfig := kombifyme.DefaultConfigFromEnv()
	switch {
	case secret == "" && !serviceauth.CanSign(""):
		log.Warn("guard_reach_disabled", "reason", guardReachSecretEnv+" and the Ed25519 service key missing")
		return nil
	case secret != "" && secret == strings.TrimSpace(os.Getenv("SERVICE_AUTH_SECRET")):
		log.Warn("guard_reach_disabled", "reason", guardReachSecretEnv+" must differ from SERVICE_AUTH_SECRET")
		return nil
	case !strings.HasPrefix(strings.TrimSpace(meConfig.APIKey), "kbi_"):
		log.Warn("guard_reach_disabled", "reason", "platform kombify.me key (KOMBIFY_ME_API_KEY) missing")
		return nil
	}
	reachStore, err := guardreach.NewPostgresStore(v2.db.DB)
	if err != nil {
		log.Warn("guard_reach_disabled", "reason", err.Error())
		return nil
	}
	gateway, err := guardreach.NewGateway(guardreach.GatewayConfig{BaseURL: os.Getenv(guardReachGatewayEnv), Secret: secret})
	if err != nil {
		log.Warn("guard_reach_disabled", "reason", err.Error())
		return nil
	}
	poll, _ := time.ParseDuration(strings.TrimSpace(os.Getenv(guardReachPollEnv)))
	meConfig.HTTPClient = &http.Client{Timeout: 20 * time.Second}
	var agentLinks guardreach.AgentLinkDelivery
	if orch != nil {
		agentLinks = guardReachAgentLinks{orch: orch, links: store}
	}
	service, err := guardreach.New(guardreach.Config{
		Store: reachStore, Me: guardreach.NewMe(kombifyme.NewClient(meConfig)), Connect: gateway,
		Identities: guardreach.ControlPlane{Workers: store, Identities: store, Servers: store}, Access: store,
		AgentLinks:     agentLinks,
		AnnounceMCPHost: os.Getenv(guardReachAnnounceMCPEnv) == "true",
		RelayURL:       os.Getenv(guardReachRelayEnv),
		RevocationPoll: poll, Logger: log.Logger,
	})
	if err != nil {
		log.Warn("guard_reach_disabled", "reason", err.Error())
		return nil
	}
	log.Info("guard_reach_ready")
	return service
}

// householdRefsByOwner is the D10 reference read the agent link needs.
type householdRefsByOwner interface {
	ListHouseholdRefsByOwner(ctx context.Context, tenantID, ownerSubjectID string) ([]controlplane.HomelabIdentityRef, error)
}

// guardReachAgentLinks delivers the homelab identity agent link of a reach row
// to its node: the owner-approved StackKit identity operation on the row's own
// Guard worker (the identity runtime of the row's stack), and the D10 members
// of that stack whose Pocket ID link is verified. Techstack, the D10 link
// authority, delivers member changes under a pinned key on its own; a new key
// or owner is approved by the Owner with a passkey step-up on the node, never
// by this assertion alone. The node binds the link to its own Guard enrollment
// and Owner custody.
type guardReachAgentLinks struct {
	orch  *orchestrator.Orchestrator
	links householdRefsByOwner
}

func (a guardReachAgentLinks) LinkedMembers(ctx context.Context, row guardreach.Row) ([]guardreach.AgentLinkMember, error) {
	refs, err := a.links.ListHouseholdRefsByOwner(ctx, row.TenantID, row.OwnerSubjectID)
	if err != nil {
		return nil, err
	}
	members := make([]guardreach.AgentLinkMember, 0, len(refs))
	for _, ref := range refs {
		if ref.StackID != row.StackID || ref.Status != controlplane.HouseholdLinkLinked || ref.IdP != controlplane.HouseholdIdPPocketID {
			continue
		}
		members = append(members, guardreach.AgentLinkMember{CloudSubject: ref.CloudSubject, Username: ref.Username})
	}
	return members, nil
}

// Install pins the link on the node. A new key or owner waits there for the
// Owner's passkey step-up; the node then answers pending and the reach
// reconciler dispatches again until the Owner approved or the request lapsed.
func (a guardReachAgentLinks) Install(ctx context.Context, row guardreach.Row, request []byte) error {
	result, err := a.run(ctx, row, agentpb.StackKitOperation_STACKKIT_OPERATION_AGENT_LINK_INSTALL, request)
	if err == nil && stackkitcommand.AgentLinkResultPending(result) {
		return guardreach.ErrAgentLinkOwnerPending
	}
	return err
}

func (a guardReachAgentLinks) Remove(ctx context.Context, row guardreach.Row) error {
	_, err := a.run(ctx, row, agentpb.StackKitOperation_STACKKIT_OPERATION_AGENT_LINK_REMOVE, nil)
	return err
}

func (a guardReachAgentLinks) run(ctx context.Context, row guardreach.Row, operation agentpb.StackKitOperation, request []byte) (*agentpb.StackKitResult, error) {
	return a.orch.RunStackKitIdentityCommand(ctx, orchestrator.StackKitIdentityCommandRequest{
		TenantID: row.TenantID, OwnerID: row.OwnerSubjectID, DeploymentID: row.StackID,
		Operation: operation, OwnerApproved: true, ExpectedAgentID: row.WorkerID, AgentLinkRequest: request,
	})
}
