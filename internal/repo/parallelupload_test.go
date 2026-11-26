package repo

import (
	"bytes"
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
)

// slowStore is a local backend that pretends to be an object store: pack
// uploads take a while, several may run at once, and one can be made to fail.
type slowStore struct {
	backend.Backend
	delay    time.Duration
	failPack int32 // fail the n-th pack upload (1-based), 0: never

	packs   atomic.Int32
	now     atomic.Int32
	maxSeen atomic.Int32
}

func (s *slowStore) Concurrency() int { return 4 }

func (s *slowStore) Save(ctx context.Context, name string, data []byte) error {
	if !strings.HasPrefix(name, "data/") {
		return s.Backend.Save(ctx, name, data)
	}
	n := s.packs.Add(1)
	cur := s.now.Add(1)
	defer s.now.Add(-1)
	for {
		m := s.maxSeen.Load()
		if cur <= m || s.maxSeen.CompareAndSwap(m, cur) {
			break
		}
	}
	// Like a real object store client, a cancelled context stops the upload
	// (the BlobWriter cancels its context on Close: uploads in flight must
	// have finished by then).
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	if n == s.failPack {
		return errors.New("503 SlowDown")
	}
	return s.Backend.Save(ctx, name, data)
}

func randomBlobs(n, size int) [][]byte {
	rng := rand.New(rand.NewPCG(7, 9))
	out := make([][]byte, n)
	for i := range out {
		b := make([]byte, size)
		for j := range b {
			b[j] = byte(rng.Uint32())
		}
		out[i] = b
	}
	return out
}

// On an object store packs are uploaded several at a time; after Flush
// every blob is indexed and readable, also with the parallel BlobWriter,
// and the index names exactly the packs that were uploaded.
func TestParallelPackUploads(t *testing.T) {
	for _, workers := range []int{0, 4} {
		local, err := backend.OpenLocal(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		be := &slowStore{Backend: local, delay: 30 * time.Millisecond}
		r := newTestRepo(t, be, 64<<10)
		if r.maxUploads != 4 {
			t.Fatalf("maxUploads %d", r.maxUploads)
		}
		blobs := randomBlobs(80, 16<<10) // ~20 packs of 64 KiB
		res := saveAll(t, r, workers, blobs)
		if got := be.maxSeen.Load(); got < 2 || got > 4 {
			t.Errorf("workers %d: %d uploads at once, want 2–4", workers, got)
		}
		for i, b := range blobs {
			got, err := r.LoadBlob(context.Background(), DataBlob, res[i].id)
			if err != nil || !bytes.Equal(got, b) {
				t.Fatalf("workers %d: blob %d: %v", workers, i, err)
			}
		}
		names, err := local.List(context.Background(), "data")
		if err != nil {
			t.Fatal(err)
		}
		if len(names) != int(be.packs.Load()) || len(names) != r.Stats().PacksUploaded {
			t.Errorf("workers %d: %d packs stored, %d uploaded, %d counted", workers, len(names), be.packs.Load(), r.Stats().PacksUploaded)
		}
		// A reopened repository sees everything from the index files.
		r2, err := Open(context.Background(), be)
		if err != nil {
			t.Fatal(err)
		}
		for i := range blobs {
			if !r2.Index().Has(BlobHandle{Type: DataBlob, ID: res[i].id}) {
				t.Fatalf("workers %d: blob %d missing after reopen", workers, i)
			}
		}
		r.Close()
		r2.Close()
	}
}

// A failed background upload fails the writes after it and Flush, so no
// snapshot can be saved that names blobs which never reached the storage;
// the failed pack never appears in an index.
func TestParallelPackUploadFailure(t *testing.T) {
	local, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	be := &slowStore{Backend: local, delay: 10 * time.Millisecond, failPack: 3}
	r := newTestRepo(t, be, 64<<10)
	ctx := context.Background()
	var saveErr error
	for _, b := range randomBlobs(80, 16<<10) {
		if _, _, err := r.SaveBlob(ctx, DataBlob, b); err != nil {
			saveErr = err
			break
		}
	}
	err = r.Flush(ctx)
	if err == nil || !strings.Contains(err.Error(), "SlowDown") {
		t.Fatalf("Flush after a failed upload: %v", err)
	}
	if saveErr != nil && !strings.Contains(saveErr.Error(), "SlowDown") {
		t.Errorf("save error: %v", saveErr)
	}
	r.Close()
	r2, err := Open(ctx, local)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	names, _ := local.List(ctx, "data")
	stored := map[string]bool{}
	for _, n := range names {
		stored[n] = true
	}
	for id := range r2.Index().packs {
		if !stored[packName(id)] {
			t.Errorf("index names pack %s that was never stored", id.Short())
		}
	}
}

// Close waits for uploads still in flight (a run that failed before Flush).
func TestParallelUploadsCloseWaits(t *testing.T) {
	local, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	be := &slowStore{Backend: local, delay: 50 * time.Millisecond}
	r := newTestRepo(t, be, 64<<10)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, b := range randomBlobs(20, 16<<10) {
			if _, _, err := r.SaveBlob(context.Background(), DataBlob, b); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	wg.Wait()
	r.Close()
	if n := be.now.Load(); n != 0 {
		t.Errorf("%d uploads still running after Close", n)
	}
}

// On an object store the index files are read several at a time; the
// progress callback sees every file, and every blob is found afterwards.
func TestParallelIndexLoad(t *testing.T) {
	local, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	w := newTestRepo(t, local, 64<<10)
	blobs := randomBlobs(60, 8<<10)
	var ids []ID
	for i, b := range blobs {
		id, _, err := w.SaveBlob(ctx, DataBlob, b)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		if i%2 == 1 { // many small index files, like many runs
			if err := w.Flush(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	w.Close()
	files, _ := local.List(ctx, "index")
	be := &slowStore{Backend: local, delay: 0}
	var calls, last atomic.Int32
	r, err := Open(ctx, be, IndexProgress(func(done, total int) {
		calls.Add(1)
		if done == total {
			last.Store(int32(total))
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if int(calls.Load()) != len(files) || int(last.Load()) != len(files) {
		t.Errorf("progress called %d times, last total %d, want %d files", calls.Load(), last.Load(), len(files))
	}
	for i, id := range ids {
		if !r.Index().Has(BlobHandle{Type: DataBlob, ID: id}) {
			t.Fatalf("blob %d missing after a parallel index load", i)
		}
	}
}

// gatedShare uploads one pack at a time like a share (no Concurrency) and
// holds every pack upload until the test lets it go.
type gatedShare struct {
	backend.Backend
	release chan struct{}
	started chan struct{}
	now     atomic.Int32
	maxSeen atomic.Int32
}

func (g *gatedShare) Save(ctx context.Context, name string, data []byte) error {
	if !strings.HasPrefix(name, "data/") {
		return g.Backend.Save(ctx, name, data)
	}
	cur := g.now.Add(1)
	defer g.now.Add(-1)
	for {
		m := g.maxSeen.Load()
		if cur <= m || g.maxSeen.CompareAndSwap(m, cur) {
			break
		}
	}
	g.started <- struct{}{}
	<-g.release
	return g.Backend.Save(ctx, name, data)
}

// On a share a pack uploads in the background: the writer fills the next
// pack meanwhile instead of waiting for the network, and never more than
// two uploads run at once.
func TestShareUploadsInBackground(t *testing.T) {
	local, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	be := &gatedShare{Backend: local, release: make(chan struct{}), started: make(chan struct{}, 16)}
	r := newTestRepo(t, be, 64<<10)
	if r.maxUploads != 1 || r.uploads != 2 {
		t.Fatalf("maxUploads %d, uploads %d", r.maxUploads, r.uploads)
	}
	ctx := context.Background()
	blobs := randomBlobs(10, 16<<10) // pack 1 is full after 4 blobs
	var ids []ID
	for _, b := range blobs[:7] {
		id, _, err := r.SaveBlob(ctx, DataBlob, b)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	// Pack 1 is uploading (held), and blobs 5–7 went into pack 2 meanwhile.
	select {
	case <-be.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first pack upload did not start")
	}
	if n := r.Stats().PacksUploaded; n != 0 {
		t.Fatalf("%d packs uploaded while the first is held", n)
	}
	done := make(chan error, 1)
	go func() {
		for _, b := range blobs[7:] {
			id, _, err := r.SaveBlob(ctx, DataBlob, b)
			if err != nil {
				done <- err
				return
			}
			ids = append(ids, id)
		}
		done <- r.Flush(ctx)
	}()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range be.started {
		}
	}()
	close(be.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	close(be.started) // no upload is left to start
	<-drained
	if got := be.maxSeen.Load(); got < 1 || got > 2 {
		t.Errorf("%d uploads at once on a share, want 1–2", got)
	}
	for i, b := range blobs {
		got, err := r.LoadBlob(ctx, DataBlob, ids[i])
		if err != nil || !bytes.Equal(got, b) {
			t.Fatalf("blob %d: %v", i, err)
		}
	}
	r.Close()
}

// orderedShare makes the first pack upload slow and, with fail, fail; the
// second finishes first.
type orderedShare struct {
	backend.Backend
	fail  bool
	packs atomic.Int32
}

func (o *orderedShare) Save(ctx context.Context, name string, data []byte) error {
	if strings.HasPrefix(name, "data/") && o.packs.Add(1) == 1 {
		time.Sleep(100 * time.Millisecond)
		if o.fail {
			return errors.New("first upload failed")
		}
	}
	return o.Backend.Save(ctx, name, data)
}

// With two uploads at once a later pack can finish first. Packs are
// indexed in the order they were filled; when the earlier one fails, the
// later one is not indexed, Flush reports the failure and nothing hangs.
func TestShareUploadsIndexedInOrder(t *testing.T) {
	for _, fail := range []bool{false, true} {
		local, err := backend.OpenLocal(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		be := &orderedShare{Backend: local, fail: fail}
		r := newTestRepo(t, be, 64<<10)
		ctx := context.Background()
		var ids []ID
		for _, b := range randomBlobs(9, 16<<10) { // two full packs, one partial
			id, _, err := r.SaveBlob(ctx, DataBlob, b)
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		done := make(chan error, 1)
		go func() { done <- r.Flush(ctx) }()
		select {
		case err = <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("Flush hangs")
		}
		if fail {
			if err == nil || !strings.Contains(err.Error(), "first upload failed") {
				t.Fatalf("Flush after a failed first upload: %v", err)
			}
			if r.Index().Has(BlobHandle{Type: DataBlob, ID: ids[4]}) {
				t.Error("a pack filled after the failed one was indexed")
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			if len(r.Index().packs) != 3 {
				t.Errorf("%d packs indexed, want 3", len(r.Index().packs))
			}
			if n := len(r.unindexed); n != 0 {
				t.Errorf("%d packs left unindexed after Flush", n)
			}
		}
		r.Close()
	}
}
