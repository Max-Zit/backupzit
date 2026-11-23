package archiver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// A parent backup whose tree is no longer in the repository (a damaged pack
// that repair took out of the index) does not fail the next backup: it runs
// without a parent and reads every file again.
func TestRunWithLostParentTree(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	be, err := backend.OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte(strings.Repeat("parent data ", 4000)), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := Options{Paths: []string{src}, Hostname: "h", Version: "test"}
	sn1, err := Run(ctx, r, opts)
	if err != nil {
		t.Fatal(err)
	}
	// Every pack goes, as after a repair of a fully damaged storage.
	var drop []repo.ID
	for p := range r.Index().Packs() {
		drop = append(drop, p)
	}
	if _, err := r.DropDamagedPacks(ctx, drop); err != nil {
		t.Fatal(err)
	}
	if _, err := r.LoadTree(ctx, sn1.Tree); err == nil {
		t.Fatal("parent tree still loads")
	}
	opts.Parent = sn1
	sn2, err := Run(ctx, r, opts)
	if err != nil {
		t.Fatalf("backup with a lost parent tree: %v", err)
	}
	if sn2.Parent != nil {
		t.Error("the unusable parent is recorded")
	}
	if sn2.Stats.FilesNew != 1 {
		t.Errorf("files read again: %+v", sn2.Stats)
	}
	used, err := r.BlobsOf(ctx, []*repo.Snapshot{sn2})
	if err != nil {
		t.Fatal(err)
	}
	for h := range used {
		if !r.Index().Has(h) {
			t.Errorf("blob %s of the new backup is not stored", h.ID.Short())
		}
	}
}
