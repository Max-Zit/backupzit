//go:build !windows

package restorer_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/restorer"
)

// linkSnapshot saves dir/pwned (a file) and .zfs/x, and returns the
// repository and the root directory node.
func linkSnapshot(t *testing.T) (*repo.Repository, repo.Node) {
	t.Helper()
	ctx := context.Background()
	be, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("not for the host")
	blob, _, err := r.SaveBlob(ctx, repo.DataBlob, data)
	if err != nil {
		t.Fatal(err)
	}
	file := repo.Node{Name: "pwned", Type: repo.NodeFile, Mode: 0o644, Size: uint64(len(data)), Content: []repo.ID{blob}}
	inner, err := r.SaveTree(ctx, &repo.Tree{Nodes: []repo.Node{file}})
	if err != nil {
		t.Fatal(err)
	}
	zfs, err := r.SaveTree(ctx, &repo.Tree{Nodes: []repo.Node{{Name: "x", Type: repo.NodeDir, Mode: 0o755 | uint32(os.ModeDir), Subtree: &inner}}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := r.SaveTree(ctx, &repo.Tree{Nodes: []repo.Node{
		{Name: ".zfs", Type: repo.NodeDir, Mode: 0o755 | uint32(os.ModeDir), Subtree: &zfs},
		{Name: "dir", Type: repo.NodeDir, Mode: 0o755 | uint32(os.ModeDir), Subtree: &inner},
		file,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	return r, repo.Node{Name: "root", Type: repo.NodeDir, Mode: 0o755 | uint32(os.ModeDir), Subtree: &root}
}

// Root in a container leaves symbolic links in its volume, pointing at
// folders and files of the host, before a restore into that volume: the
// restored files must land in the volume, never where the links point.
func TestRestoreDoesNotWriteThroughExistingLinks(t *testing.T) {
	ctx := context.Background()
	r, dir := linkSnapshot(t)
	host := t.TempDir()
	hostFile := filepath.Join(host, "victim")
	if err := os.WriteFile(hostFile, []byte("host data"), 0o600); err != nil {
		t.Fatal(err)
	}
	volume := t.TempDir()
	if err := os.Symlink(host, filepath.Join(volume, "dir")); err != nil { // folder replaced by a link
		t.Fatal(err)
	}
	if err := os.Symlink(hostFile, filepath.Join(volume, "pwned.bzpart")); err != nil { // temp name
		t.Fatal(err)
	}
	st, err := restorer.RunDir(ctx, r, &dir, volume, restorer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(host, "pwned")); err == nil {
		t.Fatal("a file was written through the folder link, onto the host")
	}
	if b, _ := os.ReadFile(hostFile); string(b) != "host data" {
		t.Fatalf("the host file was overwritten through the temp-name link: %q", b)
	}
	if fi, err := os.Lstat(filepath.Join(volume, "dir")); err != nil || !fi.IsDir() {
		t.Fatalf("the link was not replaced by a folder: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(filepath.Join(volume, "dir", "pwned")); string(b) != "not for the host" {
		t.Errorf("dir/pwned = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(volume, "pwned")); string(b) != "not for the host" {
		t.Errorf("pwned = %q", b)
	}
	// The ZFS snapshot folder is never written.
	if _, err := os.Lstat(filepath.Join(volume, ".zfs")); err == nil {
		t.Error(".zfs was restored")
	}
	if len(st.Errors) != 1 {
		t.Errorf("errors = %v, want only the skipped .zfs", st.Errors)
	}
}

// Restoring into a folder replaces a link left there by an earlier restore.
func TestRestoreIntoFolderReplacesLink(t *testing.T) {
	ctx := context.Background()
	r, dir := linkSnapshot(t)
	host := t.TempDir()
	target := t.TempDir()
	if err := os.Symlink(host, filepath.Join(target, "dir")); err != nil {
		t.Fatal(err)
	}
	if _, err := restorer.Run(ctx, r, &repo.Snapshot{Tree: *dir.Subtree}, restorer.Options{Target: target}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(host, "pwned")); err == nil {
		t.Fatal("a file was written through the link, outside the target")
	}
	if b, _ := os.ReadFile(filepath.Join(target, "dir", "pwned")); string(b) != "not for the host" {
		t.Errorf("dir/pwned = %q", b)
	}
}
