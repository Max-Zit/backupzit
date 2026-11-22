package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/archiver"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// 0.58: a retention that could not lock the repository ends the run with a
// warning, not only a note, so old backups that are never removed do not go
// unnoticed.
func TestRetentionSkippedIsAWarning(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	be, err := backend.OpenLocal(filepath.Join(dir, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be, repo.Password("retention-warn"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte(strings.Repeat("x", i+1)), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}, Version: "test", Tags: []string{jobTag(5)}}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	other, err := r.Lock(ctx, true, 0) // another run holds the repository
	if err != nil {
		t.Fatal(err)
	}
	defer other.Unlock()
	a := &Agent{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	run := api.Run{ID: 1, JobID: 5, Retention: &repo.RetentionPolicy{KeepLast: 1}}
	forgotten, note, warn := a.applyRetention(short, r, run)
	if len(forgotten) != 0 || !warn || !strings.HasPrefix(note, "retention skipped: ") {
		t.Fatalf("forgotten %v, note %q, warn %v", forgotten, note, warn)
	}
}
