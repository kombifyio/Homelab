package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kombifyio/homelab/pkg/api/agentpb"
	"gopkg.in/yaml.v3"
)

// Capture the exact regular baseline file while the existing workspace lock is
// held. The control plane attributes this document to its committed catalog;
// the Guard observation alone cannot create a committed desired snapshot.
func attachCommittedStackSpecSnapshot(workspace string, command *agentpb.StackKitCommand, result *agentpb.StackKitResult) error {
	relative, err := cleanStackKitRelativePath(command.SpecPath, "stack-spec.yaml", "spec_path")
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return err
	}
	defer root.Close()
	parts := strings.Split(filepath.ToSlash(relative), "/")
	for i := range parts {
		info, err := root.Lstat(filepath.Join(parts[:i+1]...))
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("committed baseline path is not confined")
		}
	}
	file, err := root.Open(relative)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxStackKitWorkspaceSpecBytes {
		return fmt.Errorf("committed baseline is not a bounded regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxStackKitWorkspaceSpecBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > maxStackKitWorkspaceSpecBytes || int64(len(raw)) != info.Size() {
		return fmt.Errorf("committed baseline changed during read")
	}
	sum := sha256.Sum256(raw)
	if result.WorkspaceRevision != "sha256:"+hex.EncodeToString(sum[:]) {
		return fmt.Errorf("committed baseline differs from the command workspace revision")
	}
	var document map[string]any
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return err
	}
	if document == nil {
		return fmt.Errorf("committed baseline must be an object")
	}
	canonical, err := json.Marshal(document)
	if err != nil {
		return err
	}
	var envelope map[string]json.RawMessage
	if err = json.Unmarshal(result.CommandResultJson, &envelope); err != nil {
		return err
	}
	if envelope == nil {
		return fmt.Errorf("committed baseline requires a command result object")
	}
	envelope["homelab_committed_baseline"] = canonical
	result.CommandResultJson, err = json.Marshal(envelope)
	return err
}
