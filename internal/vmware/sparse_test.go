package vmware

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math/rand"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/diskimg"
	"github.com/max-zit/backupzit/internal/repo"
)

// readSparse decodes a monolithicSparse VMDK into a flat disk.
func readSparse(t *testing.T, b []byte) []byte {
	t.Helper()
	h := b[:512]
	if binary.LittleEndian.Uint32(h) != 0x564d444b {
		t.Fatal("bad magic")
	}
	capacity := binary.LittleEndian.Uint64(h[12:])
	grain := binary.LittleEndian.Uint64(h[20:])
	gtes := uint64(binary.LittleEndian.Uint32(h[44:]))
	gd := binary.LittleEndian.Uint64(h[56:])
	rgd := binary.LittleEndian.Uint64(h[48:])
	if !bytes.Contains(b[512:512*21], []byte(`createType="monolithicSparse"`)) {
		t.Fatal("no descriptor")
	}
	out := make([]byte, capacity*512)
	grains := (capacity + grain - 1) / grain
	for _, dir := range []uint64{gd, rgd} {
		for g := uint64(0); g < grains; g++ {
			gt := uint64(binary.LittleEndian.Uint32(b[dir*512+(g/gtes)*4:]))
			sec := uint64(binary.LittleEndian.Uint32(b[gt*512+(g%gtes)*4:]))
			if sec == 0 {
				continue
			}
			if dir == rgd { // the redundant directory must agree
				if want := binary.LittleEndian.Uint32(b[gd*512+(g/gtes)*4:]); want == 0 {
					t.Fatal("directories differ")
				}
				continue
			}
			end := min(uint64(len(out)), (g+1)*grain*512)
			copy(out[g*grain*512:end], b[sec*512:sec*512+(end-g*grain*512)])
		}
	}
	return out
}

func TestSparseVMDK(t *testing.T) {
	ctx := context.Background()
	be, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	// 70 MiB + 12 KiB: crosses a grain table (32 MiB) and ends in a short
	// block; some blocks hold data, the rest stays empty.
	size := 70<<20 + 12<<10
	disk := make([]byte, size)
	rnd := rand.New(rand.NewSource(7))
	for _, off := range []int{0, 5 << 20, 33<<20 + 4096, 69 << 20, size - 1000} {
		rnd.Read(disk[off:min(off+300<<10, size)])
	}
	img, err := diskimg.Store(ctx, r, bytes.NewReader(disk), uint64(size), 0, "test", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	ids, grains, err := sparseImage(ctx, r, &img)
	if err != nil {
		t.Fatal(err)
	}
	l := newSparseLayout(img.Size, grains, "lsilogic")
	var done uint64
	s := &sparseReader{ctx: ctx, r: r, p: &img.Partitions[0], ids: ids, head: l.metadata(), progress: func(n uint64) { done += n }}
	b, err := io.ReadAll(s)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(b)) != l.Size() {
		t.Fatalf("size %d, layout says %d", len(b), l.Size())
	}
	if done != uint64(size) {
		t.Errorf("progress %d of %d", done, size)
	}
	if len(b) > 20<<20 {
		t.Errorf("sparse file %d bytes: empty blocks were stored", len(b))
	}
	got := readSparse(t, b)
	if !bytes.Equal(got[:size], disk) {
		t.Fatal("disk read back from the sparse VMDK differs")
	}
}
