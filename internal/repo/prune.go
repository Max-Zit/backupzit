package repo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
)

// PruneOptions control how aggressively space is reclaimed.
type PruneOptions struct {
	// RepackUnusedRatio: packs whose unused share (by stored bytes) is at
	// least this are rewritten with only their used blobs. Packs that are
	// entirely unused are always deleted. Default 0.3.
	RepackUnusedRatio float64
	// Progress reports phases for logging.
	Progress func(msg string)
}

// PruneStats summarises a prune run.
type PruneStats struct {
	Snapshots      int    `json:"snapshots"`
	PacksTotal     int    `json:"packs_total"`
	PacksDeleted   int    `json:"packs_deleted"`
	PacksRepacked  int    `json:"packs_repacked"`
	BlobsRepacked  int    `json:"blobs_repacked"`
	BytesFreed     uint64 `json:"bytes_freed"`
	BytesRemaining uint64 `json:"bytes_remaining"`
	// PacksSkipped packs could not be read for repacking (after the
	// backend's retries); they stay as they are, the next prune tries
	// again. SkipReason is the first error.
	PacksSkipped int    `json:"packs_skipped,omitempty"`
	SkipReason   string `json:"skip_reason,omitempty"`
	// PacksRestored were on the storage but not in the index although they
	// hold data backups use and nothing else holds (an index file lost or
	// deleted by hand): verified and put back into the index instead of
	// deleted. PacksUnchecked are such packs that could not be verified now:
	// they stay, neither deleted nor indexed.
	PacksRestored  int `json:"packs_restored,omitempty"`
	PacksUnchecked int `json:"packs_unchecked,omitempty"`
}

type unindexedVerdict int

const (
	unindexedDelete    unindexedVerdict = iota // garbage: delete it
	unindexedRestore                           // needed and verified: index it
	unindexedUnchecked                         // maybe needed, unverified: leave it
)

// neededUnindexed decides about a pack on the storage that the index does
// not list. It is deleted unless its header lists a blob that a backup uses
// and the index has nowhere else; then it is verified (three tries, as the
// data check does): indexed again when intact, deleted only when every try
// proves it damaged. A header that cannot be read for a storage problem
// leaves the pack alone; a damaged header makes it garbage.
func (r *Repository) neededUnindexed(ctx context.Context, p ID, used map[BlobHandle]bool) ([]PackedBlob, unindexedVerdict) {
	header, err := r.ReadPackHeader(ctx, p)
	switch {
	case errors.Is(err, errPackHeader), errors.Is(err, backend.ErrNotFound):
		return nil, unindexedDelete
	case err != nil:
		return nil, unindexedUnchecked
	}
	needed := false
	for _, b := range header {
		h := BlobHandle{Type: b.Type, ID: b.ID}
		if used[h] && !r.idx.Has(h) {
			needed = true
			break
		}
	}
	if !needed {
		return nil, unindexedDelete
	}
	var blobs []PackedBlob
	everyTryDamaged := true
	err = Tries(ctx, 0, func() (err error) {
		blobs, err = r.verifyPack(ctx, p)
		if !errors.Is(err, ErrPackDamaged) {
			everyTryDamaged = false
		}
		return err
	})
	switch {
	case err == nil:
		return blobs, unindexedRestore
	case errors.Is(err, ErrPackDamaged) && everyTryDamaged && !errors.Is(err, ErrPackGone):
		return nil, unindexedDelete
	}
	return nil, unindexedUnchecked
}

// repackGiveUp is how many packs in a row may fail to be read before prune
// stops repacking (the storage is then down, not slow); the packs left
// stay as they are.
const repackGiveUp = 3

// repackGap is how much unused data between two used blobs of a pack is
// read along instead of a second request; repackMaxRead caps one read.
const (
	repackGap     = 1 << 20
	repackMaxRead = 64 << 20
	repackMemory  = 128 << 20
)

// fetchUsed reads the used blobs of pack p in as few ranged reads as
// possible and verifies each of them.
func (r *Repository) fetchUsed(ctx context.Context, p ID, blobs []PackedBlob, used map[BlobHandle]bool) ([]PackedBlob, [][]byte, error) {
	var want []PackedBlob
	for _, b := range blobs {
		if used[BlobHandle{Type: b.Type, ID: b.ID}] {
			want = append(want, b)
		}
	}
	sort.Slice(want, func(i, j int) bool { return want[i].Offset < want[j].Offset })
	data := make([][]byte, len(want))
	for i := 0; i < len(want); {
		start := uint64(want[i].Offset)
		end := start + uint64(want[i].Length)
		j := i + 1
		for j < len(want) {
			o, e := uint64(want[j].Offset), uint64(want[j].Offset)+uint64(want[j].Length)
			if o > end+repackGap || e-start > repackMaxRead {
				break
			}
			end = max(end, e)
			j++
		}
		buf, err := r.be.LoadRange(ctx, packName(p), int64(start), int(end-start)) // #nosec G115 -- pack offsets, below repackMaxRead apart
		if err != nil {
			return nil, nil, err
		}
		if uint64(len(buf)) != end-start {
			return nil, nil, fmt.Errorf("short read: %d of %d bytes", len(buf), end-start)
		}
		for k := i; k < j; k++ {
			off := uint64(want[k].Offset) - start
			stored := buf[off : off+uint64(want[k].Length)]
			if _, err := r.decodeBlob(want[k].ID, want[k].Type, stored, want[k].Raw); err != nil {
				return nil, nil, err
			}
			data[k] = stored
		}
		i = j
	}
	return want, data, nil
}

// Each calls fn for every indexed blob.
func (x *Index) Each(fn func(h BlobHandle, l Location)) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	for h, l := range x.blobs {
		fn(h, l)
	}
}

// UsedBlobs returns every blob referenced by the repository's snapshots.
func (r *Repository) UsedBlobs(ctx context.Context) (map[BlobHandle]bool, int, error) {
	sns, err := r.ListSnapshots(ctx)
	if err != nil {
		return nil, 0, err
	}
	used, err := r.BlobsOf(ctx, sns)
	return used, len(sns), err
}

// Prune deletes data no longer referenced by any snapshot. The caller must
// hold an exclusive lock. The order of operations keeps the repository
// consistent if prune is interrupted: new packs and the new index are
// written before old index files and packs are deleted.
func (r *Repository) Prune(ctx context.Context, opts PruneOptions) (*PruneStats, error) {
	if opts.RepackUnusedRatio <= 0 {
		opts.RepackUnusedRatio = 0.3
	}
	say := func(f string, a ...any) {
		if opts.Progress != nil {
			opts.Progress(fmt.Sprintf(f, a...))
		}
	}
	// Prune deletes every pack the index lacks and then all old index files:
	// with a damaged index file the index is built from pack headers, and a
	// pack missing from it may still be needed (B11 review).
	if err := r.errDamagedIndexFiles(); err != nil {
		return nil, err
	}
	st := &PruneStats{}

	say("reading the backups")
	start := time.Now()
	used, nsn, err := r.UsedBlobs(ctx)
	if err != nil {
		return nil, err
	}
	st.Snapshots = nsn
	say("read %d backups in %s", nsn, time.Since(start).Round(time.Second))
	oldIndexes, err := r.be.List(ctx, "index")
	if err != nil {
		return nil, err
	}
	onDisk, err := r.ListPacks(ctx)
	if err != nil {
		return nil, err
	}
	st.PacksTotal = len(onDisk)

	// Group indexed blobs by pack.
	type packInfo struct {
		blobs                 []PackedBlob
		usedBytes, totalBytes uint64
	}
	packs := map[ID]*packInfo{}
	r.idx.Each(func(h BlobHandle, l Location) {
		pi := packs[l.Pack]
		if pi == nil {
			pi = &packInfo{}
			packs[l.Pack] = pi
		}
		pb := PackedBlob{Type: h.Type, ID: h.ID, Offset: l.Offset, Length: l.Length, Raw: l.Raw}
		pi.blobs = append(pi.blobs, pb)
		pi.totalBytes += uint64(l.Length)
		if used[h] {
			pi.usedBytes += uint64(l.Length)
		}
	})

	var deletePacks []ID
	var keep []indexPack
	var repack []ID
	for _, p := range onDisk {
		pi := packs[p]
		if pi == nil {
			// Not in the index: usually what an interrupted backup left. But
			// an index file lost or deleted by hand takes its packs out of
			// the index too, and they may hold the only copy of data backups
			// use (B5, 0.59): such a pack is verified and indexed again.
			switch blobs, verdict := r.neededUnindexed(ctx, p, used); verdict {
			case unindexedRestore:
				say("pack %s was not in the index but holds data backups use: indexed again", p.Short())
				keep = append(keep, indexPack{ID: p, Blobs: blobs})
				st.PacksRestored++
				continue
			case unindexedUnchecked:
				say("pack %s is not in the index, may hold data backups use and could not be verified now: left as it is", p.Short())
				st.PacksUnchecked++
				continue
			}
		}
		switch {
		case pi == nil || pi.usedBytes == 0:
			deletePacks = append(deletePacks, p) // unreferenced or fully unused
			if pi != nil {
				st.BytesFreed += pi.totalBytes
			}
		case float64(pi.totalBytes-pi.usedBytes)/float64(pi.totalBytes) >= opts.RepackUnusedRatio:
			repack = append(repack, p)
		default:
			keep = append(keep, indexPack{ID: p, Blobs: pi.blobs})
			st.BytesRemaining += pi.totalBytes
		}
	}
	// A pack indexed again needs the new index written even when nothing
	// goes.
	if len(deletePacks) == 0 && len(repack) == 0 && st.PacksRestored == 0 {
		say("nothing to prune")
		return st, nil
	}

	// Copy used blobs out of packs that are mostly garbage: the used blobs
	// of a pack in as few ranged reads as possible, several packs at a time
	// on an object store (one request per blob took over two hours for
	// 1,013 packs on a slow provider, #38). A pack that still cannot be read
	// after the backend's retries stays as it is, with none of its blobs
	// copied, and the next prune tries again: one slow read used to fail the
	// whole prune.
	say("repacking %d packs", len(repack))
	type fetched struct {
		p     ID
		blobs []PackedBlob
		data  [][]byte
		err   error
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan ID)
	results := make(chan fetched)
	var wg sync.WaitGroup
	// Each worker holds the used data of one pack: at most repackMemory in
	// all (large packs are read one at a time).
	workers := max(1, min(r.Concurrency(), repackMemory/max(r.cfg.PackSize, 1), len(repack)))
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				blobs, data, err := r.fetchUsed(rctx, p, packs[p].blobs, used)
				select {
				case results <- fetched{p, blobs, data, err}:
				case <-rctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, p := range repack {
			select {
			case jobs <- p:
			case <-rctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()
	handled := map[ID]bool{}
	stopped, inARow, step := false, 0, max(len(repack)/10, 1)
	skip := func(p ID, err error) {
		pi := packs[p]
		keep = append(keep, indexPack{ID: p, Blobs: pi.blobs})
		st.BytesRemaining += pi.totalBytes
		st.PacksSkipped++
		if st.SkipReason == "" && err != nil {
			st.SkipReason = fmt.Sprintf("repack %s: %v", p.Short(), err)
		}
	}
	for f := range results {
		if stopped {
			continue // drained: these packs stay as they are
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		handled[f.p] = true
		if f.err != nil {
			skip(f.p, f.err)
			if inARow++; inARow >= repackGiveUp {
				stopped = true
				cancel()
			}
			continue
		}
		inARow = 0
		for i, b := range f.blobs {
			if err := r.addStoredBlob(ctx, b.Type, b.ID, f.data[i], b.Raw); err != nil {
				return nil, err
			}
			st.BlobsRepacked++
			st.BytesRemaining += uint64(b.Length)
		}
		pi := packs[f.p]
		st.BytesFreed += pi.totalBytes - pi.usedBytes
		st.PacksRepacked++
		deletePacks = append(deletePacks, f.p)
		if n := len(handled); n%step == 0 && n < len(repack) {
			say("repacked %d of %d packs", n, len(repack))
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	for _, p := range repack {
		if !handled[p] {
			skip(p, nil) // after repackGiveUp failures in a row
		}
	}
	if st.PacksSkipped > 0 {
		say("%d packs could not be read and stay as they are: %s", st.PacksSkipped, st.SkipReason)
	}

	// Write the new index: kept packs plus the repacked ones (Flush writes
	// those), then drop the old index files and the obsolete packs.
	if err := r.Flush(ctx); err != nil {
		return nil, err
	}
	// Index files are named by their content, so a new file can have the
	// same name as an old one; those must not be deleted below.
	written := map[string]bool{}
	for start := 0; start < len(keep); start += indexFlushPacks * 8 {
		end := min(start+indexFlushPacks*8, len(keep))
		name, err := r.saveIndexFile(ctx, keep[start:end])
		if err != nil {
			return nil, err
		}
		written[name] = true
	}
	for _, n := range oldIndexes {
		if written[n] {
			continue
		}
		if err := r.be.Remove(ctx, n); err != nil && !strings.Contains(err.Error(), "not found") {
			return nil, fmt.Errorf("remove %s: %w", n, err)
		}
	}
	say("deleting %d packs", len(deletePacks))
	for _, p := range deletePacks {
		if err := r.be.Remove(ctx, packName(p)); err != nil && !strings.Contains(err.Error(), "not found") {
			return nil, fmt.Errorf("remove pack %s: %w", p.Short(), err)
		}
		st.PacksDeleted++
	}
	st.PacksDeleted -= st.PacksRepacked // reported separately

	// Reload the in-memory index from what is now on disk.
	r.mu.Lock()
	r.idx = newIndex()
	r.mu.Unlock()
	r.indexMu.Lock()
	r.indexFiles = map[string]bool{}
	r.indexMu.Unlock()
	if err := r.loadIndex(ctx, nil); err != nil {
		return nil, err
	}
	return st, nil
}

// addStoredBlob appends an already encoded blob to the current pack,
// bypassing deduplication against the index (used by repacking, where the
// blob is indexed in a pack that is about to be deleted).
func (r *Repository) addStoredBlob(ctx context.Context, t BlobType, id ID, stored []byte, raw uint32) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := BlobHandle{Type: t, ID: id}
	if _, ok := r.pending[h]; ok {
		return nil
	}
	r.pack.add(t, id, stored, int(raw))
	r.pending[h] = struct{}{}
	if r.pack.size() >= r.cfg.PackSize {
		return r.flushPackLocked(ctx)
	}
	return nil
}
