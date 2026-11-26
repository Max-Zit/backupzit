package repo

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
)

// heldShare is a local backend like a share (no Concurrency): every pack
// upload reports that it started and waits until the test releases it
// (hold) or, for the packs listed in stuck, until stuck is closed.
type heldShare struct {
	backend.Backend
	started chan int // the number of the pack (1-based)
	release chan struct{}
	stuck   map[int32]chan struct{}
	fail    map[int32]bool

	packs, now, maxSeen, done atomic.Int32
}

func newHeldShare(t *testing.T) *heldShare {
	local, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &heldShare{Backend: local, started: make(chan int, 64), release: make(chan struct{})}
}

func (h *heldShare) Save(ctx context.Context, name string, data []byte) error {
	if !strings.HasPrefix(name, "data/") {
		return h.Backend.Save(ctx, name, data)
	}
	n := h.packs.Add(1)
	cur := h.now.Add(1)
	defer h.now.Add(-1)
	for m := h.maxSeen.Load(); cur > m && !h.maxSeen.CompareAndSwap(m, cur); m = h.maxSeen.Load() {
	}
	h.started <- int(n)
	if c, ok := h.stuck[n]; ok {
		<-c
	} else {
		<-h.release
	}
	defer h.done.Add(1)
	if h.fail[n] {
		return errors.New("pack upload failed")
	}
	return h.Backend.Save(ctx, name, data)
}

func waitStarted(t *testing.T, h *heldShare) int {
	t.Helper()
	select {
	case n := <-h.started:
		return n
	case <-time.After(10 * time.Second):
		t.Fatal("no pack upload started")
	}
	return 0
}

// On a share the writer fills the next pack while one uploads, with two
// uploads at most at once (#71).
func TestShareUploadsInBackground(t *testing.T) {
	h := newHeldShare(t)
	r := newTestRepo(t, h, 64<<10)
	defer r.Close()
	if r.Concurrency() != 1 || r.uploads != 2 {
		t.Fatalf("concurrency %d, uploads %d", r.Concurrency(), r.uploads)
	}
	ctx := context.Background()
	blobs := randomBlobs(14, 16<<10) // a pack is full after 4 blobs
	ids := make([]ID, len(blobs))
	for i, b := range blobs[:10] { // packs 1 and 2 go to uploads, 3 fills
		var err error
		if ids[i], _, err = r.SaveBlob(ctx, DataBlob, b); err != nil {
			t.Fatal(err)
		}
	}
	waitStarted(t, h)
	waitStarted(t, h)
	if n := r.Stats().PacksUploaded; n != 0 {
		t.Fatalf("%d packs indexed while their uploads are held", n)
	}
	// Both slots are taken: the next full pack waits for one.
	saved := make(chan error, 1)
	go func() {
		for i, b := range blobs[10:] {
			var err error
			if ids[10+i], _, err = r.SaveBlob(ctx, DataBlob, b); err != nil {
				saved <- err
				return
			}
		}
		saved <- r.Flush(ctx)
	}()
	select {
	case err := <-saved:
		t.Fatalf("saving went on with both uploads held: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(h.release)
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
	if got := h.maxSeen.Load(); got != 2 {
		t.Errorf("%d uploads at once, want 2", got)
	}
	for i, b := range blobs {
		if got, err := r.LoadBlob(ctx, DataBlob, ids[i]); err != nil || !bytes.Equal(got, b) {
			t.Fatalf("blob %d: %v", i, err)
		}
	}
}

// A later pack may finish first: packs are indexed in the order they were
// filled, and after a failed upload no later pack is indexed and Flush
// reports the failure.
func TestUploadsIndexedInOrder(t *testing.T) {
	for _, fail := range []bool{false, true} {
		h := newHeldShare(t)
		first := make(chan struct{})
		h.stuck = map[int32]chan struct{}{1: first}
		h.fail = map[int32]bool{1: fail}
		close(h.release)
		r := newTestRepo(t, h, 64<<10)
		ctx := context.Background()
		var ids []ID
		for _, b := range randomBlobs(9, 16<<10) { // two full packs, one partial
			id, _, err := r.SaveBlob(ctx, DataBlob, b)
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		for h.done.Load() < 1 { // pack 2 is uploaded while pack 1 is held
			time.Sleep(time.Millisecond)
		}
		if r.Index().Has(BlobHandle{Type: DataBlob, ID: ids[4]}) {
			t.Error("pack 2 was indexed before pack 1")
		}
		close(first)
		err := r.Flush(ctx)
		if fail {
			if err == nil || !strings.Contains(err.Error(), "pack upload failed") {
				t.Fatalf("Flush after a failed first upload: %v", err)
			}
			if r.Index().Has(BlobHandle{Type: DataBlob, ID: ids[4]}) {
				t.Error("a pack filled after the failed one was indexed")
			}
		} else if err != nil || len(r.Index().packs) != 3 || len(r.unindexed) != 0 {
			t.Fatalf("Flush: %v, %d packs indexed, %d not in an index file", err, len(r.Index().packs), len(r.unindexed))
		}
		r.Close()
	}
}

// A slot is free when its upload ends, not when its pack is indexed: one
// slow upload does not stall the others (until maxWaitingPacks).
func TestSlowUploadKeepsOthersGoing(t *testing.T) {
	h := newHeldShare(t)
	first := make(chan struct{})
	h.stuck = map[int32]chan struct{}{1: first}
	close(h.release)
	r := newTestRepo(t, h, 64<<10)
	defer r.Close()
	ctx := context.Background()
	for _, b := range randomBlobs(5*4, 16<<10) { // five full packs
		if _, _, err := r.SaveBlob(ctx, DataBlob, b); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for h.done.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := h.done.Load(); n < 3 {
		t.Errorf("%d uploads ended while the first is held, want 3 or more", n)
	}
	close(first)
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}

// The caller's context may end once its blobs are saved, while the pack
// holding them is still on its way (Microsoft 365 backups end a context per
// folder): the upload goes on, also behind the bandwidth limit, which waits
// on the context (#71 review).
func TestUploadOutlivesCallerContext(t *testing.T) {
	local, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := newTestRepo(t, backend.Throttle(local), 64<<10)
	defer r.Close()
	job := backend.WithLimiter(context.Background(), backend.NewLimiter(func() int64 { return 256 << 10 }))
	folder, cancel := context.WithCancel(job)
	blobs := randomBlobs(10, 16<<10)
	for _, b := range blobs {
		if _, _, err := r.SaveBlob(folder, DataBlob, b); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	if err := r.Flush(job); err != nil {
		t.Fatalf("Flush after the folder's context ended: %v", err)
	}
	for i, b := range blobs {
		if got, err := r.LoadBlob(job, DataBlob, Hash(b)); err != nil || !bytes.Equal(got, b) {
			t.Fatalf("blob %d: %v", i, err)
		}
	}
}

// A run that fails before Flush releases its lock only after its uploads
// ended: a prune after the lock would take a pack landing later for a
// leftover (#71 review). Close cancels uploads that are still running.
func TestUnlockWaitsForUploads(t *testing.T) {
	h := newHeldShare(t)
	r := newTestRepo(t, h, 64<<10)
	ctx := context.Background()
	lock, err := r.Lock(ctx, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range randomBlobs(5, 16<<10) {
		if _, _, err := r.SaveBlob(ctx, DataBlob, b); err != nil {
			t.Fatal(err)
		}
	}
	waitStarted(t, h)
	unlocked := make(chan error, 1)
	go func() { unlocked <- lock.Unlock() }()
	select {
	case err := <-unlocked:
		t.Fatalf("Unlock returned while a pack was uploading: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if names, _ := h.List(ctx, "locks"); len(names) != 1 {
		t.Errorf("locks while the upload runs: %v", names)
	}
	close(h.release)
	if err := <-unlocked; err != nil {
		t.Fatal(err)
	}
	if names, _ := h.List(ctx, "locks"); len(names) != 0 {
		t.Errorf("locks after Unlock: %v", names)
	}
	if n := h.done.Load(); n != 1 {
		t.Errorf("%d uploads ended before Unlock returned, want 1", n)
	}
	r.Close()

	// Close cancels an upload that honours its context.
	local2, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	be := &ctxStore{Backend: local2, started: make(chan struct{}, 1)}
	r = newTestRepo(t, be, 64<<10)
	for _, b := range randomBlobs(5, 16<<10) {
		if _, _, err := r.SaveBlob(ctx, DataBlob, b); err != nil {
			t.Fatal(err)
		}
	}
	<-be.started
	closed := make(chan struct{})
	go func() { r.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close waits for an upload instead of cancelling it")
	}
}

// ctxStore's pack uploads wait until their context ends.
type ctxStore struct {
	backend.Backend
	started chan struct{}
}

func (c *ctxStore) Save(ctx context.Context, name string, data []byte) error {
	if !strings.HasPrefix(name, "data/") {
		return c.Backend.Save(ctx, name, data)
	}
	c.started <- struct{}{}
	<-ctx.Done()
	return ctx.Err()
}
