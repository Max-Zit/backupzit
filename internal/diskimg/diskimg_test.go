package diskimg

import (
	"bytes"
	"context"
	"math/rand"
	"testing"

	"github.com/backupzit/backupzit/internal/backend"
	"github.com/backupzit/backupzit/internal/repo"
)

type memDisk struct {
	b     []byte
	reads int
}

func (d *memDisk) ReadAt(p []byte, off int64) (int, error) {
	d.reads++
	return copy(p, d.b[off:]), nil
}

func (d *memDisk) WriteAt(p []byte, off int64) (int, error) { return copy(d.b[off:], p), nil }

func TestStoreAreas(t *testing.T) {
	ctx := context.Background()
	be, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	const size = 10*BlockSize + 4096 // last block is short
	disk := &memDisk{b: make([]byte, size)}
	rng := rand.New(rand.NewSource(1))
	fill := func(off, n int) { rng.Read(disk.b[off : off+n]) }
	fill(0, 512)                 // boot sector
	fill(3*BlockSize+100, 5000)  // inside block 3
	fill(9*BlockSize, BlockSize) // block 9
	fill(10*BlockSize, 4096)     // short last block

	// First backup: only the allocated areas are read.
	areas := []Area{{0, 4096}, {3 * BlockSize, BlockSize}, {9 * BlockSize, BlockSize + 4096}}
	img1, err := StoreAreas(ctx, r, disk, size, areas, nil, 0, "d", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if disk.reads != 3 {
		t.Errorf("first backup: %d reads, want 3 runs", disk.reads)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	check := func(img *repo.DiskImage, want []byte) {
		t.Helper()
		out := &memDisk{b: bytes.Repeat([]byte{0xee}, size)}
		if err := Write(ctx, r, img, out, true, nil); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out.b, want) {
			t.Error("restored disk differs")
		}
	}
	orig1 := append([]byte(nil), disk.b...)
	check(&img1, orig1)

	// Second backup: block 5 changed, block 3 cleared; the rest comes from
	// the first backup without reading it.
	fill(5*BlockSize, 10)
	clear(disk.b[3*BlockSize : 4*BlockSize])
	disk.reads = 0
	img2, err := StoreAreas(ctx, r, disk, size, []Area{{5 * BlockSize, 10}, {3*BlockSize + 100, 5000}}, &img1, 0, "d", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if disk.reads != 2 {
		t.Errorf("incremental backup: %d reads, want 2", disk.reads)
	}
	if img2.Partitions[0].Method != "changed-blocks" || img2.Head != img1.Head {
		t.Errorf("method %q, head reused %v", img2.Partitions[0].Method, img2.Head == img1.Head)
	}
	check(&img2, append([]byte(nil), disk.b...))
	check(&img1, orig1) // the first backup is unchanged
}

func TestWriteChanges(t *testing.T) {
	ctx := context.Background()
	be, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	const size = 6*BlockSize + 1000
	rng := rand.New(rand.NewSource(2))
	src := &memDisk{b: make([]byte, size)}
	rng.Read(src.b[:2*BlockSize])
	rng.Read(src.b[4*BlockSize : 5*BlockSize])
	img1, err := Store(ctx, r, src, size, 0, "d", "t", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Changes: block 1 rewritten, block 4 emptied, block 5 filled.
	rng.Read(src.b[BlockSize : BlockSize+100])
	clear(src.b[4*BlockSize : 5*BlockSize])
	rng.Read(src.b[5*BlockSize : size])
	img2, err := Store(ctx, r, src, size, 0, "d", "t", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Flush(ctx)

	// The replica holds image 1 (first sync: everything written).
	replica := &memDisk{b: bytes.Repeat([]byte{0xee}, size)}
	if n, err := WriteChanges(ctx, r, &img1, nil, replica, nil); err != nil || n != size {
		t.Fatalf("full write: %d %v", n, err)
	}
	// Second sync writes only blocks 1, 4 and 5.
	n, err := WriteChanges(ctx, r, &img2, &img1, replica, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := uint64(2*BlockSize + 1000 + BlockSize); n != want {
		t.Errorf("incremental write: %d bytes, want %d", n, want)
	}
	if !bytes.Equal(replica.b, src.b) {
		t.Error("replica differs from the source")
	}
}
