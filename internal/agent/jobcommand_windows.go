package agent

import (
	"os/exec"
	"strconv"
)

// killTree makes cancelling cmd stop the processes it started too.
func killTree(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run() // #nosec G204 -- fixed program; arguments are separate values, no shell
		return cmd.Process.Kill()
	}
}
