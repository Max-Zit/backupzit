package agent

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/archiver"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// Repair with "also remove the backups that need the lost data" removes
// them, except a backup a VM runs from (instant recovery, KeepSnapshots).
func TestRepoRepairKeepsInstantBackups(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "repo")
	be, err := backend.OpenLocal(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be, repo.Password("repair-test"))
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	var sns []*repo.Snapshot
	for _, content := range []string{"first ", "second "} {
		if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte(strings.Repeat(content, 5000)), 0o600); err != nil {
			t.Fatal(err)
		}
		sn, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}, Version: "test", NoParent: true})
		if err != nil {
			t.Fatal(err)
		}
		sns = append(sns, sn)
	}
	r.Close()
	// Every data pack is damaged: both backups need lost data.
	filepath.WalkDir(filepath.Join(repoDir, "data"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			b[len(b)/2] ^= 0xff
			_ = os.Chmod(p, 0o600)
			if err := os.WriteFile(p, b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	})

	a := &Agent{log: slog.New(slog.DiscardHandler), client: &Client{cfg: &Config{}}}
	res := a.repoRepair(ctx, api.Run{Kind: api.KindRepoRepair, RepairForget: true,
		Repository:    api.Repository{URL: repoDir, Password: "repair-test"},
		KeepSnapshots: []string{sns[0].ID.String()}})
	if res.Status != api.StatusWarning || !strings.Contains(res.Message, "Removed 1 of the 2 backups") {
		t.Fatalf("repair: %s %q %v", res.Status, res.Message, res.Errors)
	}
	if len(res.Forgotten) != 1 || res.Forgotten[0] != sns[1].ID.String() {
		t.Errorf("removed %v, want only %s", res.Forgotten, sns[1].ID.Short())
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "instant recovery") {
		t.Errorf("errors: %v", res.Errors)
	}
	if _, err := os.Stat(filepath.Join(repoDir, "snapshots", sns[0].ID.String())); err != nil {
		t.Errorf("the backup a VM runs from is gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoDir, "snapshots", sns[1].ID.String())); err == nil {
		t.Error("the other damaged backup was not removed")
	}
}
