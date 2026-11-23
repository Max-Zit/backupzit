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

// twoBackups makes a local repository with two backups (tagged as job 7)
// that have distinct data.
func twoBackups(t *testing.T) (string, []*repo.Snapshot) {
	t.Helper()
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
	defer r.Close()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	var sns []*repo.Snapshot
	for _, content := range []string{"first ", "second "} {
		if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte(strings.Repeat(content, 5000)), 0o600); err != nil {
			t.Fatal(err)
		}
		sn, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}, Version: "test", NoParent: true, Tags: []string{"job:7"}})
		if err != nil {
			t.Fatal(err)
		}
		sns = append(sns, sn)
	}
	return repoDir, sns
}

// cutIndexFiles cuts every index file in half (their packs are then known
// from their headers only).
func cutIndexFiles(t *testing.T, repoDir string) []string {
	t.Helper()
	var names []string
	filepath.WalkDir(filepath.Join(repoDir, "index"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			_ = os.Chmod(p, 0o600)
			if err := os.WriteFile(p, b[:len(b)/2], 0o600); err != nil {
				t.Fatal(err)
			}
			names = append(names, d.Name())
		}
		return nil
	})
	if len(names) == 0 {
		t.Fatal("no index file")
	}
	return names
}

func writeIndexGarbage(t *testing.T, repoDir string) string {
	t.Helper()
	name := filepath.Join(repoDir, "index", strings.Repeat("ab", 32))
	if err := os.WriteFile(name, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

// Review of #53 (6): the packs that the rebuild of a damaged index leaves
// out are reported like those DropDamagedPacks takes out: the result lists
// the backups that need their data and does not say that nothing was
// taken out.
func TestRepoRepairReportsPacksTheRebuildLeavesOut(t *testing.T) {
	ctx := context.Background()
	repoDir, sns := twoBackups(t)
	// Every data pack is damaged (its first blob; the header stays readable).
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
	cut := cutIndexFiles(t, repoDir)

	a := &Agent{log: slog.New(slog.DiscardHandler), client: &Client{cfg: &Config{}}}
	res := a.repoRepair(ctx, api.Run{Kind: api.KindRepoRepair, Repository: api.Repository{URL: repoDir, Password: "repair-test"}})
	if res.Status != api.StatusWarning {
		t.Fatalf("status %s: %q %v", res.Status, res.Message, res.Errors)
	}
	for _, want := range []string{"Replaced ", "index-damaged/", "damaged data packs", "2 backups need the lost data"} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("message lacks %q: %s", want, res.Message)
		}
	}
	if strings.Contains(res.Message, "nothing was taken out") || strings.Contains(res.Message, "nothing to repair") {
		t.Errorf("message says nothing was taken out: %s", res.Message)
	}
	if len(res.Errors) != 2 {
		t.Fatalf("errors: %v", res.Errors)
	}
	for i, e := range res.Errors {
		if !strings.Contains(e, "needs lost data") || !strings.Contains(e, sns[0].ID.Short()) && !strings.Contains(e, sns[1].ID.Short()) {
			t.Errorf("error %d: %s", i, e)
		}
	}
	for _, n := range cut {
		if _, err := os.Stat(filepath.Join(repoDir, "index-damaged", n)); err != nil {
			t.Errorf("the damaged index file was not kept: %v", err)
		}
	}
	// "Also remove the affected backups" works for them as well.
	res = a.repoRepair(ctx, api.Run{Kind: api.KindRepoRepair, Repository: api.Repository{URL: repoDir, Password: "repair-test"}, RepairForget: true})
	if res.Status != api.StatusSuccess && res.Status != api.StatusWarning {
		t.Fatalf("second run: %s %v", res.Message, res.Errors)
	}
}

// Review of #53 (8): retention keeps the older restore points while the
// index has damaged files. The lock refreshes the list first: a file that
// went bad since the repository was opened counts.
func TestRetentionSkippedWhileIndexIsDamaged(t *testing.T) {
	ctx := context.Background()
	repoDir, sns := twoBackups(t)
	be, err := backend.OpenLocal(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Open(ctx, be, repo.Password("repair-test"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if len(r.DamagedIndexFiles()) != 0 {
		t.Fatal("setup: already damaged")
	}
	writeIndexGarbage(t, repoDir) // after the open: only the lock's refresh sees it
	a := &Agent{log: slog.New(slog.DiscardHandler), client: &Client{cfg: &Config{}}}
	run := api.Run{ID: 1, JobID: 7, Retention: &repo.RetentionPolicy{KeepLast: 1}}
	forgotten, note, _ := a.applyRetention(ctx, r, run)
	if len(forgotten) != 0 || !strings.Contains(note, "older restore points are kept until repair") {
		t.Fatalf("forgotten %v, note %q", forgotten, note)
	}
	for _, sn := range sns {
		if _, err := os.Stat(filepath.Join(repoDir, "snapshots", sn.ID.String())); err != nil {
			t.Errorf("backup %s is gone: %v", sn.ID.Short(), err)
		}
	}
	// Once repair has written the new index, retention removes the older backup.
	if _, err := r.RebuildIndex(ctx); err != nil {
		t.Fatal(err)
	}
	forgotten, note, _ = a.applyRetention(ctx, r, run)
	if len(forgotten) != 1 {
		t.Fatalf("after the repair: forgotten %v, note %q", forgotten, note)
	}
}
