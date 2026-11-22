package agent

import (
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/macperm"
)

// Only a denied Full Disk Access warns: an undetermined one must not raise
// a false alarm in every run report.
func TestPermissionWarning(t *testing.T) {
	saved := fullDiskAccess
	defer func() { fullDiskAccess = saved }()
	for state, warn := range map[macperm.State]bool{macperm.Denied: true, macperm.Granted: false, macperm.Unknown: false} {
		fullDiskAccess = func() macperm.State { return state }
		got := permissionWarning([]string{"/Users/alice"})
		if (got != "") != warn {
			t.Fatalf("%s: %q", state, got)
		}
		if warn && !strings.Contains(got, "System Settings > Privacy & Security > Full Disk Access") {
			t.Fatalf("no way to grant it: %q", got)
		}
	}
}

func TestSnapshotMessage(t *testing.T) {
	if got := snapshotMessage([]string{"/System/Volumes/Data"}); got != "Read from an APFS local snapshot of /System/Volumes/Data." {
		t.Fatal(got)
	}
}

// A missing Full Disk Access only matters to a backup that reads where
// macOS protects files.
func TestPermissionWarningOnlyForProtectedPaths(t *testing.T) {
	saved := fullDiskAccess
	defer func() { fullDiskAccess = saved }()
	fullDiskAccess = func() macperm.State { return macperm.Denied }
	for p, warn := range map[string]bool{
		"/":                                     true,
		"/Users":                                true,
		"/Users/alice":                          true,
		"/Users/alice/Documents/":               true,
		"/System/Volumes/Data/Users/alice":      true,
		"/System/Volumes/Data":                  true,
		"/Library/Application Support/Foo":      true,
		"/Volumes/USB":                          true,
		"/Users/Shared":                         false,
		"/Users/Shared/Projects":                false,
		"/opt/data":                             false,
		"/private/var/folders/xy/T/TestBackup1": false,
		"/Usersfoo":                             false,
		"/System/Volumes/Database":              false,
	} {
		if got := permissionWarning([]string{"/opt/other", p}) != ""; got != warn {
			t.Errorf("%s: warning %v, want %v", p, got, warn)
		}
	}
	if permissionWarning(nil) != "" {
		t.Error("warning for a backup of nothing")
	}
}
