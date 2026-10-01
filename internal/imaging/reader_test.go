package imaging

import (
	"bytes"
	"context"
	"io"
	"math/rand"
	"path/filepath"
	"testing"

	"github.com/backupzit/backupzit/internal/backend"
	"github.com/backupzit/backupzit/internal/repo"
)

// TestPartitionReader checks byte-exact reads across block boundaries,
// unused blocks reading as zeros, and a short final block.
func TestPartitionReader(t *testing.T) {
	ctx := context.Background()
	be, _ := backend.OpenLocal(filepath.Join(t.TempDir(), "repo"))
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	const bs = 4096
	length := int64(10*bs + 1000) // last block is short
	want := make([]byte, length)
	rand.New(rand.NewSource(3)).Read(want)
	blocks := (length + bs - 1) / bs
	ids := make([]repo.ID, blocks)
	for i := int64(0); i < blocks; i++ {
		if i == 3 || i == 7 { // unused: must read as zeros
			clear(want[i*bs : (i+1)*bs])
			continue
		}
		end := min((i+1)*bs, length)
		ids[i], _, err = r.SaveBlob(ctx, repo.DataBlob, want[i*bs:end])
		if err != nil {
			t.Fatal(err)
		}
	}
	maps, err := r.SaveBlockMap(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	r.Flush(ctx)
	p := &repo.PartitionImage{Number: 1, Length: uint64(length), Included: true, BlockSize: bs, Blocks: uint64(blocks), Maps: maps}
	pr, err := NewPartitionReader(ctx, r, p)
	if err != nil {
		t.Fatal(err)
	}
	pr.max = 2 // force cache eviction

	rng := rand.New(rand.NewSource(9))
	for i := 0; i < 500; i++ {
		off := rng.Int63n(length)
		n := rng.Intn(3*bs) + 1
		buf := make([]byte, n)
		got, err := pr.ReadAt(buf, off)
		wantN := int(min(int64(n), length-off))
		if got != wantN || (err != nil && err != io.EOF) || (wantN == n && err != nil) {
			t.Fatalf("ReadAt(%d, %d): n=%d err=%v, want n=%d", n, off, got, err, wantN)
		}
		if !bytes.Equal(buf[:got], want[off:off+int64(got)]) {
			t.Fatalf("ReadAt(%d, %d): data differs", n, off)
		}
	}
	if _, err := pr.ReadAt(make([]byte, 1), length); err != io.EOF {
		t.Errorf("read at end: %v", err)
	}
}

func TestCleanPath(t *testing.T) {
	for in, want := range map[string]string{
		"":                     `\`,
		`\`:                    `\`,
		`Users\ana`:            `\Users\ana`,
		`/Users//ana/`:         `\Users\ana`,
		`\Users\..\..\Windows`: `\Windows`,
		`\a\.\b`:               `\a\b`,
	} {
		if got := CleanPath(in); got != want {
			t.Errorf("CleanPath(%q) = %q, want %q", in, got, want)
		}
	}
}
