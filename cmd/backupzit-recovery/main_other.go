//go:build !windows

// Command backupzit-recovery is the graphical program of the Windows
// recovery environment. It runs on Windows (PE) only; on Linux the
// recovery ISO uses the agent's console mode and command line.
package main

import (
	"fmt"
	"os"
)

func systemLang() string { return "en" }

func main() {
	fmt.Fprintln(os.Stderr, "backupzit-recovery runs on Windows only; use backupzit-agent on this system")
	os.Exit(1)
}
