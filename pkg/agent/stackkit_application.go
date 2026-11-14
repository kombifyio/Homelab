package agent

import (
	"github.com/kombifyio/techstack/internal/stackkitrelease"
	"github.com/kombifyio/techstack/pkg/api/agentpb"
	"github.com/kombifyio/techstack/pkg/stackkitcommand"
)

func (executor *StackKitExecutor) ApplicationCapabilities() []string {
	if !executor.Available() {
		return nil
	}
	release, err := executor.releaseCache().ResolvePin(executor.pinPath)
	if err != nil {
		return nil
	}
	catalog, err := release.AdvancedOperations()
	if err != nil {
		return nil
	}
	var capabilities []string
	adoption := true
	for _, op := range []string{"application.inspect", "application.adopt", "application.verify", "application.release", "application.control"} {
		if catalog.Dispatchable(op) != nil {
			adoption = false
		}
	}
	if adoption {
		capabilities = append(capabilities, stackkitcommand.ApplicationAdoptionCapability)
	}
	stack := stackkitcommand.ReleaseAtLeast(release.Receipt().Version, stackkitcommand.MinApplicationStackRelease)
	for _, op := range stackkitcommand.ApplicationStackOperations {
		if catalog.Dispatchable(op) != nil {
			stack = false
		}
	}
	if stack {
		capabilities = append(capabilities, stackkitcommand.ApplicationStackCapability)
	}
	if stack && ownApplicationLifecycleAvailable(catalog) {
		capabilities = append(capabilities, stackkitcommand.OwnApplicationCapability)
	}
	if catalog.Dispatchable("application.connector") == nil && catalog.Dispatchable("application.connector.verify") == nil {
		capabilities = append(capabilities, stackkitcommand.ApplicationConnectorCapability)
	}
	return capabilities
}
func stackKitApplicationArgs(release stackkitrelease.Release, c *agentpb.StackKitCommand) ([]string, error) {
	if err := stackkitcommand.ValidateCommand(c); err != nil {
		return nil, err
	}
	catalog, err := release.AdvancedOperations()
	if err != nil {
		return nil, err
	}
	args, err := catalog.RenderArgv(stackkitcommand.ApplicationOperationName(c.Operation), applicationArgValues(c))
	if err != nil {
		return nil, err
	}
	// A governed start, stop or restart of an adopted application carries
	// the contract hash of its own primitive; the CLI refuses another.
	hashArgs, err := stackKitRILPrimitiveHashArgs(c)
	if err != nil {
		return nil, err
	}
	return append(args, hashArgs...), nil
}

func applicationArgValues(c *agentpb.StackKitCommand) map[string]string {
	return map[string]string{"workloadRef": c.ServiceKey, "containerId": c.ApplicationContainerId, "planDigest": c.ApplicationPlanDigest, "operationId": c.ApplicationOperationId, "applicationAction": c.ApplicationAction}
}

// stackKitApplicationStackArgs renders one "Stack it" phase from the pinned
// release catalog; the release, not the Guard, owns the argv.
func stackKitApplicationStackArgs(release stackkitrelease.Release, c *agentpb.StackKitCommand) ([]string, error) {
	if err := stackkitcommand.ValidateCommand(c); err != nil {
		return nil, err
	}
	catalog, err := release.AdvancedOperations()
	if err != nil {
		return nil, err
	}
	// No plan option is selected: adopt must run with the options of its plan,
	// and the literal --no-outbound option must never render by default.
	return catalog.RenderRequiredArgv(stackkitcommand.ApplicationStackOperationName(c.Operation), map[string]string{"containerId": c.ApplicationContainerId, "ownApplicationName": c.OwnApplicationName, "planDigest": c.ApplicationPlanDigest, "operationId": c.ApplicationOperationId})
}
