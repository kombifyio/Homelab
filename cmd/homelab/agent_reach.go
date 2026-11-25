package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/kombifyio/homelab/pkg/agent"
	"github.com/kombifyio/homelab/pkg/agent/hostidentity"
	"github.com/kombifyio/homelab/pkg/agent/reachagent"
	"github.com/kombifyio/homelab/pkg/guardexec"
)

// startGuardReach runs the Guard side of reach for a key-bound Guard: the
// control-plane exchange and, unless a static relay key owns the relay, the
// kombify.me relay authenticated by host proofs. A Guard that never bound a
// host key, or a control plane without reach, leaves it idle; reach is never
// a precondition of the Guard's own work.
func startGuardReach(ctx context.Context, cfg *agentModeConfig, enrollment agentEnrollment, identity guardIdentityRuntime, manager *hostidentity.Manager, execution guardExecution, log *slog.Logger) {
	if manager == nil || identity.client == nil || identity.client.Mode() != hostidentity.ModeHostKey {
		return
	}
	exchangeURL := reachagent.ExchangeURLFromHeartbeat(enrollment.HeartbeatURL)
	if exchangeURL == "" {
		return
	}
	var paperwork reachagent.PaperworkNodeCustody
	var stackkits *agent.StackKitExecutor
	if execution.mode == guardModeLegacyRoot {
		switch executor := execution.runtime.(type) {
		case *agent.StackKitExecutor:
			stackkits = executor
		case *guardexec.LegacyRoot:
			stackkits = executor.StackKitExecutor
		}
	}
	if workspace := strings.TrimSpace(os.Getenv("HOMELAB_PAPERWORK_WORKSPACE")); workspace != "" && stackkits.Available() && os.Getenv("STACKKIT_LINKED_PAPERWORK_INSTALL") == "true" {
		var bundle agent.PaperworkBundlePin
		if json.Unmarshal([]byte(os.Getenv("HOMELAB_PAPERWORK_BUNDLE_PIN")), &bundle) == nil && bundle.Ready() {
			paperwork = agent.PaperworkSignInCustody{Executor: stackkits, Workspace: workspace, Bundle: bundle}
		}
	}
	reach, err := reachagent.New(reachagent.Config{
		ExchangeURL: exchangeURL, HTTPClient: identity.httpClient(20 * time.Second), Manager: manager,
		WorkerID: enrollment.RuntimeAgentID, ServerID: enrollment.ServerID, TenantID: enrollment.TenantID,
		Version: version, ManageRelay: strings.TrimSpace(cfg.kombifyMeAPIKey) == "", Logger: log,
		PaperworkSignIn: paperwork,
	})
	if err != nil {
		log.Warn("guard_reach_unavailable", "error", err.Error())
		return
	}
	go reach.Run(ctx)
	log.Info("guard_reach_started")
}
