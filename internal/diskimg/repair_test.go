package diskimg

import (
	"bytes"
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// After repair took a damaged pack out of the index, an incremental backup
// with no changed blocks reads the lost blocks from the disk again instead
// of reusing them, so the new backup restores completely.
func TestStoreAreasRereadsLostBlocks(t *testing.T) {
	ctx := context.Background()
	be, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	const size = 4 * BlockSize
	disk := &memDisk{b: make([]byte, size)}
	rand.New(rand.NewSource(7)).Read(disk.b)
	img1, err := StoreAreas(ctx, r, disk, size, []Area{{0, size}}, nil, 0, "d", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	ids, err := r.LoadBlockMap(ctx, &img1.Partitions[0])
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := r.Index().Lookup(repo.BlobHandle{Type: repo.DataBlob, ID: ids[2]})
	lock, err := r.Lock(ctx, true, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.DropDamagedPacks(ctx, []repo.ID{loc.Pack}); err != nil {
		t.Fatal(err)
	}
	lock.Unlock()

	disk.reads = 0
	img2, err := StoreAreas(ctx, r, disk, size, nil, &img1, 0, "d", "test", nil) // nothing changed
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if disk.reads == 0 {
		t.Fatal("the lost blocks were not read again")
	}
	out := &memDisk{b: make([]byte, size)}
	if err := Write(ctx, r, &img2, out, true, nil); err != nil {
		t.Fatalf("restore of the new backup: %v", err)
	}
	if !bytes.Equal(out.b, disk.b) {
		t.Fatal("the new backup does not restore the disk")
	}
}

// Only a data block's pack is lost, the block map survives in another
// pack: just that block is read again, the rest is reused.
func TestStoreAreasRereadsOnlyLostBlock(t *testing.T) {
	ctx := context.Background()
	be, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	const size = 40 * BlockSize // several ~16 MiB packs; the map lands in the last
	disk := &memDisk{b: make([]byte, size)}
	rand.New(rand.NewSource(8)).Read(disk.b)
	img1, err := StoreAreas(ctx, r, disk, size, []Area{{0, size}}, nil, 0, "d", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	ids, _ := r.LoadBlockMap(ctx, &img1.Partitions[0])
	first, _ := r.Index().Lookup(repo.BlobHandle{Type: repo.DataBlob, ID: ids[0]})
	mapLoc, _ := r.Index().Lookup(repo.BlobHandle{Type: repo.MapBlob, ID: img1.Partitions[0].Maps[0]})
	if first.Pack == mapLoc.Pack {
		t.Skip("block 0 and the map ended up in one pack")
	}
	lock, err := r.Lock(ctx, true, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.DropDamagedPacks(ctx, []repo.ID{first.Pack}); err != nil {
		t.Fatal(err)
	}
	lock.Unlock()
	lost := 0
	for _, id := range ids {
		if !r.Index().Has(repo.BlobHandle{Type: repo.DataBlob, ID: id}) {
			lost++
		}
	}
	var read uint64
	img2, err := StoreAreas(ctx, r, disk, size, nil, &img1, 0, "d", "test", func(n uint64) { read += n })
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if lost == 0 || lost == len(ids) || read >= size {
		t.Errorf("%d of %d blocks lost, %d of %d bytes read: want only the lost ones read", lost, len(ids), read, size)
	}
	out := &memDisk{b: make([]byte, size)}
	if err := Write(ctx, r, &img2, out, true, nil); err != nil || !bytes.Equal(out.b, disk.b) {
		t.Fatalf("restore of the new backup: %v", err)
	}
}
