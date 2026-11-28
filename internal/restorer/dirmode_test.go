//go:build !windows

package restorer_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/restorer"
)

// MED 17 (0.63): a restored folder was created 0755 and got its own mode
// only after everything below it, so the files of a private folder (/root,
// a 0700 home) were open to everyone while it restored. It is now 0700 until
// then; a folder above the selection keeps 0755.
func TestRestoreFoldersPrivateWhileRestored(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	ctx := context.Background()
	be, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("secret")
	blob, _, err := r.SaveBlob(ctx, repo.DataBlob, data)
	if err != nil {
		t.Fatal(err)
	}
	files, err := r.SaveTree(ctx, &repo.Tree{Nodes: []repo.Node{{Name: "notes", Type: repo.NodeFile, Mode: 0o644, Size: uint64(len(data)), Content: []repo.ID{blob}}}})
	if err != nil {
		t.Fatal(err)
	}
	home, err := r.SaveTree(ctx, &repo.Tree{Nodes: []repo.Node{{Name: "ana", Type: repo.NodeDir, Mode: uint32(0o750 | fs.ModeDir), Subtree: &files}}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := r.SaveTree(ctx, &repo.Tree{Nodes: []repo.Node{{Name: "home", Type: repo.NodeDir, Mode: uint32(0o751 | fs.ModeDir), Subtree: &home}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	perm := func(p string) fs.FileMode {
		fi, err := os.Stat(filepath.Join(target, p))
		if err != nil {
			t.Fatal(err)
		}
		return fi.Mode().Perm()
	}
	var during map[string]fs.FileMode
	progress := func(string, *restorer.Stats) {
		during = map[string]fs.FileMode{"home": perm("home"), "home/ana": perm("home/ana")}
	}
	st, err := restorer.Run(ctx, r, &repo.Snapshot{Tree: root}, restorer.Options{Target: target, Include: []string{"home/ana"}, Progress: progress})
	if err != nil || len(st.Errors) > 0 {
		t.Fatal(err, st.Errors)
	}
	if during["home"] != 0o755 || during["home/ana"] != 0o700 {
		t.Errorf("while the file was restored: home %v (want 0755), home/ana %v (want 0700)", during["home"], during["home/ana"])
	}
	if got := perm("home/ana"); got != 0o750 {
		t.Errorf("home/ana ends with %v, want 0750", got)
	}
	if got := perm("home"); got != 0o755 {
		t.Errorf("home (above the selection) ends with %v, want 0755", got)
	}
}
