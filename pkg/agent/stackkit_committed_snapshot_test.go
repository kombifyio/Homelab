package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/kombifyio/homelab/pkg/api/agentpb"
	"github.com/kombifyio/homelab/pkg/stackkitcommand"
)

func TestCommittedBaselineObservationBindsTheExactWorkspace(t *testing.T) {
	workspace := t.TempDir()
	raw := []byte(`{"metadata":{"stackId":"stack"},"workloads":{}}`)
	if err := os.WriteFile(filepath.Join(workspace, "stack-spec.yaml"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	command := &agentpb.StackKitCommand{SpecPath: "stack-spec.yaml"}
	result := &agentpb.StackKitResult{Success: true, WorkspaceRevision: "sha256:" + hex.EncodeToString(sum[:]), CommandResultJson: []byte(`{"data":{}}`)}
	if err := attachCommittedStackSpecSnapshot(workspace, command, result); err != nil {
		t.Fatal(err)
	}
	if _, err := stackkitcommand.CommittedBaselineSpec(result); err != nil {
		t.Fatal(err)
	}
	result.WorkspaceRevision = "sha256:foreign"
	if err := attachCommittedStackSpecSnapshot(workspace, command, result); err == nil {
		t.Fatal("foreign workspace revision admitted")
	}
	if err := os.Remove(filepath.Join(workspace, "stack-spec.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(workspace, "outside"), filepath.Join(workspace, "stack-spec.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := attachCommittedStackSpecSnapshot(workspace, command, result); err == nil {
		t.Fatal("symlink admitted as baseline")
	}
}
