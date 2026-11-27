package agent

import (
	"slices"
	"strconv"

	"github.com/kombifyio/homelab/internal/stackkitrelease"
	"github.com/kombifyio/homelab/pkg/api/agentpb"
	"github.com/kombifyio/homelab/pkg/stackkitcommand"
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
// release catalog; the release, not the Guard, owns the argv. Optional plan
// overrides (--http-port, --no-outbound, --memory-mib, --cpus) are rendered
// only when explicitly set so adopt runs with the exact options of its plan.
func stackKitApplicationStackArgs(release stackkitrelease.Release, c *agentpb.StackKitCommand) ([]string, error) {
	if err := stackkitcommand.ValidateCommand(c); err != nil {
		return nil, err
	}
	catalog, err := release.AdvancedOperations()
	if err != nil {
		return nil, err
	}
	operation := stackkitcommand.ApplicationStackOperationName(c.Operation)
	values := map[string]string{
		"containerId":        c.ApplicationContainerId,
		"ownApplicationName": c.OwnApplicationName,
		"planDigest":         c.ApplicationPlanDigest,
		"operationId":        c.ApplicationOperationId,
	}
	var literals []string
	if c.StackItHttpPort > 0 {
		values["httpPort"] = strconv.Itoa(int(c.StackItHttpPort))
	}
	if c.StackItNoOutbound {
		literals = append(literals, "--no-outbound")
	}
	if c.StackItMemoryMib > 0 {
		values["memoryMiB"] = strconv.Itoa(int(c.StackItMemoryMib))
	}
	if c.StackItCpus != "" {
		values["cpus"] = c.StackItCpus
	}
	if c.StackItAdoptComposeProject {
		literals = append(literals, "--adopt-compose-project")
	}
	if c.StackItHttpService != "" {
		values["httpService"] = c.StackItHttpService
	}
	args, err := catalog.RenderSelectedArgv(operation, values, literals...)
	if err != nil {
		return nil, err
	}
	if c.Operation == agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_STACK_PLAN || c.Operation == agentpb.StackKitOperation_STACKKIT_OPERATION_APPLICATION_STACK {
		// Older release catalogs (v0.57.10) expose --memory-mib, --cpus,
		// --adopt-compose-project and --http-service on
		// `stackkit app own --from-container` without listing them in optionalArgs.
		if c.StackItMemoryMib > 0 && !slices.Contains(args, "--memory-mib") {
			args = append(args, "--memory-mib", strconv.Itoa(int(c.StackItMemoryMib)))
		}
		if c.StackItCpus != "" && !slices.Contains(args, "--cpus") {
			args = append(args, "--cpus", c.StackItCpus)
		}
		if c.StackItAdoptComposeProject && !slices.Contains(args, "--adopt-compose-project") {
			args = append(args, "--adopt-compose-project")
		}
		if c.StackItHttpService != "" && !slices.Contains(args, "--http-service") {
			args = append(args, "--http-service", c.StackItHttpService)
		}
	}
	return args, nil
}
