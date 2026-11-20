package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/kombifyio/homelab/pkg/controlplane"
)

func (o *Orchestrator) requestHostInventory(ctx context.Context, tenantID, serverID, stackID string, deadline time.Time) error {
	requester, ok := o.canonicalServerRuntimeStore().(controlplane.ServerInventoryRefreshRequester)
	if !ok {
		return fmt.Errorf("canonical server inventory refresh is unavailable")
	}
	return requester.RequestServerInventoryRefresh(ctx, tenantID, serverID, stackID, deadline)
}
