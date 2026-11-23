package archiver

import (
	"runtime"
	"slices"
	"testing"
)

// macArchiver is an archiver of a macOS backup of roots, with the rules
// of macOSSkips and the firmlinks of a macOS 15 system.
func macArchiver(roots ...string) *Archiver {
	const firmlinks = "/AppleInternal\tAppleInternal\n/Applications\tApplications\n/Library\tLibrary\n" +
		"/System/Library/Caches\tSystem/Library/Caches\n/Users\tUsers\n/Volumes\tVolumes\n/private\tprivate\n/usr/local\tusr/local\n"
	return &Archiver{
		skips: newSkipSet(skipPatterns(macOSSkips), firmlinkDuplicates(firmlinks, roots)),
		roots: roots,
	}
}

// The macOS system folders are kept empty below a source, on / and on the
// data volume, and at the root of other volumes; user data next to them is
// read.
func TestMacOSSkips(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("macOS paths")
	}
	a := macArchiver("/", "/Volumes/USB")
	for _, p := range []string{
		"/Volumes", "/System/Volumes/VM", "/System/Volumes/Preboot", "/private/var/vm", "/dev",
		"/private/var/folders", "/private/tmp", "/System/Volumes/Data/private/tmp",
		"/Library/Caches", "/System/Library/Caches", "/Users/bob/Library/Caches", "/users/BOB/library/caches",
		"/Users/bob/.Trash", "/System/Volumes/Data/Users/bob/Library/Caches", "/private/var/root/Library/Caches",
		"/System/Volumes/Data/.Spotlight-V100", "/System/Volumes/Data/.fseventsd", "/.DocumentRevisions-V100",
		"/Volumes/USB/.Spotlight-V100", "/Volumes/USB/.Trashes", "/Volumes/USB/Backups.backupdb", "/System/Volumes/Data/.MobileBackups",
	} {
		if !a.storeEmpty(p, 0) {
			t.Errorf("%s is read", p)
		}
	}
	for _, p := range []string{
		"/Users", "/Users/bob", "/Users/bob/Library", "/Users/bob/Library/Preferences", "/Users/bob/Documents/Caches",
		"/Users/bob/Library/Caches2", "/Applications", "/private/var/db", "/System/Volumes/Data", "/System/Volumes/Data/sw",
		"/Volumes/USB", "/Volumes/USB/Photos", "/Users/bob/Trash", "/opt/homebrew",
	} {
		if a.storeEmpty(p, 0) {
			t.Errorf("%s is kept empty", p)
		}
	}
}

// A source is read although a rule names it: the job asked for it.
func TestMacOSSkipsSourceIsRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("macOS paths")
	}
	a := macArchiver("/Users/bob/.Trash", "/Volumes", "/Users/bob/Library/Caches")
	for _, p := range a.roots {
		if a.storeEmpty(p, 0) {
			t.Errorf("source %s is kept empty", p)
		}
	}
	if !a.storeEmpty("/Volumes/USB/.Spotlight-V100", 0) {
		t.Error("the Spotlight index of a volume below a source is read")
	}
}

// With / as a source, the data volume's folders that / reads through their
// firmlinks are not read a second time; with only the data volume as a
// source, all of it is.
func TestFirmlinkDuplicates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("macOS paths")
	}
	a := macArchiver("/")
	for _, p := range []string{"/System/Volumes/Data/Users", "/System/Volumes/Data/Applications", "/System/Volumes/Data/usr/local"} {
		if !a.storeEmpty(p, 0) {
			t.Errorf("%s is read twice", p)
		}
	}
	if a.storeEmpty("/System/Volumes/Data/sw", 0) {
		t.Error("a folder of the data volume without firmlink is not read")
	}
	b := macArchiver("/System/Volumes/Data")
	if b.storeEmpty("/System/Volumes/Data/Users", 0) || b.storeEmpty("/System/Volumes/Data/Applications", 0) {
		t.Error("the data volume backed up alone misses its folders")
	}
	c := macArchiver("/Users/bob", "/System/Volumes/Data")
	if c.storeEmpty("/System/Volumes/Data/Users", 0) {
		t.Error("/Users is not covered by /Users/bob: the other users' folders are lost")
	}
	got := firmlinkDuplicates("/Users\tUsers\n\nbad line\n/x\t../etc\n/Library\t/Library/\n", []string{"/Users", "/Library/Fonts"})
	if !got["/System/Volumes/Data/Users"] || len(got) != 1 {
		t.Errorf("firmlinkDuplicates = %v", got)
	}
}

// Linux and Windows backups keep no folder empty but kernel file systems.
func TestNoSystemSkipsElsewhere(t *testing.T) {
	if runtime.GOOS == "darwin" {
		if s := systemSkips([]string{"/"}); !s.match("/private/var/vm") {
			t.Error("macOS rules not used")
		}
		return
	}
	a := &Archiver{skips: systemSkips([]string{"/"}), roots: []string{"/"}}
	for _, p := range []string{"/Volumes", "/home/bob/.cache", "/Users/bob/Library/Caches", "/dev", "/tmp"} {
		if a.storeEmpty(p, 0) {
			t.Errorf("%s kept empty on %s", p, runtime.GOOS)
		}
	}
}

// Every rule is a relative path of the expected form, and each scope
// expands to its roots.
func TestSkipPatterns(t *testing.T) {
	for _, r := range macOSSkips {
		if r.path == "" || r.path[0] == '/' || r.why == "" {
			t.Errorf("bad rule %+v", r)
		}
	}
	got := skipPatterns([]skipRule{{".fseventsd", onVolume, "x"}, {"private/tmp", onData, "x"}, {"dev", onSystem, "x"}})
	want := []string{"/.fseventsd", "/System/Volumes/Data/.fseventsd", "/Volumes/*/.fseventsd", "/private/tmp", "/System/Volumes/Data/private/tmp", "/dev"}
	if !slices.Equal(got, want) {
		t.Errorf("skipPatterns = %v", got)
	}
}
