package archiver

import (
	"bytes"
	"context"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// Files go through the parallel blob writer and their chunks are hashed
// alongside reading (#71): every file reads back byte for byte in chunk
// order, after Run and after Dir (whose caller flushes), and a second run
// stores nothing new.
func TestRunAndDirStoreChunksInOrder(t *testing.T) {
	ctx := context.Background()
	be, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	src := t.TempDir()
	rng := rand.New(rand.NewPCG(1, 2))
	big := make([]byte, 24<<20) // many chunks
	for i := range big {
		big[i] = byte(rng.Uint32())
	}
	files := map[string][]byte{"big.bin": big, "small.txt": []byte("small file\n"), "empty": nil,
		"sub/copy.bin": big[:9<<20]}
	for name, b := range files {
		p := filepath.Join(src, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	check := func(what string, tree repo.ID) {
		t.Helper()
		got := map[string][]byte{}
		var walk func(dir string, id repo.ID)
		walk = func(dir string, id repo.ID) {
			tr, err := r.LoadTree(ctx, id)
			if err != nil {
				t.Fatalf("%s: tree %s: %v", what, dir, err)
			}
			for _, n := range tr.Nodes {
				p := dir + n.Name
				if n.Type == repo.NodeDir && n.Subtree != nil {
					walk(p+"/", *n.Subtree)
					continue
				}
				var b []byte
				for _, c := range n.Content {
					d, err := r.LoadBlob(ctx, repo.DataBlob, c)
					if err != nil {
						t.Fatalf("%s: %s: %v", what, p, err)
					}
					b = append(b, d...)
				}
				got[p] = b
			}
		}
		walk("", tree)
		for name, want := range files {
			var b []byte
			for p, d := range got {
				if p == name || filepath.ToSlash(p) == name || len(p) > len(name) && p[len(p)-len(name)-1:] == "/"+name {
					b = d
				}
			}
			if !bytes.Equal(b, want) {
				t.Errorf("%s: %s reads back %d bytes, want %d", what, name, len(b), len(want))
			}
		}
	}
	sn, err := Run(ctx, r, Options{Paths: []string{src}, NoParent: true})
	if err != nil {
		t.Fatal(err)
	}
	check("Run", sn.Tree)
	before := r.Stats()
	n, _, err := Dir(ctx, r, src, "rootfs", nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	check("Dir", *n.Subtree)
	if after := r.Stats(); after.StoredBytes-before.StoredBytes > 64<<10 {
		t.Errorf("the second backup of the same files stored %d bytes", after.StoredBytes-before.StoredBytes)
	}
}
