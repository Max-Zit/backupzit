package repo

import (
	"context"
	"errors"
	"runtime"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// DefaultWriteWorkers is the number of blobs a BlobWriter compresses at
// once when it is created with workers <= 0: the CPUs Go may use, at most 4,
// so that a backup leaves room for the machine it runs on.
func DefaultWriteWorkers() int { return min(runtime.GOMAXPROCS(0), 4) }

// BlobWriter saves blobs like Repository.SaveBlob but compresses (and
// encrypts) them on several goroutines.
//
// The result is the same as calling SaveBlob for each blob in the order
// of the Save calls: blobs reach the pack in that order, deduplication
// decisions and statistics are the same, and for an unencrypted repository
// packs and indexes are byte-identical. In an encrypted repository each
// sealed blob has a random nonce, as with SaveBlob, so only the decrypted
// content is identical.
//
// Save returns the blob ID at once (it is the hash of the plain data); the
// blob is in the repository's current pack only after Close. Close must be
// called before Repository.Flush and before saving anything that refers
// to the blobs. A pack upload error stops the writer: Save and Close then
// return it. With one worker Save is SaveBlob, the sequential reference.
type BlobWriter struct {
	r       *Repository
	ctx     context.Context // for pack uploads
	cancel  context.CancelFunc
	workers int

	enc        *zstd.Encoder
	noCompress bool

	submitMu sync.Mutex // keeps queue order equal to dedup order
	closed   bool       // guarded by submitMu
	order    chan *blobJob
	jobs     chan *blobJob
	wg       sync.WaitGroup
	done     chan struct{} // closed when the committer ends

	errMu sync.Mutex
	err   error

	closeOnce sync.Once
	closeErr  error
}

type blobJob struct {
	h      BlobHandle
	data   []byte
	stored []byte
	rawLen int
	ready  chan struct{}
}

// NewBlobWriter returns a writer with the given number of compression
// workers (<= 0: DefaultWriteWorkers). At most twice that many blobs are
// held in memory, compressed or not, at any time. ctx bounds the writer's
// pack uploads; cancelling it stops the writer.
func NewBlobWriter(ctx context.Context, r *Repository, workers int) (*BlobWriter, error) {
	if workers <= 0 {
		workers = DefaultWriteWorkers()
	}
	w := &BlobWriter{r: r, workers: workers}
	if workers == 1 {
		return w, nil
	}
	r.mu.Lock()
	lvl, off := r.encLevel, r.noCompress
	r.mu.Unlock()
	if lvl == 0 {
		lvl = zstd.SpeedDefault
	}
	w.noCompress = off
	if !off {
		// EncodeAll is safe for concurrent use, but runs at most
		// "concurrency" calls at once; the output does not depend on it.
		enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(lvl), zstd.WithEncoderConcurrency(workers))
		if err != nil {
			return nil, err
		}
		w.enc = enc
	}
	window := 2 * workers
	w.ctx, w.cancel = context.WithCancel(ctx)
	// Save puts a job in order before jobs, and the committer holds at most
	// one job out of order, so jobs never holds more than window+1 jobs and
	// sending to it never blocks.
	w.order = make(chan *blobJob, window-1)
	w.jobs = make(chan *blobJob, window+1)
	w.done = make(chan struct{})
	w.wg.Add(workers)
	for range workers {
		go w.work()
	}
	go w.commit()
	return w, nil
}

// Workers returns the number of compression workers.
func (w *BlobWriter) Workers() int { return w.workers }

func (w *BlobWriter) fail(err error) {
	w.errMu.Lock()
	if w.err == nil {
		w.err = err
	}
	w.errMu.Unlock()
	w.cancel()
}

func (w *BlobWriter) failed() error {
	w.errMu.Lock()
	defer w.errMu.Unlock()
	return w.err
}

// Save stores data unless an identical blob exists, like SaveBlob. data
// may be reused by the caller as soon as Save returns.
func (w *BlobWriter) Save(ctx context.Context, t BlobType, data []byte) (ID, bool, error) {
	if w.workers == 1 {
		return w.r.SaveBlob(ctx, t, data)
	}
	if err := w.failed(); err != nil {
		return ID{}, false, err
	}
	if err := ctx.Err(); err != nil {
		return ID{}, false, err
	}
	id := Hash(data)
	h := BlobHandle{Type: t, ID: id}
	r := w.r

	w.submitMu.Lock()
	defer w.submitMu.Unlock()
	if w.closed {
		return id, false, errors.New("blob writer closed")
	}
	r.mu.Lock()
	if _, ok := r.pending[h]; ok || r.idx.Has(h) {
		r.stats.DupBlobs++
		r.mu.Unlock()
		return id, false, nil
	}
	r.pending[h] = struct{}{}
	r.stats.NewBlobs++
	r.stats.RawBytes += uint64(len(data))
	r.mu.Unlock()

	j := &blobJob{h: h, data: append([]byte(nil), data...), ready: make(chan struct{})}
	select {
	case w.order <- j:
		w.jobs <- j // never blocks, see NewBlobWriter
		return id, true, nil
	case <-ctx.Done():
		w.unreserve(j)
		return id, false, ctx.Err()
	case <-w.ctx.Done():
		w.unreserve(j)
		if err := w.failed(); err != nil {
			return id, false, err
		}
		return id, false, w.ctx.Err()
	}
}

// unreserve undoes Save's bookkeeping for a blob that never reached a pack,
// so that a later save of the same data stores it.
func (w *BlobWriter) unreserve(j *blobJob) {
	r := w.r
	r.mu.Lock()
	delete(r.pending, j.h)
	r.stats.NewBlobs--
	r.stats.RawBytes -= uint64(len(j.data))
	r.mu.Unlock()
}

// work compresses and seals blobs, as SaveBlob does under the lock.
func (w *BlobWriter) work() {
	defer w.wg.Done()
	for j := range w.jobs {
		if w.ctx.Err() == nil {
			var stored []byte
			if !w.noCompress {
				stored = w.enc.EncodeAll(j.data, nil)
			}
			rawLen := len(j.data)
			if w.noCompress || len(stored) >= len(j.data) {
				stored, rawLen = j.data, 0
			}
			j.stored, j.rawLen = w.r.seal(stored), rawLen
		}
		close(j.ready)
	}
}

// commit adds blobs to the pack in the order of the Save calls.
func (w *BlobWriter) commit() {
	defer close(w.done)
	r := w.r
	for j := range w.order {
		if w.ctx.Err() == nil {
			select {
			case <-j.ready:
			case <-w.ctx.Done():
			}
		}
		if err := w.ctx.Err(); err != nil {
			w.fail(err)
			w.unreserve(j)
			continue
		}
		r.mu.Lock()
		r.pack.add(j.h.Type, j.h.ID, j.stored, j.rawLen)
		r.stats.StoredBytes += uint64(len(j.stored))
		var err error
		if r.pack.size() >= r.cfg.PackSize {
			err = r.flushPackLocked(w.ctx)
		}
		r.mu.Unlock()
		j.data, j.stored = nil, nil
		if err != nil {
			w.fail(err)
		}
	}
}

// Close waits until every saved blob is in the repository's current pack
// (or an uploaded pack) and stops the writer's goroutines. It returns the
// first error. It does not flush the repository.
func (w *BlobWriter) Close() error {
	if w.workers == 1 {
		return nil
	}
	w.closeOnce.Do(func() {
		w.submitMu.Lock()
		close(w.order)
		close(w.jobs)
		w.closed = true
		w.submitMu.Unlock()
		<-w.done
		w.wg.Wait()
		if w.enc != nil {
			_ = w.enc.Close()
		}
		w.closeErr = w.failed()
		w.cancel()
	})
	return w.closeErr
}
