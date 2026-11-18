package diskimg

import (
	"context"
	"sync"

	"github.com/max-zit/backupzit/internal/repo"
)

// readAhead loads the blocks a restore will write next, several at once on
// object stores (where one ranged read mostly waits on the network), and
// hands them out in disk order. With a concurrency of 1 it reads each block
// when asked, as before.
type readAhead struct {
	ctx    context.Context
	cancel context.CancelFunc
	r      *repo.Repository
	ids    []repo.ID
	want   func(i int) bool // blocks that are read (not null, not skipped)
	n      int              // reads at once
	window int              // blocks started ahead of the writer

	next int // first block not started yet
	futs map[int]*blockFuture
	sem  chan struct{}
	wg   sync.WaitGroup
}

type blockFuture struct {
	done chan struct{}
	b    []byte
	err  error
}

func newReadAhead(ctx context.Context, r *repo.Repository, ids []repo.ID, want func(int) bool) *readAhead {
	n := r.Concurrency()
	ctx, cancel := context.WithCancel(ctx)
	return &readAhead{ctx: ctx, cancel: cancel, r: r, ids: ids, want: want, n: n, window: 2 * n,
		futs: map[int]*blockFuture{}, sem: make(chan struct{}, n)}
}

// get returns block i. Blocks must be asked for in increasing order.
func (a *readAhead) get(i int) ([]byte, error) {
	if a.n <= 1 {
		return a.r.LoadBlob(a.ctx, repo.DataBlob, a.ids[i])
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
		return a.r.LoadBlob(a.ctx, repo.DataBlob, a.ids[i])
	}
	delete(a.futs, i)
	select {
	case <-f.done:
	case <-a.ctx.Done():
		return nil, a.ctx.Err()
	}
	return f.b, f.err
}

func (a *readAhead) start(i int) {
	f := &blockFuture{done: make(chan struct{})}
	a.futs[i] = f
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer close(f.done)
		select {
		case a.sem <- struct{}{}:
		case <-a.ctx.Done():
			f.err = a.ctx.Err()
			return
		}
		f.b, f.err = a.r.LoadBlob(a.ctx, repo.DataBlob, a.ids[i])
		<-a.sem
	}()
}

// close stops reads still in flight and waits for them, so none outlives
// the repository.
func (a *readAhead) close() {
	a.cancel()
	a.wg.Wait()
}
