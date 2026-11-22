//go:build !darwin

package agent

import (
	"runtime"
	"strings"
)

// readsSnapshots: files backups read from VSS snapshots on Windows.
const readsSnapshots = runtime.GOOS == "windows"

// snapshotMessage tells in the run report which volumes were read from a
// snapshot.
func snapshotMessage(volumes []string) string {
	return "Read from VSS snapshot of " + strings.Join(volumes, ", ")
}

// permissionWarning: only macOS has permissions the user must grant.
func permissionWarning([]string) string { return "" }
