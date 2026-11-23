package repo

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// Repair takes a damaged pack out of the index: its data is stored again by
// the next backup instead of being reused, and the snapshot that needed it
// is named; the others are untouched.
func TestDropDamagedPacks(t *testing.T) {
	ctx := context.Background()
	be := newMemBackend()
	r := newTestRepo(t, be, 1<<20)
	snapshotOf := func(data []byte) (ID, ID) {
		id, _, err := r.SaveBlob(ctx, DataBlob, data)
		if err != nil {
			t.Fatal(err)
		}
		tree, err := r.SaveTree(ctx, &Tree{Nodes: []Node{{Name: "f", Type: NodeFile, Content: []ID{id}, Size: uint64(len(data))}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		sn, err := r.SaveSnapshot(ctx, &Snapshot{Time: time.Now(), Tree: tree})
		if err != nil {
			t.Fatal(err)
		}
		return id, sn
	}
	lostData := bytes.Repeat([]byte("on the damaged pack "), 60000)
	lostID, lostSn := snapshotOf(lostData)
	keptID, _ := snapshotOf(bytes.Repeat([]byte("elsewhere "), 60000))
	// A third backup whose folder list cannot be read now (its pack is gone
	// from this agent's view, not proven damaged): kept, not affected (A2).
	_, unreadSn := snapshotOf(bytes.Repeat([]byte("third "), 60000))
	unreadTree := func() ID {
		sn, err := r.LoadSnapshot(ctx, unreadSn.String())
		if err != nil {
			t.Fatal(err)
		}
		return sn.Tree
	}()
	tl, ok := r.idx.Lookup(BlobHandle{Type: TreeBlob, ID: unreadTree})
	if !ok {
		t.Fatal("setup: tree not indexed")
	}
	be.mu.Lock()
	delete(be.objs, "data/"+tl.Pack.String()[:2]+"/"+tl.Pack.String())
	be.mu.Unlock()
	loc, ok := r.idx.Lookup(BlobHandle{Type: DataBlob, ID: lostID})
	if !ok {
		t.Fatal("setup: blob not indexed")
	}
	if l2, _ := r.idx.Lookup(BlobHandle{Type: DataBlob, ID: keptID}); l2.Pack == loc.Pack {
		t.Fatal("setup: both blobs in one pack")
	}

	lock, err := r.Lock(ctx, true, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.DropDamagedPacks(ctx, []ID{loc.Pack})
	lock.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if res.Packs != 1 || res.Blobs < 1 || len(res.Snapshots) != 1 || res.Snapshots[0].ID != lostSn || len(res.Unchecked) != 1 || res.Unchecked[0].ID != unreadSn {
		t.Fatalf("result %+v (snapshots %d)", res, len(res.Snapshots))
	}
	if r.idx.Has(BlobHandle{Type: DataBlob, ID: lostID}) || !r.idx.Has(BlobHandle{Type: DataBlob, ID: keptID}) {
		t.Fatal("index after repair: the lost blob is still there or the kept one is gone")
	}
	// A fresh open sees the same index (written to the storage).
	r2, err := Open(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	if r2.idx.Has(BlobHandle{Type: DataBlob, ID: lostID}) || !r2.idx.Has(BlobHandle{Type: DataBlob, ID: keptID}) {
		t.Fatal("reopened index differs")
	}
	if _, added, err := r2.SaveBlob(ctx, DataBlob, lostData); err != nil || !added {
		t.Fatalf("the next backup does not store the lost data again: added=%v err=%v", added, err)
	}
}
