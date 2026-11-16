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

// lockingLocal is a local backend with object lock that records which files
// it was asked to keep locked.
type lockingLocal struct {
	*backend.Local
	kept map[string]bool
}

func (l *lockingLocal) LockDays() int { return 7 }

func (l *lockingLocal) KeepLocked(_ context.Context, names []string) (int, error) {
	for _, n := range names {
		l.kept[n] = true
	}
	return len(names), nil
}

// TestKeepImmutableRetained checks that data only an older kept snapshot
// uses (a file deleted since) stays locked, and that data of a snapshot the
// retention policy drops is not extended.
func TestKeepImmutableRetained(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	loc, err := backend.OpenLocal(filepath.Join(dir, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	be := &lockingLocal{Local: loc, kept: map[string]bool{}}
	r, err := repo.Init(ctx, be, repo.Password("keep-test"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(src, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	backup := func() *repo.Snapshot {
		t.Helper()
		sn, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}, Version: "test", Tags: []string{jobTag(5)}})
		if err != nil {
			t.Fatal(err)
		}
		return sn
	}
	write("dropped.txt", strings.Repeat("only in the oldest backup ", 2000))
	old := backup()
	time.Sleep(10 * time.Millisecond)
	if err := os.Remove(filepath.Join(src, "dropped.txt")); err != nil {
		t.Fatal(err)
	}
	write("monthly.txt", strings.Repeat("only in the monthly backup ", 2000))
	monthly := backup()
	time.Sleep(10 * time.Millisecond)
	if err := os.Remove(filepath.Join(src, "monthly.txt")); err != nil {
		t.Fatal(err)
	}
	write("new.txt", strings.Repeat("in the newest backup ", 2000))
	newest := backup()

	packsOf := func(sn *repo.Snapshot) map[string]bool {
		t.Helper()
		used, err := r.BlobsOf(ctx, []*repo.Snapshot{sn})
		if err != nil {
			t.Fatal(err)
		}
		packs := map[string]bool{}
		for h := range used {
			l, ok := r.Index().Lookup(h)
			if !ok {
				t.Fatalf("blob %s not in index", h.ID.Short())
			}
			packs["data/"+l.Pack.String()[:2]+"/"+l.Pack.String()] = true
		}
		return packs
	}
	onlyIn := func(a, b, c *repo.Snapshot) []string {
		pa, pb, pc := packsOf(a), packsOf(b), packsOf(c)
		var out []string
		for p := range pa {
			if !pb[p] && !pc[p] {
				out = append(out, p)
			}
		}
		return out
	}
	monthlyOnly, oldOnly := onlyIn(monthly, old, newest), onlyIn(old, monthly, newest)
	if len(monthlyOnly) == 0 || len(oldOnly) == 0 {
		t.Fatalf("test setup: monthly-only packs %d, old-only packs %d", len(monthlyOnly), len(oldOnly))
	}

	// Keep the two newest: the monthly backup stays, the oldest is dropped.
	a := &Agent{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	run := api.Run{ID: 1, JobID: 5, Retention: &repo.RetentionPolicy{KeepLast: 2}}
	if err := a.keepImmutable(ctx, r, run, newest.ID.String()); err != nil {
		t.Fatal(err)
	}
	for _, p := range monthlyOnly {
		if !be.kept[p] {
			t.Errorf("pack %s used only by the kept monthly backup was not kept locked", p)
		}
	}
	for _, p := range oldOnly {
		if be.kept[p] {
			t.Errorf("pack %s of the dropped backup was kept locked", p)
		}
	}
	for _, sn := range []*repo.Snapshot{monthly, newest} {
		if !be.kept["snapshots/"+sn.ID.String()] {
			t.Errorf("snapshot file %s not kept", sn.ID.Short())
		}
	}
	if be.kept["snapshots/"+old.ID.String()] {
		t.Error("snapshot file of the dropped backup kept")
	}
}

func TestKeepSet(t *testing.T) {
	mk := func(b byte, h int) *repo.Snapshot {
		sn := &repo.Snapshot{Time: time.Date(2026, 11, 1, h, 0, 0, 0, time.UTC)}
		sn.ID[0] = b
		return sn
	}
	s1, s2, s3 := mk(1, 1), mk(2, 2), mk(3, 3)
	mine := []*repo.Snapshot{s1, s2, s3}
	noLoad := func() (*repo.Snapshot, error) { t.Fatal("load called"); return nil, nil }
	ids := func(sns []*repo.Snapshot) string {
		var s []string
		for _, sn := range sns {
			s = append(s, sn.ID.Short())
		}
		return strings.Join(s, ",")
	}
	// No policy: everything.
	got, _ := keepSet(mine, api.Run{}, s3.ID.String(), noLoad)
	if ids(got) != ids(mine) {
		t.Errorf("no policy: %s", ids(got))
	}
	// KeepLast 1: only the newest; a pinned (instant recovery) one stays.
	run := api.Run{Retention: &repo.RetentionPolicy{KeepLast: 1}, KeepSnapshots: []string{s1.ID.String()}}
	got, _ = keepSet(mine, run, s3.ID.String(), noLoad)
	if ids(got) != ids([]*repo.Snapshot{s1, s3}) {
		t.Errorf("keep last 1 + pinned: %s", ids(got))
	}
	// A new snapshot not among mine is loaded.
	s4 := mk(4, 4)
	got, _ = keepSet(nil, api.Run{}, s4.ID.String(), func() (*repo.Snapshot, error) { return s4, nil })
	if ids(got) != s4.ID.Short() {
		t.Errorf("loaded: %s", ids(got))
	}
	// No job, no new snapshot: nothing.
	if got, _ = keepSet(nil, api.Run{}, "", noLoad); len(got) != 0 {
		t.Errorf("empty: %s", ids(got))
	}
}
