package repo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
)

// MED 5 (0.63): a whole pack was read without a size limit (data check,
// prune and the index rebuild verify packs that way), so a huge file
// planted under data/ exhausted the memory. It is now read with a limit,
// and a pack over it is neither verified nor counted as damaged.

func setPackLimit(t *testing.T, n int64) {
	t.Helper()
	old := maxPackFile
	maxPackFile = n
	t.Cleanup(func() { maxPackFile = old })
}

// limitedMem reads with a size limit, as the storage backends do, and
// counts the reads of data packs.
type limitedMem struct {
	*memBackend
	packReads int
}

func (l *limitedMem) LoadMax(ctx context.Context, name string, max int64) ([]byte, error) {
	if strings.HasPrefix(name, "data/") {
		l.packReads++
	}
	if size, err := l.Size(ctx, name); err == nil && size > max {
		return nil, fmt.Errorf("%s is larger than %d bytes: %w", name, max, backend.ErrTooLarge)
	}
	return l.Load(ctx, name)
}

func TestVerifyPackOverTheLimit(t *testing.T) {
	ctx := context.Background()
	mem := newMemBackend()
	snapshotRepo(t, mem, 12)
	be := &limitedMem{memBackend: mem}
	r, err := Open(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	packs, err := r.ListPacks(ctx)
	if err != nil || len(packs) == 0 {
		t.Fatalf("packs: %v %v", packs, err)
	}
	var big ID // a data pack, not the small one of the folder list
	for _, p := range packs {
		if size, _ := mem.Size(ctx, packName(p)); size > 32<<10 {
			big = p
		}
	}
	if _, err := r.VerifyPack(ctx, big); err != nil {
		t.Fatalf("under the limit: %v", err)
	}
	setPackLimit(t, 16<<10)
	be.packReads = 0
	err = Tries(ctx, 0, func() error {
		_, err := r.VerifyPack(ctx, big)
		return err
	})
	if !errors.Is(err, ErrPackTooLarge) || errors.Is(err, ErrPackDamaged) || errors.Is(err, ErrPackGone) {
		t.Fatalf("over the limit: %v", err)
	}
	if be.packReads != 1 {
		t.Errorf("%d reads of a pack over the limit, want 1 (no retries)", be.packReads)
	}
}

// Prune leaves a pack over the limit alone when it may hold data backups use
// (it cannot be verified), instead of deleting or indexing it.
func TestPruneLeavesAPackOverTheLimit(t *testing.T) {
	ctx := context.Background()
	mem := newMemBackend()
	_, second := unindexedRepo(t, mem)
	r, err := Open(ctx, &limitedMem{memBackend: mem})
	if err != nil {
		t.Fatal(err)
	}
	setPackLimit(t, 16<<10)
	st, err := r.Prune(ctx, PruneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if st.PacksRestored != 0 || st.PacksUnchecked != len(second) {
		t.Errorf("stats: %+v", st)
	}
	for _, p := range second {
		if _, ok := mem.objs[packName(p)]; !ok {
			t.Errorf("pack %s over the limit was deleted", p.Short())
		}
	}
}

// The index rebuild refuses, touching nothing, when a pack it has to verify
// is over the limit: it is no proof of damage, and trying again does not
// help.
func TestRebuildStopsAtAPackOverTheLimit(t *testing.T) {
	ctx := context.Background()
	mem := newMemBackend()
	snapshotRepo(t, mem, 12)
	truncateIndexFiles(mem)
	r, err := Open(ctx, &limitedMem{memBackend: mem})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.DamagedIndexFiles()) == 0 {
		t.Fatal("setup: no damaged index file")
	}
	before := copyObjects(mem)
	setPackLimit(t, 16<<10)
	_, err = r.RebuildIndex(ctx)
	if !errors.Is(err, ErrPackTooLarge) || !strings.Contains(err.Error(), "no BackupZit pack is that large") || !strings.Contains(err.Error(), "not touched") {
		t.Fatalf("rebuild: %v", err)
	}
	requireUntouched(t, mem, before)
}
