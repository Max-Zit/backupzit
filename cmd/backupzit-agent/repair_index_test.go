package main

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/archiver"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/checker"
	"github.com/max-zit/backupzit/internal/repo"
)

// Review of #53 (6): the command line repair lists the backups that need
// the data of packs the rebuild leaves out, and does not claim there was
// nothing to repair.
func TestRepairCommandReportsPacksTheRebuildLeavesOut(t *testing.T) {
	ctx := context.Background()
	oldChecker, oldRepo := checker.DefaultRetryPause, repo.DefaultRetryPause
	checker.DefaultRetryPause, repo.DefaultRetryPause = time.Millisecond, time.Millisecond
	t.Cleanup(func() { checker.DefaultRetryPause, repo.DefaultRetryPause = oldChecker, oldRepo })

	dir := t.TempDir()
	repoDir := filepath.Join(dir, "repo")
	be, err := backend.OpenLocal(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be, repo.Password("pw"))
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte(strings.Repeat("data ", 5000)), 0o600); err != nil {
		t.Fatal(err)
	}
	sn, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}, Version: "test", NoParent: true})
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	filepath.WalkDir(filepath.Join(repoDir, "data"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			b[0] ^= 0xff
			_ = os.Chmod(p, 0o600)
			if err := os.WriteFile(p, b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	})
	// The index files are cut short: the packs are known from their headers.
	filepath.WalkDir(filepath.Join(repoDir, "index"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			_ = os.Chmod(p, 0o600)
			if err := os.WriteFile(p, b[:len(b)/2], 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	})

	old := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = pw
	runErr := cmdRepair(ctx, []string{"--repo", repoDir, "--password", "pw"})
	os.Stdout = old
	_ = pw.Close()
	out, _ := io.ReadAll(pr)
	if runErr != nil {
		t.Fatalf("repair: %v\n%s", runErr, out)
	}
	text := string(out)
	for _, want := range []string{"index-damaged/", "stays out of the index", "snapshot " + sn.ID.Short(), "needs lost data"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "nothing to repair") {
		t.Errorf("says there was nothing to repair:\n%s", text)
	}
}
