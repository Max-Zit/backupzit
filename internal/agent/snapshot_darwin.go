package agent

import (
	"strings"

	"github.com/max-zit/backupzit/internal/macperm"
)

// readsSnapshots: files backups on a Mac read from an APFS local snapshot
// (apfssnap), so files a program keeps writing are consistent.
const readsSnapshots = true

// snapshotMessage tells in the run report which volume was read from a
// snapshot.
func snapshotMessage(volumes []string) string {
	return "Read from an APFS local snapshot of " + strings.Join(volumes, ", ") + "."
}

// fullDiskAccess is replaced by tests.
var fullDiskAccess = macperm.FullDiskAccess

// fullDiskAccessWarning is the run report's warning when the agent may not
// read the folders macOS protects: the backup is incomplete without anyone
// noticing otherwise. The permission can only be granted by the user; an
// update of an unsigned agent may also take it away.
const fullDiskAccessWarning = "Full Disk Access is not granted to the BackupZit agent: folders macOS protects (Mail, Messages, Safari, Desktop, Documents, other apps' data) are skipped. Grant it in System Settings > Privacy & Security > Full Disk Access."

// permissionWarning returns the warning of a missing permission ("" when
// granted or undetermined: no false alarm).
func permissionWarning() string {
	if fullDiskAccess() == macperm.Denied {
		return fullDiskAccessWarning
	}
	return ""
}
