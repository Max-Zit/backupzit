package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/archiver"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// damagedIndexAfterOpen opens a repository with two backups with the given
// tags and then adds an index file that cannot be read: only the refresh
// after the lock sees it.
func damagedIndexAfterOpen(t *testing.T, tags ...string) (*repo.Repository, string, []*repo.Snapshot) {
	t.Helper()
	ctx := context.Background()
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
	t.Cleanup(func() { r.Close() })
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	var sns []*repo.Snapshot
	for _, c := range []string{"one ", "two "} {
		if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte(strings.Repeat(c, 3000)), 0o600); err != nil {
			t.Fatal(err)
		}
		sn, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}, Version: "test", NoParent: true, Tags: tags})
		if err != nil {
			t.Fatal(err)
		}
		sns = append(sns, sn)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "index", strings.Repeat("ef", 32)), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	return r, repoDir, sns
}

func requireBackupsKept(t *testing.T, repoDir string, sns []*repo.Snapshot) {
	t.Helper()
	for _, sn := range sns {
		if _, err := os.Stat(filepath.Join(repoDir, "snapshots", sn.ID.String())); err != nil {
			t.Errorf("backup %s is gone: %v", sn.ID.Short(), err)
		}
	}
}

// Review of #53 (8): removing an agent keeps the older backups while the
// index has damaged files.
func TestAgentRemovalKeepsBackupsWhileIndexIsDamaged(t *testing.T) {
	r, repoDir, sns := damagedIndexAfterOpen(t, "job:1")
	msg, ids := keepNewestBackups(context.Background(), r)
	if len(ids) != 0 || !strings.Contains(msg, "kept until repair has run") {
		t.Fatalf("message %q, removed %v", msg, ids)
	}
	requireBackupsKept(t, repoDir, sns)
}

// Review of #53 (8): the console backup stores the new backup but keeps the
// older ones while the index has damaged files, and says so.
func TestConsoleBackupKeepsOlderBackupsWhileIndexIsDamaged(t *testing.T) {
	r, repoDir, sns := damagedIndexAfterOpen(t, consoleTag)
	err := keepNewestConsoleBackups(context.Background(), r, 1)
	if err == nil || !strings.Contains(err.Error(), "kept") || !strings.Contains(err.Error(), "damaged index") {
		t.Fatalf("got %v", err)
	}
	requireBackupsKept(t, repoDir, sns)
}
