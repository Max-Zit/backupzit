package repo

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// CopyOfTag marks a snapshot copied from another repository:
// "copy-of:<source snapshot id>".
const CopyOfTag = "copy-of:"

// CopyStats summarizes a copy.
type CopyStats struct {
	Snapshots   int    `json:"snapshots"`         // snapshots copied
	Skipped     int    `json:"snapshots_skipped"` // already in the destination
	Blobs       int    `json:"blobs"`             // blobs uploaded
	BytesRead   uint64 `json:"bytes_read"`        // plaintext bytes copied
	BytesStored uint64 `json:"bytes_stored"`      // bytes uploaded to the destination
	// 0.62 (#75): First marks the first copy of the job to the destination
	// (everything is transferred); Packed is what was read from the
	// source's packs; ReadSeconds and WriteSeconds the time with a read
	// from the source or an upload to the destination in flight (which
	// side is slow); Seconds the copy's duration.
	First        bool    `json:"first,omitempty"`
	Packed       uint64  `json:"packed,omitempty"`
	ReadSeconds  float64 `json:"read_seconds,omitempty"`
	WriteSeconds float64 `json:"write_seconds,omitempty"`
	Seconds      float64 `json:"seconds,omitempty"`
}

// Add adds the numbers of another part of the same copy.
func (s *CopyStats) Add(o *CopyStats) {
	s.Snapshots += o.Snapshots
	s.Skipped += o.Skipped
	s.Blobs += o.Blobs
	s.BytesRead += o.BytesRead
	s.BytesStored += o.BytesStored
	s.First = s.First || o.First
	s.Packed += o.Packed
	s.ReadSeconds += o.ReadSeconds
	s.WriteSeconds += o.WriteSeconds
	s.Seconds += o.Seconds
}

// The phases a copy reports.
const (
	CopyPlanning = "plan" // finding the data the destination lacks
	CopyTransfer = "copy" // reading it from the source and storing it
)

// CopyProgress is how far a copy is.
type CopyProgress struct {
	Phase     string
	Snapshot  int       // the backup worked on, 1-based
	Snapshots int       // the backups to copy (not those copied before)
	Time      time.Time // the time of that backup
	// Done of Total bytes of the source's packs are copied; Total is exact
	// once the planning is done (it adds up what the destination lacks).
	Done, Total uint64
	First       bool
}

// CopyOptions are the optional settings of Copy.
type CopyOptions struct {
	// JobTag is the tag of the copy job ("job:N"): without a copy of it in
	// the destination yet, the copy is its first (CopyStats.First).
	JobTag   string
	Progress func(CopyProgress)
}

// Copy copies snapshots from src to dst (both opened with their own keys).
// Only blobs missing in dst are transferred, so repeated copies upload just
// the new data. Snapshots already copied earlier are skipped. Copies get
// tags (replacing the source's) plus "copy-of:<id>". The caller holds
// locks on both repositories.
//
// First the blobs each snapshot needs and dst lacks are found (each with
// the first snapshot that needs it), which gives the exact amount to copy.
// Then each snapshot's blobs are read in pack order, several neighbouring
// blobs with one request and a few requests at once (ReadConcurrency), and
// stored through a BlobWriter; the snapshot is saved after its blobs.
// Reading blob by blob in no order cost hundreds of thousands of small
// reads on a share for a large repository (#75).
func Copy(ctx context.Context, src, dst *Repository, sns []*Snapshot, tags []string, opts *CopyOptions) (*CopyStats, []*Snapshot, error) {
	if opts == nil {
		opts = &CopyOptions{}
	}
	start := time.Now()
	st := &CopyStats{}
	existing, err := dst.ListSnapshots(ctx)
	if err != nil {
		return nil, nil, err
	}
	have := map[string]bool{}
	jobSeen := false
	for _, sn := range existing {
		for _, t := range sn.Tags {
			if strings.HasPrefix(t, CopyOfTag) {
				have[strings.TrimPrefix(t, CopyOfTag)] = true
			}
			if t == opts.JobTag {
				jobSeen = true
			}
		}
	}
	st.First = opts.JobTag != "" && !jobSeen
	var todo []*Snapshot
	for _, sn := range sns {
		if have[sn.ID.String()] {
			st.Skipped++
		} else {
			todo = append(todo, sn)
		}
	}
	p := CopyProgress{Snapshots: len(todo), First: st.First}
	report := func() {
		if opts.Progress != nil {
			opts.Progress(p)
		}
	}

	// Plan. A snapshot that cannot be read ends the plan there: the ones
	// before it are still copied, and its error is returned after them.
	plan := make([][]copyBlob, len(todo))
	assigned := map[BlobHandle]bool{}
	var planErr error
	p.Phase = CopyPlanning
	for i, sn := range todo {
		if err := ctx.Err(); err != nil {
			return st, nil, err
		}
		p.Snapshot, p.Time = i+1, sn.Time
		report()
		blobs, err := src.BlobsOf(ctx, []*Snapshot{sn})
		if err == nil {
			for h := range blobs {
				if assigned[h] || dst.hasBlob(h) {
					continue
				}
				loc, ok := src.idx.Lookup(h)
				if !ok {
					err = fmt.Errorf("blob %s not found in index", h.ID.Short())
					break
				}
				assigned[h] = true
				plan[i] = append(plan[i], copyBlob{h: h, loc: loc})
				p.Total += uint64(loc.Length)
			}
		}
		if err != nil {
			planErr = fmt.Errorf("snapshot %s: %w", sn.ID.Short(), err)
			for _, b := range plan[i] { // not copied: not counted
				p.Total -= uint64(b.loc.Length)
			}
			todo, plan = todo[:i], plan[:i]
			p.Snapshots = len(todo)
			break
		}
	}
	assigned = nil

	var copied []*Snapshot
	before := dst.Stats()
	reads := &busyTimer{}
	finish := func(err error) (*CopyStats, []*Snapshot, error) {
		after := dst.Stats()
		st.BytesStored = after.StoredBytes - before.StoredBytes
		st.WriteSeconds = (after.UploadBusy - before.UploadBusy).Seconds()
		st.ReadSeconds = reads.total().Seconds()
		st.Packed = p.Done
		st.Seconds = time.Since(start).Seconds()
		return st, copied, err
	}
	p.Phase = CopyTransfer
	for i, sn := range todo {
		if err := ctx.Err(); err != nil {
			return finish(err)
		}
		p.Snapshot, p.Time = i+1, sn.Time
		report()
		if err := copyBlobs(ctx, src, dst, plan[i], st, &p, reads, report); err != nil {
			return finish(fmt.Errorf("snapshot %s: %w", sn.ID.Short(), err))
		}
		plan[i] = nil
		// Snapshots reference only blobs that are already stored.
		if err := dst.Flush(ctx); err != nil {
			return finish(err)
		}
		c := *sn
		c.ID = ID{}
		c.Parent = nil
		c.Tags = append(append([]string{}, tags...), CopyOfTag+sn.ID.String())
		if _, err := dst.SaveSnapshot(ctx, &c); err != nil {
			return finish(err)
		}
		copied = append(copied, &c)
		st.Snapshots++
	}
	return finish(planErr)
}

type copyBlob struct {
	h   BlobHandle
	loc Location
}

// copyRange is neighbouring blobs of one pack, read with one request.
type copyRange struct {
	pack   ID
	off, n uint64
	blobs  []copyBlob
	done   chan struct{}
	data   []byte
	err    error
}

const (
	copyRangeMax = 4 << 20   // bytes per request
	copyRangeGap = 256 << 10 // bytes between two blobs read along rather than in a request of their own
)

// copyRanges sorts blobs by pack and offset and groups them into ranges.
func copyRanges(blobs []copyBlob) []*copyRange {
	slices.SortFunc(blobs, func(a, b copyBlob) int {
		if c := bytes.Compare(a.loc.Pack[:], b.loc.Pack[:]); c != 0 {
			return c
		}
		return cmp.Compare(a.loc.Offset, b.loc.Offset)
	})
	var out []*copyRange
	for _, b := range blobs {
		off, end := uint64(b.loc.Offset), uint64(b.loc.Offset)+uint64(b.loc.Length)
		if n := len(out); n > 0 {
			r := out[n-1]
			if r.pack == b.loc.Pack && off >= r.off+r.n && off <= r.off+r.n+copyRangeGap && end-r.off <= copyRangeMax {
				r.n = end - r.off
				r.blobs = append(r.blobs, b)
				continue
			}
		}
		out = append(out, &copyRange{pack: b.loc.Pack, off: off, n: end - off, blobs: []copyBlob{b}})
	}
	return out
}

// copyBlobs reads blobs from src a few ranges ahead, in order, and stores
// them in dst.
func copyBlobs(ctx context.Context, src, dst *Repository, blobs []copyBlob, st *CopyStats, p *CopyProgress, reads *busyTimer, report func()) error {
	if len(blobs) == 0 {
		return nil
	}
	ranges := copyRanges(blobs)
	bw, err := NewBlobWriter(ctx, dst, 0)
	if err != nil {
		return err
	}
	defer func() { _ = bw.Close() }()
	rctx, stop := context.WithCancel(ctx)
	n := src.ReadConcurrency()
	queue := make(chan *copyRange, len(ranges))
	var wg sync.WaitGroup
	defer func() { // no read outlives the copy
		stop()
		wg.Wait()
	}()
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range queue {
				if err := rctx.Err(); err != nil {
					r.err = err
				} else {
					reads.begin()
					r.data, r.err = src.be.LoadRange(rctx, packName(r.pack), int64(r.off), int(r.n)) // #nosec G115 -- offsets and lengths of one pack
					reads.end()
					if r.err == nil && uint64(len(r.data)) != r.n {
						r.err = fmt.Errorf("read %d bytes, want %d", len(r.data), r.n)
					}
				}
				close(r.done)
			}
		}()
	}
	// The readers start at most window ranges ahead of the one stored.
	window := 2 * n
	next := 0
	feed := func(upTo int) {
		for ; next < len(ranges) && next <= upTo; next++ {
			ranges[next].done = make(chan struct{})
			queue <- ranges[next]
		}
	}
	defer close(queue)
	lastReport := time.Now()
	for i, r := range ranges {
		feed(i + window)
		select {
		case <-r.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		if r.err != nil {
			return fmt.Errorf("read pack %s: %w", r.pack.Short(), r.err)
		}
		for _, b := range r.blobs {
			rel := uint64(b.loc.Offset) - r.off
			data, err := src.decodeBlob(b.h.ID, b.h.Type, r.data[rel:rel+uint64(b.loc.Length)], b.loc.Raw)
			if err != nil {
				return fmt.Errorf("read blob %s: %w", b.h.ID.Short(), err)
			}
			_, added, err := bw.SaveHashed(ctx, b.h.Type, b.h.ID, data)
			if err != nil {
				return err
			}
			if added {
				st.Blobs++
				st.BytesRead += uint64(len(data))
			}
			p.Done += uint64(b.loc.Length)
		}
		r.data = nil
		if time.Since(lastReport) >= time.Second || i == len(ranges)-1 {
			report()
			lastReport = time.Now()
		}
	}
	return bw.Close()
}

// busyTimer adds up the time with at least one operation in flight.
type busyTimer struct {
	mu     sync.Mutex
	active int
	since  time.Time
	busy   time.Duration
}

func (t *busyTimer) begin() {
	t.mu.Lock()
	if t.active == 0 {
		t.since = time.Now()
	}
	t.active++
	t.mu.Unlock()
}

func (t *busyTimer) end() {
	t.mu.Lock()
	t.active--
	if t.active == 0 {
		t.busy += time.Since(t.since)
	}
	t.mu.Unlock()
}

func (t *busyTimer) total() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active > 0 {
		return t.busy + time.Since(t.since)
	}
	return t.busy
}

func (r *Repository) hasBlob(h BlobHandle) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, pending := r.pending[h]
	return pending || r.idx.Reusable(h)
}
