package recovery

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/max-zit/backupzit/internal/imaging"
)

// BootBackupDir chooses the folder for the backup of the boot data before
// a boot repair of disk target: \backupzit on a drive that is not on that
// disk and not X: (the RAM disk of Windows PE, lost at the restart). An
// existing \backupzit folder wins (the recovery stick, the backup disk);
// otherwise one is made on the first writable drive.
func BootBackupDir(disks []imaging.Disk, target int, roots []string) (string, error) {
	usable := func(root string) bool {
		if strings.EqualFold(filepath.VolumeName(root), "X:") {
			return false
		}
		n, ok := imaging.DiskOfPath(disks, root)
		return !ok || n != target
	}
	for _, r := range roots {
		d := filepath.Join(r, "backupzit")
		if fi, err := os.Stat(d); err == nil && fi.IsDir() && usable(r) && writableDir(d) {
			return d, nil
		}
	}
	for _, r := range roots {
		d := filepath.Join(r, "backupzit")
		if usable(r) && os.MkdirAll(d, 0o700) == nil && writableDir(d) {
			return d, nil
		}
	}
	return "", errors.New("no writable drive outside the disk being repaired for the backup of the boot data: connect a USB stick or name a folder")
}

// writableDir tells whether a file can be created in dir.
func writableDir(dir string) bool {
	f, err := os.CreateTemp(dir, ".bz-write-test-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}
