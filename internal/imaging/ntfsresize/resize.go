package ntfsresize

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"sort"
	"strings"
)

// Well-known file record numbers.
const (
	recMFT     = 0
	recMFTMirr = 1
	recLogFile = 2
	recVolume  = 3
	recBitmap  = 6
	recBadClus = 8
)

// Volume is an NTFS file system read from an image.
type Volume struct {
	src            io.ReaderAt
	boot           []byte
	BytesPerSector uint64
	ClusterSize    uint64
	TotalSectors   uint64
	Clusters       uint64
	RecordSize     uint64
	mftRuns        []Run
	mftRecords     uint64
	// records holds the system files and the records changed by Plan;
	// the others are read again when needed (volumes can have millions).
	records map[uint64]*Record
	bitmap  []byte
	// UsedClusters is the number of allocated clusters.
	UsedClusters uint64
}

// Open reads the metadata of the NTFS file system in src: boot sector,
// every file record and the cluster bitmap.
func Open(src io.ReaderAt) (*Volume, error) {
	v := &Volume{src: src, boot: make([]byte, 512)}
	if _, err := src.ReadAt(v.boot, 0); err != nil {
		return nil, fmt.Errorf("read boot sector: %w", err)
	}
	b := v.boot
	if string(b[3:11]) != "NTFS    " {
		return nil, errors.New("not an NTFS file system")
	}
	v.BytesPerSector = uint64(binary.LittleEndian.Uint16(b[0x0B:]))
	spc := uint64(b[0x0D])
	if spc > 0x80 {
		spc = 1 << (256 - spc)
	}
	v.ClusterSize = spc * v.BytesPerSector
	v.TotalSectors = binary.LittleEndian.Uint64(b[0x28:])
	if v.BytesPerSector < 256 || v.ClusterSize == 0 || v.TotalSectors == 0 {
		return nil, errors.New("implausible NTFS boot sector")
	}
	v.Clusters = v.TotalSectors * v.BytesPerSector / v.ClusterSize
	if c := int8(b[0x40]); c > 0 {
		v.RecordSize = uint64(c) * v.ClusterSize
	} else {
		v.RecordSize = 1 << uint(-c)
	}
	if v.RecordSize < 1024 || v.RecordSize > 64<<10 {
		return nil, fmt.Errorf("unsupported file record size %d", v.RecordSize)
	}
	mftLCN := binary.LittleEndian.Uint64(b[0x30:])
	if err := v.readMFTRuns(mftLCN); err != nil {
		return nil, err
	}
	if err := v.readSystemRecords(); err != nil {
		return nil, err
	}
	if err := v.readBitmap(); err != nil {
		return nil, err
	}
	return v, nil
}

func (v *Volume) readRecordAt(off uint64) ([]byte, error) {
	b := make([]byte, v.RecordSize)
	if _, err := v.src.ReadAt(b, int64(off)); err != nil { // #nosec G115 -- volume offsets fit in int64
		return nil, err
	}
	return b, nil
}

// readVirtual reads n bytes at byte offset off of a stream through runs.
func (v *Volume) readVirtual(runs []Run, off, n uint64) ([]byte, error) {
	out := make([]byte, n)
	for done := uint64(0); done < n; {
		vcn := (off + done) / v.ClusterSize
		in := (off + done) % v.ClusterSize
		var r *Run
		for i := range runs {
			if vcn >= runs[i].VCN && vcn < runs[i].VCN+runs[i].Len {
				r = &runs[i]
			}
		}
		if r == nil {
			return nil, fmt.Errorf("cluster %d of the stream is not mapped", vcn)
		}
		avail := (r.VCN+r.Len-vcn)*v.ClusterSize - in
		chunk := min(avail, n-done)
		if !r.Sparse {
			pos := (r.LCN+vcn-r.VCN)*v.ClusterSize + in
			if _, err := v.src.ReadAt(out[done:done+chunk], int64(pos)); err != nil { // #nosec G115 -- volume offsets fit in int64
				return nil, err
			}
		}
		done += chunk
	}
	return out, nil
}

// readMFTRuns collects the extents of $MFT, also from extension records
// when $MFT is so fragmented that it has an attribute list.
func (v *Volume) readMFTRuns(mftLCN uint64) error {
	raw, err := v.readRecordAt(mftLCN * v.ClusterSize)
	if err != nil {
		return fmt.Errorf("read $MFT record: %w", err)
	}
	if err := removeFixups(raw); err != nil {
		return fmt.Errorf("$MFT record: %w", err)
	}
	r0, err := parseRecord(0, raw)
	if err != nil {
		return err
	}
	for _, a := range r0.Attrs {
		if a.Type == attrData && a.Name == "" {
			v.mftRuns = append(v.mftRuns, a.Runs...)
		}
	}
	if al := r0.unnamed(attrAttributeList); al != nil {
		var list []byte
		if al.NonResident {
			// Read with the runs known so far (the list lives in the
			// first extent in practice).
			v.mftRuns = mergeRuns(v.mftRuns)
			if list, err = v.readVirtual(al.Runs, 0, al.DataSize); err != nil {
				return fmt.Errorf("$MFT attribute list: %w", err)
			}
		} else {
			list = al.residentValue()
		}
		for off := 0; off+0x1A <= len(list); {
			t := binary.LittleEndian.Uint32(list[off:])
			l := int(binary.LittleEndian.Uint16(list[off+4:]))
			nl := int(list[off+6])
			rec := binary.LittleEndian.Uint64(list[off+0x10:]) & 0xFFFFFFFFFFFF
			if l == 0 {
				break
			}
			if t == attrData && nl == 0 && rec != 0 {
				ext, err := v.readVirtual(v.mftRuns, rec*v.RecordSize, v.RecordSize)
				if err != nil {
					return fmt.Errorf("$MFT extension record %d: %w", rec, err)
				}
				if err := removeFixups(ext); err != nil {
					return fmt.Errorf("$MFT extension record %d: %w", rec, err)
				}
				er, err := parseRecord(rec, ext)
				if err != nil {
					return err
				}
				if a := er.unnamed(attrData); a != nil {
					v.mftRuns = append(v.mftRuns, a.Runs...)
				}
			}
			off += l
		}
	}
	sort.Slice(v.mftRuns, func(i, j int) bool { return v.mftRuns[i].VCN < v.mftRuns[j].VCN })
	if len(v.mftRuns) == 0 {
		return errors.New("$MFT has no data")
	}
	return nil
}

// systemRecords are kept in memory: Plan changes them.
var systemRecords = []uint64{recMFT, recMFTMirr, recLogFile, recVolume, recBitmap, recBadClus}

// scanRecords calls fn for every file record in use, in order. Records
// already held in v.records are passed as they are (with changes).
func (v *Volume) scanRecords(fn func(*Record) error) error {
	const batch = 256
	for start := uint64(0); start < v.mftRecords; start += batch {
		cnt := min(batch, v.mftRecords-start)
		buf, err := v.readVirtual(v.mftRuns, start*v.RecordSize, cnt*v.RecordSize)
		if err != nil {
			return fmt.Errorf("read file records: %w", err)
		}
		for i := uint64(0); i < cnt; i++ {
			num := start + i
			if r, ok := v.records[num]; ok {
				if err := fn(r); err != nil {
					return err
				}
				continue
			}
			b := buf[i*v.RecordSize : (i+1)*v.RecordSize]
			if string(b[0:4]) != "FILE" || binary.LittleEndian.Uint16(b[0x16:])&1 == 0 {
				continue
			}
			if err := removeFixups(b); err != nil {
				return fmt.Errorf("file record %d: %w", num, err)
			}
			r, err := parseRecord(num, b)
			if err != nil {
				return err
			}
			if err := fn(r); err != nil {
				return err
			}
		}
	}
	return nil
}

// readSystemRecords loads the system files the resize changes.
func (v *Volume) readSystemRecords() error {
	var mftSize uint64
	for _, r := range v.mftRuns {
		mftSize = max(mftSize, (r.VCN+r.Len)*v.ClusterSize)
	}
	v.mftRecords = mftSize / v.RecordSize
	v.records = map[uint64]*Record{}
	for _, num := range systemRecords {
		if num >= v.mftRecords {
			return fmt.Errorf("system file record %d missing", num)
		}
		b, err := v.readVirtual(v.mftRuns, num*v.RecordSize, v.RecordSize)
		if err != nil {
			return fmt.Errorf("read file record %d: %w", num, err)
		}
		if err := removeFixups(b); err != nil {
			return fmt.Errorf("file record %d: %w", num, err)
		}
		r, err := parseRecord(num, b)
		if err != nil {
			return err
		}
		if !r.InUse {
			return fmt.Errorf("system file record %d not in use", num)
		}
		v.records[num] = r
	}
	return nil
}

func (v *Volume) readBitmap() error {
	a := v.records[recBitmap].unnamed(attrData)
	if a == nil || !a.NonResident {
		return errors.New("$Bitmap has no data")
	}
	b, err := v.readVirtual(a.Runs, 0, a.DataSize)
	if err != nil {
		return fmt.Errorf("read $Bitmap: %w", err)
	}
	need := (v.Clusters + 7) / 8
	if uint64(len(b)) < need {
		return fmt.Errorf("$Bitmap holds %d bytes, the volume needs %d", len(b), need)
	}
	v.bitmap = b
	for c := uint64(0); c < v.Clusters; c += 8 {
		v.UsedClusters += uint64(bits.OnesCount8(b[c/8] & lowBits(v.Clusters-c)))
	}
	return nil
}

func lowBits(n uint64) byte {
	if n >= 8 {
		return 0xFF
	}
	return byte(1<<n) - 1
}

func (v *Volume) used(c uint64) bool { return v.bitmap[c/8]&(1<<(c%8)) != 0 }

func (v *Volume) setUsed(c uint64, on bool) {
	if on {
		v.bitmap[c/8] |= 1 << (c % 8)
	} else {
		v.bitmap[c/8] &^= 1 << (c % 8)
	}
}

// MinimumSize is the smallest volume (partition) size Plan can shrink to:
// every allocated cluster plus a little room, and the backup boot sector.
func (v *Volume) MinimumSize() uint64 {
	slack := max(v.UsedClusters/100, 16)
	return (v.UsedClusters+slack)*v.ClusterSize + v.BytesPerSector
}

// LastUsedCluster is the highest allocated cluster (0 if none).
func (v *Volume) LastUsedCluster() uint64 {
	for c := v.Clusters; c > 0; c-- {
		if v.used(c - 1) {
			return c - 1
		}
	}
	return 0
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
	Moves           []Move
	MovedClusters   uint64
	// Orphaned counts allocated clusters behind the new end that no file
	// refers to; they are dropped (chkdsk would free them anyway).
	Orphaned uint64
	dirty    []*Record
}

// extent is a range of free clusters.
type extent struct{ start, len uint64 }

// freeExtents lists the free cluster ranges below end.
func (v *Volume) freeExtents(end uint64) []extent {
	var out []extent
	for c := uint64(0); c < end; {
		if v.used(c) {
			c++
			continue
		}
		s := c
		for c < end && !v.used(c) {
			c++
		}
		out = append(out, extent{s, c - s})
	}
	return out
}

// allocator hands out free clusters below the new end: one extent that
// fits (the smallest), otherwise the largest extents in turn.
type allocator struct{ free []extent }

func (a *allocator) take(n uint64) ([]extent, error) {
	best := -1
	for i, e := range a.free {
		if e.len >= n && (best < 0 || e.len < a.free[best].len) {
			best = i
		}
	}
	if best >= 0 {
		e := extent{a.free[best].start, n}
		a.free[best].start += n
		a.free[best].len -= n
		return []extent{e}, nil
	}
	var out []extent
	for n > 0 {
		big := -1
		for i, e := range a.free {
			if e.len > 0 && (big < 0 || e.len > a.free[big].len) {
				big = i
			}
		}
		if big < 0 {
			return nil, errors.New("not enough free space before the new end of the volume")
		}
		k := min(n, a.free[big].len)
		out = append(out, extent{a.free[big].start, k})
		a.free[big].start += k
		a.free[big].len -= k
		n -= k
	}
	return out, nil
}

// Plan works out how to shrink the volume to a partition of newSize
// bytes. It changes the in-memory metadata of v; call it once.
func (v *Volume) Plan(newSize uint64) (*Plan, error) {
	if newSize < 2*v.BytesPerSector {
		return nil, errors.New("new size too small")
	}
	p := &Plan{v: v, NewSize: newSize, NewTotalSectors: newSize/v.BytesPerSector - 1}
	p.NewClusters = p.NewTotalSectors * v.BytesPerSector / v.ClusterSize
	if p.NewClusters >= v.Clusters {
		return nil, fmt.Errorf("the new size (%d clusters) is not smaller than the volume (%d clusters)", p.NewClusters, v.Clusters)
	}
	if vi := v.records[recVolume].unnamed(attrVolumeInfo); vi == nil || len(vi.residentValue()) < 12 || vi.residentValue()[8] < 3 {
		return nil, errors.New("only NTFS 3.x (Windows 2000 and later) can be shrunk")
	}
	// $BadClus:$Bad must not list real bad clusters.
	bad := v.records[recBadClus].named(attrData, "$Bad")
	if bad == nil || !bad.NonResident {
		return nil, errors.New("$BadClus:$Bad not found")
	}
	for _, r := range bad.Runs {
		if !r.Sparse {
			return nil, errors.New("the volume has bad clusters; run chkdsk on the original disk instead")
		}
	}

	alloc := &allocator{free: v.freeExtents(p.NewClusters)}
	var free uint64
	for _, e := range alloc.free {
		free += e.len
	}
	var behind uint64
	for c := p.NewClusters; c < v.Clusters; c++ {
		if v.used(c) {
			behind++
		}
	}
	if behind > free {
		return nil, fmt.Errorf("the data does not fit: %d clusters behind the new end, %d free before it", behind, free)
	}

	var marked uint64 // clusters behind the end that belong to a file
	mftExt := map[uint64]*Record{}
	err := v.scanRecords(func(rec *Record) error {
		if rec.Extension && rec.Base == recMFT {
			mftExt[rec.Number] = rec
		}
		for _, a := range rec.Attrs {
			if !a.NonResident || a == bad {
				continue
			}
			var out []Run
			changed := false
			for _, r := range a.Runs {
				if r.Sparse || r.LCN+r.Len <= p.NewClusters {
					out = append(out, r)
					continue
				}
				changed = true
				keep := uint64(0)
				if r.LCN < p.NewClusters {
					keep = p.NewClusters - r.LCN
					out = append(out, Run{VCN: r.VCN, LCN: r.LCN, Len: keep})
				}
				exts, err := alloc.take(r.Len - keep)
				if err != nil {
					return err
				}
				vcn, from := r.VCN+keep, r.LCN+keep
				for _, e := range exts {
					p.Moves = append(p.Moves, Move{From: from, To: e.start, Len: e.len})
					out = append(out, Run{VCN: vcn, LCN: e.start, Len: e.len})
					for c := uint64(0); c < e.len; c++ {
						v.setUsed(e.start+c, true)
					}
					marked += e.len
					p.MovedClusters += e.len
					vcn += e.len
					from += e.len
				}
			}
			if changed {
				a.Runs = mergeRuns(out)
				a.dirty = true
				rec.dirty = true
			}
		}
		if rec.dirty {
			if _, err := rec.encode(); err != nil {
				return err
			}
			v.records[rec.Number] = rec
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	p.Orphaned = behind - marked

	// $MFT runs as they are after the moves (all segments, when a
	// fragmented $MFT has extension records).
	if mft := v.records[recMFT].unnamed(attrData); mft.dirty || anyDirty(mftExt) {
		runs := append([]Run(nil), mft.Runs...)
		for _, e := range mftExt {
			if d := e.unnamed(attrData); d != nil {
				runs = append(runs, d.Runs...)
			}
		}
		sort.Slice(runs, func(i, j int) bool { return runs[i].VCN < runs[j].VCN })
		v.mftRuns = runs
	}

	// The runs of $MFT must still describe exactly $MFT: as many clusters
	// as before, and its first cluster where record 0 says.
	var mftClusters uint64
	for _, r := range v.mftRuns {
		mftClusters += r.Len
	}
	if mftClusters*v.ClusterSize/v.RecordSize != v.mftRecords {
		return nil, fmt.Errorf("internal error: $MFT would have %d clusters, it has %d records of %d bytes", mftClusters, v.mftRecords, v.RecordSize)
	}

	// $Bitmap: shorter, covering the new cluster count.
	bm := v.records[recBitmap].unnamed(attrData)
	newBitmapLen := ((p.NewClusters+7)/8 + 7) / 8 * 8
	bm.DataSize, bm.InitializedSize = newBitmapLen, newBitmapLen
	bm.dirty = true
	v.records[recBitmap].dirty = true
	for c := p.NewClusters; c < uint64(len(v.bitmap))*8; c++ {
		v.setUsed(c, false)
	}
	v.bitmap = v.bitmap[:newBitmapLen]
	// Windows marks the bits past the last cluster as used, so they are
	// never allocated; chkdsk calls the bitmap incorrect otherwise.
	for c := p.NewClusters; c < newBitmapLen*8; c++ {
		v.setUsed(c, true)
	}

	// $BadClus:$Bad: a sparse stream as long as the volume.
	bad.Runs = []Run{{VCN: 0, Len: p.NewClusters, Sparse: true}}
	bad.LastVCN = p.NewClusters - 1
	bad.AllocSize = p.NewClusters * v.ClusterSize
	bad.DataSize = bad.AllocSize
	bad.InitializedSize = min(bad.InitializedSize, bad.DataSize)
	bad.dirty = true
	v.records[recBadClus].dirty = true

	// Mark the volume dirty: Windows runs chkdsk once at the next start.
	vi := v.records[recVolume].unnamed(attrVolumeInfo)
	val := vi.residentValue()
	binary.LittleEndian.PutUint16(val[0x0A:], binary.LittleEndian.Uint16(val[0x0A:])|0x0001)
	v.records[recVolume].dirty = true

	for _, num := range sortedKeys(v.records) {
		rec := v.records[num]
		if rec.dirty {
			if _, err := rec.encode(); err != nil {
				return nil, err
			}
			p.dirty = append(p.dirty, rec)
		}
	}
	return p, nil
}

func anyDirty(m map[uint64]*Record) bool {
	for _, r := range m {
		if r.dirty {
			return true
		}
	}
	return false
}

func sortedKeys(m map[uint64]*Record) []uint64 {
	out := make([]uint64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// writeVirtual writes data at byte offset off of a stream through runs.
func (v *Volume) writeVirtual(dst io.WriterAt, runs []Run, off uint64, data []byte) error {
	for done := uint64(0); done < uint64(len(data)); {
		vcn := (off + done) / v.ClusterSize
		in := (off + done) % v.ClusterSize
		lcn, err := lcnOf(runs, vcn)
		if err != nil {
			return err
		}
		chunk := min(v.ClusterSize-in, uint64(len(data))-done)
		if _, err := dst.WriteAt(data[done:done+chunk], int64(lcn*v.ClusterSize+in)); err != nil { // #nosec G115 -- volume offsets fit in int64
			return err
		}
		done += chunk
	}
	return nil
}

// Apply writes the shrunk file system to dst, a writer over the target
// partition that already holds the clusters before the new end (as the
// image had them). It copies the moved clusters from the source, then
// writes the changed metadata.
func (p *Plan) Apply(dst io.WriterAt, progress func(done, total uint64)) error {
	v := p.v
	buf := make([]byte, 1<<20)
	var done uint64
	total := p.MovedClusters * v.ClusterSize
	for _, m := range p.Moves {
		for k := uint64(0); k < m.Len*v.ClusterSize; {
			n := min(uint64(len(buf)), m.Len*v.ClusterSize-k)
			if _, err := v.src.ReadAt(buf[:n], int64(m.From*v.ClusterSize+k)); err != nil { // #nosec G115 -- volume offsets fit in int64
				return fmt.Errorf("read cluster %d: %w", m.From+k/v.ClusterSize, err)
			}
			if _, err := dst.WriteAt(buf[:n], int64(m.To*v.ClusterSize+k)); err != nil { // #nosec G115 -- volume offsets fit in int64
				return fmt.Errorf("write cluster %d: %w", m.To+k/v.ClusterSize, err)
			}
			k += n
			done += n
			if progress != nil {
				progress(done, total)
			}
		}
	}

	// $Bitmap contents (rest of its allocation zeroed).
	bm := v.records[recBitmap].unnamed(attrData)
	data := make([]byte, bm.AllocSize)
	copy(data, v.bitmap)
	if err := v.writeVirtual(dst, bm.Runs, 0, data); err != nil {
		return fmt.Errorf("write $Bitmap: %w", err)
	}

	// Reset $LogFile: clusters moved, so the journal must not be replayed.
	lf := v.records[recLogFile].unnamed(attrData)
	if lf == nil || !lf.NonResident {
		return errors.New("$LogFile has no data")
	}
	ff := make([]byte, lf.DataSize)
	for i := range ff {
		ff[i] = 0xFF
	}
	if err := v.writeVirtual(dst, lf.Runs, 0, ff); err != nil {
		return fmt.Errorf("reset $LogFile: %w", err)
	}

	// Changed file records, and the mirror of the first four.
	mirr := v.records[recMFTMirr].unnamed(attrData)
	for _, rec := range p.dirty {
		b, err := rec.encode()
		if err != nil {
			return err
		}
		applyFixups(b)
		if err := v.writeVirtual(dst, v.mftRuns, rec.Number*v.RecordSize, b); err != nil {
			return fmt.Errorf("write file record %d: %w", rec.Number, err)
		}
		if rec.Number < 4 && mirr != nil {
			if err := v.writeVirtual(dst, mirr.Runs, rec.Number*v.RecordSize, b); err != nil {
				return fmt.Errorf("write $MFTMirr record %d: %w", rec.Number, err)
			}
		}
	}

	// Boot sector and its backup in the last sector.
	boot := make([]byte, v.BytesPerSector)
	if _, err := v.src.ReadAt(boot, 0); err != nil {
		return err
	}
	binary.LittleEndian.PutUint64(boot[0x28:], p.NewTotalSectors)
	mftLCN, err := lcnOf(v.mftRuns, 0)
	if err != nil {
		return err
	}
	binary.LittleEndian.PutUint64(boot[0x30:], mftLCN)
	if mirr != nil {
		if l, err := lcnOf(mirr.Runs, 0); err == nil {
			binary.LittleEndian.PutUint64(boot[0x38:], l)
		}
	}
	if _, err := dst.WriteAt(boot, 0); err != nil {
		return fmt.Errorf("write boot sector: %w", err)
	}
	if _, err := dst.WriteAt(boot, int64(p.NewTotalSectors*v.BytesPerSector)); err != nil { // #nosec G115 -- volume offsets fit in int64
		return fmt.Errorf("write backup boot sector: %w", err)
	}
	return nil
}

// Check reads every file record and reports inconsistencies: runs that
// reach past the end of the volume, clusters in use by a file but free in
// $Bitmap, and clusters used by two files. A healthy volume returns none.
// max limits the number of problems reported.
func (v *Volume) Check(max int) ([]string, error) {
	var out []string
	add := func(f string, a ...any) {
		if len(out) < max {
			out = append(out, fmt.Sprintf(f, a...))
		}
	}
	owner := map[uint64]uint64{}
	err := v.scanRecords(func(rec *Record) error {
		for _, a := range rec.Attrs {
			if !a.NonResident || (rec.Number == recBadClus && a.Name == "$Bad") {
				continue
			}
			for _, r := range a.Runs {
				if r.Sparse {
					continue
				}
				if r.LCN+r.Len > v.Clusters {
					add("record %d attribute %#x %q: run %d+%d ends past the volume (%d clusters)", rec.Number, a.Type, a.Name, r.LCN, r.Len, v.Clusters)
					continue
				}
				for c := r.LCN; c < r.LCN+r.Len; c++ {
					if !v.used(c) {
						add("record %d attribute %#x: cluster %d free in $Bitmap", rec.Number, a.Type, c)
						break
					}
					if o, ok := owner[c]; ok && o != rec.Number {
						add("cluster %d used by records %d and %d", c, o, rec.Number)
						break
					}
					owner[c] = rec.Number
				}
			}
		}
		return nil
	})
	return out, err
}

// DumpRecord describes the non-resident attributes of one file record (a
// diagnostic): header fields, mapping pairs as stored and decoded.
func (v *Volume) DumpRecord(num uint64) (string, error) {
	b, err := v.readVirtual(v.mftRuns, num*v.RecordSize, v.RecordSize)
	if err != nil {
		return "", err
	}
	if err := removeFixups(b); err != nil {
		return "", err
	}
	r, err := parseRecord(num, b)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "record %d in use %v extension %v base %d, %d attributes\n", num, r.InUse, r.Extension, r.Base, len(r.Attrs))
	for _, a := range r.Attrs {
		if !a.NonResident {
			fmt.Fprintf(&sb, "  attr %#x %q resident, %d bytes\n", a.Type, a.Name, len(a.raw))
			continue
		}
		flags := binary.LittleEndian.Uint16(a.raw[0x0C:])
		mp := int(binary.LittleEndian.Uint16(a.raw[0x20:]))
		cu := binary.LittleEndian.Uint16(a.raw[0x22:])
		fmt.Fprintf(&sb, "  attr %#x %q flags %#x len %d mp@%#x cu %d vcn %d-%d alloc %d data %d init %d\n",
			a.Type, a.Name, flags, len(a.raw), mp, cu, a.StartVCN, a.LastVCN, a.AllocSize, a.DataSize, a.InitializedSize)
		if mp < len(a.raw) {
			fmt.Fprintf(&sb, "    pairs % x\n", a.raw[mp:])
		}
		var total uint64
		for _, run := range a.Runs {
			total += run.Len
		}
		fmt.Fprintf(&sb, "    %d runs, %d clusters: %v\n", len(a.Runs), total, a.Runs)
	}
	return sb.String(), nil
}

// BitmapReport compares $Bitmap with the clusters files actually use (a
// diagnostic): bits set without an owner, owned clusters missing, the
// bits past the last cluster, and the size of $Bitmap.
func (v *Volume) BitmapReport() (string, error) {
	owned := make([]byte, len(v.bitmap))
	err := v.scanRecords(func(rec *Record) error {
		for _, a := range rec.Attrs {
			if !a.NonResident || (rec.Number == recBadClus && a.Name == "$Bad") {
				continue
			}
			for _, r := range a.Runs {
				if r.Sparse {
					continue
				}
				for c := r.LCN; c < r.LCN+r.Len && c/8 < uint64(len(owned)); c++ {
					owned[c/8] |= 1 << (c % 8)
				}
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	var unowned, missing []uint64
	var nUnowned, nMissing uint64
	for c := uint64(0); c < v.Clusters; c++ {
		u, o := v.used(c), owned[c/8]&(1<<(c%8)) != 0
		switch {
		case u && !o:
			nUnowned++
			if len(unowned) < 10 {
				unowned = append(unowned, c)
			}
		case o && !u:
			nMissing++
			if len(missing) < 10 {
				missing = append(missing, c)
			}
		}
	}
	var tail []int
	for c := v.Clusters; c < uint64(len(v.bitmap))*8 && len(tail) < 72; c++ {
		if v.used(c) {
			tail = append(tail, 1)
		} else {
			tail = append(tail, 0)
		}
	}
	bm := v.records[recBitmap].unnamed(attrData)
	return fmt.Sprintf("bitmap: %d bytes read, data size %d, alloc %d, init %d, need %d bytes for %d clusters\n"+
		"used but no owner: %d %v\nowned but free: %d %v\nbits past the end (first 72): %v\n",
		len(v.bitmap), bm.DataSize, bm.AllocSize, bm.InitializedSize, (v.Clusters+7)/8, v.Clusters,
		nUnowned, unowned, nMissing, missing, tail), nil
}
