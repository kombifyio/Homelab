package jobs

import (
	"context"
	"errors"

	"github.com/kombifyio/homelab/pkg/api/agentpb"
	"github.com/kombifyio/homelab/pkg/stackkitcommand"
)

// Homelab exposes one lifecycle job status. Only the closed node receipt is
// projected; arbitrary command output, events and stderr cannot enter it.
func normalizePaperworkMoveResult(req StackKitLifecycleRequest, result *agentpb.StackKitResult) (map[string]interface{}, error) {
	status := "interrupted"
	out := map[string]interface{}{
		"schema_version":       "homelab.stackkit-lifecycle-result/v1",
		"kit_deployment_id":    req.StackID,
		"stackkit_instance_id": req.StackKitInstanceID,
		"operation":            req.Operation,
		"agent_id":             req.AgentID,
		"success":              result.Success,
		"exit_code":            result.ExitCode,
	}
	if result.Success {
		move, err := stackkitcommand.ParsePaperworkMoveEvidence(&agentpb.StackKitCommand{
			Operation:   agentpb.StackKitOperation_STACKKIT_OPERATION_WALLET_ITEM_IMPORT,
			WorkloadRef: "paperwork-move", WalletItemRef: req.WalletItemID, DeliveryTenantRef: req.DeliveryTenantRef,
		}, result)
		if err != nil {
			return nil, err
		}
		status = move.State
		out["paperwork_move"] = move
	}
	out["status"] = status
	return out, nil
}

func authorizePaperworkMove(ctx context.Context, cfg StackKitLifecycleConfig, req StackKitLifecycleRequest) error {
	if req.PaperworkMoveServerID == "" || cfg.ApplicationAuthorize == nil || cfg.ServiceRuntimeAuthorize == nil {
		return errors.New("paperwork move operate authority is unavailable")
	}
	if err := cfg.ServiceRuntimeAuthorize(ctx, req, req.PaperworkMoveServerID); err != nil {
		return errors.New("paperwork move destination binding is no longer admitted")
	}
	if err := cfg.ApplicationAuthorize(ctx, req.TenantID, req.OwnerID, req.PaperworkMoveServerID); err != nil {
		return errors.New("paperwork move backend operate permit denied")
	}
	return nil
}
