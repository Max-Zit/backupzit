//go:build !windows && !darwin

// Command backupzit-tray is the Windows notification area and macOS menu
// bar app of the agent. On other systems use "backupzit-agent status".
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "backupzit-tray is only available on Windows and macOS; use \"backupzit-agent status\"")
	os.Exit(1)
}
