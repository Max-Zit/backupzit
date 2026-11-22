package archiver

import (
	"path"
	"strings"
)

// Folders the system rebuilds or that hold no data of the machine are kept
// as empty folders when a folder above them is backed up, as the kernel
// file systems of Linux are (pseudoMounts): restoring them would bring back
// stale caches, swap or other disks, and reading them costs time and space.
// They are never skipped when they are a source of the job themselves
// (backing up /Volumes/USB or ~/.Trash on purpose reads it).
//
// The rules are data, matched against the original path: macOS only (see
// systemSkips); Linux and Windows backups read everything they did before.

// skipScope says below which roots a rule's path applies.
type skipScope int

const (
	// onSystem: the path as written, below / only (support volumes and
	// kernel file systems of the system volume).
	onSystem skipScope = iota
	// onData: below / and below /System/Volumes/Data, the data volume
	// that / shows through firmlinks (a job may name either).
	onData
	// onVolume: at the root of every volume: /, the data volume and each
	// disk mounted in /Volumes (one named as a source).
	onVolume
)

// skipRule is one folder kept empty, relative to the roots of its scope
// ("*" matches one path component).
type skipRule struct {
	path  string
	scope skipScope
	why   string
}

// macOSSkips are the folders kept empty in a macOS backup. The user's Trash
// is skipped like Time Machine does: it holds what the user deleted, can be
// large, and a file put there by mistake is usually taken back within days;
// a job that wants it lists ~/.Trash as a source.
var macOSSkips = []skipRule{
	{"Volumes", onData, "other disks, network shares, disk images and Time Machine disks mounted there (a job lists the ones to back up)"},
	{"System/Volumes/VM", onSystem, "swap files and the sleep image"},
	{"System/Volumes/Preboot", onSystem, "boot support volume of macOS (no user data; macOS itself is reinstalled, not restored)"},
	{"System/Volumes/Recovery", onSystem, "recoveryOS volume"},
	{"System/Volumes/Update", onSystem, "software update staging volume"},
	{"System/Volumes/xarts", onSystem, "Secure Enclave support volume"},
	{"System/Volumes/iSCPreboot", onSystem, "boot support volume (Apple silicon)"},
	{"System/Volumes/Hardware", onSystem, "hardware support volume (Apple silicon)"},
	{"System/Volumes/BaseSystem", onSystem, "recoveryOS base system"},
	{"System/Volumes/FieldService", onSystem, "Apple service volume"},
	{"System/Volumes/FieldServiceDiagnostic", onSystem, "Apple service volume"},
	{"System/Volumes/FieldServiceRepair", onSystem, "Apple service volume"},
	{"dev", onSystem, "device file system (devfs), rebuilt at boot"},
	{"private/var/vm", onData, "swap files and the sleep image (older layout)"},
	{"private/var/folders", onData, "per-user temporary files and caches"},
	{"private/tmp", onData, "temporary files, emptied at boot"},
	{"cores", onData, "core dumps of crashed programs"},
	{"Library/Caches", onData, "caches of the system, rebuilt on demand"},
	{"System/Library/Caches", onData, "caches of the system, rebuilt on demand"},
	{"private/var/root/Library/Caches", onData, "caches of root, rebuilt on demand"},
	{"Users/*/Library/Caches", onData, "caches of each user's apps, rebuilt on demand"},
	{"Users/*/.Trash", onData, "the user's Trash (see macOSSkips)"},
	{".Spotlight-V100", onVolume, "Spotlight index, rebuilt by the system"},
	{".fseventsd", onVolume, "file system event log of the volume"},
	{".DocumentRevisions-V100", onVolume, "version store of documents, only usable by the system that wrote it"},
	{".MobileBackups", onVolume, "local Time Machine snapshots (older macOS)"},
	{".MobileBackups.trash", onVolume, "local Time Machine snapshots being removed"},
	{".TemporaryItems", onVolume, "temporary files of the volume"},
	{".Trashes", onVolume, "Trash of the volume (see macOSSkips)"},
	{"Backups.backupdb", onVolume, "Time Machine backups on an HFS+ disk"},
}

// dataVolume is where macOS mounts the data volume that / shows through
// firmlinks.
const dataVolume = "/System/Volumes/Data"

// skipPatterns turns rules into full-path patterns for matchPath.
func skipPatterns(rules []skipRule) []string {
	var out []string
	for _, r := range rules {
		roots := []string{"/"}
		switch r.scope {
		case onData:
			roots = append(roots, dataVolume)
		case onVolume:
			roots = append(roots, dataVolume, "/Volumes/*")
		}
		for _, root := range roots {
			out = append(out, path.Join(root, r.path))
		}
	}
	return out
}

// firmlinkDuplicates lists the folders of the data volume that a backup
// already reads through their firmlink: with / as a source, /Users and
// /System/Volumes/Data/Users are the same folder, read and stored twice.
// Only those whose firmlink path is covered by a root are listed, so a job
// that backs up just /System/Volumes/Data still reads all of it. firmlinks
// is the content of /usr/share/firmlinks ("/Users<TAB>Users" per line).
func firmlinkDuplicates(firmlinks string, roots []string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(firmlinks, "\n") {
		link, rel, ok := strings.Cut(strings.TrimSpace(line), "\t")
		rel = strings.Trim(strings.TrimSpace(rel), "/")
		if !ok || !strings.HasPrefix(link, "/") || rel == "" || strings.Contains(rel, "..") {
			continue
		}
		if coveredBy(path.Clean(link), roots) {
			out[path.Join(dataVolume, rel)] = true
		}
	}
	return out
}

// coveredBy tells whether p is one of roots or below one of them.
func coveredBy(p string, roots []string) bool {
	for _, r := range roots {
		r = path.Clean(r)
		if p == r || r == "/" || strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return false
}

// skipSet is what systemSkips found for one backup run, in lower case:
// macOS volumes are case-insensitive by default, and a job may name
// /users/bob for /Users/bob.
type skipSet struct {
	patterns []string
	exact    map[string]bool
}

// newSkipSet builds the set of patterns and exact paths.
func newSkipSet(patterns []string, exact map[string]bool) *skipSet {
	s := &skipSet{exact: make(map[string]bool, len(exact))}
	for _, p := range patterns {
		s.patterns = append(s.patterns, strings.ToLower(p))
	}
	for p := range exact {
		s.exact[strings.ToLower(path.Clean(p))] = true
	}
	return s
}

// match tells whether p is kept empty.
func (s *skipSet) match(p string) bool {
	if s == nil || (len(s.patterns) == 0 && len(s.exact) == 0) {
		return false
	}
	lp := strings.ToLower(p)
	if s.exact[path.Clean(lp)] {
		return true
	}
	for _, pat := range s.patterns {
		if matchPath(pat, lp) {
			return true
		}
	}
	return false
}
