package imaging

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// writePartition reads its blocks ahead (several at once, also from a
// share) and still puts every block at its offset: null blocks stay
// untouched, the block at a shrunk partition's end is cut, blocks past it
// are left out.
func TestWritePartitionReadsAheadInPlace(t *testing.T) {
	ctx := context.Background()
	be, err := backend.OpenLocal(filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.ReadConcurrency() < 2 {
		t.Fatalf("read concurrency %d on a local repository", r.ReadConcurrency())
	}
	const bs, blocks = 4096, 12
	ids := make([]repo.ID, blocks)
	for i := range ids {
		if i == 3 {
			continue // null block
		}
		if ids[i], _, err = r.SaveBlob(ctx, repo.DataBlob, bytes.Repeat([]byte{byte('a' + i)}, bs)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	// The partition starts at 8 KiB on the target and ends inside block 9.
	const base, length = 2 * bs, 9*bs + bs/2
	disk := filepath.Join(t.TempDir(), "disk.img")
	f, err := os.OpenFile(disk, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fill := bytes.Repeat([]byte{0xee}, base+length+2*bs)
	if _, err := f.WriteAt(fill, 0); err != nil {
		t.Fatal(err)
	}
	w := partitionWrite{Src: &repo.PartitionImage{Number: 2, BlockSize: bs, Blocks: blocks}, Offset: base, Length: length, Restore: true}
	var st RestoreStats
	var reported uint64
	if err := writePartition(ctx, r, f, w, ids, nil, 512, &st, func(n uint64) { reported += n }); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(fill))
	if _, err := f.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	want := bytes.Clone(fill)
	for i := 0; i < blocks; i++ {
		start := base + i*bs
		end := min(start+bs, base+length)
		if i == 3 || start >= base+length {
			continue
		}
		copy(want[start:end], bytes.Repeat([]byte{byte('a' + i)}, end-start))
	}
	if !bytes.Equal(got, want) {
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("disk differs first at byte %d (block %d of the partition)", i, (i-base)/bs)
			}
		}
	}
	if wantBytes := uint64(8*bs + bs/2); st.BytesWritten != wantBytes || reported != wantBytes {
		t.Errorf("written %d, reported %d; want %d", st.BytesWritten, reported, wantBytes)
	}
}
