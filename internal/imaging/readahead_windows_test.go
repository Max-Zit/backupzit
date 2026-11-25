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

// writePartition reads blocks ahead (#65) and still writes each one where
// it belongs: null blocks leave the disk as it was, the block at the end of
// a partition that became smaller is cut, and blocks past that end are
// neither read nor written.
func TestWritePartitionReadAheadKeepsPlaces(t *testing.T) {
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
	const bs, blocks = 4096, 14
	ids := make([]repo.ID, blocks)
	for i := range ids {
		if i == 2 || i == 7 {
			continue // null blocks
		}
		if ids[i], _, err = r.SaveBlob(ctx, repo.DataBlob, bytes.Repeat([]byte{byte('A' + i)}, bs)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	// The partition starts at 12 KiB and now ends a quarter into block 10.
	const base, length = 3 * bs, 10*bs + bs/4
	img := filepath.Join(t.TempDir(), "disk.img")
	f, err := os.OpenFile(img, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	before := bytes.Repeat([]byte{0x5a}, base+length+3*bs)
	if _, err := f.WriteAt(before, 0); err != nil {
		t.Fatal(err)
	}
	w := partitionWrite{Src: &repo.PartitionImage{Number: 3, BlockSize: bs, Blocks: blocks}, Offset: base, Length: length, Restore: true}
	var st RestoreStats
	var told uint64
	if err := writePartition(ctx, r, f, w, ids, nil, 512, &st, func(n uint64) { told += n }); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(before))
	if _, err := f.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	want := bytes.Clone(before)
	for i := 0; i < blocks; i++ {
		start := base + i*bs
		if ids[i].IsNull() || start >= base+length {
			continue
		}
		end := min(start+bs, base+length)
		copy(want[start:end], bytes.Repeat([]byte{byte('A' + i)}, end-start))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("disk differs first at byte %d (partition block %d)", i, (i-base)/bs)
		}
	}
	// Blocks 0-9 without the two null ones, plus a quarter of block 10.
	if wantN := uint64(8*bs + bs/4); st.BytesWritten != wantN || told != wantN {
		t.Errorf("written %d, reported %d; want %d", st.BytesWritten, told, wantN)
	}
}
