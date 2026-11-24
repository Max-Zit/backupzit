package recovery

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/max-zit/backupzit/internal/imaging"
)

func TestBootBackupDir(t *testing.T) {
	onTarget, other := t.TempDir(), t.TempDir()
	disks := []imaging.Disk{
		{Number: 0, Partitions: []imaging.Partition{{MountPoints: []string{onTarget}}}},
		{Number: 1, Partitions: []imaging.Partition{{MountPoints: []string{other}}}},
	}
	// \backupzit exists only on the disk being repaired: it is not used.
	if err := os.Mkdir(filepath.Join(onTarget, "backupzit"), 0o700); err != nil {
		t.Fatal(err)
	}
	d, err := BootBackupDir(disks, 0, []string{onTarget, other})
	if err != nil || d != filepath.Join(other, "backupzit") {
		t.Errorf("BootBackupDir = %q, %v; want the folder on the other disk", d, err)
	}
	if _, err := BootBackupDir(disks, 0, []string{onTarget}); err == nil {
		t.Error("a folder on the disk being repaired was accepted")
	}
}
