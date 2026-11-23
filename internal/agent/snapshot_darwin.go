package agent

import (
	"path/filepath"
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

// permissionWarning returns the warning of a missing permission for a
// backup of paths ("" when granted or undetermined, or when none of the
// paths reaches a place macOS protects: no false alarm on every run of a
// job that backs up, say, /opt/data).
func permissionWarning(paths []string) string {
	if !reachesProtected(paths) || fullDiskAccess() != macperm.Denied {
		return ""
	}
	return fullDiskAccessWarning
}

// dataVolume is where the folders of the data volume are reached without
// their firmlinks (/Users is /System/Volumes/Data/Users).
const dataVolume = "/System/Volumes/Data"

// protectedRoots are the folders whose contents macOS keeps from a process
// without Full Disk Access: the users' folders (Desktop, Documents, Mail,
// Messages, Safari, other apps' data), the apps' data of the system and
// removable and network volumes.
var protectedRoots = []string{"/Users", "/Library", "/Volumes"}

// reachesProtected reports whether a backup of paths reads below one of
// protectedRoots: a path inside one (except /Users/Shared, open to every
// user), or above one, such as "/".
func reachesProtected(paths []string) bool {
	for _, p := range paths {
		p = filepath.Clean(p)
		if rest, ok := strings.CutPrefix(p, dataVolume); ok && (rest == "" || rest[0] == '/') {
			p = "/" + strings.TrimPrefix(rest, "/")
		}
		for _, root := range protectedRoots {
			switch {
			case p == root || p == "/" || strings.HasPrefix(root, p+"/"):
				return true
			case strings.HasPrefix(p, root+"/"):
				if root == "/Users" && (p == "/Users/Shared" || strings.HasPrefix(p, "/Users/Shared/")) {
					continue
				}
				return true
			}
		}
	}
	return false
}
