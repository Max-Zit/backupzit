package ntfsresize

import (
	"bytes"
	"encoding/binary"
	"math/bits"
	"testing"
	"unicode/utf16"
)

// synthFile is a file of a synthetic volume.
type synthFile struct {
	num    uint64 // file record number (9 and up)
	ext    uint64 // non-zero: the data is in this extension record, behind an attribute list
	name   string
	parent uint64 // directory record; 0 means the root (5)
	runs   []Run  // data runs; none means resident data
	size   uint64 // data size of non-resident data (0: all its clusters)
	head   []byte // the first bytes of the data (the rest is a pattern)
}

// synthVolume describes a small NTFS volume built from scratch, so tests
// and fuzz seeds do not depend on a sample image. Zero fields take the
// defaults: 512-byte sectors and clusters, 1 KiB records, 128 clusters.
type synthVolume struct {
	bps, cs, rs uint64
	clusters    uint64
	mftRecords  uint64
	boot        uint64 // clusters of $Boot (default 8 KiB)
	files       []synthFile
	volFlags    uint16
	// $LogFile restart area: version (default 1.1), first client in use
	// (default none) and flags (default clean); logEmpty fills it with 0xFF.
	logMajor, logMinor uint16
	logInUse, logFlags uint16
	logSet, logEmpty   bool
	vssCatalog         uint64 // non-zero: a VSS volume header with this catalog offset
}

// synthLayout is where build put the system files.
type synthLayout struct {
	mft, mirr, log, bm, firstFree uint64
	mftClusters                   uint64
}

// defaults fills in the zero fields.
func (s *synthVolume) defaults() {
	if s.bps == 0 {
		s.bps = 512
	}
	if s.cs == 0 {
		s.cs = s.bps
	}
	if s.rs == 0 {
		s.rs = max(1024, s.bps)
	}
	if s.clusters == 0 {
		s.clusters = 128
	}
	if s.mftRecords == 0 {
		s.mftRecords = 16
	}
	if s.boot == 0 {
		s.boot = max(8192/s.cs, 1)
	}
	if !s.logSet {
		s.logMajor, s.logMinor, s.logInUse, s.logFlags = 1, 1, logNoClient, restartVolumeIsClean
	}
}

// layout returns the cluster positions of the system files.
func (s *synthVolume) layout() synthLayout {
	s.defaults()
	var l synthLayout
	l.mft = s.boot
	l.mftClusters = max(s.mftRecords*s.rs/s.cs, 1)
	l.mirr = l.mft + l.mftClusters
	l.log = l.mirr + max(4*s.rs/s.cs, 1)
	l.bm = l.log + max(8192/s.cs, 1)
	l.firstFree = l.bm + (s.bitmapBytes()+s.cs-1)/s.cs
	return l
}

// bitmapBytes is the size of $Bitmap's data.
func (s *synthVolume) bitmapBytes() uint64 { return ((s.clusters+7)/8 + 7) / 8 * 8 }

// utf16le encodes a name as NTFS stores it.
func utf16le(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	return b
}

// align8 rounds n up to a multiple of 8.
func align8(n int) int { return (n + 7) &^ 7 }

// resAttr builds a resident attribute.
func resAttr(typ uint32, name string, id uint16, value []byte) []byte {
	nm := utf16le(name)
	vo := align8(0x18 + len(nm))
	b := make([]byte, align8(vo+len(value)))
	binary.LittleEndian.PutUint32(b[0:], typ)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)))
	b[9] = byte(len(nm) / 2)
	binary.LittleEndian.PutUint16(b[0x0A:], 0x18)
	binary.LittleEndian.PutUint16(b[0x0E:], id)
	binary.LittleEndian.PutUint32(b[0x10:], uint32(len(value)))
	binary.LittleEndian.PutUint16(b[0x14:], uint16(vo))
	copy(b[0x18:], nm)
	copy(b[vo:], value)
	return b
}

// nonResAttr builds a non-resident attribute over runs.
func nonResAttr(typ uint32, name string, id uint16, runs []Run, alloc, data, init uint64) []byte {
	nm := utf16le(name)
	mp := align8(0x40 + len(nm))
	pairs := encodeRuns(runs)
	b := make([]byte, align8(mp+len(pairs)))
	binary.LittleEndian.PutUint32(b[0:], typ)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)))
	b[8] = 1
	b[9] = byte(len(nm) / 2)
	binary.LittleEndian.PutUint16(b[0x0A:], 0x40)
	binary.LittleEndian.PutUint16(b[0x0E:], id)
	var last uint64
	for _, r := range runs {
		last = r.VCN + r.Len
	}
	if len(runs) > 0 {
		binary.LittleEndian.PutUint64(b[0x10:], runs[0].VCN)
	}
	binary.LittleEndian.PutUint64(b[0x18:], last-1)
	binary.LittleEndian.PutUint16(b[0x20:], uint16(mp))
	binary.LittleEndian.PutUint64(b[0x28:], alloc)
	binary.LittleEndian.PutUint64(b[0x30:], data)
	binary.LittleEndian.PutUint64(b[0x38:], init)
	copy(b[0x40:], nm)
	copy(b[mp:], pairs)
	return b
}

// fileNameAttr builds a $FILE_NAME attribute.
func fileNameAttr(parent uint64, name string) []byte {
	nm := utf16le(name)
	v := make([]byte, 0x42+len(nm))
	binary.LittleEndian.PutUint64(v, parent|1<<48)
	v[0x40] = byte(len(nm) / 2)
	v[0x41] = 1 // Win32 name space
	copy(v[0x42:], nm)
	return resAttr(attrFileName, "", 1, v)
}

// synthRecord builds a file record (without fixups).
func synthRecord(rs, num, base uint64, dir bool, attrs ...[]byte) []byte {
	b := make([]byte, rs)
	copy(b, "FILE")
	usaCount := rs/fixupStride + 1
	binary.LittleEndian.PutUint16(b[4:], 0x30)
	binary.LittleEndian.PutUint16(b[6:], uint16(usaCount))
	binary.LittleEndian.PutUint16(b[0x10:], 1) // sequence number
	binary.LittleEndian.PutUint16(b[0x12:], 1) // links
	first := align8(0x30 + 2*int(usaCount))
	binary.LittleEndian.PutUint16(b[0x14:], uint16(first))
	flags := uint16(1)
	if dir {
		flags |= 2
	}
	binary.LittleEndian.PutUint16(b[0x16:], flags)
	binary.LittleEndian.PutUint32(b[0x1C:], uint32(rs))
	if base != 0 {
		binary.LittleEndian.PutUint64(b[0x20:], base|1<<48)
	}
	binary.LittleEndian.PutUint32(b[0x2C:], uint32(num))
	binary.LittleEndian.PutUint16(b[0x30:], 1) // update sequence number
	off := first
	for _, a := range attrs {
		off += copy(b[off:], a)
	}
	binary.LittleEndian.PutUint32(b[off:], attrEnd)
	binary.LittleEndian.PutUint32(b[0x18:], uint32(off+8))
	return b
}

// pattern is the content of cluster vcn of file num.
func pattern(num, vcn uint64, b []byte) {
	for i := range b {
		b[i] = byte(num*31 + vcn*7 + uint64(i)*13)
	}
}

// restartPageBytes builds a $LogFile restart page of 4096 bytes.
func restartPageBytes(major, minor, inUse, flags uint16, lsn uint64) []byte {
	p := make([]byte, 4096)
	copy(p, "RSTR")
	binary.LittleEndian.PutUint16(p[4:], 0x1E)
	binary.LittleEndian.PutUint16(p[6:], 9)
	binary.LittleEndian.PutUint32(p[16:], 4096)
	binary.LittleEndian.PutUint32(p[20:], 4096)
	binary.LittleEndian.PutUint16(p[24:], 0x30)
	binary.LittleEndian.PutUint16(p[26:], minor)
	binary.LittleEndian.PutUint16(p[28:], major)
	ra := p[0x30:]
	binary.LittleEndian.PutUint64(ra[0:], lsn)
	binary.LittleEndian.PutUint16(ra[8:], 1)
	binary.LittleEndian.PutUint16(ra[10:], logNoClient)
	binary.LittleEndian.PutUint16(ra[12:], inUse)
	binary.LittleEndian.PutUint16(ra[14:], flags)
	applyFixups(p)
	return p
}

// build returns the image: the volume and its backup boot sector.
func (s *synthVolume) build(t testing.TB) []byte {
	t.Helper()
	l := s.layout()
	cs, rs := s.cs, s.rs
	ts := s.clusters * cs / s.bps
	img := make([]byte, (ts+1)*s.bps)

	type rec struct {
		num  uint64
		base uint64
		dir  bool
		attr [][]byte
		runs []Run // owned clusters
	}
	var recs []rec
	add := func(num uint64, dir bool, runs []Run, attrs ...[]byte) {
		recs = append(recs, rec{num: num, dir: dir, attr: attrs, runs: runs})
	}
	mftBM := make([]byte, 8)
	mftRun := []Run{{VCN: 0, LCN: l.mft, Len: l.mftClusters}}
	add(recMFT, false, mftRun,
		nonResAttr(attrData, "", 1, mftRun, l.mftClusters*cs, s.mftRecords*rs, s.mftRecords*rs),
		resAttr(attrBitmap, "", 2, mftBM))
	mirrRun := []Run{{VCN: 0, LCN: l.mirr, Len: l.log - l.mirr}}
	add(recMFTMirr, false, mirrRun, nonResAttr(attrData, "", 1, mirrRun, (l.log-l.mirr)*cs, 4*rs, 4*rs))
	logRun := []Run{{VCN: 0, LCN: l.log, Len: l.bm - l.log}}
	logSize := (l.bm - l.log) * cs
	add(recLogFile, false, logRun, nonResAttr(attrData, "", 1, logRun, logSize, logSize, logSize))
	vi := make([]byte, 12)
	vi[8], vi[9] = 3, 1
	binary.LittleEndian.PutUint16(vi[0x0A:], s.volFlags)
	add(recVolume, false, nil, resAttr(attrVolumeInfo, "", 1, vi))
	add(4, false, nil)
	add(recRoot, true, nil, fileNameAttr(recRoot, "."))
	bmRun := []Run{{VCN: 0, LCN: l.bm, Len: l.firstFree - l.bm}}
	add(recBitmap, false, bmRun, nonResAttr(attrData, "", 1, bmRun, (l.firstFree-l.bm)*cs, s.bitmapBytes(), s.bitmapBytes()))
	bootRun := []Run{{VCN: 0, LCN: 0, Len: s.boot}}
	add(recBoot, false, bootRun, nonResAttr(attrData, "", 1, bootRun, s.boot*cs, s.boot*cs, s.boot*cs))
	badRun := []Run{{VCN: 0, Len: s.clusters, Sparse: true}}
	add(recBadClus, false, nil, resAttr(attrData, "", 1, nil), nonResAttr(attrData, "$Bad", 2, badRun, s.clusters*cs, s.clusters*cs, 0))
	for _, f := range s.files {
		parent := f.parent
		if parent == 0 {
			parent = recRoot
		}
		if len(f.runs) == 0 {
			add(f.num, false, nil, fileNameAttr(parent, f.name), resAttr(attrData, "", 2, f.head))
			continue
		}
		n, _ := runClusters(f.runs)
		size := f.size
		if size == 0 {
			size = n * cs
		}
		data := nonResAttr(attrData, "", 2, f.runs, n*cs, size, size)
		if f.ext != 0 {
			list := append(attrListEntry(attrFileName, 0, f.num, 1), attrListEntry(attrData, 0, f.ext, 2)...)
			add(f.num, false, nil, resAttr(attrAttributeList, "", 3, list), fileNameAttr(parent, f.name))
			recs = append(recs, rec{num: f.ext, base: f.num, attr: [][]byte{data}, runs: f.runs})
		} else {
			add(f.num, false, f.runs, fileNameAttr(parent, f.name), data)
		}
		for _, r := range f.runs {
			if r.Sparse {
				continue
			}
			for k := uint64(0); k < r.Len; k++ {
				pattern(f.num, r.VCN+k, img[(r.LCN+k)*cs:(r.LCN+k+1)*cs])
			}
		}
		if len(f.head) > 0 {
			lcn, err := lcnOf(f.runs, 0)
			if err != nil {
				t.Fatal(err)
			}
			copy(img[lcn*cs:], f.head)
		}
	}

	bm := make([]byte, s.bitmapBytes())
	for _, r := range recs {
		if r.num >= s.mftRecords {
			t.Fatalf("record %d beyond the %d records of $MFT", r.num, s.mftRecords)
		}
		mftBM[r.num/8] |= 1 << (r.num % 8)
		for _, run := range r.runs {
			if !run.Sparse {
				setBits(bm, run.LCN, run.Len, true)
			}
		}
	}
	setBits(bm, s.clusters, uint64(len(bm))*8-s.clusters, true)
	copy(img[l.bm*cs:], bm)
	for _, r := range recs {
		b := synthRecord(rs, r.num, r.base, r.dir, r.attr...)
		if r.num == recMFT { // its $BITMAP is complete only now
			b = synthRecord(rs, r.num, 0, r.dir, r.attr[0], resAttr(attrBitmap, "", 2, mftBM))
		}
		applyFixups(b)
		copy(img[l.mft*cs+r.num*rs:], b)
		if r.num < 4 {
			copy(img[l.mirr*cs+r.num*rs:], b)
		}
	}

	if s.logEmpty {
		for i := range logSize {
			img[l.log*cs+i] = 0xFF
		}
	} else {
		copy(img[l.log*cs:], restartPageBytes(s.logMajor, s.logMinor, s.logInUse, s.logFlags, 100))
		copy(img[l.log*cs+4096:], restartPageBytes(s.logMajor, s.logMinor, s.logInUse, s.logFlags, 100))
	}
	if s.vssCatalog != 0 {
		h := img[vssHeaderOffset:]
		copy(h, vssIdentifier)
		binary.LittleEndian.PutUint32(h[16:], 1)
		binary.LittleEndian.PutUint32(h[20:], 1)
		binary.LittleEndian.PutUint64(h[24:], vssHeaderOffset)
		binary.LittleEndian.PutUint64(h[vssCatalogOffset:], s.vssCatalog)
		binary.LittleEndian.PutUint64(h[56:], 1<<30)
	}

	boot := img[:s.bps]
	copy(boot, []byte{0xEB, 0x52, 0x90})
	copy(boot[3:], "NTFS    ")
	binary.LittleEndian.PutUint16(boot[0x0B:], uint16(s.bps))
	if spc := cs / s.bps; spc <= 0x80 {
		boot[0x0D] = byte(spc)
	} else {
		boot[0x0D] = byte(256 - bits.TrailingZeros64(spc))
	}
	boot[0x15] = 0xF8
	binary.LittleEndian.PutUint64(boot[0x28:], ts)
	binary.LittleEndian.PutUint64(boot[0x30:], l.mft)
	binary.LittleEndian.PutUint64(boot[0x38:], l.mirr)
	boot[0x40] = byte(256 - bits.TrailingZeros64(rs))
	boot[0x44] = 0xF4
	boot[0x1FE], boot[0x1FF] = 0x55, 0xAA
	copy(img[ts*s.bps:], boot)
	return img
}

// fileData reads the data of file record num of a volume through its
// runs (this package's reading code, checked against pattern).
func fileData(t *testing.T, img []byte, num uint64) []byte {
	t.Helper()
	v := mustOpen(t, img)
	r, err := v.readRecord(num)
	if err != nil {
		t.Fatal(err)
	}
	a := r.unnamed(attrData)
	b, err := v.readVirtual(a.Runs, 0, a.DataSize)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// wantData is the data build wrote for a file.
func (s *synthVolume) wantData(f synthFile) []byte {
	n, _ := runClusters(f.runs)
	b := make([]byte, n*s.cs)
	for vcn := uint64(0); vcn < n; vcn++ {
		pattern(f.num, vcn, b[vcn*s.cs:(vcn+1)*s.cs])
	}
	copy(b, f.head)
	if f.size != 0 {
		b = b[:f.size]
	}
	return b
}

// TestSynthVolumeOpens checks the builder: the volume opens and is
// consistent, and go-ntfs reads the files' data the same.
func TestSynthVolumeOpens(t *testing.T) {
	s := &synthVolume{files: []synthFile{
		{num: 9, name: "a.bin", runs: []Run{{VCN: 0, LCN: 100, Len: 10}}},
		{num: 10, name: "b.txt", head: []byte("hello")},
	}}
	img := s.build(t)
	v := checkConsistent(t, img)
	if v.Clusters != 128 || v.RecordSize != 1024 {
		t.Fatalf("geometry %d clusters, %d byte records", v.Clusters, v.RecordSize)
	}
	if st, _ := v.logState(); st != logClean {
		t.Errorf("log state %v", st)
	}
	if got := fileData(t, img, 9); !bytes.Equal(got, s.wantData(s.files[0])) {
		t.Error("file data read back wrong")
	}
}

// attrListEntry builds an $ATTRIBUTE_LIST entry for an unnamed attribute
// starting at vcn in record rec.
func attrListEntry(typ uint32, vcn, rec uint64, id uint16) []byte {
	e := make([]byte, 0x20)
	binary.LittleEndian.PutUint32(e[0:], typ)
	binary.LittleEndian.PutUint16(e[4:], 0x20)
	e[7] = 0x1A
	binary.LittleEndian.PutUint64(e[8:], vcn)
	binary.LittleEndian.PutUint64(e[0x10:], rec|1<<48)
	binary.LittleEndian.PutUint16(e[0x18:], id)
	return e
}
