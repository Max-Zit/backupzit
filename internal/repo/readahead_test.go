package repo_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// slowShare is a local backend that behaves like an SMB share for reads:
// it uploads one pack at a time (no Concurrency), each ranged read of a
// pack takes a while, and one read can be made to fail.
type slowShare struct {
	backend.Backend
	delay   time.Duration
	failAt  int32 // fail the n-th ranged pack read (1-based), 0: never
	reads   atomic.Int32
	now     atomic.Int32
	maxSeen atomic.Int32
}

func (s *slowShare) LoadRange(ctx context.Context, name string, off int64, n int) ([]byte, error) {
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
		return nil, errors.New("connection reset")
	}
	return s.Backend.LoadRange(ctx, name, off, n)
}

// storeBlocks saves 30 distinct data blocks (every seventh a null block)
// and opens the repository again on a slow share.
func storeBlocks(t *testing.T, failAt int32) (*repo.Repository, *slowShare, []repo.ID, [][]byte) {
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
	ids := make([]repo.ID, 30)
	data := make([][]byte, 30)
	for i := range ids {
		if i%7 == 6 {
			continue
		}
		data[i] = bytes.Repeat([]byte(fmt.Sprintf("block %02d ", i)), 512)
		if ids[i], _, err = w.SaveBlob(ctx, repo.DataBlob, data[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	w.Close()
	slow := &slowShare{Backend: local, delay: 15 * time.Millisecond, failAt: failAt}
	r, err := repo.Open(ctx, slow)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, slow, ids, data
}

// A backend that uploads one pack at a time (SMB, SFTP, local) still reads
// a few blocks at once for a restore, and hands them out in order.
func TestReadAheadOnShareReadsSeveralAtOnce(t *testing.T) {
	r, slow, ids, data := storeBlocks(t, 0)
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
		if err != nil {
			t.Fatalf("block %d: %v", i, err)
		}
		if !bytes.Equal(b, data[i]) {
			t.Fatalf("block %d: wrong data", i)
		}
	}
	if got := slow.maxSeen.Load(); got < 2 || got > 3 {
		t.Errorf("%d reads at once, want 2–3", got)
	}
}

// A failed read is returned for its block, and Close leaves no read
// running.
func TestReadAheadError(t *testing.T) {
	r, slow, ids, _ := storeBlocks(t, 5)
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
	if n := slow.now.Load(); n != 0 {
		t.Errorf("%d reads still running after Close", n)
	}
}
