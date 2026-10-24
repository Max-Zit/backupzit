package agent

import (
	"errors"
	"os/exec"
	"regexp"
	"strings"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

var msiProductCode = regexp.MustCompile(`\{[0-9A-Fa-f-]{36}\}`)

// uninstallerCommand finds the agent's MSI product code in the list of
// installed programs and runs msiexec /x as an independent process.
func uninstallerCommand() (func() error, error) {
	code, err := agentProductCode()
	if err != nil {
		return nil, err
	}
	return func() error {
		cmd := exec.Command("msiexec.exe", "/x", code, "/qn", "/norestart")
		const detachedProcess, newProcessGroup = 0x00000008, 0x00000200
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | newProcessGroup}
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}, nil
}

func agentProductCode() (string, error) {
	for _, root := range []string{`SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`, `SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`} {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, root, registry.ENUMERATE_SUB_KEYS|registry.READ)
		if err != nil {
			continue
		}
		names, _ := k.ReadSubKeyNames(-1)
		k.Close()
		for _, n := range names {
			sk, err := registry.OpenKey(registry.LOCAL_MACHINE, root+`\`+n, registry.QUERY_VALUE)
			if err != nil {
				continue
			}
			name, _, _ := sk.GetStringValue("DisplayName")
			sk.Close()
			if strings.HasPrefix(name, "BackupZit Agent") && msiProductCode.MatchString(n) {
				return n, nil
			}
		}
	}
	return "", errors.New("the BackupZit Agent installation was not found in the installed programs; uninstall it by hand")
}
