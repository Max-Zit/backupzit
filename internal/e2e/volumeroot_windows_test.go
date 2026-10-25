//go:build windows

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/max-zit/backupzit/internal/archiver"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/restorer"
)

// Restored into a folder, the volume root folder ("C") is not hidden like
// C:\ itself, so the restored files can be seen.
func TestRestoreVolumeRootVisible(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(tempDir(t), "src")
	writeFile(t, filepath.Join(src, "a.txt"), []byte("a"))
	be, err := backend.OpenLocal(filepath.Join(tempDir(t), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be, repo.Password("x"))
	if err != nil {
		t.Fatal(err)
	}
	sn, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}})
	if err != nil {
		t.Fatal(err)
	}
	tgt := filepath.Join(tempDir(t), "restore")
	if st, err := restorer.Run(ctx, r, sn, restorer.Options{Target: tgt}); err != nil || len(st.Errors) > 0 {
		t.Fatalf("restore: %v %v", err, st.Errors)
	}
	vol := filepath.VolumeName(src)[:1]
	p, _ := syscall.UTF16PtrFromString(filepath.Join(tgt, vol))
	attrs, err := syscall.GetFileAttributes(p)
	if err != nil {
		t.Fatal(err)
	}
	if attrs&(syscall.FILE_ATTRIBUTE_HIDDEN|syscall.FILE_ATTRIBUTE_SYSTEM) != 0 {
		t.Fatalf("volume root folder %s restored hidden/system (attributes %#x)", vol, attrs)
	}
	if _, err := os.Stat(filepath.Join(tgt, vol, src[3:], "a.txt")); err != nil {
		t.Fatal(err)
	}
}
