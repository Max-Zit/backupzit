package agent

import (
	"fmt"
	"strings"

	"github.com/max-zit/backupzit/internal/repo"
)

// cloudOnlyFoldersShown is how many folders the run message names; the run
// page and the mail list all of SnapshotStats.CloudOnly.
const cloudOnlyFoldersShown = 5

// cloudOnlyMessage tells which files of the backup are only in the cloud
// (archiver: not opened, not stored), where, and what the user can do.
func cloudOnlyMessage(goos string, st repo.SnapshotStats) string {
	files := count(int(st.CloudOnlyFiles), "file", "files") // #nosec G115 -- a file count fits
	are, their := "are", "their content is"
	if st.CloudOnlyFiles == 1 {
		are, their = "is", "its content is"
	}
	var where string
	switch goos {
	case "darwin":
		where = fmt.Sprintf("%s (%s) %s in iCloud Drive but not downloaded on this Mac (Optimize Mac Storage): %s not in this backup.", files, humanSize(st.CloudOnlyBytes), are, their)
	case "windows":
		where = fmt.Sprintf("%s (%s) %s online-only (OneDrive Files On-Demand or another cloud sync app) and not on this computer: %s not in this backup.", files, humanSize(st.CloudOnlyBytes), are, their)
	default:
		where = fmt.Sprintf("%s (%s) %s only in the cloud and not on this computer: %s not in this backup.", files, humanSize(st.CloudOnlyBytes), are, their)
	}
	var b strings.Builder
	b.WriteString(where)
	listed := uint64(0)
	for i, f := range st.CloudOnly {
		if i == cloudOnlyFoldersShown {
			break
		}
		if i == 0 {
			b.WriteString(" In ")
		} else {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s: %s (%s)", f.Path, count(int(f.Files), "file", "files"), humanSize(f.Bytes)) // #nosec G115 -- a file count fits
		listed += f.Files
	}
	if listed > 0 {
		if rest := st.CloudOnlyFiles - listed; rest > 0 {
			fmt.Fprintf(&b, "; %s in other folders", count(int(rest), "file", "files")) // #nosec G115 -- a file count fits
		}
		b.WriteString(".")
	}
	switch goos {
	case "darwin":
		b.WriteString(" Download such files on the Mac (Finder: Download Now) or turn off Optimize Mac Storage. Files that are already in a backup keep their content there.")
	case "windows":
		b.WriteString(" Keep such files on this computer (Explorer: Always keep on this device); OneDrive for work or school can also be backed up from the cloud with a Microsoft 365 job. Files that are already in a backup keep their content there.")
	}
	return b.String()
}
