package restorer_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/restorer"
)

// NEW-1 (0.63): the PostgreSQL cluster backup is written by postgres, and a
// restore gave the staging folder and base.tar their recorded owner, so
// postgres could swap base.tar while root waited to unpack it. A Private
// restore writes files and folders as the agent's own, private, without the
// recorded owner, mode (setuid), extended attributes or ACL, and leaves out
// links and special files.
func TestPrivateRestore(t *testing.T) {
	ctx := context.Background()
	be, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("a tar archive")
	blob, _, err := r.SaveBlob(ctx, repo.DataBlob, data)
	if err != nil {
		t.Fatal(err)
	}
	pg := &repo.UnixMeta{UID: 12345, GID: 12345, User: "postgres", Group: "postgres",
		Xattrs: map[string][]byte{"user.mark": []byte("x")}}
	inner, err := r.SaveTree(ctx, &repo.Tree{Nodes: []repo.Node{
		{Name: "base.tar", Type: repo.NodeFile, Mode: uint32(0o755 | fs.ModeSetuid), Size: uint64(len(data)), Content: []repo.ID{blob}, Unix: pg},
		{Name: "evil", Type: repo.NodeSymlink, Mode: uint32(0o777 | fs.ModeSymlink), LinkTarget: "/etc/passwd", Unix: pg},
		{Name: "pipe", Type: repo.NodeFifo, Mode: uint32(0o666 | fs.ModeNamedPipe), Unix: pg},
	}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := r.SaveTree(ctx, &repo.Tree{Nodes: []repo.Node{
		{Name: "cluster", Type: repo.NodeDir, Mode: uint32(0o777 | fs.ModeDir), Subtree: &inner, Unix: pg},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	st, err := restorer.Run(ctx, r, &repo.Snapshot{Tree: root}, restorer.Options{Target: target, Include: []string{"cluster"}, Private: true})
	if err != nil {
		t.Fatal(err)
	}
	tar := filepath.Join(target, "cluster", "base.tar")
	if got, err := os.ReadFile(tar); err != nil || string(got) != string(data) {
		t.Fatalf("base.tar: %q %v", got, err)
	}
	for _, name := range []string{"evil", "pipe"} {
		if _, err := os.Lstat(filepath.Join(target, "cluster", name)); err == nil {
			t.Errorf("%s was restored", name)
		}
	}
	if len(st.Errors) != 2 || !strings.Contains(strings.Join(st.Errors, "\n"), "not a file or folder") {
		t.Errorf("errors: %q", st.Errors)
	}
	if runtime.GOOS == "windows" {
		return // no Unix modes
	}
	for p, want := range map[string]fs.FileMode{tar: 0o600, filepath.Join(target, "cluster"): fs.ModeDir | 0o700} {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode() & (fs.ModeType | fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky); got != want {
			t.Errorf("%s: mode %v, want %v", p, got, want)
		}
		if uid, ok := ownerOf(fi); ok && uid != os.Geteuid() {
			t.Errorf("%s belongs to %d, not to the agent's user", p, uid)
		}
	}
	if hasXattr(tar, "user.mark") {
		t.Error("the extended attribute of the backup was set")
	}
}
