package e2e

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/max-zit/backupzit/internal/archiver"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/checker"
	"github.com/max-zit/backupzit/internal/repo"
)

const testKey = "TQ7K-29XD-PL4M-W8ZR-3HFN-6YBC"

func TestBackupRestoreEncrypted(t *testing.T) {
	repoDir := filepath.Join(tempDir(t), "repo")
	be, err := backend.OpenLocal(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	runCycle(t, func() backend.Backend { return be }, true, repo.Password(testKey))
}

func TestEncryptionHidesData(t *testing.T) {
	ctx := context.Background()
	repoDir := filepath.Join(tempDir(t), "repo")
	be, _ := backend.OpenLocal(repoDir)
	r, err := repo.Init(ctx, be, repo.Password(testKey))
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(tempDir(t), "secret-folder-name")
	marker := "TOP SECRET payroll 2026 marker"
	writeFile(t, filepath.Join(src, "salaries-confidential.txt"), bytes.Repeat([]byte(marker+"\n"), 1000))
	sn, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}, Hostname: "host-marker-xyz"})
	if err != nil {
		t.Fatal(err)
	}

	// Nothing readable may reach the storage: contents, file names,
	// folder names, hostname or the chunker polynomial.
	needles := [][]byte{[]byte("TOP SECRET"), []byte("salaries-confidential"), []byte("secret-folder-name"), []byte("host-marker-xyz")}
	filepath.Walk(repoDir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		for _, n := range needles {
			if bytes.Contains(b, n) {
				t.Errorf("%s contains plaintext %q", p, n)
			}
		}
		if filepath.Base(p) == "config" && bytes.Contains(b, []byte("chunker_polynomial")) {
			t.Errorf("config leaks the chunker polynomial")
		}
		return nil
	})

	// Opening needs the right key.
	if _, err := repo.Open(ctx, be); !errors.Is(err, repo.ErrPasswordRequired) {
		t.Errorf("open without key: %v", err)
	}
	if _, err := repo.Open(ctx, be, repo.Password("WRONG-KEY")); !errors.Is(err, repo.ErrWrongPassword) {
		t.Errorf("open with wrong key: %v", err)
	}
	r2, err := repo.Open(ctx, be, repo.Password(testKey))
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Encrypted() {
		t.Error("repository not reported as encrypted")
	}
	got, err := r2.LoadSnapshot(ctx, sn.ID.String())
	if err != nil || got.Hostname != "host-marker-xyz" {
		t.Fatalf("snapshot after reopen: %v", err)
	}

	// Prune works on encrypted repositories (index rewrite and repack).
	if err := r2.RemoveSnapshot(ctx, sn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Prune(ctx, repo.PruneOptions{}); err != nil {
		t.Fatal(err)
	}
	r3, err := repo.Open(ctx, be, repo.Password(testKey))
	if err != nil {
		t.Fatal(err)
	}
	if res, err := checker.Run(ctx, r3, checker.Options{ReadData: true}); err != nil || !res.OK() || res.Packs != 0 {
		t.Fatalf("check after prune: %v %+v", err, res)
	}
}
