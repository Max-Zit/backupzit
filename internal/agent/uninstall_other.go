//go:build !windows

package agent

import (
	"errors"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// uninstallerCommand removes the agent package outside the service's
// control group (systemd-run), so stopping the service does not stop the
// package manager. The agent's configuration and log stay (as with a
// normal package removal) until the package is purged.
func uninstallerCommand() (func() error, error) {
	script := uninstallScript(func(name string) bool { _, err := exec.LookPath(name); return err == nil },
		func(pkg string) bool { return exec.Command("dpkg", "-s", pkg).Run() == nil })
	if script == "" {
		return nil, errors.New("the agent was not installed from a deb or rpm package; uninstall it by hand")
	}
	return func() error {
		if _, err := exec.LookPath("systemd-run"); err == nil {
			unit := "backupzit-agent-uninstall-" + strconv.FormatInt(time.Now().Unix(), 10)
			return exec.Command("systemd-run", "--unit", unit, "--collect", "--quiet", "/bin/sh", "-c", script).Run()
		}
		cmd := exec.Command("/bin/sh", "-c", script)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}, nil
}

// uninstallScript is the package manager command for this system ("" if
// the agent is not a package).
func uninstallScript(has func(string) bool, debInstalled func(string) bool) string {
	switch {
	case has("dpkg") && debInstalled("backupzit-agent"):
		return "sleep 2; dpkg -r backupzit-agent >/var/tmp/backupzit-agent-uninstall.log 2>&1"
	case has("rpm"):
		return "sleep 2; rpm -q backupzit-agent >/dev/null 2>&1 && rpm -e backupzit-agent >/var/tmp/backupzit-agent-uninstall.log 2>&1"
	}
	return ""
}
