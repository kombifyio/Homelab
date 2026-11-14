package jobs

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/kombifyio/techstack/pkg/controlplane"
)

// NodeUpdateSettingReader reads the update setting of the agent's worker
// (ask, automatic or with_stackkits_releases; D-2026-11-12-06).
type NodeUpdateSettingReader func(ctx context.Context, tenantID, agentID string) (string, error)

var nodeUpdateSettingReader atomic.Pointer[NodeUpdateSettingReader]

// SetNodeUpdateSettingReader installs the node setting source; nil removes it.
func SetNodeUpdateSettingReader(reader NodeUpdateSettingReader) {
	if reader == nil {
		nodeUpdateSettingReader.Store(nil)
		return
	}
	nodeUpdateSettingReader.Store(&reader)
}

// ReadNodeUpdateSetting returns the node's update setting. ok is false when no
// source is installed or the read failed; a node without a stored setting
// asks.
func ReadNodeUpdateSetting(ctx context.Context, tenantID, agentID string) (string, bool) {
	reader := nodeUpdateSettingReader.Load()
	if reader == nil || strings.TrimSpace(tenantID) == "" || strings.TrimSpace(agentID) == "" {
		return "", false
	}
	setting, err := (*reader)(ctx, tenantID, agentID)
	if err != nil || !controlplane.ValidStackKitsUpdateBehavior(setting) {
		return "", false
	}
	return setting, true
}
