package agent

import (
	"os/exec"
	"strconv"
)

// killTree makes cancelling cmd stop the processes it started too.
func killTree(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		return cmd.Process.Kill()
	}
}
