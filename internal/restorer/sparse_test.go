//go:build !windows

package restorer_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/restorer"
)

// Chunks of zeros are restored as holes: the file has its content and
// size, but does not take its full size on disk.
func TestRestoreKeepsFilesSparse(t *testing.T) {
	ctx := context.Background()
	be, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	zeros := make([]byte, 4<<20)
	data := bytes.Repeat([]byte("data"), 1024)
	z, _, err := r.SaveBlob(ctx, repo.DataBlob, zeros)
	if err != nil {
		t.Fatal(err)
	}
	d, _, err := r.SaveBlob(ctx, repo.DataBlob, data)
	if err != nil {
		t.Fatal(err)
	}
	size := uint64(2*len(zeros) + len(data))
	root, err := r.SaveTree(ctx, &repo.Tree{Nodes: []repo.Node{{Name: "disk.img", Type: repo.NodeFile, Mode: 0o644, Size: size, Content: []repo.ID{z, d, z}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if _, err := restorer.Run(ctx, r, &repo.Snapshot{Tree: root}, restorer.Options{Target: target, Verify: true}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(target, "disk.img")
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := append(append(append([]byte{}, zeros...), data...), zeros...); !bytes.Equal(got, want) {
		t.Fatalf("content differs (%d bytes, want %d)", len(got), len(want))
	}
	fi, _ := os.Stat(p)
	if used := fi.Sys().(*syscall.Stat_t).Blocks * 512; used >= int64(size)/2 {
		t.Errorf("%d of %d bytes allocated: the zeros were written", used, size)
	}
}
