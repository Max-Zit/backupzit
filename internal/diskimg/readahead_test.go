package diskimg

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// slowObjects is a local backend that behaves like an object store for
// reads: each ranged read takes a while, several may run at once, and one
// block can be made to fail.
type slowObjects struct {
	backend.Backend
	delay   time.Duration
	failAt  int32 // fail the n-th ranged pack read (1-based), 0: never
	reads   atomic.Int32
	now     atomic.Int32
	maxSeen atomic.Int32
}

func (s *slowObjects) Concurrency() int { return 4 }

func (s *slowObjects) LoadRange(ctx context.Context, name string, off int64, n int) ([]byte, error) {
	if !strings.HasPrefix(name, "data/") {
		return s.Backend.LoadRange(ctx, name, off, n)
	}
	k := s.reads.Add(1)
	cur := s.now.Add(1)
	defer s.now.Add(-1)
	for {
		m := s.maxSeen.Load()
		if cur <= m || s.maxSeen.CompareAndSwap(m, cur) {
			break
		}
	}
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if k == s.failAt {
		return nil, errors.New("503 SlowDown")
	}
	return s.Backend.LoadRange(ctx, name, off, n)
}

// storeTestDisk saves a 24-block disk (some blocks empty) and returns the
// disk, its image and a repository opened on a slow, concurrent backend.
func storeTestDisk(t *testing.T, failAt int32) (*memDisk, repo.DiskImage, *repo.Repository, *slowObjects) {
	t.Helper()
	ctx := context.Background()
	local, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := repo.Init(ctx, local)
	if err != nil {
		t.Fatal(err)
	}
	const size = 24 * BlockSize
	disk := &memDisk{b: make([]byte, size)}
	rng := rand.New(rand.NewSource(5))
	for blk := 0; blk < 24; blk++ {
		if blk%5 != 3 { // every fifth block stays empty
			rng.Read(disk.b[blk*BlockSize : (blk+1)*BlockSize])
		}
	}
	img, err := StoreAreas(ctx, w, disk, size, []Area{{0, size}}, nil, 0, "d", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	w.Close()
	slow := &slowObjects{Backend: local, delay: 20 * time.Millisecond, failAt: failAt}
	r, err := repo.Open(ctx, slow)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return disk, img, r, slow
}

// A restore from an object store reads several blocks at once and still
// writes the disk exactly, in order, also when only changes are written.
func TestWriteReadsAhead(t *testing.T) {
	disk, img, r, slow := storeTestDisk(t, 0)
	ctx := context.Background()
	out := &memDisk{b: bytes.Repeat([]byte{0xee}, len(disk.b))}
	if err := Write(ctx, r, &img, out, true, nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.b, disk.b) {
		t.Fatal("restored disk differs")
	}
	if got := slow.maxSeen.Load(); got < 2 || got > 4 {
		t.Errorf("%d reads at once, want 2–4", got)
	}
	// WriteChanges against the same image writes nothing; against no
	// earlier image it writes every block.
	if n, err := WriteChanges(ctx, r, &img, &img, out, nil); err != nil || n != 0 {
		t.Errorf("unchanged: %d bytes, %v", n, err)
	}
	out2 := &memDisk{b: bytes.Repeat([]byte{0xee}, len(disk.b))}
	if n, err := WriteChanges(ctx, r, &img, nil, out2, nil); err != nil || n != uint64(len(disk.b)) || !bytes.Equal(out2.b, disk.b) {
		t.Errorf("full: %d bytes, %v", n, err)
	}
}

// A block that cannot be read stops the restore with its number, and no
// read outlives Write.
func TestWriteReadAheadError(t *testing.T) {
	disk, img, r, slow := storeTestDisk(t, 6)
	out := &memDisk{b: make([]byte, len(disk.b))}
	err := Write(context.Background(), r, &img, out, false, nil)
	if err == nil || !strings.Contains(err.Error(), "SlowDown") || !strings.Contains(err.Error(), "block ") {
		t.Fatalf("want a block error, got %v", err)
	}
	if n := slow.now.Load(); n != 0 {
		t.Errorf("%d reads still running after Write returned", n)
	}
}
