//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// killTree makes cancelling cmd stop the processes it started too.
func killTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
