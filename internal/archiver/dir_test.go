package archiver

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// Dir (container root file systems) stores its files through the same
// parallel writer as a backup run, and every blob is in a pack when it
// returns, so the caller's Flush makes the node readable.
func TestDirStoresFiles(t *testing.T) {
	ctx := context.Background()
	be, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	want := bytes.Repeat([]byte("container file "), 300000) // several chunks
	if err := os.WriteFile(filepath.Join(src, "f.bin"), want, 0o644); err != nil {
		t.Fatal(err)
	}
	n, _, err := Dir(ctx, r, src, "rootfs", nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	tree, err := r.LoadTree(ctx, *n.Subtree)
	if err != nil || len(tree.Nodes) != 1 {
		t.Fatalf("tree %v %v", tree, err)
	}
	var got []byte
	for _, id := range tree.Nodes[0].Content {
		b, err := r.LoadBlob(ctx, repo.DataBlob, id)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, b...)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("restored %d bytes, want %d", len(got), len(want))
	}
}
