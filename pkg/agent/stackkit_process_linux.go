package agent

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func configureStackKitProcess(cmd *exec.Cmd) {
	// The CLI and its ordinary children share a private process group. Killing
	// only the CLI can leave a child mutating and keeping stdout/stderr open,
	// preventing the agent from returning its bounded failure result.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	// Bound inherited-pipe completion inside the existing result grace even
	// when a descendant has deliberately left the process group.
	cmd.WaitDelay = time.Second
}

// The move CLI handles TERM by cancelling its pipe protocol and removing its
// exactly owned daemon container. Killing its process group immediately would
// prevent that cleanup; WaitDelay still bounds an unresponsive CLI.
func configurePaperworkMoveProcess(cmd *exec.Cmd) {
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 20 * time.Second
}
