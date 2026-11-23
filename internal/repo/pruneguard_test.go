package repo

import (
	"context"
	"testing"
	"time"
)

// unindexedRepo stores two groups of blobs, each flushed into index files of
// its own, and one snapshot using all of them; then it deletes the index
// files of the second group, as if they were lost or deleted by hand. It
// returns the blob IDs and the packs of the second group.
func unindexedRepo(t *testing.T, mem *memBackend) ([]ID, []ID) {
	t.Helper()
	ctx := context.Background()
	r, err := Init(ctx, mem)
	if err != nil {
		t.Fatal(err)
	}
	r.cfg.PackSize = 64 << 10
	var ids []ID
	save := func(from int) map[string]bool {
		for i := from; i < from+6; i++ {
			id, _, err := r.SaveBlob(ctx, DataBlob, randBlob(int64(i)+100, 12<<10))
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		if err := r.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		names := map[string]bool{}
		for n := range mem.objects("index/") {
			names[n] = true
		}
		return names
	}
	first := save(0)
	both := save(6)
	tree, err := r.SaveTree(ctx, &Tree{Nodes: []Node{{Name: "f", Type: NodeFile, Content: ids}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SaveSnapshot(ctx, &Snapshot{Time: time.Now(), Tree: tree}); err != nil {
		t.Fatal(err)
	}
	var second []ID
	for _, id := range ids[6:] {
		l, _ := r.idx.Lookup(BlobHandle{Type: DataBlob, ID: id})
		if len(second) == 0 || second[len(second)-1] != l.Pack {
			second = append(second, l.Pack)
		}
	}
	for n := range both {
		if !first[n] {
			delete(mem.objs, n)
		}
	}
	return ids, second
}

// B5 (0.59): prune deletes every pack the index lacks. A pack an index file
// lost (or deleted by hand) may hold the only copy of data a backup uses:
// it is verified and indexed again instead.
func TestPruneIndexesAgainAPackBackupsNeed(t *testing.T) {
	ctx := context.Background()
	mem := newMemBackend()
	ids, second := unindexedRepo(t, mem)
	r, err := Open(ctx, mem)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.DamagedIndexFiles()) != 0 {
		t.Fatalf("setup: damaged %v", r.DamagedIndexFiles())
	}
	st, err := r.Prune(ctx, PruneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if st.PacksRestored != len(second) || st.PacksDeleted != 0 {
		t.Fatalf("restored %d (want %d), deleted %d", st.PacksRestored, len(second), st.PacksDeleted)
	}
	for _, p := range second {
		if _, ok := mem.objs[packName(p)]; !ok {
			t.Fatalf("pack %s was deleted", p.Short())
		}
	}
	r2, err := Open(ctx, mem)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if _, err := r2.LoadBlob(ctx, DataBlob, id); err != nil {
			t.Fatalf("blob %s after prune: %v", id.Short(), err)
		}
	}
	// The next prune finds nothing to do.
	if st, err := r2.Prune(ctx, PruneOptions{}); err != nil || st.PacksRestored != 0 || st.PacksDeleted != 0 {
		t.Fatalf("second prune: %+v %v", st, err)
	}
}

// A pack that is proven damaged on every try is garbage as before; one
// that cannot be read now stays as it is.
func TestPruneDeletesOnlyProvenDamagedNeededPacks(t *testing.T) {
	ctx := context.Background()
	mem := newMemBackend()
	_, second := unindexedRepo(t, mem)
	mem.objs[packName(second[0])][0] ^= 0xff
	r, err := Open(ctx, mem)
	if err != nil {
		t.Fatal(err)
	}
	st, err := r.Prune(ctx, PruneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mem.objs[packName(second[0])]; ok {
		t.Error("a pack proven damaged was kept")
	}
	if st.PacksRestored != len(second)-1 {
		t.Errorf("restored %d, want %d", st.PacksRestored, len(second)-1)
	}
}

// A pack an interrupted backup left (no backup uses its data) goes as
// before.
func TestPruneStillDeletesLeftoverPacks(t *testing.T) {
	ctx := context.Background()
	mem := newMemBackend()
	snapshotRepo(t, mem, 6)
	r, err := Open(ctx, mem)
	if err != nil {
		t.Fatal(err)
	}
	r.cfg.PackSize = 64 << 10
	before := mem.objects("index/")
	if _, _, err := r.SaveBlob(ctx, DataBlob, randBlob(999, 12<<10)); err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	// The leftover's index entry is lost with the "interrupted" backup.
	for n := range mem.objects("index/") {
		if _, ok := before[n]; !ok {
			delete(mem.objs, n)
		}
	}
	r2, err := Open(ctx, mem)
	if err != nil {
		t.Fatal(err)
	}
	st, err := r2.Prune(ctx, PruneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if st.PacksRestored != 0 || st.PacksUnchecked != 0 || st.PacksDeleted != 1 {
		t.Errorf("the leftover pack: %+v", st)
	}
}
