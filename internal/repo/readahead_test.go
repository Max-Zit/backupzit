package repo_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// shareLike reads like an SMB share or an SFTP server: it takes uploads one
// after another (no Concurrency method), each read of a pack takes a moment,
// and the n-th one can fail. It counts the reads running at once.
type shareLike struct {
	backend.Backend
	delay  time.Duration
	failAt int32 // 1-based number of the pack read that fails, 0: none
	reads  atomic.Int32
	active atomic.Int32
	most   atomic.Int32
}

func (s *shareLike) LoadRange(ctx context.Context, name string, off int64, n int) ([]byte, error) {
	if !strings.HasPrefix(name, "data/") {
		return s.Backend.LoadRange(ctx, name, off, n)
	}
	k := s.reads.Add(1)
	now := s.active.Add(1)
	defer s.active.Add(-1)
	for m := s.most.Load(); now > m && !s.most.CompareAndSwap(m, now); m = s.most.Load() {
	}
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if k == s.failAt {
		return nil, errors.New("connection reset by peer")
	}
	return s.Backend.LoadRange(ctx, name, off, n)
}

// blocksOnShare stores 28 data blocks (every fifth one null, as unused disk
// space is) and opens the repository again through a shareLike backend.
func blocksOnShare(t *testing.T, failAt int32) (*repo.Repository, *shareLike, []repo.ID, [][]byte) {
	t.Helper()
	ctx := context.Background()
	local, err := backend.OpenLocal(filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := repo.Init(ctx, local)
	if err != nil {
		t.Fatal(err)
	}
	ids, data := make([]repo.ID, 28), make([][]byte, 28)
	for i := range ids {
		if i%5 == 4 {
			continue
		}
		data[i] = bytes.Repeat([]byte(fmt.Sprintf("disk block %02d|", i)), 300)
		if ids[i], _, err = w.SaveBlob(ctx, repo.DataBlob, data[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	w.Close()
	sh := &shareLike{Backend: local, delay: 10 * time.Millisecond, failAt: failAt}
	r, err := repo.Open(ctx, sh)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, sh, ids, data
}

// From storage that takes one upload at a time, a restore still reads up to
// three blocks at once and gets them in disk order (#65).
func TestReadAheadFromShare(t *testing.T) {
	r, sh, ids, data := blocksOnShare(t, 0)
	if r.Concurrency() != 1 || r.ReadConcurrency() != 3 {
		t.Fatalf("concurrency %d, read concurrency %d; want 1 and 3", r.Concurrency(), r.ReadConcurrency())
	}
	ra := r.ReadAhead(context.Background(), ids, func(i int) bool { return !ids[i].IsNull() })
	defer ra.Close()
	for i, id := range ids {
		if id.IsNull() {
			continue
		}
		b, err := ra.Get(i)
		if err != nil || !bytes.Equal(b, data[i]) {
			t.Fatalf("block %d: %v (right data %v)", i, err, bytes.Equal(b, data[i]))
		}
	}
	if m := sh.most.Load(); m < 2 || m > 3 {
		t.Errorf("%d reads at once, want 2 or 3", m)
	}
}

// A failed read reaches the restore with its block; after Close no read is
// left running, also when the restore stops early.
func TestReadAheadFailedRead(t *testing.T) {
	r, sh, ids, _ := blocksOnShare(t, 6)
	ra := r.ReadAhead(context.Background(), ids, func(i int) bool { return !ids[i].IsNull() })
	var err error
	for i, id := range ids {
		if id.IsNull() {
			continue
		}
		if _, err = ra.Get(i); err != nil {
			break
		}
	}
	ra.Close()
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("want the read error, got %v", err)
	}
	if n := sh.active.Load(); n != 0 {
		t.Errorf("%d reads still running after Close", n)
	}

	r2, sh2, ids2, _ := blocksOnShare(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	ra2 := r2.ReadAhead(ctx, ids2, func(i int) bool { return !ids2[i].IsNull() })
	if _, err := ra2.Get(0); err != nil {
		t.Fatal(err)
	}
	cancel() // the restore was cancelled after the first block
	ra2.Close()
	if n := sh2.active.Load(); n != 0 {
		t.Errorf("%d reads still running after a cancel", n)
	}
}

// Close right after the first Get, before every reader may have started,
// returns (a reader that looked for the queue only after Close cleared it
// would wait for ever); one thread makes a late reader likely. Blocks that
// are skipped are read correctly after them.
func TestReadAheadCloseAndSkip(t *testing.T) {
	r, _, ids, data := blocksOnShare(t, 0)
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			ra := r.ReadAhead(context.Background(), ids, func(i int) bool { return !ids[i].IsNull() })
			_, _ = ra.Get(0)
			ra.Close()
			ra.Close() // a second Close does nothing
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("ReadAhead.Close hangs")
	}
	ra := r.ReadAhead(context.Background(), ids, func(i int) bool { return !ids[i].IsNull() })
	defer ra.Close()
	for i := 0; i < len(ids); i += 3 { // the restore asks for every third block
		if ids[i].IsNull() {
			continue
		}
		if b, err := ra.Get(i); err != nil || !bytes.Equal(b, data[i]) {
			t.Fatalf("block %d: %v", i, err)
		}
	}
}
