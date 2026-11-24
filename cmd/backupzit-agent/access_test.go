package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/recovery"
	"github.com/max-zit/backupzit/internal/repo"
)

// access-package create refuses a weak passphrase before writing anything,
// records the repository ID it opened (so the recovery side can tell two
// disks with the same folder apart), and leaves no temporary file behind.
func TestAccessCreate(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "repo")
	be, err := backend.OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be, repo.Password("repo-pw"))
	if err != nil {
		t.Fatal(err)
	}
	id := r.Config().ID
	_ = r.Close()

	outDir := t.TempDir()
	out := filepath.Join(outDir, "stick")
	args := []string{"--repo", dir, "--password", "repo-pw", "--out", out}
	t.Setenv("BACKUPZIT_ACCESS_PASSPHRASE", "aaaaaaaaaaaaaaaaaaaaaaaa")
	if err := cmdAccessCreate(ctx, args); !errors.Is(err, recovery.ErrPassphrasePredictable) {
		t.Fatalf("weak passphrase: %v", err)
	}
	if _, err := os.Stat(out + recovery.AccessPackageExt); !os.IsNotExist(err) {
		t.Fatal("package written with a weak passphrase")
	}

	const pass = "correct horse battery"
	t.Setenv("BACKUPZIT_ACCESS_PASSPHRASE", pass)
	if err := cmdAccessCreate(ctx, args); err != nil {
		t.Fatal(err)
	}
	// Adding the same repository again replaces its entry.
	if err := cmdAccessCreate(ctx, args); err != nil {
		t.Fatal(err)
	}
	b, err := recovery.ReadAccessPackageFile(out + recovery.AccessPackageExt)
	if err != nil {
		t.Fatal(err)
	}
	p, err := recovery.OpenAccessPackage(b, pass)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Repositories) != 1 || p.Repositories[0].RepoID != id || p.Repositories[0].Location != dir {
		t.Errorf("package %+v, want repository ID %s", p.Repositories, id)
	}
	entries, _ := os.ReadDir(outDir)
	if len(entries) != 1 {
		t.Errorf("folder holds %d entries, want only the package", len(entries))
	}
	if err := writeAccess(filepath.Join(outDir, "missing", "x.bzaccess"), b, 1); err == nil {
		t.Error("write into a missing folder succeeded")
	}
	if entries, _ := os.ReadDir(outDir); len(entries) != 1 {
		t.Errorf("failed write left %d entries", len(entries))
	}
}
