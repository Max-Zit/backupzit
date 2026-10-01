//go:build windows

package agent

import (
	"fmt"

	"golang.org/x/sys/windows/registry"
)

// osDescription returns e.g. "Windows Server 2022 Standard (10.0.20348)".
func osDescription() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return "windows"
	}
	defer k.Close()
	name, _, _ := k.GetStringValue("ProductName")
	build, _, _ := k.GetStringValue("CurrentBuildNumber")
	if name == "" {
		return "windows"
	}
	// Windows 11 still reports "Windows 10" in ProductName.
	if len(build) >= 5 && build >= "22000" && len(name) >= 10 && name[:10] == "Windows 10" {
		name = "Windows 11" + name[10:]
	}
	return fmt.Sprintf("%s (build %s)", name, build)
}
