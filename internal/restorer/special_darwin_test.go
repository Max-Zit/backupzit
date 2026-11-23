package restorer_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/restorer"
)

// On a Mac a named pipe is restored (relative to its folder), a device node
// is not: devices live in /dev there, and a device node could only be
// changed by path.
func TestSpecialFilesOnMac(t *testing.T) {
	ctx := context.Background()
	be, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	root, err := r.SaveTree(ctx, &repo.Tree{Nodes: []repo.Node{
		{Name: "dev", Type: repo.NodeDev, Mode: 0o600, Unix: &repo.UnixMeta{Rdev: 0x1000001, Char: true}},
		{Name: "pipe", Type: repo.NodeFifo, Mode: 0o640},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	st, err := restorer.Run(ctx, r, &repo.Snapshot{Tree: root}, restorer.Options{Target: target})
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(target, "pipe")); err != nil || fi.Mode()&os.ModeNamedPipe == 0 || fi.Mode().Perm() != 0o640 {
		t.Errorf("pipe: %v %v", fi, err)
	}
	if _, err := os.Lstat(filepath.Join(target, "dev")); !os.IsNotExist(err) {
		t.Errorf("device node restored: %v", err)
	}
	if len(st.Errors) != 1 || !strings.Contains(st.Errors[0], "device nodes are not restored on macOS") {
		t.Errorf("errors: %q", st.Errors)
	}
}
