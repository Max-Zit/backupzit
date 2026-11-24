package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/repo"
)

func TestCloudOnlyMessage(t *testing.T) {
	st := repo.SnapshotStats{CloudOnlyFiles: 3360, CloudOnlyBytes: 1288490189}
	for i := 0; i < 7; i++ {
		st.CloudOnly = append(st.CloudOnly, repo.CloudOnlyFolder{Path: fmt.Sprintf("/Users/u%d/Documents", i), Files: 400, Bytes: 1 << 20})
	}
	mac := cloudOnlyMessage("darwin", st)
	for _, want := range []string{"3360 files (1.2 GiB) are in iCloud Drive but not downloaded on this Mac", "/Users/u0/Documents: 400 files", "/Users/u4/Documents", "1360 files in other folders", "Download Now", "Optimize Mac Storage"} {
		if !strings.Contains(mac, want) {
			t.Errorf("macOS message lacks %q: %s", want, mac)
		}
	}
	if strings.Contains(mac, "/Users/u5/") {
		t.Errorf("more than %d folders named: %s", cloudOnlyFoldersShown, mac)
	}
	one := cloudOnlyMessage("windows", repo.SnapshotStats{CloudOnlyFiles: 1, CloudOnlyBytes: 2048, CloudOnly: []repo.CloudOnlyFolder{{Path: `C:\Users\anna\OneDrive`, Files: 1, Bytes: 2048}}})
	for _, want := range []string{"1 file (2.0 KiB) is online-only", "its content is not", `In C:\Users\anna\OneDrive: 1 file (2.0 KiB).`, "Always keep on this device"} {
		if !strings.Contains(one, want) {
			t.Errorf("Windows message lacks %q: %s", want, one)
		}
	}
}
