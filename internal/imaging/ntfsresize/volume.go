package ntfsresize

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
)

// Well-known file record numbers.
const (
	recMFT     = 0
	recMFTMirr = 1
	recLogFile = 2
	recVolume  = 3
	recRoot    = 5
	recBitmap  = 6
	recBoot    = 7
	recBadClus = 8
)

// Limits of what Open accepts. Every size read from disk is checked
// against them and against the volume before memory is allocated for it,
// so a crafted image cannot make Open allocate more than the volume holds.
const (
	maxClusterSize = 2 << 20 // the largest cluster Windows formats
	// maxVolumeBytes keeps every byte offset of the volume an int64.
	maxVolumeBytes = 1 << 62
	// maxAttrList is the largest attribute list Windows writes.
	maxAttrList = 256 << 10
)

// ErrNotNTFS is returned by Open for data without an NTFS boot sector.
var ErrNotNTFS = errors.New("not an NTFS file system")

// Volume is an NTFS file system read from an image.
type Volume struct {
	src            io.ReaderAt
	boot           []byte
	BytesPerSector uint64
	ClusterSize    uint64
	TotalSectors   uint64
	Clusters       uint64
	RecordSize     uint64
	mftLCN         uint64
	mftRuns        []Run
	// mftClusters is the length of $MFT's runs; mftRecords the number of
	// records in its initialized part, the ones that are scanned.
	mftClusters uint64
	mftRecords  uint64
	// records holds the system files; the others are read again when
	// needed (volumes can have millions). Nothing changes them: Plan
	// works on copies, so one Volume can be planned several times.
	records map[uint64]*Record
	bitmap  []byte
	// UsedClusters is the number of allocated clusters.
	UsedClusters uint64
}

// sizer is implemented by readers that know their length (the partition
// readers of the restore, bytes.Reader, io.SectionReader).
type sizer interface{ Size() int64 }

// Open reads the metadata of the NTFS file system in src: the boot
// sector, the system file records and the cluster bitmap. The other file
// records are read by Plan, MinimumSize and Check.
func Open(src io.ReaderAt) (*Volume, error) {
	v := &Volume{src: src, boot: make([]byte, 512)}
	if err := v.readAt(v.boot, 0); err != nil {
		return nil, fmt.Errorf("read boot sector: %w", err)
	}
	if err := v.parseBoot(src); err != nil {
		return nil, err
	}
	if err := v.readMFTRuns(); err != nil {
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

// parseBoot reads the geometry from the boot sector and checks it.
func (v *Volume) parseBoot(src io.ReaderAt) error {
	b := v.boot
	if string(b[3:11]) != "NTFS    " {
		return ErrNotNTFS
	}
	bps := uint64(binary.LittleEndian.Uint16(b[0x0B:]))
	if bps < 512 || bps > 4096 || bps&(bps-1) != 0 {
		return fmt.Errorf("implausible NTFS boot sector: %d bytes per sector", bps)
	}
	// Sectors per cluster: a power of two up to 128, or 2^(256-x) for
	// larger clusters.
	var cs uint64
	switch spc := uint64(b[0x0D]); {
	case spc == 0 || (spc <= 0x80 && spc&(spc-1) != 0):
		return errors.New("implausible NTFS boot sector: sectors per cluster")
	case spc <= 0x80:
		cs = spc * bps
	case 256-spc > 12:
		return errors.New("implausible NTFS boot sector: cluster size")
	default:
		cs = bps << (256 - spc)
	}
	if cs > maxClusterSize {
		return fmt.Errorf("unsupported cluster size %d", cs)
	}
	ts := binary.LittleEndian.Uint64(b[0x28:])
	if ts == 0 || ts > maxVolumeBytes/bps {
		return errors.New("implausible NTFS boot sector: sector count")
	}
	vol := ts * bps
	if s, ok := src.(sizer); ok {
		if n := s.Size(); n > 0 {
			if vol > uint64(n) {
				return fmt.Errorf("the file system (%d bytes) is larger than its partition (%d bytes)", vol, n)
			}
		}
	}
	clusters := vol / cs
	if clusters < 16 {
		return errors.New("implausible NTFS boot sector: volume too small")
	}
	// File record size: clusters per record, or 2^(256-x) bytes.
	var rs uint64
	switch c := uint64(b[0x40]); {
	case c == 0:
		return errors.New("implausible NTFS boot sector: file record size")
	case c < 0x80:
		rs = c * cs
	case 256-c > 16:
		return errors.New("implausible NTFS boot sector: file record size")
	default:
		rs = 1 << (256 - c)
	}
	// Records are written whole: they must be at least a sector long, or
	// writing one would need a read-modify-write of the sector.
	if rs < 1024 || rs > maxRecordSize || rs&(rs-1) != 0 || rs < bps {
		return fmt.Errorf("unsupported file record size %d", rs)
	}
	mftLCN := binary.LittleEndian.Uint64(b[0x30:])
	if mftLCN >= clusters || rs > vol || mftLCN*cs > vol-rs {
		return errors.New("implausible NTFS boot sector: $MFT position")
	}
	v.BytesPerSector, v.ClusterSize, v.TotalSectors, v.Clusters, v.RecordSize, v.mftLCN = bps, cs, ts, clusters, rs, mftLCN
	return nil
}

// off64 converts a byte offset for ReadAt/WriteAt.
func off64(off uint64) (int64, error) {
	if off > math.MaxInt64 {
		return 0, fmt.Errorf("offset %d out of range", off)
	}
	return int64(off), nil
}

// readAt fills p from the source at off. A reader that returns io.EOF
// together with the last bytes is fine.
func (v *Volume) readAt(p []byte, off uint64) error {
	o, err := off64(off)
	if err != nil {
		return err
	}
	n, err := v.src.ReadAt(p, o)
	if n == len(p) {
		return nil
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// volumeBytes is the size of the volume's clusters.
func (v *Volume) volumeBytes() uint64 { return v.Clusters * v.ClusterSize }

// readVirtual reads n bytes at byte offset off of a stream through runs.
func (v *Volume) readVirtual(runs []Run, off, n uint64) ([]byte, error) {
	// No stream is larger than the volume; checking before allocating
	// keeps crafted sizes from exhausting memory.
	if n > v.volumeBytes() {
		return nil, fmt.Errorf("stream read of %d bytes is larger than the volume", n)
	}
	out := make([]byte, n)
	return out, v.readVirtualInto(out, runs, off)
}

// readVirtualInto fills out from byte offset off of a stream through runs.
// Sparse parts read as zeros; runs outside the volume are refused.
func (v *Volume) readVirtualInto(out []byte, runs []Run, off uint64) error {
	n := uint64(len(out))
	if off > maxVolumeBytes || n > maxVolumeBytes-off {
		return fmt.Errorf("stream offset %d out of range", off)
	}
	cs := v.ClusterSize
	for done := uint64(0); done < n; {
		pos := off + done
		vcn, in := pos/cs, pos%cs
		r, ok := findRun(runs, vcn)
		if !ok {
			return fmt.Errorf("cluster %d of the stream is not mapped", vcn)
		}
		// Clusters left in this run, capped so the byte count cannot
		// overflow for long (sparse) runs.
		left := min(r.VCN+r.Len-vcn, (n-done)/cs+1)
		chunk := min(left*cs-in, n-done)
		if r.Sparse {
			clear(out[done : done+chunk])
		} else {
			if !inVolume(r, v.Clusters) {
				return fmt.Errorf("run at cluster %d+%d lies outside the volume", r.LCN, r.Len)
			}
			if err := v.readAt(out[done:done+chunk], (r.LCN+vcn-r.VCN)*cs+in); err != nil {
				return err
			}
		}
		done += chunk
	}
	return nil
}

// readRecord reads, checks and parses file record num.
func (v *Volume) readRecord(num uint64) (*Record, error) {
	if num >= v.mftRecords {
		return nil, fmt.Errorf("file record %d is beyond $MFT (%d records)", num, v.mftRecords)
	}
	b, err := v.readVirtual(v.mftRuns, num*v.RecordSize, v.RecordSize)
	if err != nil {
		return nil, fmt.Errorf("read file record %d: %w", num, err)
	}
	if err := removeFixups(b); err != nil {
		return nil, fmt.Errorf("file record %d: %w", num, err)
	}
	return parseRecord(num, b)
}

// dataSegments returns the unnamed $DATA attributes of a record: the
// segments of a stream that is split over extension records.
func dataSegments(r *Record) []*Attribute {
	var out []*Attribute
	for _, a := range r.Attrs {
		if a.Type == attrData && a.Name == "" {
			out = append(out, a)
		}
	}
	return out
}

// joinSegments puts the segments of a stream in VCN order and returns
// their runs. The segments must join up without gaps or overlaps and must
// not be sparse (only used for $MFT, which never is).
func joinSegments(segs []*Attribute, clusters uint64) ([]Run, uint64, error) {
	segs = append([]*Attribute(nil), segs...)
	sort.SliceStable(segs, func(i, j int) bool { return segs[i].StartVCN < segs[j].StartVCN })
	var runs []Run
	var next uint64
	for _, s := range segs {
		if !s.NonResident || s.StartVCN != next {
			return nil, 0, errors.New("the segments of $MFT do not join up")
		}
		for _, r := range s.Runs {
			if r.Sparse || r.VCN != next || !inVolume(r, clusters) {
				return nil, 0, errors.New("$MFT has a sparse run or one outside the volume")
			}
			runs = append(runs, r)
			next += r.Len
		}
		if next == 0 || s.LastVCN != next-1 {
			return nil, 0, errors.New("the segments of $MFT do not match their cluster ranges")
		}
	}
	return runs, next, nil
}

// readMFTRuns collects the extents of $MFT, also from extension records
// when $MFT is so fragmented that it has an attribute list.
func (v *Volume) readMFTRuns() error {
	raw := make([]byte, v.RecordSize)
	if err := v.readAt(raw, v.mftLCN*v.ClusterSize); err != nil {
		return fmt.Errorf("read $MFT record: %w", err)
	}
	if err := removeFixups(raw); err != nil {
		return fmt.Errorf("$MFT record: %w", err)
	}
	r0, err := parseRecord(recMFT, raw)
	if err != nil {
		return err
	}
	if !r0.InUse || r0.Extension {
		return errors.New("$MFT record not in use")
	}
	segs := dataSegments(r0)
	if al := r0.unnamed(attrAttributeList); al != nil {
		// The extension records are read through the runs of record 0
		// (Windows keeps them in the first extent).
		known, _, err := joinSegments(segs, v.Clusters)
		if err != nil {
			return err
		}
		v.mftRuns = known
		list, err := v.attrListBytes(al)
		if err != nil {
			return fmt.Errorf("$MFT attribute list: %w", err)
		}
		seen := map[uint64]bool{}
		err = walkAttrList(list, func(t uint32, nameLen int, _ uint64, ref uint64) error {
			rec := ref & recordNumberMask
			// Two entries can point to the same extension record; it is
			// read once, with all its segments.
			if t != attrData || nameLen != 0 || rec == recMFT || seen[rec] {
				return nil
			}
			seen[rec] = true
			if rec > maxVolumeBytes/v.RecordSize {
				return fmt.Errorf("$MFT extension record %d out of range", rec)
			}
			ext, err := v.readVirtual(known, rec*v.RecordSize, v.RecordSize)
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
			if !er.InUse || !er.Extension || er.Base != recMFT {
				return fmt.Errorf("$MFT extension record %d does not belong to $MFT", rec)
			}
			segs = append(segs, dataSegments(er)...)
			return nil
		})
		if err != nil {
			return err
		}
	}
	if len(segs) == 0 {
		return errors.New("$MFT has no data")
	}
	runs, total, err := joinSegments(segs, v.Clusters)
	if err != nil {
		return err
	}
	// Runs may name the same clusters twice (the scan refuses that), but
	// $MFT can never be larger than the volume: this bounds every scan.
	if total > v.Clusters {
		return errors.New("$MFT is larger than the volume")
	}
	var first *Attribute
	for _, s := range segs {
		if s.StartVCN == 0 {
			first = s
		}
	}
	if runs[0].LCN != v.mftLCN || runs[0].Len*v.ClusterSize < v.RecordSize {
		return errors.New("$MFT does not start where the boot sector says")
	}
	if first.AllocSize > total*v.ClusterSize || first.DataSize > first.AllocSize || first.InitializedSize > first.DataSize {
		return errors.New("implausible $MFT sizes")
	}
	v.mftRuns, v.mftClusters = mergeRuns(runs), total
	// Records past the initialized size are not part of the file system,
	// whatever bytes the clusters hold (ntfs-3g does the same).
	v.mftRecords = first.InitializedSize / v.RecordSize
	if v.mftRecords <= recBadClus {
		return errors.New("$MFT is too small")
	}
	return nil
}

// attrListBytes returns the contents of an attribute list.
func (v *Volume) attrListBytes(al *Attribute) ([]byte, error) {
	if !al.NonResident {
		return al.residentValue(), nil
	}
	if al.DataSize > maxAttrList {
		return nil, fmt.Errorf("attribute list of %d bytes", al.DataSize)
	}
	return v.readVirtual(al.Runs, 0, al.DataSize)
}

// walkAttrList calls fn for each entry of an attribute list with the
// attribute type, its name length, its starting VCN and the reference of
// the record that holds it.
func walkAttrList(list []byte, fn func(t uint32, nameLen int, vcn, ref uint64) error) error {
	for off := 0; off+0x1A <= len(list); {
		l := int(binary.LittleEndian.Uint16(list[off+4:]))
		if l == 0 {
			break
		}
		if l < 0x1A || l > len(list)-off {
			return errors.New("invalid attribute list entry")
		}
		e := list[off : off+l]
		if err := fn(binary.LittleEndian.Uint32(e), int(e[6]), binary.LittleEndian.Uint64(e[8:]), binary.LittleEndian.Uint64(e[0x10:])); err != nil {
			return err
		}
		off += l
	}
	return nil
}

// systemRecords are kept in memory: Plan changes copies of them.
var systemRecords = []uint64{recMFT, recMFTMirr, recLogFile, recVolume, recBitmap, recBadClus}

// readSystemRecords loads the system files the resize changes.
func (v *Volume) readSystemRecords() error {
	v.records = map[uint64]*Record{}
	for _, num := range systemRecords {
		r, err := v.readRecord(num)
		if err != nil {
			return err
		}
		if !r.InUse || r.Extension {
			return fmt.Errorf("system file record %d not in use", num)
		}
		v.records[num] = r
	}
	return nil
}

// streamAttr returns the unnamed $DATA of a system record when it is a
// plain non-resident stream whose allocation is whole clusters held by
// its runs: the shape Apply writes through.
func (v *Volume) streamAttr(num uint64, what string) (*Attribute, error) {
	a := v.records[num].unnamed(attrData)
	if a == nil || !a.NonResident || a.StartVCN != 0 {
		return nil, fmt.Errorf("%s has no data", what)
	}
	n, ok := runClusters(a.Runs)
	if !ok || n > v.Clusters || a.AllocSize%v.ClusterSize != 0 || a.AllocSize > n*v.ClusterSize || a.DataSize > a.AllocSize || a.InitializedSize > a.DataSize {
		return nil, fmt.Errorf("implausible %s sizes", what)
	}
	for _, r := range a.Runs {
		if r.Sparse || !inVolume(r, v.Clusters) {
			return nil, fmt.Errorf("%s has a sparse run or one outside the volume", what)
		}
	}
	return a, nil
}

// readBitmap reads $Bitmap: only the bytes that cover the volume (rounded
// to 8 bytes), so a crafted size cannot make it allocate more.
func (v *Volume) readBitmap() error {
	a, err := v.streamAttr(recBitmap, "$Bitmap")
	if err != nil {
		return err
	}
	need := (v.Clusters + 7) / 8
	if a.DataSize < need || a.InitializedSize < need {
		return fmt.Errorf("$Bitmap holds %d bytes, the volume needs %d", a.DataSize, need)
	}
	b, err := v.readVirtual(a.Runs, 0, min(a.DataSize, (need+7)/8*8))
	if err != nil {
		return fmt.Errorf("read $Bitmap: %w", err)
	}
	v.bitmap = b
	v.UsedClusters = countOnes(b, 0, v.Clusters)
	return nil
}

// used tells whether cluster c is allocated in $Bitmap.
func (v *Volume) used(c uint64) bool { return v.bitmap[c/8]&(1<<(c%8)) != 0 }

// LastUsedCluster is the highest allocated cluster (0 if none).
func (v *Volume) LastUsedCluster() uint64 {
	if c, ok := lastSet(v.bitmap, v.Clusters); ok {
		return c
	}
	return 0
}

// UnchangedSize is the smallest partition that holds the volume as it
// is: its sectors and the backup boot sector after them. Plan leaves a
// volume alone for partitions at least this large.
func (v *Volume) UnchangedSize() uint64 { return (v.TotalSectors + 1) * v.BytesPerSector }

// scanRecords calls fn for every file record in use in the initialized
// part of $MFT, in order. The system records are passed as Open read
// them. progress, if set, gets the records done and the total.
func (v *Volume) scanRecords(progress func(done, total uint64), fn func(*Record) error) error {
	const batch = 256
	buf := make([]byte, min(batch, v.mftRecords)*v.RecordSize)
	for start := uint64(0); start < v.mftRecords; start += batch {
		cnt := min(batch, v.mftRecords-start)
		chunk := buf[:cnt*v.RecordSize]
		if err := v.readVirtualInto(chunk, v.mftRuns, start*v.RecordSize); err != nil {
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
			b := chunk[i*v.RecordSize : (i+1)*v.RecordSize]
			switch string(b[0:4]) {
			case "FILE":
			case "BAAD":
				// Windows marks records it found torn; chkdsk repairs them.
				return fmt.Errorf("%w: file record %d is marked damaged (BAAD); %s", ErrInconsistent, num, hintChkdsk)
			default:
				continue
			}
			if binary.LittleEndian.Uint16(b[0x16:])&1 == 0 {
				continue
			}
			if err := removeFixups(b); err != nil {
				return fmt.Errorf("%w: file record %d: %v; %s", ErrInconsistent, num, err, hintChkdsk)
			}
			r, err := parseRecord(num, b)
			if err != nil {
				return fmt.Errorf("%w: %v; %s", ErrInconsistent, err, hintChkdsk)
			}
			if err := fn(r); err != nil {
				return err
			}
		}
		if progress != nil {
			progress(start+cnt, v.mftRecords)
		}
	}
	return nil
}
