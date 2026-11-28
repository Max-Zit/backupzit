package repo

import (
	"context"
	"strings"
	"testing"
	"time"
)

// MED 4 (0.63): prune built the new index from the storage listing and
// dropped every indexed pack the listing lacked without asking the storage,
// and a pack the index did not list counted as garbage when its blobs were
// indexed elsewhere, even in a pack that was gone: the last copy of data
// backups use was deleted.

// packOf is the pack the index places blob id in.
func packOf(t *testing.T, r *Repository, id ID) ID {
	t.Helper()
	l, ok := r.idx.Lookup(BlobHandle{Type: DataBlob, ID: id})
	if !ok {
		t.Fatalf("blob %s not indexed", id.Short())
	}
	return l.Pack
}

// A blob indexed in a pack that is gone and held by a pack the index does
// not list (a backup that died before writing its index, then the same data
// stored again): the unindexed pack is verified and indexed again instead of
// being deleted.
func TestPruneKeepsTheOtherCopyOfABlobInAGonePack(t *testing.T) {
	ctx := context.Background()
	mem := newMemBackend()
	data := randBlob(42, 12<<10)

	// First copy: pack B, whose index file is then lost.
	r1 := newTestRepo(t, mem, 64<<10)
	x, _, err := r1.SaveBlob(ctx, DataBlob, data)
	if err != nil {
		t.Fatal(err)
	}
	if err := r1.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	b := packOf(t, r1, x)
	for n := range mem.objects("index/") {
		delete(mem.objs, n)
	}

	// Second copy: pack A (with another blob, so that it is not the same
	// file), indexed; the folder list in a pack of its own.
	r2, err := Open(ctx, mem)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r2.SaveBlob(ctx, DataBlob, randBlob(43, 1<<10)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r2.SaveBlob(ctx, DataBlob, data); err != nil {
		t.Fatal(err)
	}
	if err := r2.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	a := packOf(t, r2, x)
	if a == b {
		t.Fatal("setup: one pack")
	}
	tree, err := r2.SaveTree(ctx, &Tree{Nodes: []Node{{Name: "f", Type: NodeFile, Content: []ID{x}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r2.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r2.SaveSnapshot(ctx, &Snapshot{Time: time.Now(), Tree: tree}); err != nil {
		t.Fatal(err)
	}
	// Pack A is lost on the storage.
	delete(mem.objs, packName(a))

	r, err := Open(ctx, mem)
	if err != nil {
		t.Fatal(err)
	}
	st, err := r.Prune(ctx, PruneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if st.PacksRestored != 1 || st.PacksDeleted != 0 {
		t.Errorf("stats: %+v", st)
	}
	if _, ok := mem.objs[packName(b)]; !ok {
		t.Fatal("prune deleted the last copy of a blob a backup uses")
	}
	r3, err := Open(ctx, mem)
	if err != nil {
		t.Fatal(err)
	}
	if got := packOf(t, r3, x); got != b {
		t.Errorf("blob indexed in %s, want %s", got.Short(), b.Short())
	}
	requireBlobs(t, r3, []ID{x})
}

// An indexed pack the listing does not show but the storage still has: the
// listing is short, and prune stops without touching anything.
func TestPruneStopsOnAShortListing(t *testing.T) {
	ctx := context.Background()
	mem := newMemBackend()
	ids := snapshotRepo(t, mem, 12)
	r0, err := Open(ctx, mem)
	if err != nil {
		t.Fatal(err)
	}
	hidden := packName(packOf(t, r0, ids[3]))
	// Something to prune: a pack no index lists and no backup uses.
	if err := mem.Save(ctx, packName(Hash([]byte("left over"))), []byte("left over")); err != nil {
		t.Fatal(err)
	}
	be := &listHidingBackend{memBackend: mem, hide: map[string]bool{hidden: true}}
	r, err := Open(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	before := copyObjects(mem)
	_, err = r.Prune(ctx, PruneOptions{})
	if err == nil || !strings.Contains(err.Error(), "missing from its listing") {
		t.Fatalf("prune with a short listing: %v", err)
	}
	requireUntouched(t, mem, before)

	// Nothing listed at all: refused as well.
	be.hideAll = true
	if _, err := r.Prune(ctx, PruneOptions{}); err == nil || !strings.Contains(err.Error(), "lists no data packs at all") {
		t.Fatalf("prune with an empty listing: %v", err)
	}
	requireUntouched(t, mem, before)
}

// Data backups use that is only in a pack the storage no longer has: prune
// refuses (repair is the step for lost data) and touches nothing; after
// repair took the pack out of the index, prune works again.
func TestPruneRefusesWhenUsedDataIsInAGonePack(t *testing.T) {
	ctx := context.Background()
	mem := newMemBackend()
	ids := snapshotRepo(t, mem, 12)
	r0, err := Open(ctx, mem)
	if err != nil {
		t.Fatal(err)
	}
	gone := packOf(t, r0, ids[3])
	delete(mem.objs, packName(gone))
	if err := mem.Save(ctx, packName(Hash([]byte("left over"))), []byte("left over")); err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, mem)
	if err != nil {
		t.Fatal(err)
	}
	before := copyObjects(mem)
	_, err = r.Prune(ctx, PruneOptions{})
	if err == nil || !strings.Contains(err.Error(), "run a data check and repair") || !strings.Contains(err.Error(), gone.Short()) {
		t.Fatalf("prune with used data in a gone pack: %v", err)
	}
	requireUntouched(t, mem, before)

	if _, err := r.DropDamagedPacks(ctx, []ID{gone}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Prune(ctx, PruneOptions{}); err != nil {
		t.Fatalf("prune after repair: %v", err)
	}
}

// A gone pack whose data no backup uses leaves the index, even when nothing
// else is pruned: a new backup must not reuse its blobs.
func TestPruneDropsAGonePackNoBackupUses(t *testing.T) {
	ctx := context.Background()
	mem := newMemBackend()
	snapshotRepo(t, mem, 6)
	r0, err := Open(ctx, mem)
	if err != nil {
		t.Fatal(err)
	}
	r0.cfg.PackSize = 64 << 10
	unused, _, err := r0.SaveBlob(ctx, DataBlob, randBlob(999, 12<<10))
	if err != nil {
		t.Fatal(err)
	}
	if err := r0.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	gone := packOf(t, r0, unused)
	delete(mem.objs, packName(gone))

	r, err := Open(ctx, mem)
	if err != nil {
		t.Fatal(err)
	}
	st, err := r.Prune(ctx, PruneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if st.PacksGone != 1 {
		t.Errorf("stats: %+v", st)
	}
	if _, ok := r.idx.Packs()[gone]; ok {
		t.Error("the gone pack is still in the index")
	}
	if r.idx.Has(BlobHandle{Type: DataBlob, ID: unused}) {
		t.Error("a new backup would reuse a blob of the gone pack")
	}
}

// Most packs gone at once is a folder that is not mounted, not lost data:
// nothing is pruned.
func TestPruneStopsWhenMostPacksAreGone(t *testing.T) {
	ctx := context.Background()
	mem := newMemBackend()
	snapshotRepo(t, mem, 150)
	r0, err := Open(ctx, mem)
	if err != nil {
		t.Fatal(err)
	}
	packs := r0.idx.Packs()
	if len(packs) < 22 {
		t.Fatalf("setup: %d packs", len(packs))
	}
	// The folder list stays, so that the backups can be read.
	sns, err := r0.ListSnapshots(ctx)
	if err != nil || len(sns) != 1 {
		t.Fatalf("snapshots: %v %v", sns, err)
	}
	l, ok := r0.idx.Lookup(BlobHandle{Type: TreeBlob, ID: sns[0].Tree})
	if !ok {
		t.Fatal("tree not indexed")
	}
	removed := 0
	for p := range packs {
		if removed*2 <= len(packs) && p != l.Pack {
			delete(mem.objs, packName(p))
			removed++
		}
	}
	r, err := Open(ctx, mem)
	if err != nil {
		t.Fatal(err)
	}
	before := copyObjects(mem)
	if _, err := r.Prune(ctx, PruneOptions{}); err == nil || !strings.Contains(err.Error(), "not mounted") {
		t.Fatalf("prune with most packs gone: %v", err)
	}
	requireUntouched(t, mem, before)
}
