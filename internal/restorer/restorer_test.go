package restorer_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/restorer"
)

// A tampered snapshot pairs a symlink with a folder of the same name: the
// folder's file must not be written where the link points.
func TestDuplicateNameDoesNotFollowLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	ctx := context.Background()
	be, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("not for /etc")
	blob, _, err := r.SaveBlob(ctx, repo.DataBlob, data)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := r.SaveTree(ctx, &repo.Tree{Nodes: []repo.Node{{Name: "pwned", Type: repo.NodeFile, Mode: 0o644, Size: uint64(len(data)), Content: []repo.ID{blob}}}})
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	root, err := r.SaveTree(ctx, &repo.Tree{Nodes: []repo.Node{
		{Name: "x", Type: repo.NodeSymlink, Mode: 0o777 | uint32(os.ModeSymlink), LinkTarget: outside},
		{Name: "x", Type: repo.NodeDir, Mode: 0o755 | uint32(os.ModeDir), Subtree: &inner},
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
	if _, err := os.Stat(filepath.Join(outside, "pwned")); err == nil {
		t.Fatal("the folder's file was written through the symlink, outside the target")
	}
	if len(st.Errors) == 0 {
		t.Error("the duplicate name was not reported")
	}
}
