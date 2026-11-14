package agent

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/kombifyio/techstack/internal/stackkitrelease"
	"github.com/kombifyio/techstack/pkg/api/agentpb"
	"github.com/kombifyio/techstack/pkg/stackkitcommand"
)

func stackKitConnectorArgs(release stackkitrelease.Release, workDir string, c *agentpb.StackKitCommand) ([]string, []string, error) {
	if err := stackkitcommand.ValidateCommand(c); err != nil {
		return nil, nil, err
	}
	catalog, err := release.AdvancedOperations()
	if err != nil {
		return nil, nil, err
	}
	op := stackkitcommand.ApplicationConnectorOperationName(c.Operation)
	if err = catalog.Dispatchable(op); err != nil {
		return nil, nil, err
	}
	dir := filepath.Join(workDir, ".stackkit")
	if err = os.MkdirAll(dir, 0750); err != nil {
		return nil, nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("connector metadata directory is not confined to its workspace")
	}
	path, err := writeStackKitPrivateFile(workDir, ".techstack-connector-*.json", c.ApplicationConnectorRequestJson)
	if err != nil {
		return nil, nil, err
	}
	files := []string{path}
	argv, err := catalog.RenderArgv(op, map[string]string{"connectorRequestFile": relativeStackKitPath(workDir, path), "operationId": c.ApplicationOperationId})
	if err != nil {
		removeStackKitTempFiles(files)
		return nil, nil, err
	}
	return argv, files, nil
}

// stackKitHomeGraphArgs renders catalog operation application.home-graph.sync.
// Both documents are private workspace files that the executor removes after
// the command; the graph is public household structure, the connector request
// is closed metadata, and the connector credential never leaves node custody.
func stackKitHomeGraphArgs(release stackkitrelease.Release, workDir string, c *agentpb.StackKitCommand) ([]string, []string, error) {
	if err := stackkitcommand.ValidateCommand(c); err != nil {
		return nil, nil, err
	}
	catalog, err := release.AdvancedOperations()
	if err != nil {
		return nil, nil, err
	}
	if err = catalog.Dispatchable(stackkitcommand.HomeGraphSyncOperationName); err != nil {
		return nil, nil, err
	}
	dir := filepath.Join(workDir, ".stackkit")
	if err = os.MkdirAll(dir, 0750); err != nil {
		return nil, nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("home graph metadata directory is not confined to its workspace")
	}
	connectorPath, err := writeStackKitPrivateFile(workDir, ".techstack-connector-*.json", c.ApplicationConnectorRequestJson)
	if err != nil {
		return nil, nil, err
	}
	files := []string{connectorPath}
	graphPath, err := writeStackKitPrivateFile(workDir, ".techstack-home-graph-*.json", c.HomeGraphSyncRequest)
	if err != nil {
		removeStackKitTempFiles(files)
		return nil, nil, err
	}
	files = append(files, graphPath)
	argv, err := catalog.RenderArgv(stackkitcommand.HomeGraphSyncOperationName, map[string]string{
		"connectorRequestFile": relativeStackKitPath(workDir, connectorPath),
		"operationId":          c.ApplicationOperationId,
		"homeGraphFile":        relativeStackKitPath(workDir, graphPath),
	})
	if err != nil {
		removeStackKitTempFiles(files)
		return nil, nil, err
	}
	return argv, files, nil
}

// stackKitHomeGraphLoxoneImportArgs renders catalog operation
// application.home-graph.loxone-import. The graph is a private workspace file
// the executor removes after the command. The Miniserver credential is the
// node's own workspace file the catalog argv names; it never travels.
func stackKitHomeGraphLoxoneImportArgs(release stackkitrelease.Release, workDir string, c *agentpb.StackKitCommand) ([]string, []string, error) {
	if err := stackkitcommand.ValidateCommand(c); err != nil {
		return nil, nil, err
	}
	catalog, err := release.AdvancedOperations()
	if err != nil {
		return nil, nil, err
	}
	if err = catalog.Dispatchable(stackkitcommand.HomeGraphLoxoneImportOperationName); err != nil {
		return nil, nil, err
	}
	dir := filepath.Join(workDir, ".stackkit")
	if err = os.MkdirAll(dir, 0750); err != nil {
		return nil, nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("home graph metadata directory is not confined to its workspace")
	}
	graphPath, err := writeStackKitPrivateFile(workDir, ".techstack-home-graph-*.json", c.HomeGraphDocument)
	if err != nil {
		return nil, nil, err
	}
	files := []string{graphPath}
	argv, err := catalog.RenderArgv(stackkitcommand.HomeGraphLoxoneImportOperationName, map[string]string{
		"homeGraphDocumentFile": relativeStackKitPath(workDir, graphPath),
	})
	if err != nil {
		removeStackKitTempFiles(files)
		return nil, nil, err
	}
	return argv, files, nil
}

func stackKitHomeGraphAdoptionArgs(release stackkitrelease.Release, workDir string, c *agentpb.StackKitCommand) ([]string, []string, error) {
	if err := stackkitcommand.ValidateCommand(c); err != nil {
		return nil, nil, err
	}
	catalog, err := release.AdvancedOperations()
	if err != nil {
		return nil, nil, err
	}
	if err = catalog.Dispatchable(stackkitcommand.HomeGraphAdoptionOperationName); err != nil {
		return nil, nil, err
	}
	dir := filepath.Join(workDir, ".stackkit")
	if err = os.MkdirAll(dir, 0750); err != nil {
		return nil, nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("home graph metadata directory is not confined to its workspace")
	}
	connectorPath, err := writeStackKitPrivateFile(workDir, ".techstack-connector-*.json", c.ApplicationConnectorRequestJson)
	if err != nil {
		return nil, nil, err
	}
	files := []string{connectorPath}
	graphPath, err := writeStackKitPrivateFile(workDir, ".techstack-home-graph-*.json", c.HomeGraphAdoptionRequest)
	if err != nil {
		removeStackKitTempFiles(files)
		return nil, nil, err
	}
	files = append(files, graphPath)
	argv, err := catalog.RenderArgv(stackkitcommand.HomeGraphAdoptionOperationName, map[string]string{
		"connectorRequestFile":  relativeStackKitPath(workDir, connectorPath),
		"operationId":           c.ApplicationOperationId,
		"homeGraphAdoptionFile": relativeStackKitPath(workDir, graphPath),
	})
	if err != nil {
		removeStackKitTempFiles(files)
		return nil, nil, err
	}
	return argv, files, nil
}
