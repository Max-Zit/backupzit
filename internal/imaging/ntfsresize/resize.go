package ntfsresize

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
)

// PlanOptions change what PlanWith and MinimumSize accept. The zero value
// is the strict one.
type PlanOptions struct {
	// FromSnapshot says the image was read from a VSS shadow copy. A
	// shadow copy is taken of a mounted volume, so its $LogFile is open;
	// VSS flushed the file system before taking it, so resetting the log
	// loses nothing. Only then is an open $LogFile accepted. Leave it
	// false for anything else: live reads, offline copies, unknown
	// sources.
	FromSnapshot bool
	// NoMove shrinks the volume only down to just after its last used
	// cluster, so no data moves. Use it for images that were not read
	// from a VSS snapshot: their metadata is less certain to describe
	// their data, and nothing can be overwritten when nothing moves.
	NoMove bool
	// Progress, if set, is called while the file records are read, with
	// the records done and the total. Planning reads them all once (twice
	// when a size needs the fragmentation fallback).
	Progress func(done, total uint64)
}

// TooSmallError is returned when the data does not fit the size asked
// for. MinSize is the smallest partition size PlanWith accepts with the
// same options; every larger size is accepted too.
type TooSmallError struct {
	Size    uint64
	MinSize uint64
	Why     string
}

// Error tells what is wrong and the size to choose instead.
func (e *TooSmallError) Error() string {
	return fmt.Sprintf("%s: choose a partition of at least %d bytes", e.Why, e.MinSize)
}

// Move is a range of clusters that changes place.
type Move struct {
	From, To, Len uint64
}

// Plan describes how the volume shrinks.
type Plan struct {
	v               *Volume
	NewSize         uint64 // partition size in bytes
	NewTotalSectors uint64
	NewClusters     uint64
	// Moves are sorted by From.
	Moves         []Move
	MovedClusters uint64
	// Orphaned counts allocated clusters behind the new end that no file
	// refers to; they are dropped (chkdsk would free them anyway).
	Orphaned uint64
	// Unchanged is set when the volume already fits the partition (it was
	// smaller than its partition): nothing is written.
	Unchanged bool
	// ShadowCopiesRemoved is set when the volume had VSS shadow copies
	// (previous versions, restore points). Their stores refer to volume
	// offsets that change, so the shrunk volume starts without them.
	ShadowCopiesRemoved bool

	// sectorsOnly: only sectors after the last cluster are cut, so only
	// the boot sectors change.
	sectorsOnly bool
	// allocCut is where moving starts: NewClusters, or less when the free
	// space before NewClusters is too fragmented (see layoutFor).
	allocCut      uint64
	records       map[uint64]*Record // changed copies of file records
	dirty         []*Record          // the same, by record number
	mftRuns       []Run
	bitmap        []byte
	boot          []byte
	vssSector     []byte
	vssAt         uint64
	mirrorRecords uint64
}

// planProblems is how many inconsistencies a refusal lists.
const planProblems = 5

// Plan works out how to shrink the volume to a partition of newSize bytes
// with the strict options: see PlanWith.
func (v *Volume) Plan(newSize uint64) (*Plan, error) {
	return v.PlanWith(newSize, PlanOptions{})
}

// PlanWith works out how to shrink the volume to a partition of newSize
// bytes. It reads every file record and refuses, before anything is
// written, a source that is inconsistent (a file uses clusters that are
// free in $Bitmap, past the end of the volume or used by another file),
// marked dirty, not shut down cleanly or hibernated: see PlanOptions.
// When MinimumSize returned a size, every size from it up is accepted
// (on the same volume with the same options). A volume that already fits
// gives a plan that changes nothing. PlanWith does not change v.
func (v *Volume) PlanWith(newSize uint64, opts PlanOptions) (*Plan, error) {
	if newSize < 2*v.BytesPerSector {
		return nil, errors.New("new size too small")
	}
	p := &Plan{v: v, NewSize: newSize, NewTotalSectors: newSize/v.BytesPerSector - 1}
	p.NewClusters = p.NewTotalSectors * v.BytesPerSector / v.ClusterSize
	switch {
	case p.NewTotalSectors >= v.TotalSectors:
		// The volume was smaller than its partition and fits as it is.
		p.Unchanged = true
		p.NewTotalSectors, p.NewClusters = v.TotalSectors, v.Clusters
		return p, nil
	case p.NewClusters >= v.Clusters:
		// Only the sectors after the last whole cluster go: a new sector
		// count and backup boot sector, nothing else.
		p.sectorsOnly, p.NewClusters = true, v.Clusters
		return p, p.prepareBoot(v.mftLCN, binary.LittleEndian.Uint64(v.boot[0x38:]))
	}
	if err := v.checkState(opts); err != nil {
		return nil, err
	}
	sr, err := v.scan(p.NewClusters, !opts.NoMove, planProblems, opts.Progress)
	if err != nil {
		return nil, err
	}
	if err := v.checkScan(sr); err != nil {
		return nil, err
	}
	var lay *layout
	if opts.NoMove {
		if cut := noMoveCut(sr); p.NewClusters < cut {
			return nil, &TooSmallError{Size: newSize, MinSize: v.sizeFor(cut),
				Why: "without moving data (the backup was not read from a VSS snapshot) the volume can shrink only to just after its last used cluster"}
		}
		lay = &layout{allocCut: p.NewClusters}
	} else if lay, err = v.layoutFor(sr, p.NewClusters, newSize, opts); err != nil {
		return nil, err
	}
	if err := v.finalize(p, sr, lay); err != nil {
		return nil, err
	}
	return p, nil
}

// MinimumSize is the smallest partition size PlanWith accepts with these
// options; it accepts every larger size too. It reads every file record
// and refuses the same sources PlanWith refuses. The size is what the
// file system needs, before any partition alignment.
func (v *Volume) MinimumSize(opts PlanOptions) (uint64, error) {
	if err := v.checkState(opts); err != nil {
		return 0, err
	}
	lo := v.estimateCut()
	sr, err := v.scan(lo, !opts.NoMove, planProblems, opts.Progress)
	if err != nil {
		return 0, err
	}
	if err := v.checkScan(sr); err != nil {
		return 0, err
	}
	cut := noMoveCut(sr)
	if !opts.NoMove {
		cut = v.minCut(sr, lo)
	}
	if cut >= v.Clusters {
		return v.UnchangedSize(), nil
	}
	return v.sizeFor(cut), nil
}

// sizeFor is the partition size whose volume has cut clusters.
func (v *Volume) sizeFor(cut uint64) uint64 { return cut*v.ClusterSize + v.BytesPerSector }

// estimateCut is the smallest cluster count worth trying when data
// moves: every allocated cluster and a little room (1 %, at least 16
// clusters), so the shrunk Windows still has some free space.
func (v *Volume) estimateCut() uint64 {
	return v.UsedClusters + max(v.UsedClusters/100, 16)
}

// noMoveCut is the smallest cluster count that keeps every owned cluster
// in place.
func noMoveCut(sr *scanResult) uint64 { return max(sr.ownedEnd, sr.bootEnd, 1) }

// minCut is the smallest cluster count PlanWith accepts with moves: the
// first that works from estimateCut up (bisecting, every answer checked
// by a simulation), or the no-move count if that is smaller. sr must
// hold the candidates from lo on.
func (v *Volume) minCut(sr *scanResult, lo uint64) uint64 {
	nm := noMoveCut(sr)
	if nm <= lo {
		return nm
	}
	works := func(c uint64) bool { _, err := v.simulate(sr, c); return err == nil }
	if works(lo) {
		return lo
	}
	// works(nm) holds: nothing lies at or after it, so nothing moves.
	l, h := lo, nm
	for h-l > 1 {
		if m := l + (h-l)/2; works(m) {
			h = m
		} else {
			l = m
		}
	}
	return h
}

// layoutFor finds where the clusters behind cut go. When they do not fit
// below cut (the free space is too fragmented, so a record's run list
// would outgrow the record), it moves data further down instead: the
// largest working cut between the minimum and cut. A volume that fits in
// fewer clusters fits in cut clusters too, so every size from
// MinimumSize up works.
func (v *Volume) layoutFor(sr *scanResult, cut, newSize uint64, opts PlanOptions) (*layout, error) {
	lay, err := v.simulate(sr, cut)
	var fe *fitError
	if err == nil || !errors.As(err, &fe) {
		return lay, err
	}
	lo := v.estimateCut()
	if lo < sr.from {
		// The fallback needs the records with clusters from lo on.
		if sr, err = v.scan(lo, true, planProblems, opts.Progress); err != nil {
			return nil, err
		}
	}
	low := v.minCut(sr, lo)
	if cut < low {
		return nil, &TooSmallError{Size: newSize, MinSize: v.sizeFor(low), Why: fe.msg}
	}
	best, err := v.simulate(sr, low)
	if err != nil {
		return nil, fmt.Errorf("internal error: the minimum layout no longer works: %w", err)
	}
	for l, h := low, cut; h-l > 1; {
		m := l + (h-l)/2
		if lm, err := v.simulate(sr, m); err == nil {
			l, best = m, lm
		} else {
			h = m
		}
	}
	return best, nil
}

// layout is where the clusters behind allocCut go.
type layout struct {
	allocCut uint64
	moves    []Move
	moved    uint64
	changed  []*Record // records whose runs change, by record number
	runs     map[*Record]map[int][]Run
}

// fitError says the data does not fit at a cut: too little free space,
// or a record that would outgrow itself.
type fitError struct{ msg string }

// Error returns the reason.
func (e *fitError) Error() string { return e.msg }

// simulate plans moving every owned cluster at or after cut to free
// clusters before it, without changing anything. It depends only on the
// volume and cut, so planning the same cut twice gives the same layout.
// sr must hold the candidates from cut or less.
func (v *Volume) simulate(sr *scanResult, cut uint64) (*layout, error) {
	// One request per attribute with clusters behind the cut, so a moved
	// stream can land in one piece.
	type request struct {
		rec   *Record
		ai    int
		total uint64
	}
	var reqs []request
	var need uint64
	for _, rec := range sr.cands {
		for ai, a := range rec.Attrs {
			if !a.NonResident || isBadClus(rec, a) {
				continue
			}
			var n uint64
			for _, r := range a.Runs {
				if !r.Sparse && r.LCN+r.Len > cut {
					n += r.LCN + r.Len - max(r.LCN, cut)
				}
			}
			if n > 0 {
				reqs = append(reqs, request{rec, ai, n})
				need += n
			}
		}
	}
	lay := &layout{allocCut: cut, runs: map[*Record]map[int][]Run{}}
	if len(reqs) == 0 {
		return lay, nil
	}
	ext := freeExtents(v.bitmap, cut, maxFreeExtents)
	var free uint64
	for _, e := range ext {
		free += e.len
	}
	if need > free {
		return nil, &fitError{fmt.Sprintf("the data does not fit: %d clusters lie behind the new end, %d are free before it", need, free)}
	}
	// Largest requests first (first fit decreasing): the big streams get
	// the big extents, so they stay in few pieces.
	sort.SliceStable(reqs, func(i, j int) bool { return reqs[i].total > reqs[j].total })
	alloc := newAllocator(ext)
	for _, rq := range reqs {
		exts, err := alloc.take(rq.total)
		if err != nil {
			return nil, &fitError{err.Error()}
		}
		a := rq.rec.Attrs[rq.ai]
		out := make([]Run, 0, len(a.Runs)+len(exts))
		ei, used := 0, uint64(0)
		for _, r := range a.Runs {
			if r.Sparse || r.LCN+r.Len <= cut {
				out = append(out, r)
				continue
			}
			vcn, from, left := r.VCN, r.LCN, r.Len
			if r.LCN < cut {
				keep := cut - r.LCN
				out = append(out, Run{VCN: vcn, LCN: from, Len: keep})
				vcn, from, left = vcn+keep, from+keep, left-keep
			}
			for left > 0 {
				e := exts[ei]
				k := min(left, e.len-used)
				to := e.start + used
				lay.moves = append(lay.moves, Move{From: from, To: to, Len: k})
				out = append(out, Run{VCN: vcn, LCN: to, Len: k})
				vcn, from, left, used = vcn+k, from+k, left-k, used+k
				if used == e.len {
					ei, used = ei+1, 0
				}
			}
		}
		lay.moved += rq.total
		m := lay.runs[rq.rec]
		if m == nil {
			m = map[int][]Run{}
			lay.runs[rq.rec] = m
			lay.changed = append(lay.changed, rq.rec)
		}
		m[rq.ai] = mergeRuns(out)
	}
	sort.Slice(lay.changed, func(i, j int) bool { return lay.changed[i].Number < lay.changed[j].Number })
	for _, rec := range lay.changed {
		if recordLen(rec, lay.runs[rec]) > rec.size {
			return nil, &fitError{fmt.Sprintf("file record %d would outgrow itself: the free space before the new end is too fragmented for its moved clusters", rec.Number)}
		}
	}
	// Copy in source order: the restore reads the source from the backup
	// through a block cache, and the moves do not depend on each other.
	sort.Slice(lay.moves, func(i, j int) bool { return lay.moves[i].From < lay.moves[j].From })
	return lay, nil
}

// recordLen is the length Record.encode needs for rec with the changed
// runs (by attribute index).
func recordLen(rec *Record, changed map[int][]Run) int {
	n := len(rec.header) + 8
	for i, a := range rec.Attrs {
		if runs, ok := changed[i]; ok {
			n += a.encodedLen(runs)
		} else {
			n += len(a.raw)
		}
	}
	return n
}

// record returns the plan's copy of a system record, or the original.
func (p *Plan) record(num uint64) *Record {
	if r, ok := p.records[num]; ok {
		return r
	}
	return p.v.records[num]
}

// mutable returns the plan's copy of a system record, making one first.
func (p *Plan) mutable(num uint64) *Record {
	if r, ok := p.records[num]; ok {
		return r
	}
	c := p.v.records[num].clone()
	c.dirty = true
	p.records[num] = c
	return c
}

// finalize turns a layout into the plan: the changed records, the new
// runs of $MFT, $Bitmap, $BadClus, the dirty flag, the boot sector and
// the shadow copy header.
func (v *Volume) finalize(p *Plan, sr *scanResult, lay *layout) error {
	p.allocCut = lay.allocCut
	p.Moves, p.MovedClusters = lay.moves, lay.moved
	p.records = map[uint64]*Record{}
	for _, rec := range lay.changed {
		c := rec.clone()
		for ai, runs := range lay.runs[rec] {
			c.Attrs[ai].Runs, c.Attrs[ai].dirty = runs, true
		}
		c.dirty = true
		p.records[c.Number] = c
	}

	// $MFT runs as they are after the moves (all segments, when a
	// fragmented $MFT has extension records). They must still describe
	// exactly $MFT: as many clusters as before.
	segs := dataSegments(p.record(recMFT))
	for _, e := range sr.mftExt {
		if c, ok := p.records[e.Number]; ok {
			e = c
		}
		segs = append(segs, dataSegments(e)...)
	}
	runs, total, err := joinSegments(segs, p.NewClusters)
	if err != nil {
		return fmt.Errorf("internal error: %w", err)
	}
	if total != v.mftClusters {
		return fmt.Errorf("internal error: $MFT would have %d clusters, it has %d", total, v.mftClusters)
	}
	p.mftRuns = mergeRuns(runs)

	// $Bitmap: shorter, covering the new cluster count.
	newLen := ((p.NewClusters+7)/8 + 7) / 8 * 8
	bm := p.mutable(recBitmap).unnamed(attrData)
	if newLen > bm.AllocSize {
		return errors.New("internal error: the new $Bitmap is larger than the old one")
	}
	bm.DataSize, bm.InitializedSize, bm.dirty = newLen, newLen, true
	p.bitmap = v.shrunkBitmap(lay, p.NewClusters, newLen)

	// $BadClus:$Bad: a sparse stream as long as the volume.
	bad := p.mutable(recBadClus).named(attrData, "$Bad")
	bad.Runs = []Run{{VCN: 0, Len: p.NewClusters, Sparse: true}}
	bad.LastVCN = p.NewClusters - 1
	bad.AllocSize = p.NewClusters * v.ClusterSize
	bad.DataSize = bad.AllocSize
	bad.InitializedSize = min(bad.InitializedSize, bad.DataSize)
	bad.dirty = true

	// Mark the volume dirty: Windows runs chkdsk once at the next start.
	val := p.mutable(recVolume).unnamed(attrVolumeInfo).residentValue()
	binary.LittleEndian.PutUint16(val[0x0A:], binary.LittleEndian.Uint16(val[0x0A:])|volumeDirty)

	for _, num := range sortedKeys(p.records) {
		rec := p.records[num]
		if _, err := rec.encode(); err != nil {
			return err
		}
		p.dirty = append(p.dirty, rec)
	}
	// Allocated clusters from the allocation cut on that no file owns.
	p.Orphaned = countAndNot(v.bitmap, sr.owned, lay.allocCut, v.Clusters)

	mirr := p.record(recMFTMirr).unnamed(attrData)
	p.mirrorRecords = max(1, min(mirr.DataSize/v.RecordSize, v.mftRecords))
	mftLCN, err := lcnOf(p.mftRuns, 0)
	if err != nil {
		return err
	}
	mirrLCN, err := lcnOf(mirr.Runs, 0)
	if err != nil {
		return err
	}
	if err := p.prepareBoot(mftLCN, mirrLCN); err != nil {
		return err
	}
	// The VSS header lives in $Boot; on a volume whose $Boot does not
	// reach it, those bytes belong to something else.
	if sr.bootEnd*v.ClusterSize >= vssHeaderOffset+512 {
		sec, at, err := v.shadowCopyHeader()
		if err != nil {
			return err
		}
		p.vssSector, p.vssAt, p.ShadowCopiesRemoved = sec, at, sec != nil
	}
	return nil
}

// shrunkBitmap builds the new $Bitmap contents: the old bits before the
// allocation cut, the moved clusters, nothing from the cut to the new end
// (their data moved; orphans go) and the bits past the last cluster set.
func (v *Volume) shrunkBitmap(lay *layout, newClusters, newLen uint64) []byte {
	b := make([]byte, newLen)
	cut := lay.allocCut
	copy(b, v.bitmap[:(cut+7)/8])
	if end := (cut + 7) / 8 * 8; end > cut {
		setBits(b, cut, end-cut, false)
	}
	for _, m := range lay.moves {
		setBits(b, m.To, m.Len, true)
	}
	// Windows marks the bits past the last cluster as used, so they are
	// never allocated; chkdsk calls the bitmap incorrect otherwise.
	setBits(b, newClusters, newLen*8-newClusters, true)
	return b
}

// prepareBoot builds the new boot sector: the source's with the new sector
// count and the positions of $MFT and $MFTMirr.
func (p *Plan) prepareBoot(mftLCN, mirrLCN uint64) error {
	boot := make([]byte, p.v.BytesPerSector)
	if err := p.v.readAt(boot, 0); err != nil {
		return fmt.Errorf("read boot sector: %w", err)
	}
	binary.LittleEndian.PutUint64(boot[0x28:], p.NewTotalSectors)
	binary.LittleEndian.PutUint64(boot[0x30:], mftLCN)
	binary.LittleEndian.PutUint64(boot[0x38:], mirrLCN)
	p.boot = boot
	return nil
}

// sortedKeys returns the record numbers of m in order.
func sortedKeys(m map[uint64]*Record) []uint64 {
	out := make([]uint64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// writeAt writes b at byte offset off of dst.
func writeAt(dst io.WriterAt, b []byte, off uint64) error {
	o, err := off64(off)
	if err != nil {
		return err
	}
	_, err = dst.WriteAt(b, o)
	return err
}

// writeVirtual writes data at byte offset off of a stream through runs,
// a cluster at a time.
func (v *Volume) writeVirtual(dst io.WriterAt, runs []Run, off uint64, data []byte) error {
	for done := uint64(0); done < uint64(len(data)); {
		vcn := (off + done) / v.ClusterSize
		in := (off + done) % v.ClusterSize
		lcn, err := lcnOf(runs, vcn)
		if err != nil {
			return err
		}
		chunk := min(v.ClusterSize-in, uint64(len(data))-done)
		if err := writeAt(dst, data[done:done+chunk], lcn*v.ClusterSize+in); err != nil {
			return err
		}
		done += chunk
	}
	return nil
}

// writeStream writes the first size bytes (whole clusters) of a stream
// through runs in pieces of up to 1 MiB, each filled by fill with the
// bytes for its stream offset. Every write starts and ends on a cluster
// boundary, as raw disk devices require.
func (v *Volume) writeStream(dst io.WriterAt, runs []Run, size uint64, fill func(off uint64, b []byte)) error {
	cs := v.ClusterSize
	piece := min(max(cs, (1<<20)/cs*cs), size)
	buf := make([]byte, piece)
	for off := uint64(0); off < size; {
		vcn := off / cs
		r, ok := findRun(runs, vcn)
		if !ok || r.Sparse {
			return fmt.Errorf("cluster %d of the stream is not mapped", vcn)
		}
		n := min(piece, (r.VCN+r.Len-vcn)*cs, size-off)
		b := buf[:n]
		fill(off, b)
		if err := writeAt(dst, b, (r.LCN+vcn-r.VCN)*cs); err != nil {
			return err
		}
		off += n
	}
	return nil
}

// Apply writes the shrunk file system to dst, a writer over the target
// partition that already holds the clusters before the new end (as the
// image had them). It copies the moved clusters from the source, then
// writes $Bitmap, resets $LogFile, clears the shadow copy catalog and
// writes the changed file records and, last, the boot sectors.
//
// The source is only read. If Apply stops halfway (a failed write, power
// loss), the boot sector still gives the old, larger size, so Windows
// does not mount the incomplete target; run the whole restore again
// (Apply cannot be resumed). Call Verify afterwards, before anything
// mounts the volume.
func (p *Plan) Apply(dst io.WriterAt, progress func(done, total uint64)) error {
	if p.Unchanged {
		return nil
	}
	v := p.v
	if !p.sectorsOnly {
		if err := p.copyMoves(dst, progress); err != nil {
			return err
		}
		// $Bitmap contents (rest of its allocation zeroed).
		bm := p.record(recBitmap).unnamed(attrData)
		err := v.writeStream(dst, bm.Runs, bm.AllocSize, func(off uint64, b []byte) {
			clear(b)
			if off < uint64(len(p.bitmap)) {
				copy(b, p.bitmap[off:])
			}
		})
		if err != nil {
			return fmt.Errorf("write $Bitmap: %w", err)
		}
		// Reset $LogFile: clusters moved, so the journal must not be
		// replayed. Windows accepts a log of 0xFF and starts a new one.
		lf := p.record(recLogFile).unnamed(attrData)
		size := (lf.DataSize + v.ClusterSize - 1) / v.ClusterSize * v.ClusterSize
		err = v.writeStream(dst, lf.Runs, size, func(_ uint64, b []byte) {
			for i := range b {
				b[i] = 0xFF
			}
		})
		if err != nil {
			return fmt.Errorf("reset $LogFile: %w", err)
		}
		if p.vssSector != nil {
			if err := writeAt(dst, p.vssSector, p.vssAt); err != nil {
				return fmt.Errorf("clear the shadow copy header: %w", err)
			}
		}
		// Changed file records, and their mirror.
		mirr := p.record(recMFTMirr).unnamed(attrData)
		for _, rec := range p.dirty {
			b, err := rec.encode()
			if err != nil {
				return err
			}
			applyFixups(b)
			if err := v.writeVirtual(dst, p.mftRuns, rec.Number*v.RecordSize, b); err != nil {
				return fmt.Errorf("write file record %d: %w", rec.Number, err)
			}
			if rec.Number < p.mirrorRecords {
				if err := v.writeVirtual(dst, mirr.Runs, rec.Number*v.RecordSize, b); err != nil {
					return fmt.Errorf("write $MFTMirr record %d: %w", rec.Number, err)
				}
			}
		}
	}
	// Boot sector and its backup in the last sector.
	if err := writeAt(dst, p.boot, 0); err != nil {
		return fmt.Errorf("write boot sector: %w", err)
	}
	if err := writeAt(dst, p.boot, p.NewTotalSectors*v.BytesPerSector); err != nil {
		return fmt.Errorf("write backup boot sector: %w", err)
	}
	return nil
}

// copyMoves copies the moved clusters from the source to the target.
func (p *Plan) copyMoves(dst io.WriterAt, progress func(done, total uint64)) error {
	v := p.v
	cs := v.ClusterSize
	total := p.MovedClusters * cs
	buf := make([]byte, min(max(cs, (1<<20)/cs*cs), total))
	var done uint64
	for _, m := range p.Moves {
		for k := uint64(0); k < m.Len*cs; {
			n := min(uint64(len(buf)), m.Len*cs-k)
			if err := v.readAt(buf[:n], m.From*cs+k); err != nil {
				return fmt.Errorf("read cluster %d: %w", m.From+k/cs, err)
			}
			if err := writeAt(dst, buf[:n], m.To*cs+k); err != nil {
				return fmt.Errorf("write cluster %d: %w", m.To+k/cs, err)
			}
			k += n
			done += n
			if progress != nil {
				progress(done, total)
			}
		}
	}
	return nil
}
