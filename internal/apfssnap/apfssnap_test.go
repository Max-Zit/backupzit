package apfssnap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/leftover"
)

// fakeMac plays tmutil, mount_apfs and umount: it records every command
// line and keeps the snapshots and mounts they make.
type fakeMac struct {
	calls   []string
	dates   []string // existing snapshots
	mounts  map[string]bool
	volumes map[string]string // path prefix -> mount point
	next    string            // date of the next snapshot
	fail    map[string]error  // command (tool + first argument) -> error
	removed []string
}

func newFakeMac() *fakeMac {
	return &fakeMac{
		mounts: map[string]bool{},
		volumes: map[string]string{
			"/Users":            DataVolume,
			"/Library":          DataVolume,
			"/opt":              DataVolume,
			"/private":          DataVolume,
			"/usr/local":        DataVolume,
			"/Volumes":          DataVolume,
			DataVolume:          DataVolume,
			"/Volumes/External": "/Volumes/External",
			"/Volumes/Share":    "/Volumes/Share",
		},
		next: "2026-11-22-194429",
		fail: map[string]error{},
	}
}

func (f *fakeMac) run(_ context.Context, name string, args ...string) ([]byte, error) {
	line := strings.TrimSpace(filepath.Base(name) + " " + strings.Join(args, " "))
	f.calls = append(f.calls, line)
	key := filepath.Base(name)
	if len(args) > 0 {
		key += " " + args[0]
	}
	if err := f.fail[key]; err != nil {
		return []byte("failed"), err
	}
	switch {
	case key == "tmutil localsnapshot":
		if !slices.Contains(f.dates, f.next) {
			f.dates = append(f.dates, f.next)
		}
		return []byte("NOTE: local snapshots are considered purgeable and may be removed at any time by deleted(8).\nCreated local snapshot with date: " + f.next + "\n"), nil
	case key == "tmutil deletelocalsnapshots":
		f.dates = slices.DeleteFunc(f.dates, func(d string) bool { return d == args[1] })
		return []byte("Deleted local snapshot '" + args[1] + "'\n"), nil
	case key == "tmutil listlocalsnapshotdates":
		return []byte("Snapshot dates for disk " + args[1] + ":\n" + strings.Join(f.dates, "\n") + "\n"), nil
	case filepath.Base(name) == "mount_apfs":
		f.mounts[args[len(args)-1]] = true
		return nil, nil
	case filepath.Base(name) == "umount":
		delete(f.mounts, args[len(args)-1])
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected command %s", line)
}

func (f *fakeMac) mountOf(p string) (string, error) {
	if f.mounts[p] {
		return p, nil
	}
	best, m := "", systemVolume
	for prefix, mp := range f.volumes {
		if within(p, prefix) && len(prefix) > len(best) {
			best, m = prefix, mp
		}
	}
	return m, nil
}

// system is the fake machine; each test starts without snapshots in use.
func (f *fakeMac) system() *system {
	mu.Lock()
	clear(active)
	mu.Unlock()
	return &system{
		run:            f.run,
		mountOf:        f.mountOf,
		mkdirTemp:      func() (string, error) { return "/tmp/backupzit-apfs-1", nil },
		removeDir:      func(d string) error { f.removed = append(f.removed, d); return nil },
		firmlinks:      func() []firmlink { return defaultFirmlinks },
		cleanupTimeout: time.Minute,
		retryWait:      time.Millisecond,
	}
}

func collect() (*[]string, ErrorHandler) {
	var errs []string
	return &errs, func(item string, err error) { errs = append(errs, item+": "+err.Error()) }
}

// The commands run in this order: snapshot, mount read-only, and at the
// end unmount, delete, check; the files are read from the mount.
func TestSnapshotLifecycle(t *testing.T) {
	f := newFakeMac()
	errs, onErr := collect()
	s, err := f.system().create(context.Background(), []string{"/Users/anna"}, time.Minute, onErr)
	if err != nil || len(*errs) > 0 {
		t.Fatalf("create: %v %v", err, *errs)
	}
	if got := s.Map("/Users/anna/Documents/a.txt"); got != "/tmp/backupzit-apfs-1/Users/anna/Documents/a.txt" {
		t.Fatalf("map: %s", got)
	}
	if v := s.Volumes(); !slices.Equal(v, []string{DataVolume}) {
		t.Fatalf("volumes: %v", v)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil { // a second Close does nothing
		t.Fatal(err)
	}
	want := []string{
		"tmutil localsnapshot",
		"mount_apfs -o rdonly,nobrowse -s com.apple.TimeMachine.2026-11-22-194429.local /System/Volumes/Data /tmp/backupzit-apfs-1",
		"umount /tmp/backupzit-apfs-1",
		"tmutil deletelocalsnapshots 2026-11-22-194429",
		"tmutil listlocalsnapshotdates /System/Volumes/Data",
	}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	if len(f.dates) != 0 || len(f.mounts) != 0 || !slices.Equal(f.removed, []string{"/tmp/backupzit-apfs-1"}) {
		t.Fatalf("left: dates %v mounts %v removed %v", f.dates, f.mounts, f.removed)
	}
	if got := s.Map("/Users/anna/a"); got != "/Users/anna/a" {
		t.Fatalf("map after close: %s", got)
	}
	if len(active) != 0 {
		t.Fatalf("active: %v", active)
	}
}

func TestMapPaths(t *testing.T) {
	f := newFakeMac()
	_, onErr := collect()
	s, _ := f.system().create(context.Background(), []string{"/", "/Volumes/External/photos"}, time.Minute, onErr)
	defer func() { _ = s.Close() }()
	m := "/tmp/backupzit-apfs-1"
	for in, want := range map[string]string{
		"/Users/anna":                    m + "/Users/anna",
		"/Users":                         m + "/Users",
		"/Library/Preferences/x.plist":   m + "/Library/Preferences/x.plist",
		"/private/var/db/a":              m + "/private/var/db/a",
		"/usr/local/bin/tool":            m + "/usr/local/bin/tool",
		"/opt/homebrew":                  m + "/opt/homebrew",
		"/System/Library/Caches/c":       m + "/System/Library/Caches/c",
		DataVolume + "/Users/anna":       m + "/Users/anna",
		DataVolume:                       m,
		"/System/Library/CoreServices/x": "/System/Library/CoreServices/x", // sealed system volume
		"/usr/bin/ls":                    "/usr/bin/ls",
		"/":                              "/",
		"/Volumes/External/photos/a.jpg": "/Volumes/External/photos/a.jpg", // another volume
		"/Volumes/Share/x":               "/Volumes/Share/x",               // other mounts below /Volumes
		"/Volumes":                       m + "/Volumes",
		"/Applications/Safari.app/Info":  m + "/Applications/Safari.app/Info",
		"/Usersx/anna":                   "/Usersx/anna", // not below /Users
	} {
		if got := s.Map(in); got != want {
			t.Errorf("Map(%s) = %s, want %s", in, got, want)
		}
	}
}

// Paths on other volumes are read live and reported; without a path on
// the data volume no snapshot is made.
func TestOtherVolumesReadLive(t *testing.T) {
	f := newFakeMac()
	errs, onErr := collect()
	s, _ := f.system().create(context.Background(), []string{"/Volumes/External/photos", "/usr/bin"}, time.Minute, onErr)
	if len(f.calls) != 0 {
		t.Fatalf("no snapshot needed, ran %v", f.calls)
	}
	if len(*errs) != 1 || !strings.Contains((*errs)[0], "/Volumes/External/photos is not on the startup disk's data volume") {
		t.Fatalf("errors: %v", *errs)
	}
	if got := s.Map("/Volumes/External/photos/a"); got != "/Volumes/External/photos/a" {
		t.Fatal(got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// A failed snapshot (tmutil refuses, mount_apfs needs root) is reported;
// what was made is removed and everything is read live.
func TestSnapshotFailureFallsBack(t *testing.T) {
	for _, tc := range []struct {
		fail, want string
		calls      []string
	}{
		{"tmutil localsnapshot", "APFS local snapshot of /System/Volumes/Data failed, reading files without a snapshot", []string{"tmutil localsnapshot"}},
		{"mount_apfs -o", "mounting a snapshot needs root", []string{
			"tmutil localsnapshot",
			"mount_apfs -o rdonly,nobrowse -s com.apple.TimeMachine.2026-11-22-194429.local /System/Volumes/Data /tmp/backupzit-apfs-1",
			"tmutil deletelocalsnapshots 2026-11-22-194429",
			"tmutil listlocalsnapshotdates /System/Volumes/Data",
		}},
	} {
		f := newFakeMac()
		f.fail[tc.fail] = errors.New("exit status 77: mount_apfs: volume could not be mounted: Operation not permitted")
		errs, onErr := collect()
		s, err := f.system().create(context.Background(), []string{"/Users/anna"}, time.Minute, onErr)
		if err != nil {
			t.Fatal(err)
		}
		if len(*errs) != 1 || !strings.Contains((*errs)[0], tc.want) || !strings.HasPrefix((*errs)[0], "apfs snapshot: ") {
			t.Fatalf("%s: errors %v", tc.fail, *errs)
		}
		if got := s.Map("/Users/anna/a"); got != "/Users/anna/a" {
			t.Fatalf("%s: map %s", tc.fail, got)
		}
		if !slices.Equal(f.calls, tc.calls) {
			t.Fatalf("%s: calls %v", tc.fail, f.calls)
		}
		if len(f.dates) != 0 || s.Volumes() != nil || s.Close() != nil {
			t.Fatalf("%s: snapshots %v", tc.fail, f.dates)
		}
	}
}

// An unmount that fails (a file still open) is forced; a snapshot that
// cannot be unmounted is not deleted and stays in the ledger.
func TestCloseForcesUnmountAndRecordsFailure(t *testing.T) {
	ledger := filepath.Join(t.TempDir(), "leftovers.json")
	leftover.Open(ledger)
	defer leftover.Close()

	f := newFakeMac()
	_, onErr := collect()
	y := f.system()
	s, _ := y.create(leftover.WithRun(context.Background(), 7), []string{"/Users/anna"}, time.Minute, onErr)
	if items := leftover.List(); len(items) != 1 || items[0].RunID != 7 || items[0].Detail != "/tmp/backupzit-apfs-1" || items[0].Platform != leftover.MacOS {
		t.Fatalf("ledger during the backup: %+v", items)
	}
	// Busy: a plain unmount fails, the forced one works.
	f.fail["umount /tmp/backupzit-apfs-1"] = errors.New("resource busy")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.calls, "umount -f /tmp/backupzit-apfs-1") || len(f.dates) != 0 {
		t.Fatalf("calls %v dates %v", f.calls, f.dates)
	}
	if items := leftover.List(); len(items) != 0 {
		t.Fatalf("ledger after the backup: %+v", items)
	}

	// Neither works: the snapshot stays, recorded with the error.
	f = newFakeMac()
	y = f.system()
	s, _ = y.create(context.Background(), []string{"/Users/anna"}, time.Minute, onErr)
	f.fail["umount /tmp/backupzit-apfs-1"] = errors.New("resource busy")
	f.fail["umount -f"] = errors.New("resource busy")
	if err := s.Close(); err == nil || !strings.Contains(err.Error(), "still mounted") {
		t.Fatalf("close: %v", err)
	}
	if slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "tmutil deletelocalsnapshots") }) {
		t.Fatal("a mounted snapshot was deleted")
	}
	items := leftover.List()
	if len(items) != 1 || items[0].Error == "" || items[0].Name != "com.apple.TimeMachine.2026-11-22-194429.local" {
		t.Fatalf("ledger: %+v", items)
	}

	// The janitor finishes the job once the mount is free.
	delete(f.fail, "umount /tmp/backupzit-apfs-1")
	delete(f.fail, "umount -f")
	removed, err := y.cleanLeftovers(context.Background(), leftover.List(), nil)
	if err != nil || !slices.Equal(removed, []string{"com.apple.TimeMachine.2026-11-22-194429.local"}) {
		t.Fatalf("clean: %v %v", removed, err)
	}
	if len(f.dates) != 0 || len(f.mounts) != 0 || len(leftover.List()) != 0 {
		t.Fatalf("left: %v %v %v", f.dates, f.mounts, leftover.List())
	}
}

// A deletion tmutil does not carry out is tried again, then reported.
func TestDeleteVerified(t *testing.T) {
	f := newFakeMac()
	_, onErr := collect()
	y := f.system()
	s, _ := y.create(context.Background(), []string{"/Users/anna"}, time.Minute, onErr)
	f.fail["tmutil deletelocalsnapshots"] = errors.New("exit status 1")
	err := s.Close()
	if err == nil || !strings.Contains(err.Error(), "delete APFS snapshot com.apple.TimeMachine.2026-11-22-194429.local") {
		t.Fatalf("close: %v", err)
	}
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, "tmutil deletelocalsnapshots") {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d deletions: %v", n, f.calls)
	}
}

// Two backups that start in the same second share the snapshot; the last
// one deletes it. The janitor leaves a snapshot in use alone.
func TestSharedSnapshot(t *testing.T) {
	f := newFakeMac()
	_, onErr := collect()
	y := f.system()
	a, _ := y.create(context.Background(), []string{"/Users/anna"}, time.Minute, onErr)
	b, _ := y.create(context.Background(), []string{"/Library"}, time.Minute, onErr)
	if a.snap != b.snap || a.snap.refs != 2 {
		t.Fatal("not shared")
	}
	items := []leftover.Item{ledgerItem("2026-11-22-194429", "/tmp/backupzit-apfs-1")}
	if removed, err := y.cleanLeftovers(context.Background(), items, nil); err != nil || len(removed) != 0 {
		t.Fatalf("janitor touched a snapshot in use: %v %v", removed, err)
	}
	if err := a.Close(); err != nil || len(f.dates) != 1 || !f.mounts["/tmp/backupzit-apfs-1"] {
		t.Fatalf("first close: %v %v %v", err, f.dates, f.mounts)
	}
	if err := b.Close(); err != nil || len(f.dates) != 0 || len(f.mounts) != 0 {
		t.Fatalf("last close: %v %v %v", err, f.dates, f.mounts)
	}
}

func TestParse(t *testing.T) {
	out := []byte("NOTE: local snapshots are considered purgeable and may be removed at any time by deleted(8).\nCreated local snapshot with date: 2026-11-22-201115\n")
	if d, ok := parseCreatedDate(out); !ok || d != "2026-11-22-201115" {
		t.Fatalf("created: %q %v", d, ok)
	}
	if _, ok := parseCreatedDate([]byte("Failed to create APFS snapshot with date: 2026-11-22-201115")); ok {
		t.Fatal("a failure read as a success")
	}
	dates := parseDates([]byte("Snapshot dates for disk /System/Volumes/Data:\n2026-11-22-191737\n2026-11-22-201115\n"))
	if !slices.Equal(dates, []string{"2026-11-22-191737", "2026-11-22-201115"}) {
		t.Fatalf("dates: %v", dates)
	}
	if d, ok := dateOfName(SnapshotName("2026-11-22-201115")); !ok || d != "2026-11-22-201115" {
		t.Fatalf("name: %q", d)
	}
	if _, ok := dateOfName("com.apple.TimeMachine.2026-11-22-201115.local; rm -rf /"); ok {
		t.Fatal("bad name accepted")
	}
	links := parseFirmlinks([]byte("/Users\tUsers\n/usr/local\tusr/local\nbroken line\n/\troot\n/x\t../escape\n"))
	if !slices.Equal(links, []firmlink{{"/Users", "Users"}, {"/usr/local", "usr/local"}}) {
		t.Fatalf("firmlinks: %v", links)
	}
}

// The firmlinks of this Mac are read and include the user folders.
func TestSystemFirmlinks(t *testing.T) {
	if _, err := os.Stat("/usr/share/firmlinks"); err != nil {
		t.Skip("not macOS")
	}
	if !slices.Contains(systemFirmlinks(), firmlink{"/Users", "Users"}) {
		t.Fatalf("%v", systemFirmlinks())
	}
}

// A ledger record naming another folder only deletes the snapshot: the
// janitor never unmounts or removes what this package did not make.
func TestJanitorKeepsForeignFolders(t *testing.T) {
	f := newFakeMac()
	y := f.system()
	f.dates = []string{"2026-11-22-194429"}
	f.mounts["/Users/anna"] = true
	removed, err := y.cleanLeftovers(context.Background(), []leftover.Item{ledgerItem("2026-11-22-194429", "/Users/anna")}, nil)
	if err != nil || len(removed) != 1 || len(f.removed) != 0 || !f.mounts["/Users/anna"] {
		t.Fatalf("removed %v err %v dirs %v mounts %v", removed, err, f.removed, f.mounts)
	}
	for dir, own := range map[string]bool{"/tmp/backupzit-apfs-1": true, "/var/folders/x/T/backupzit-apfs-42": true, "backupzit-apfs-1": false, "/tmp/../Users/backupzit-apfs-1": false, "/Users/anna": false, "": false} {
		if ownMountDir(dir) != own {
			t.Errorf("ownMountDir(%q) = %v", dir, !own)
		}
	}
}
