package backend

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// After all objects are deleted, the empty folders of a repository go too;
// folders that still hold files stay.
func TestLocalRemoveDirs(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "repo")
	l, err := OpenLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"config", "data/0a/0a11", "data/0b/0b22", "index/i1"} {
		if err := l.Save(ctx, n, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	names, _ := l.List(ctx, "")
	for _, n := range names {
		if n != "index/i1" {
			l.Remove(ctx, n)
		}
	}
	if err := l.RemoveDirs(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "data")); !os.IsNotExist(err) {
		t.Errorf("empty data folder stays: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "index", "i1")); err != nil {
		t.Errorf("folder with a file removed: %v", err)
	}
	l.Remove(ctx, "index/i1")
	l.RemoveDirs(ctx)
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("repository folder stays: %v", err)
	}
}
