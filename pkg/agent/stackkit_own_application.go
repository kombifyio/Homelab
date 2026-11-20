package agent

import (
	"fmt"

	"github.com/kombifyio/homelab/internal/stackkitrelease"
	"github.com/kombifyio/homelab/pkg/api/agentpb"
	"github.com/kombifyio/homelab/pkg/stackkitcommand"
)

// ownApplicationLifecycleAvailable reports a pinned release whose catalog
// dispatches every own-application.* operation Homelab sends.
func ownApplicationLifecycleAvailable(catalog stackkitrelease.AdvancedOperationsCatalog) bool {
	for _, operation := range stackkitcommand.OwnApplicationOperations() {
		if catalog.Dispatchable(operation) != nil {
			return false
		}
	}
	return true
}

// writeCommandCapability writes the command's scoped capability to a private
// 0600 file in the workspace; the caller removes it after the command.
func writeCommandCapability(workDir string, command *agentpb.StackKitCommand) (string, []string, error) {
	if len(command.AdvancedCapability) == 0 {
		return "", nil, nil
	}
	if len(command.AdvancedCapability) > stackkitcommand.MaxAdvancedCapabilityBytes {
		return "", nil, fmt.Errorf("StackKit capability exceeds %d bytes", stackkitcommand.MaxAdvancedCapabilityBytes)
	}
	path, err := writeStackKitPrivateFile(workDir, ".homelab-capability-*.json", command.AdvancedCapability)
	if err != nil {
		return "", nil, fmt.Errorf("write StackKit capability: %w", err)
	}
	return relativeStackKitPath(workDir, path), []string{path}, nil
}

// stackKitApplicationUpdateArgs renders the single-application update from
// the pinned release's application-release.update catalog entry, with the
// application-release.update capability in a private file.
func stackKitApplicationUpdateArgs(release stackkitrelease.Release, workDir string, command *agentpb.StackKitCommand) ([]string, []string, error) {
	if err := stackkitcommand.ValidateCommand(command); err != nil {
		return nil, nil, err
	}
	catalog, err := release.AdvancedOperations()
	if err != nil {
		return nil, nil, err
	}
	if err := catalog.Dispatchable(stackkitcommand.ApplicationUpdateOperation); err != nil {
		return nil, nil, err
	}
	capability, files, err := writeCommandCapability(workDir, command)
	if err != nil {
		return nil, nil, err
	}
	values, err := stackkitcommand.ApplicationUpdateArgValues(command, capability)
	if err == nil {
		var args []string
		if args, err = catalog.RenderRequiredArgv(stackkitcommand.ApplicationUpdateOperation, values); err == nil {
			// The catalog requires the flag whenever the capability names an
			// untested release; it is the Owner's acknowledgement.
			if command.AcceptUntested {
				args = append(args, "--accept-untested")
			}
			return args, files, nil
		}
	}
	removeStackKitTempFiles(files)
	return nil, nil, err
}

// stackKitOwnApplicationArgs renders one own-application operation from the
// pinned release catalog: the required argv, the capability file of an
// execution and, for a tag update the review admitted, --allow-major.
func stackKitOwnApplicationArgs(release stackkitrelease.Release, workDir string, command *agentpb.StackKitCommand) ([]string, []string, error) {
	if err := stackkitcommand.ValidateCommand(command); err != nil {
		return nil, nil, err
	}
	catalog, err := release.AdvancedOperations()
	if err != nil {
		return nil, nil, err
	}
	operation := stackkitcommand.OwnApplicationCatalogOperation(command)
	if err := catalog.Dispatchable(operation); err != nil {
		return nil, nil, err
	}
	capability, files, err := writeCommandCapability(workDir, command)
	if err != nil {
		return nil, nil, err
	}
	// The capability renders through the catalog's own optional
	// --capability {capabilityFile} group (an execution only: reads carry no
	// file); --allow-major only when the review admitted it.
	var literals []string
	if command.OwnApplicationAllowMajor {
		literals = append(literals, "--allow-major")
	}
	args, err := catalog.RenderSelectedArgv(operation, stackkitcommand.OwnApplicationArgValues(command, capability), literals...)
	if err != nil {
		removeStackKitTempFiles(files)
		return nil, nil, err
	}
	if command.OwnApplicationAllowMajor && !catalog.DeclaresOptional(operation, "--allow-major") {
		// The tag and --allow-major are part of the review digest, so the dry
		// run carries the flag its execution takes. The pinned catalog lists it
		// on own-application.update only; the review takes it only when that
		// execution declares it.
		if operation != "own-application.update.review" || !catalog.DeclaresOptional("own-application.update", "--allow-major") {
			removeStackKitTempFiles(files)
			return nil, nil, fmt.Errorf("the pinned StackKits release does not declare --allow-major for %s", operation)
		}
		args = append(args, "--allow-major")
	}
	return args, files, nil
}
