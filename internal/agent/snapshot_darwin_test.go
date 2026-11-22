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
		got := permissionWarning()
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
