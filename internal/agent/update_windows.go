package agent

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// installerCommand runs the MSI as an independent process: Windows Installer
// stops this service, replaces the files and starts the service again.
func installerCommand(path string) (func() error, error) {
	if !strings.EqualFold(filepath.Ext(path), ".msi") {
		return nil, fmt.Errorf("%s is not an MSI installer", filepath.Base(path))
	}
	return func() error {
		logFile := strings.TrimSuffix(path, filepath.Ext(path)) + ".log"
		cmd := exec.Command("msiexec.exe", "/i", path, "/qn", "/norestart", "/l*v", logFile)
		const detachedProcess, newProcessGroup = 0x00000008, 0x00000200
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | newProcessGroup}
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}, nil
}
