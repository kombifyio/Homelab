package guardexec

import "github.com/kombifyio/homelab/pkg/guardlayout"

// Default layout of a kombify Guard host. install.sh and the layout migration
// create it; every path is also overridable through the executor's flags.
const (
	// DefaultSocket is where systemd listens for the executor.
	DefaultSocket = guardlayout.DefaultSocketPath
	// GuardUser is the dedicated system user of the unprivileged daemon.
	GuardUser = guardlayout.DefaultUser
	// AgentUnit is the daemon's systemd service.
	AgentUnit = guardlayout.AgentUnit
	// DefaultStateDir is the root-only executor state: verified manifest, trial,
	// replay records and the recovery flag.
	DefaultStateDir = guardlayout.ExecutorStateDir
	// DefaultHandoffDir is the only directory both sides write: downloaded
	// artifacts, release evidence and non-secret update status.
	DefaultHandoffDir = guardlayout.HandoffDir
	// DefaultAgentPath is the installed Guard executable.
	DefaultAgentPath = guardlayout.DefaultBinaryPath
	// DefaultOperationsPath is the StackKits operations executable, a copy of the
	// Guard executable.
	DefaultOperationsPath = guardlayout.DefaultOperationsPath
	// DefaultOperationsSocket is where the daemon serves StackKits operations
	// requests to the operations executable, so the credential never leaves it.
	DefaultOperationsSocket = guardlayout.RuntimeDir + "/operations.sock"
)
