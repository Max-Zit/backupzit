package archiver

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/max-zit/backupzit/internal/repo"
)

// Files whose content is only in the cloud: iCloud Drive keeps "dataless"
// files on a Mac with Optimize Mac Storage (SF_DATALESS), OneDrive Files
// On-Demand keeps "online-only" placeholders on Windows
// (FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS). Their content is not on the
// disk: a snapshot cannot hold it, and a live read either downloads it in
// the owner's session or fails. Such a file is never opened. If the
// previous backup has it unchanged, its content is kept from there (a file
// freed up after a backup stays in the backups). Otherwise it is counted per
// folder and left out of the snapshot: it is never stored as an empty file,
// which a restore could write over the real one.

// errCloudOnly marks a file left out because its content is only in the
// cloud; it is counted in the stats, not listed as an error.
var errCloudOnly = errors.New("content only in the cloud")

// cloudOnlyFolder is the folder a cloud-only file at p is summed up under:
// the source it was found below or, when the source is above a user's
// profile (/, /Users), the profile's top folder (/Users/anna/Documents,
// C:\Users\anna\OneDrive).
func (a *Archiver) cloudOnlyFolder(p string) string {
	root := ""
	for _, r := range a.roots {
		if within(p, r) && len(r) > len(root) {
			root = r
		}
	}
	if root == "" {
		return filepath.Dir(p)
	}
	if prof := profileFolder(p); len(prof) > len(root) && within(prof, root) {
		return prof
	}
	return root
}

// within tells whether p is root or below it.
func within(p, root string) bool {
	if p == root {
		return true
	}
	sep := string(filepath.Separator)
	if !strings.HasSuffix(root, sep) {
		root += sep
	}
	return strings.HasPrefix(p, root)
}

// profileFolder is the top folder of the user profile that holds the file
// p (Users/<name>/<folder> at the root of its volume), or the profile
// itself for a file directly in it; "" outside a profile.
func profileFolder(p string) string {
	vol := filepath.VolumeName(p)
	sep := string(filepath.Separator)
	rest := strings.TrimPrefix(p[len(vol):], sep)
	parts := strings.Split(rest, sep)
	// Users, the name, the folder, and the file itself.
	if len(parts) < 3 || !strings.EqualFold(parts[0], "Users") {
		return ""
	}
	n := min(3, len(parts)-1)
	return vol + sep + strings.Join(parts[:n], sep)
}

// skipCloudOnly records the file p of size bytes as cloud-only and takes
// it out of the counts archiveFile already made; read tells whether it was
// counted as a new or changed file (a read was tried).
func (a *Archiver) skipCloudOnly(p string, size uint64, read, changed bool) error {
	a.stats.Files--
	a.stats.Bytes -= size
	if read {
		if changed {
			a.stats.FilesChanged--
		} else {
			a.stats.FilesNew--
		}
	}
	a.stats.CloudOnlyFiles++
	a.stats.CloudOnlyBytes += size
	dir := a.cloudOnlyFolder(p)
	for i := range a.stats.CloudOnly {
		if f := &a.stats.CloudOnly[i]; f.Path == dir {
			f.Files++
			f.Bytes += size
			return errCloudOnly
		}
	}
	if len(a.stats.CloudOnly) < repo.MaxCloudOnlyFolders {
		a.stats.CloudOnly = append(a.stats.CloudOnly, repo.CloudOnlyFolder{Path: dir, Files: 1, Bytes: size})
	}
	return errCloudOnly
}

// inCloud and inCloudLive are the platform checks (cloud_*.go); tests
// replace them, since no CI runner can make such a file.
var (
	// inCloud tells from the entry's stat (of the snapshot, when the file
	// is read from one) whether its content is only in the cloud.
	inCloud = entryInCloud
	// inCloudLive checks the live file p, after a read of the snapshot
	// failed: a snapshot may not carry the flag.
	inCloudLive = pathInCloud
)
