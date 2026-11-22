// Package apfssnap reads macOS file backups from an APFS local snapshot of
// the startup disk's data volume, the macOS counterpart of VSS: the backup
// sees every file as of one moment, also those a program keeps writing.
//
// The snapshot is made with "tmutil localsnapshot" (no root needed), mounted
// read-only in a private temporary folder with mount_apfs (root and Full
// Disk Access needed: macOS refuses the mount to root without it) and
// deleted with "tmutil deletelocalsnapshots" after the backup. Files live on
// the data volume (/System/Volumes/Data) and macOS shows most of them at
// their usual place through firmlinks (/Users is /System/Volumes/Data/Users),
// so Map translates a path the user knows into the same file inside the
// mounted snapshot; the backup keeps storing the original path, which is
// what restores and the console show.
//
// Paths on other volumes (external disks and network shares under /Volumes)
// are read live and reported, as is everything when the snapshot fails.
package apfssnap

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/max-zit/backupzit/internal/leftover"
	"github.com/max-zit/backupzit/internal/macperm"
)

// DataVolume is where macOS mounts the startup disk's data volume.
const DataVolume = "/System/Volumes/Data"

// systemVolume is the sealed, read-only system volume: it cannot change
// during a backup, so reading it live is as consistent as a snapshot.
const systemVolume = "/"

// Tools the provider runs, by absolute path so a PATH of the service
// cannot substitute them.
const (
	tmutil    = "/usr/bin/tmutil"
	mountAPFS = "/sbin/mount_apfs"
	umount    = "/sbin/umount"
)

// ErrorHandler receives non-fatal errors (the archiver lists them in the
// run report).
type ErrorHandler func(item string, err error)

// Runner runs a command and returns its combined output. Tests replace it,
// so the command lines, the order of the steps and the fallbacks are
// checked without root.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// system is everything the provider asks the machine; tests swap it.
type system struct {
	run Runner
	// mountOf returns the mount point of the file system holding p.
	mountOf func(p string) (string, error)
	// mkdirTemp makes the private folder the snapshot is mounted on.
	mkdirTemp func() (string, error)
	// resolve follows the symbolic links of a path (the temporary folder
	// is below /tmp or /var, links to /private/...).
	resolve func(string) (string, error)
	// removeDir removes that folder once nothing is mounted on it.
	removeDir func(string) error
	// firmlinks returns the firmlinks of the system volume.
	firmlinks func() []firmlink
	// cleanupTimeout bounds unmounting and deleting: the run may be
	// cancelled, its clean-up still has to finish.
	cleanupTimeout time.Duration
	// retryWait is the pause before checking a deletion again.
	retryWait time.Duration
	// fullDiskAccess tells whether this process has Full Disk Access, and
	// isRoot whether it runs as root: both decide whether a snapshot can
	// be mounted at all.
	fullDiskAccess func() macperm.State
	isRoot         func() bool
}

var sys = &system{
	run:            runCommand,
	mountOf:        mountOf,
	mkdirTemp:      func() (string, error) { return os.MkdirTemp("", mountDirPrefix+"*") },
	resolve:        filepath.EvalSymlinks,
	removeDir:      os.Remove,
	firmlinks:      systemFirmlinks,
	cleanupTimeout: 2 * time.Minute,
	retryWait:      2 * time.Second,
	fullDiskAccess: macperm.FullDiskAccess,
	isRoot:         func() bool { return os.Geteuid() == 0 },
}

// runCommand is the real Runner. A failure carries the tool's own message.
func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() // #nosec G204 -- fixed tool paths; the arguments are a snapshot name and folders this package made
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return out, fmt.Errorf("%s %s: %w: %s", filepath.Base(name), strings.Join(args, " "), err, msg)
		}
		return out, fmt.Errorf("%s %s: %w", filepath.Base(name), strings.Join(args, " "), err)
	}
	return out, nil
}

// snapshot is one mounted local snapshot. Backups that start in the same
// second get the same snapshot from tmutil: they share it, and the last
// one to finish deletes it.
type snapshot struct {
	date  string // tmutil's name of it: 2026-11-22-194429
	mount string // where it is mounted
	refs  int
	item  leftover.Item
}

// mu serializes creating and deleting snapshots, so a snapshot is never
// deleted while another backup starts to use it; active lists the
// snapshots in use by this process (the janitor leaves them alone).
var (
	mu     sync.Mutex
	active = map[string]*snapshot{}
)

// Set is the snapshot one backup reads from. A Set without a snapshot (it
// failed, or no path needed one) maps every path to itself.
type Set struct {
	sys  *system
	snap *snapshot
	// live are sources on other volumes, read live although a snapshot
	// exists (they are not in it).
	live   []string
	closed bool
}

// Create takes an APFS local snapshot of the data volume when one of paths
// (absolute, cleaned) is on it, and mounts it read-only. Paths on other
// volumes and a failed snapshot are reported through onErr; their files are
// then read live. Create never fails the backup: the error is always nil
// and the Set is always usable. ctx is the run's: cancelling it stops the
// creation, and the ledger records the snapshot with its run.
func Create(ctx context.Context, paths []string, timeout time.Duration, onErr ErrorHandler) (*Set, error) {
	return sys.create(ctx, paths, timeout, onErr)
}

func (y *system) create(ctx context.Context, paths []string, timeout time.Duration, onErr ErrorHandler) (*Set, error) {
	s := &Set{sys: y}
	links := y.firmlinks()
	need := false
	for _, p := range paths {
		switch y.classify(p, links) {
		case onData:
			need = true
		case onSystem:
			// Sealed and read-only; what is below it on the data volume
			// (/usr/local below /usr, everything below /) needs the snapshot.
			need = need || hasFirmlinkBelow(p, links)
		case onOther:
			s.live = append(s.live, p)
			onErr(p, fmt.Errorf("%s is not on the startup disk's data volume (an external disk or a network share): APFS local snapshots only cover the data volume, so it is read without a snapshot", p))
		case unknown:
			s.live = append(s.live, p)
			onErr(p, fmt.Errorf("the volume of %s could not be determined; it is read without a snapshot", p))
		}
	}
	if !need {
		return s, nil
	}
	// macOS (TCC) mounts a snapshot only for a program with Full Disk
	// Access, root included. Without it tmutil would still make a snapshot,
	// which takes up to a minute and could then not be mounted: the files
	// are read without a snapshot at once, with the real reason.
	// An undetermined state is tried, as before.
	if y.fullDiskAccess() == macperm.Denied {
		onErr("apfs snapshot", fmt.Errorf("no APFS local snapshot of %s was taken, reading files without a snapshot: macOS mounts a snapshot only for a program with Full Disk Access (System Settings > Privacy & Security > Full Disk Access)", DataVolume))
		return &Set{sys: y}, nil
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	snap, err := y.acquire(ctx, timeout)
	if err != nil {
		onErr("apfs snapshot", fmt.Errorf("the APFS local snapshot of %s failed, reading files without a snapshot: %w", DataVolume, err))
		return &Set{sys: y}, nil
	}
	s.snap = snap
	return s, nil
}

// Volume classes of a source path.
const (
	unknown = iota
	onData
	onSystem
	onOther
)

// classify tells which volume p is on. A path on the data volume whose
// place in it is unknown (no firmlink leads there) is read live.
func (y *system) classify(p string, links []firmlink) int {
	m, err := y.mountOf(p)
	if err != nil {
		// A broken link or a file gone since the archiver checked it:
		// its folder tells the volume.
		if m, err = y.mountOf(filepath.Dir(p)); err != nil {
			return unknown
		}
	}
	switch m {
	case DataVolume:
		if underNoMap(p) {
			return onOther // a folder where other volumes are mounted
		}
		if _, ok := dataRel(p, links); ok {
			return onData
		}
		if isSystemAlias(p) {
			// /var itself is the link on the sealed system volume (statfs
			// followed it): the backup stores the link, read live.
			return onSystem
		}
		return unknown
	case systemVolume:
		return onSystem
	}
	return onOther
}

// acquire makes a new snapshot (or joins one made in the same second) and
// mounts it. Every step that can leave something behind is written to the
// leftover ledger first, so a crash leaves a record the janitor works
// through.
func (y *system) acquire(runCtx context.Context, timeout time.Duration) (*snapshot, error) {
	mu.Lock()
	defer mu.Unlock()
	ctx, cancel := context.WithTimeout(runCtx, timeout)
	defer cancel()
	out, err := y.run(ctx, tmutil, "localsnapshot")
	if err != nil {
		return nil, err
	}
	date, ok := parseCreatedDate(out)
	if !ok {
		return nil, fmt.Errorf("tmutil did not name the snapshot it made: %q", strings.TrimSpace(string(out)))
	}
	if sn, ok := active[date]; ok {
		sn.refs++
		return sn, nil
	}
	sn := &snapshot{date: date, refs: 1, item: ledgerItem(date, "")}
	leftover.Add(runCtx, sn.item)
	dir, err := y.mountDir()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("mount point: %w", err), y.release(sn))
	}
	sn.mount = dir
	sn.item.Detail = dir
	leftover.Add(runCtx, sn.item)
	if _, err := y.run(ctx, mountAPFS, mountArgs(date, dir)...); err != nil {
		return nil, errors.Join(withMountHint(err, y.isRoot()), y.release(sn))
	}
	if m, err := y.mountOf(dir); err != nil || m != dir {
		return nil, errors.Join(fmt.Errorf("the snapshot is not mounted on %s after mount_apfs", dir), y.release(sn))
	}
	active[date] = sn
	return sn, nil
}

// mountDir makes the mount point and returns its canonical path. statfs
// names a mount point by its resolved path (/private/tmp/..., never
// /tmp/...), so the checks that the snapshot is mounted, the ledger and
// Map all use that path.
func (y *system) mountDir() (string, error) {
	dir, err := y.mkdirTemp()
	if err != nil {
		return "", err
	}
	canon, err := y.resolve(dir)
	if err != nil {
		_ = y.removeDir(dir) // empty, made just above
		return "", err
	}
	return canon, nil
}

// mountArgs is the mount_apfs command line: read-only, hidden from the
// Finder, the snapshot of the data volume on dir.
func mountArgs(date, dir string) []string {
	return []string{"-o", "rdonly,nobrowse", "-s", SnapshotName(date), DataVolume, dir}
}

// withMountHint explains the usual reasons mount_apfs refuses ("Operation
// not permitted"): a process that is not root (a command line run; the
// agent service runs as root), or, for root, missing Full Disk Access,
// which macOS requires to mount a snapshot even for root. Saying "needs
// root" to the root service misled.
func withMountHint(err error, root bool) error {
	s := err.Error()
	if !strings.Contains(s, "Operation not permitted") && !strings.Contains(s, "exit status 77") {
		return err
	}
	if root {
		return fmt.Errorf("%w (macOS mounts a snapshot only for a program with Full Disk Access: System Settings > Privacy & Security > Full Disk Access)", err)
	}
	return fmt.Errorf("%w (mounting a snapshot needs root: the agent service runs as root)", err)
}

// release unmounts the snapshot, removes its mount point and deletes it,
// in that order (a mounted snapshot cannot be deleted). The ledger item
// goes only once the snapshot is verified gone; mu must be held.
func (y *system) release(sn *snapshot) error {
	ctx, cancel := context.WithTimeout(context.Background(), y.cleanupTimeout)
	defer cancel()
	err := y.removeSnapshot(ctx, sn.date, sn.mount)
	if err != nil {
		leftover.Failed(sn.item, err)
		return err
	}
	leftover.Done(sn.item)
	return nil
}

// removeSnapshot is the clean-up of a backup and of the janitor: unmount
// (forced when a plain unmount fails), remove the folder, delete the
// snapshot, check it is gone.
func (y *system) removeSnapshot(ctx context.Context, date, dir string) error {
	if dir != "" {
		if err := y.unmount(ctx, dir); err != nil {
			return err
		}
		// An empty folder left in the temporary folder is not worth
		// keeping the snapshot for: its removal error is ignored.
		_ = y.removeDir(dir)
	}
	for try := 0; ; try++ {
		_, derr := y.run(ctx, tmutil, "deletelocalsnapshots", date)
		gone, lerr := y.snapshotGone(ctx, date)
		switch {
		case lerr == nil && gone:
			return nil
		case try > 0 || ctx.Err() != nil:
			return errors.Join(fmt.Errorf("delete APFS snapshot %s", SnapshotName(date)), derr, lerr)
		}
		select {
		case <-ctx.Done():
		case <-time.After(y.retryWait):
		}
	}
}

// unmount unmounts dir if something is mounted on it.
func (y *system) unmount(ctx context.Context, dir string) error {
	if !y.mounted(dir) {
		return nil
	}
	_, err := y.run(ctx, umount, dir)
	if err != nil && y.mounted(dir) {
		// A file left open (a reader of a cancelled backup): force it,
		// the snapshot is read-only so nothing is lost.
		_, err = y.run(ctx, umount, "-f", dir)
	}
	if y.mounted(dir) {
		return fmt.Errorf("unmount %s: %w", dir, errors.Join(err, errors.New("still mounted")))
	}
	return nil
}

// mounted tells whether a file system is mounted on dir. statfs reports
// the resolved path, so a ledger record written before mount points were
// canonical (/tmp/... for /private/tmp/...) still matches.
func (y *system) mounted(dir string) bool {
	m, err := y.mountOf(dir)
	if err != nil {
		return false
	}
	if m == dir {
		return true
	}
	canon, err := y.resolve(dir)
	return err == nil && m == canon
}

// snapshotGone checks tmutil's list of snapshot dates of the data volume.
func (y *system) snapshotGone(ctx context.Context, date string) (bool, error) {
	out, err := y.run(ctx, tmutil, "listlocalsnapshotdates", DataVolume)
	if err != nil {
		return false, err
	}
	return !slices.Contains(parseDates(out), date), nil
}

// Map translates a path into the same file inside the mounted snapshot.
// Paths not on the data volume, below a mount point of another volume or
// without a snapshot are returned unchanged.
func (s *Set) Map(p string) string {
	if s == nil || s.snap == nil || s.closed {
		return p
	}
	for _, l := range s.live {
		if within(p, l) {
			return p
		}
	}
	if underNoMap(p) {
		return p
	}
	rel, ok := dataRel(p, s.sys.firmlinks())
	if !ok {
		return p
	}
	return filepath.Join(s.snap.mount, rel)
}

// Volumes returns the volume read from a snapshot (none when there is no
// snapshot).
func (s *Set) Volumes() []string {
	if s == nil || s.snap == nil {
		return nil
	}
	return []string{DataVolume}
}

// Close unmounts and deletes the snapshot, unless another backup still
// reads it. It may be called more than once.
func (s *Set) Close() error {
	if s == nil || s.snap == nil || s.closed {
		return nil
	}
	s.closed = true
	mu.Lock()
	defer mu.Unlock()
	sn := s.snap
	if sn.refs--; sn.refs > 0 {
		return nil
	}
	delete(active, sn.date)
	return s.sys.release(sn)
}

// Available reports whether this system makes APFS local snapshots.
func Available() bool { return available }

// CleanLeftovers removes the snapshots earlier backups of this agent left
// (a crash, a power loss, a failed removal), from the leftover ledger.
// Snapshots a running backup reads are skipped. Time Machine's own
// snapshots are never touched: only those in the ledger are BackupZit's.
func CleanLeftovers(ctx context.Context, log func(string, ...any)) (removed []string, err error) {
	return sys.cleanLeftovers(ctx, leftover.List(), log)
}

func (y *system) cleanLeftovers(ctx context.Context, items []leftover.Item, log func(string, ...any)) (removed []string, err error) {
	if log == nil {
		log = func(string, ...any) {}
	}
	mu.Lock()
	defer mu.Unlock()
	var errs []error
	for _, it := range items {
		if it.Platform != leftover.MacOS || it.Kind != leftover.KindSnapshot {
			continue
		}
		date, ok := dateOfName(it.Name)
		if !ok {
			leftover.Done(it) // not a name this package writes
			continue
		}
		if _, busy := active[date]; busy {
			continue
		}
		dir := it.Detail
		if !ownMountDir(dir) {
			dir = "" // never unmount or remove a folder this package did not make
		}
		cctx, cancel := context.WithTimeout(ctx, y.cleanupTimeout)
		rerr := y.removeSnapshot(cctx, date, dir)
		cancel()
		if rerr != nil {
			leftover.Failed(it, rerr)
			errs = append(errs, rerr)
			continue
		}
		leftover.Done(it)
		removed = append(removed, it.Name)
		log("removed a leftover of an earlier backup", "item", "APFS snapshot "+it.Name)
	}
	return removed, errors.Join(errs...)
}

// mountDirPrefix starts the name of every mount point this package makes.
const mountDirPrefix = "backupzit-apfs-"

// ownMountDir tells whether dir, read back from the ledger, is a mount
// point this package made (an absolute, clean path to a backupzit-apfs-*
// folder).
func ownMountDir(dir string) bool {
	return filepath.IsAbs(dir) && filepath.Clean(dir) == dir && strings.HasPrefix(filepath.Base(dir), mountDirPrefix)
}

// ledgerItem is the ledger record of a snapshot; Detail holds its mount
// point once there is one.
func ledgerItem(date, mount string) leftover.Item {
	return leftover.Item{Platform: leftover.MacOS, Kind: leftover.KindSnapshot, VM: DataVolume, Name: SnapshotName(date), Detail: mount}
}

// SnapshotName is the APFS name of the snapshot tmutil made at date.
func SnapshotName(date string) string { return "com.apple.TimeMachine." + date + ".local" }

var (
	dateRe    = regexp.MustCompile(`\b(\d{4}-\d{2}-\d{2}-\d{6})\b`)
	createdRe = regexp.MustCompile(`(?i)created local snapshot with date:\s*(\d{4}-\d{2}-\d{2}-\d{6})`)
	nameRe    = regexp.MustCompile(`^com\.apple\.TimeMachine\.(\d{4}-\d{2}-\d{2}-\d{6})\.local$`)
)

// parseCreatedDate finds the date in tmutil's answer ("Created local
// snapshot with date: 2026-11-22-194429", after a note about purgeable
// snapshots).
func parseCreatedDate(out []byte) (string, bool) {
	if m := createdRe.FindSubmatch(out); m != nil {
		return string(m[1]), true
	}
	return "", false
}

// parseDates lists the dates of "tmutil listlocalsnapshotdates" (a header
// line, then one date per line).
func parseDates(out []byte) []string {
	var dates []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if m := dateRe.FindStringSubmatch(line); m != nil && m[1] == line {
			dates = append(dates, line)
		}
	}
	return dates
}

// dateOfName is the date of a snapshot name SnapshotName made.
func dateOfName(name string) (string, bool) {
	m := nameRe.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// firmlink makes the folder target of the data volume appear at source on
// the system volume (/Users -> Users).
type firmlink struct{ source, target string }

// defaultFirmlinks are those of /usr/share/firmlinks on macOS 15 to 27,
// used when the file cannot be read.
var defaultFirmlinks = []firmlink{
	{"/AppleInternal", "AppleInternal"},
	{"/Applications", "Applications"},
	{"/Library", "Library"},
	{"/System/Library/Caches", "System/Library/Caches"},
	{"/System/Library/Assets", "System/Library/Assets"},
	{"/System/Library/PreinstalledAssets", "System/Library/PreinstalledAssets"},
	{"/System/Library/AssetsV2", "System/Library/AssetsV2"},
	{"/System/Library/PreinstalledAssetsV2", "System/Library/PreinstalledAssetsV2"},
	{"/System/Library/CoreServices/CoreTypes.bundle/Contents/Library", "System/Library/CoreServices/CoreTypes.bundle/Contents/Library"},
	{"/System/Library/Speech", "System/Library/Speech"},
	{"/Users", "Users"},
	{"/Volumes", "Volumes"},
	{"/cores", "cores"},
	{"/opt", "opt"},
	{"/pkg", "pkg"},
	{"/private", "private"},
	{"/usr/local", "usr/local"},
	{"/usr/libexec/cups", "usr/libexec/cups"},
	{"/usr/share/snmp", "usr/share/snmp"},
}

var (
	firmlinksOnce sync.Once
	firmlinksList []firmlink
)

// systemFirmlinks reads /usr/share/firmlinks once (a new macOS may add
// some); the built-in list stands in when it is missing or empty.
func systemFirmlinks() []firmlink {
	firmlinksOnce.Do(func() {
		if b, err := os.ReadFile("/usr/share/firmlinks"); err == nil {
			firmlinksList = parseFirmlinks(b)
		}
		if len(firmlinksList) == 0 {
			firmlinksList = defaultFirmlinks
		}
	})
	return firmlinksList
}

// parseFirmlinks reads lines "<source>\t<target>".
func parseFirmlinks(b []byte) []firmlink {
	var out []firmlink
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) != 2 {
			continue
		}
		src, dst := filepath.Clean(strings.TrimSpace(f[0])), strings.Trim(strings.TrimSpace(f[1]), "/")
		if !filepath.IsAbs(src) || src == "/" || dst == "" || strings.Contains(dst, "..") {
			continue
		}
		out = append(out, firmlink{src, dst})
	}
	return out
}

// systemAliases are the symbolic links of the sealed system volume that
// lead into the data volume (/var -> private/var). Unlike firmlinks they
// are not in /usr/share/firmlinks, but the system volume is read-only and
// sealed, so they cannot change. They are kept out of hasFirmlinkBelow:
// below / they are links, which the backup stores as links.
var systemAliases = []firmlink{
	{"/etc", "private/etc"},
	{"/tmp", "private/tmp"},
	{"/var", "private/var"},
}

// isSystemAlias tells whether p is one of those links itself.
func isSystemAlias(p string) bool {
	p = filepath.Clean(p)
	return slices.ContainsFunc(systemAliases, func(a firmlink) bool { return a.source == p })
}

// dataRel returns where p is inside the data volume: below
// /System/Volumes/Data, below a firmlink (the longest one wins:
// /System/Library/Caches is on the data volume, /System is not), or below
// a link of systemAliases. A link itself is not mapped: in the snapshot
// private/var is a folder, and the backup must keep /var a link.
func dataRel(p string, links []firmlink) (string, bool) {
	p = filepath.Clean(p)
	if within(p, DataVolume) {
		return strings.TrimPrefix(strings.TrimPrefix(p, DataVolume), "/"), true
	}
	var best *firmlink
	for i, l := range links {
		if within(p, l.source) && (best == nil || len(l.source) > len(best.source)) {
			best = &links[i]
		}
	}
	for i, a := range systemAliases {
		if within(p, a.source) && p != a.source && (best == nil || len(a.source) > len(best.source)) {
			best = &systemAliases[i]
		}
	}
	if best == nil {
		return "", false
	}
	return best.target + strings.TrimPrefix(p, best.source), true
}

// hasFirmlinkBelow tells whether a firmlink is p or below it: backing up
// p then reads files of the data volume.
func hasFirmlinkBelow(p string, links []firmlink) bool {
	for _, l := range links {
		if within(l.source, p) {
			return true
		}
	}
	return false
}

// noMap are folders of the data volume where other volumes are mounted:
// in the snapshot they are empty, so they are read live.
var noMap = []string{"/Volumes", DataVolume + "/Volumes"}

func underNoMap(p string) bool {
	for _, n := range noMap {
		if within(p, n) && p != n {
			return true
		}
	}
	return false
}

// within tells whether p is dir or below it.
func within(p, dir string) bool {
	if dir == "/" {
		return strings.HasPrefix(p, "/")
	}
	return p == dir || strings.HasPrefix(p, dir+"/")
}
