package diskimg

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"strings"
	"sync"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// oddDisk fails, cuts short or ends early at given offsets and counts the
// reads that are running.
type oddDisk struct {
	b       []byte
	failAt  int64 // a read at this offset fails
	shortAt int64 // a read at this offset returns less without an error
	endAt   int64 // the disk ends here: reads past it get io.EOF

	mu     sync.Mutex
	active int
}

func (d *oddDisk) ReadAt(p []byte, off int64) (int, error) {
	d.mu.Lock()
	d.active++
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.active--
		d.mu.Unlock()
	}()
	switch {
	case off == d.failAt:
		return 0, errors.New("device not ready")
	case off == d.shortAt:
		return copy(p[:len(p)/2], d.b[off:]), nil
	case d.endAt > 0 && off+int64(len(p)) > d.endAt:
		return copy(p, d.b[off:d.endAt]), io.EOF
	}
	return copy(p, d.b[off:]), nil
}

func testRepo(t *testing.T) *repo.Repository {
	t.Helper()
	be, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(context.Background(), be)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// The disk is read by a goroutine ahead of hashing (#71): a read error or
// a short read stops the backup with an error, and no read is running any
// more when StoreAreas returns (the caller then closes the disk).
func TestStoreAreasReadFailures(t *testing.T) {
	ctx := context.Background()
	const size = 100 * BlockSize
	for _, tc := range []struct {
		name string
		disk *oddDisk
		want string
	}{
		{"error", &oddDisk{failAt: 64 * BlockSize, shortAt: -1}, "device not ready"},
		{"short read", &oddDisk{failAt: -1, shortAt: 32 * BlockSize}, "unexpected EOF"},
	} {
		tc.disk.b = make([]byte, size)
		rand.New(rand.NewSource(2)).Read(tc.disk.b)
		_, err := StoreAreas(ctx, testRepo(t), tc.disk, size, []Area{{0, size}}, nil, 0, "d", "test", nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v", tc.name, err)
		}
		tc.disk.mu.Lock()
		if tc.disk.active != 0 {
			t.Errorf("%s: %d reads still running", tc.name, tc.disk.active)
		}
		tc.disk.mu.Unlock()
	}
}

// A disk that ends before its size reads as zeros past its end, not as
// bytes of the run read before into the same buffer (#71 review).
func TestStoreAreasDiskEndsEarly(t *testing.T) {
	ctx := context.Background()
	r := testRepo(t)
	const size = 70 * BlockSize // three runs: buffers A, B, A
	disk := &oddDisk{b: make([]byte, size), failAt: -1, shortAt: -1, endAt: 66 * BlockSize}
	rand.New(rand.NewSource(3)).Read(disk.b[:disk.endAt])
	img, err := StoreAreas(ctx, r, disk, size, []Area{{0, size}}, nil, 0, "d", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	out := &memDisk{b: bytes.Repeat([]byte{0xee}, size)}
	if err := Write(ctx, r, &img, out, true, nil); err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), disk.b[:disk.endAt]...), make([]byte, size-disk.endAt)...)
	if !bytes.Equal(out.b, want) {
		t.Error("the restored disk differs: the part past the end is not zeros")
	}
}
