package e2e

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/backupzit/backupzit/internal/archiver"
	"github.com/backupzit/backupzit/internal/backend"
	"github.com/backupzit/backupzit/internal/checker"
	"github.com/backupzit/backupzit/internal/repo"
	"github.com/backupzit/backupzit/internal/restorer"
)

func dirSize(t *testing.T, root string) int64 {
	var n int64
	filepath.Walk(root, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			n += fi.Size()
		}
		return nil
	})
	return n
}

// TestForgetAndPrune removes an old snapshot and checks that its unique
// data is freed while the remaining snapshot stays complete.
func TestForgetAndPrune(t *testing.T) {
	ctx := context.Background()
	repoDir := filepath.Join(tempDir(t), "repo")
	be, _ := backend.OpenLocal(repoDir)
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(5))
	src := filepath.Join(tempDir(t), "src")
	for i := 0; i < 6; i++ {
		writeFile(t, filepath.Join(src, fmt.Sprintf("f%d.bin", i)), randBytes(rng, 3*mib))
	}
	sn1, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		os.Remove(filepath.Join(src, fmt.Sprintf("f%d.bin", i)))
	}
	writeFile(t, filepath.Join(src, "g.bin"), randBytes(rng, 2*mib))
	want := manifest(t, src)
	sn2, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}})
	if err != nil {
		t.Fatal(err)
	}
	before := dirSize(t, filepath.Join(repoDir, "data"))

	// Locks: prune needs exclusive access.
	shared, err := r.Lock(ctx, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Lock(ctx, true, 0); !errors.Is(err, repo.ErrLocked) {
		t.Fatalf("exclusive lock while shared lock held: %v", err)
	}
	shared.Unlock()
	lock, err := r.Lock(ctx, true, 0)
	if err != nil {
		t.Fatal(err)
	}

	keep, remove := repo.ApplyPolicy([]*repo.Snapshot{sn1, sn2}, repo.RetentionPolicy{KeepLast: 1}, time.Local)
	if len(keep) != 1 || keep[0].ID != sn2.ID || len(remove) != 1 {
		t.Fatalf("policy: keep %d remove %d", len(keep), len(remove))
	}
	if err := r.RemoveSnapshot(ctx, sn1.ID); err != nil {
		t.Fatal(err)
	}
	st, err := r.Prune(ctx, repo.PruneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	lock.Unlock()
	after := dirSize(t, filepath.Join(repoDir, "data"))
	t.Logf("prune: %+v, data %d -> %d bytes", st, before, after)
	if before-after < 11*mib {
		t.Errorf("prune freed only %d bytes, expected about 12 MiB", before-after)
	}

	// Remaining snapshot is complete and restores exactly; a fresh open
	// (index reloaded from disk) agrees.
	r2, err := repo.Open(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	res, err := checker.Run(ctx, r2, checker.Options{ReadData: true})
	if err != nil || !res.OK() || len(res.Warnings) > 0 || res.Snapshots != 1 {
		t.Fatalf("check after prune: %v %+v", err, res)
	}
	tgt := filepath.Join(tempDir(t), "out")
	if st, err := restorer.Run(ctx, r2, sn2, restorer.Options{Target: tgt, Verify: true}); err != nil || len(st.Errors) > 0 {
		t.Fatalf("restore after prune: %v %v", err, st)
	}
	compareManifests(t, want, manifest(t, restoredRoot(t, tgt, src)))

	// A second prune has nothing to do.
	st, err = r2.Prune(ctx, repo.PruneOptions{})
	if err != nil || st.PacksDeleted != 0 || st.PacksRepacked != 0 {
		t.Errorf("second prune: %+v %v", st, err)
	}
}

func TestStaleLockIgnored(t *testing.T) {
	ctx := context.Background()
	be, _ := backend.OpenLocal(filepath.Join(tempDir(t), "repo"))
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	old := fmt.Sprintf(`{"time":%q,"exclusive":true,"hostname":"crashed","pid":1}`, time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339))
	be.Save(ctx, "locks/0000000000000001", []byte(old))
	l, err := r.Lock(ctx, true, 0)
	if err != nil {
		t.Fatalf("stale lock blocked: %v", err)
	}
	l.Unlock()
	fresh := fmt.Sprintf(`{"time":%q,"exclusive":true,"hostname":"other","pid":1}`, time.Now().UTC().Format(time.RFC3339))
	be.Save(ctx, "locks/0000000000000002", []byte(fresh))
	if _, err := r.Lock(ctx, false, 0); !errors.Is(err, repo.ErrLocked) {
		t.Fatalf("live exclusive lock not respected: %v", err)
	}
}
