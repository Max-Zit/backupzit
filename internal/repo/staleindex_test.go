package repo

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// A run opens the repository (and its index) long before it gets its lock,
// for example while another run prunes. After the lock its index must show
// what happened meanwhile: blobs a prune deleted are no longer "stored",
// and packs another run wrote are known (so a prune keeps them).
func TestIndexRefreshedAfterLock(t *testing.T) {
	ctx := context.Background()
	be := newMemBackend()
	other := newTestRepo(t, be, 1<<20)
	gone := bytes.Repeat([]byte("old data "), 50000)
	id, _, err := other.SaveBlob(ctx, DataBlob, gone)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	tree, err := other.SaveTree(ctx, &Tree{Nodes: []Node{{Name: "f", Type: NodeFile, Content: []ID{id}, Size: uint64(len(gone))}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	snID, err := other.SaveSnapshot(ctx, &Snapshot{Time: time.Now(), Tree: tree})
	if err != nil {
		t.Fatal(err)
	}

	// The waiting run opens the repository now: its index has the blob.
	run, err := Open(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	if !run.idx.Has(BlobHandle{Type: DataBlob, ID: id}) {
		t.Fatal("setup: the blob is not in the index")
	}

	// Meanwhile the other run forgets the backup and prunes it away, and
	// writes new data.
	if err := other.RemoveSnapshot(ctx, snID); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Prune(ctx, PruneOptions{}); err != nil {
		t.Fatal(err)
	}
	fresh := bytes.Repeat([]byte("new data "), 50000)
	freshID, _, err := other.SaveBlob(ctx, DataBlob, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	lock, err := run.Lock(ctx, false, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	if run.idx.Has(BlobHandle{Type: DataBlob, ID: id}) {
		t.Error("after the lock the pruned blob still counts as stored: a backup would reference deleted data")
	}
	if !run.idx.Has(BlobHandle{Type: DataBlob, ID: freshID}) {
		t.Error("after the lock the other run's new pack is unknown: a prune would delete it")
	}
	if _, added, err := run.SaveBlob(ctx, DataBlob, gone); err != nil || !added {
		t.Errorf("saving the pruned data again: added=%v err=%v, want it stored", added, err)
	}
}
