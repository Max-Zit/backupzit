package repo

import (
	"context"
	"sync"
)

// ReadAhead loads the data blocks a restore writes next, several at once
// (ReadConcurrency), and hands them out in disk order, so the storage and the
// target disk work at the same time instead of waiting for each other. With
// a concurrency of 1 it reads each block when asked.
type ReadAhead struct {
	ctx    context.Context
	cancel context.CancelFunc
	r      *Repository
	ids    []ID
	want   func(i int) bool // blocks that are read (not null, not skipped)
	n      int              // reads at once
	window int              // blocks started ahead of the writer

	next  int // first block not started yet
	futs  map[int]*blockFuture
	queue chan *blockFuture // started blocks in disk order, taken by n workers
	wg    sync.WaitGroup
}

type blockFuture struct {
	i    int
	done chan struct{}
	b    []byte
	err  error
}

// ReadAhead prepares reading the data blobs ids in order; want tells which of
// them will be asked for (no other is read ahead). It holds at most about
// twice ReadConcurrency blocks in memory. Close it when done.
func (r *Repository) ReadAhead(ctx context.Context, ids []ID, want func(i int) bool) *ReadAhead {
	n := r.ReadConcurrency()
	ctx, cancel := context.WithCancel(ctx)
	return &ReadAhead{ctx: ctx, cancel: cancel, r: r, ids: ids, want: want, n: n, window: 2 * n,
		futs: map[int]*blockFuture{}}
}

// Get returns block i. Blocks must be asked for in increasing order.
func (a *ReadAhead) Get(i int) ([]byte, error) {
	if a.n <= 1 {
		return a.r.LoadBlob(a.ctx, DataBlob, a.ids[i])
	}
	if a.next <= i {
		a.next = i
	}
	for ; a.next < len(a.ids) && a.next <= i+a.window; a.next++ {
		if a.want(a.next) {
			a.start(a.next)
		}
	}
	f := a.futs[i]
	if f == nil { // not wanted by the filter, or asked out of order
		return a.r.LoadBlob(a.ctx, DataBlob, a.ids[i])
	}
	delete(a.futs, i)
	select {
	case <-f.done:
	case <-a.ctx.Done():
		return nil, a.ctx.Err()
	}
	return f.b, f.err
}

// start queues block i. The workers take blocks in the order they were
// queued (disk order), so the block the writer needs next is never held
// up behind blocks it needs later.
func (a *ReadAhead) start(i int) {
	if a.queue == nil {
		// Get never has more than window+1 blocks outstanding, so sending
		// to the queue never blocks.
		a.queue = make(chan *blockFuture, a.window+2)
		a.wg.Add(a.n)
		for range a.n {
			go a.work()
		}
	}
	f := &blockFuture{i: i, done: make(chan struct{})}
	a.futs[i] = f
	a.queue <- f
}

func (a *ReadAhead) work() {
	defer a.wg.Done()
	for f := range a.queue {
		if err := a.ctx.Err(); err != nil {
			f.err = err
		} else {
			f.b, f.err = a.r.LoadBlob(a.ctx, DataBlob, a.ids[f.i])
		}
		close(f.done)
	}
}

// Close stops reads still in flight and waits for them, so none outlives
// the repository.
func (a *ReadAhead) Close() {
	a.cancel()
	if a.queue != nil {
		close(a.queue)
		a.queue = nil
	}
	a.wg.Wait()
}
