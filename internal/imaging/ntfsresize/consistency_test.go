package ntfsresize

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

// planErr plans img at size with opts and returns the error.
func planErr(t *testing.T, img []byte, size uint64, opts PlanOptions) error {
	t.Helper()
	_, err := mustOpen(t, img).PlanWith(size, opts)
	return err
}

// wantRefusal fails unless err wraps target and mentions every part.
func wantRefusal(t *testing.T, err, target error, parts ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("accepted")
	}
	if !errors.Is(err, target) {
		t.Fatalf("error %q does not wrap %q", err, target)
	}
	for _, p := range parts {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("error %q does not mention %q", err, p)
		}
	}
}

// B1, the review's probe 5b: every free cluster before the cut is marked
// used, except six clusters of $Secure:$SDS (record 9, in use), which are
// cleared. The allocator used to hand those six to moved clusters and
// overwrite the volume's security descriptors.
func TestPlanRefusesClustersInUseButFree(t *testing.T) {
	src := testImage(t)
	v := mustOpen(t, src)
	img := append([]byte(nil), src...)
	const cut = 7700
	for c := uint64(0); c < cut; c++ {
		if !v.used(c) {
			setBitmapBits(t, v, img, c, 1, true)
		}
	}
	for _, c := range []uint64{300, 302, 304, 306, 308, 310} {
		setBitmapBits(t, v, img, c, 1, false)
	}
	for _, opts := range []PlanOptions{{}, snapshot, {NoMove: true}} {
		err := planErr(t, img, cut*1024+512, opts)
		wantRefusal(t, err, ErrInconsistent, "record 9", "cluster 300", "free in $Bitmap", "chkdsk /f")
	}
	if _, err := mustOpen(t, img).MinimumSize(snapshot); !errors.Is(err, ErrInconsistent) {
		t.Errorf("MinimumSize: %v", err)
	}
}

// B1, probe 5: a live file's clusters free in $Bitmap (record 33).
func TestPlanRefusesFileOverBitmapHole(t *testing.T) {
	src := testImage(t)
	img := append([]byte(nil), src...)
	setBitmapBits(t, mustOpen(t, src), img, 475, 64, false)
	wantRefusal(t, planErr(t, img, 5<<20, snapshot), ErrInconsistent, "record 33", "cluster 475")
	probs, err := mustOpen(t, img).Check(5)
	if err != nil || len(probs) == 0 || !strings.Contains(probs[0], "cluster 475") {
		t.Errorf("Check: %v %v", probs, err)
	}
}

// B1, probe 6: record 33's data points to clusters of $T (record 32)
// behind the cut. Each owner used to get its own copy, and Orphaned
// underflowed to 18446744073709551552.
func TestPlanRefusesCrossLinks(t *testing.T) {
	src := testImage(t)
	img := append([]byte(nil), src...)
	patchRecord(t, img, 33, func(r *Record) {
		r.Attrs[2].Runs, r.Attrs[2].dirty = []Run{{VCN: 0, LCN: 5000, Len: 64}}, true
	})
	setBitmapBits(t, mustOpen(t, src), img, 475, 64, false) // the used count stays the same
	wantRefusal(t, planErr(t, img, 4400*1024, snapshot), ErrInconsistent, "cluster 5000 is used twice", "record 33")
}

// B1, probe 12b: an in-use record whose run lies past the end of the
// volume. Plan used to accept it and Apply failed after 170 writes.
func TestPlanRefusesRunPastTheEnd(t *testing.T) {
	src := testImage(t)
	img := append([]byte(nil), src...)
	v := mustOpen(t, src)
	r33, err := v.readRecord(33)
	if err != nil {
		t.Fatal(err)
	}
	r := r33.clone()
	r.Number = 200
	r.Attrs[2].Runs, r.Attrs[2].dirty = []Run{{VCN: 0, LCN: 20000, Len: 64}}, true
	writeRecord(t, v, img, v.mftRuns, nil, r)
	out := &countingDisk{memDisk: memDisk{b: append([]byte(nil), img[:5<<20]...)}}
	p, err := mustOpen(t, img).PlanWith(5<<20, snapshot)
	if err == nil {
		_ = p.Apply(out, nil)
	}
	wantRefusal(t, err, ErrInconsistent, "record 200", "past the end of the volume")
	if out.n != 0 {
		t.Errorf("%d writes", out.n)
	}
}

// Records Windows marked torn (BAAD) are refused too.
func TestPlanRefusesDamagedRecords(t *testing.T) {
	src := testImage(t)
	img := append([]byte(nil), src...)
	v := mustOpen(t, src)
	off, _ := lcnOf(v.mftRuns, 33*v.RecordSize/v.ClusterSize)
	copy(img[off*v.ClusterSize:], "BAAD")
	wantRefusal(t, planErr(t, img, 5<<20, snapshot), ErrInconsistent, "record 33", "BAAD")
}

// B1, probe 7: a source marked dirty, and one where chkdsk was cut off.
func TestPlanRefusesDirtyVolumes(t *testing.T) {
	src := testImage(t)
	for _, c := range []struct {
		flags uint16
		want  string
	}{{0x81, "dirty"}, {0x4080, "chkdsk run was interrupted"}} {
		img := append([]byte(nil), src...)
		setVolumeFlags(t, img, c.flags)
		for _, opts := range []PlanOptions{{}, snapshot, {NoMove: true}} {
			wantRefusal(t, planErr(t, img, 5<<20, opts), ErrNotClean, c.want, "chkdsk /f")
		}
		if _, err := mustOpen(t, img).MinimumSize(snapshot); !errors.Is(err, ErrNotClean) {
			t.Errorf("MinimumSize: %v", err)
		}
	}
	// The flags the sample has (0x80) are fine.
	if err := planErr(t, src, 5<<20, PlanOptions{}); err != nil {
		t.Errorf("clean sample: %v", err)
	}
}

// N2: only NTFS 3.0 and 3.1 are shrunk.
func TestPlanChecksTheVersion(t *testing.T) {
	src := testImage(t)
	for _, c := range []struct {
		major, minor byte
		ok           bool
	}{{3, 0, true}, {3, 1, true}, {3, 2, false}, {4, 0, false}, {2, 1, false}} {
		img := append([]byte(nil), src...)
		patchRecord(t, img, recVolume, func(r *Record) {
			val := r.unnamed(attrVolumeInfo).residentValue()
			val[8], val[9] = c.major, c.minor
		})
		err := planErr(t, img, 5<<20, snapshot)
		if c.ok != (err == nil) {
			t.Errorf("version %d.%d: %v", c.major, c.minor, err)
		}
	}
}

// setLogRestart changes both $LogFile restart pages of img in place.
func setLogRestart(t *testing.T, img []byte, fn func(page, ra []byte)) {
	t.Helper()
	v := mustOpen(t, img)
	lf := v.records[recLogFile].unnamed(attrData)
	for _, pos := range []uint64{0, 4096} {
		page, err := v.readVirtual(lf.Runs, pos, 4096)
		if err != nil {
			t.Fatal(err)
		}
		if err := removeFixups(page); err != nil {
			t.Fatal(err)
		}
		fn(page, page[binary.LittleEndian.Uint16(page[24:]):])
		applyFixups(page)
		if err := v.writeVirtual(diskOf(img), lf.Runs, pos, page); err != nil {
			t.Fatal(err)
		}
	}
}

// B1: the $LogFile restart area decides whether the source was shut down
// cleanly. An open log is accepted only for images from a snapshot.
func TestPlanChecksTheLogFile(t *testing.T) {
	src := testImage(t)
	if st, _ := mustOpen(t, src).logState(); st != logClean {
		t.Fatalf("sample log: %v", st)
	}
	open := append([]byte(nil), src...)
	setLogRestart(t, open, func(_, ra []byte) {
		binary.LittleEndian.PutUint16(ra[12:], 0) // a client in use
		binary.LittleEndian.PutUint16(ra[14:], 0) // without the clean flag
	})
	wantRefusal(t, planErr(t, open, 5<<20, PlanOptions{}), ErrNotClean, "open transactions", "shut it down fully")
	wantRefusal(t, planErr(t, open, 5<<20, PlanOptions{NoMove: true}), ErrNotClean, "open transactions")
	if err := planErr(t, open, 5<<20, snapshot); err != nil {
		t.Errorf("an open log from a snapshot: %v", err)
	}
	// In use but with the clean flag (Windows XP and later): clean.
	inUseClean := append([]byte(nil), src...)
	setLogRestart(t, inUseClean, func(_, ra []byte) { binary.LittleEndian.PutUint16(ra[12:], 0) })
	if err := planErr(t, inUseClean, 5<<20, PlanOptions{}); err != nil {
		t.Errorf("open with the clean flag: %v", err)
	}
	// Version 2.0: Windows 8 and later kept metadata cached.
	cached := append([]byte(nil), src...)
	setLogRestart(t, cached, func(page, _ []byte) {
		binary.LittleEndian.PutUint16(page[26:], 0)
		binary.LittleEndian.PutUint16(page[28:], 2)
	})
	wantRefusal(t, planErr(t, cached, 5<<20, PlanOptions{}), ErrNotClean, "Fast Startup")
	// Only the newer page counts: the second one says open.
	newer := append([]byte(nil), src...)
	n := 0
	setLogRestart(t, newer, func(_, ra []byte) {
		if n++; n == 2 {
			binary.LittleEndian.PutUint64(ra[0:], binary.LittleEndian.Uint64(ra[0:])+1)
			binary.LittleEndian.PutUint16(ra[12:], 0)
			binary.LittleEndian.PutUint16(ra[14:], 0)
		}
	})
	wantRefusal(t, planErr(t, newer, 5<<20, PlanOptions{}), ErrNotClean, "open transactions")
	// A log reset to 0xFF (chkdsk, an earlier shrink) is clean.
	empty := append([]byte(nil), src...)
	v := mustOpen(t, empty)
	lf := v.records[recLogFile].unnamed(attrData)
	if err := v.writeStream(diskOf(empty), lf.Runs, lf.AllocSize, func(_ uint64, b []byte) {
		for i := range b {
			b[i] = 0xFF
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := planErr(t, empty, 5<<20, PlanOptions{}); err != nil {
		t.Errorf("empty log: %v", err)
	}
	// Garbage: cannot tell.
	garbage := append([]byte(nil), src...)
	if err := v.writeStream(diskOf(garbage), lf.Runs, 8192, func(_ uint64, b []byte) { copy(b, "RCRD") }); err != nil {
		t.Fatal(err)
	}
	wantRefusal(t, planErr(t, garbage, 5<<20, PlanOptions{}), ErrNotClean, "cannot tell")
	// A torn first page: the second one is found.
	torn := append([]byte(nil), src...)
	off, _ := lcnOf(lf.Runs, 0)
	torn[off*v.ClusterSize+1022] ^= 0xFF
	if st, d := mustOpen(t, torn).logState(); st != logClean {
		t.Errorf("torn first page: %v %s", st, d)
	}
}

// Bad clusters are refused, as before.
func TestPlanRefusesBadClusters(t *testing.T) {
	img := append([]byte(nil), testImage(t)...)
	patchRecord(t, img, recBadClus, func(r *Record) {
		bad := r.named(attrData, "$Bad")
		bad.Runs = []Run{{VCN: 0, Len: 100, Sparse: true}, {VCN: 100, LCN: 100, Len: 1}, {VCN: 101, Len: 10138, Sparse: true}}
		bad.dirty = true
	})
	wantRefusal(t, planErr(t, img, 5<<20, snapshot), ErrUnsupported, "bad clusters")
}

// hiberVolume is a synthetic volume with \hiberfil.sys starting with head.
func hiberVolume(t *testing.T, head string, parent uint64, ext uint64) (*synthVolume, []byte) {
	s := &synthVolume{clusters: 256, files: []synthFile{
		{num: 9, name: "hiberfil.sys", parent: parent, ext: ext, runs: []Run{{VCN: 0, LCN: 120, Len: 20}}, head: []byte(head)},
		{num: 10, name: "data.bin", runs: []Run{{VCN: 0, LCN: 200, Len: 30}}},
	}}
	return s, s.build(t)
}

// B1: an active hibernation image (hibernated, or shut down with Fast
// Startup) makes Windows resume with metadata cached for the old layout.
func TestPlanRefusesActiveHibernation(t *testing.T) {
	for _, head := range []string{"HIBR", "hibr", "WAKE", "wake", "RSTR", "rstr"} {
		for _, ext := range []uint64{0, 11} {
			_, img := hiberVolume(t, head+"xxxx", 0, ext)
			for _, opts := range []PlanOptions{{}, snapshot, {NoMove: true}} {
				wantRefusal(t, planErr(t, img, 180*512+512, opts), ErrNotClean, "hiberfil.sys", head, "Fast Startup")
			}
			if probs, _ := mustOpen(t, img).Check(5); len(probs) != 1 || !strings.Contains(probs[0], "hibernation") {
				t.Errorf("%s: Check: %v", head, probs)
			}
		}
	}
	// A cleared header, and a hiberfil.sys elsewhere than in the root, are
	// no hibernation.
	for _, c := range []struct {
		head   string
		parent uint64
	}{{"\x00\x00\x00\x00", 0}, {"HIBR", 4}} {
		s, img := hiberVolume(t, c.head, c.parent, 0)
		out, p := shrink(t, img, 180*512+512, snapshot)
		if !bytes.Equal(fileData(t, out, 10), s.wantData(s.files[1])) || p.MovedClusters == 0 {
			t.Errorf("%q in %d: data not moved intact", c.head, c.parent)
		}
	}
}

// N1, the review's probe 12: an in-use record past $MFT's initialized
// size is no file; Plan used to move its clusters and underflow Orphaned.
func TestPlanIgnoresRecordsPastTheInitializedSize(t *testing.T) {
	src := testImage(t)
	img := append([]byte(nil), src...)
	v := mustOpen(t, src)
	r33, _ := v.readRecord(33)
	r := r33.clone()
	r.Number = 200
	r.Attrs[2].Runs, r.Attrs[2].dirty = []Run{{VCN: 0, LCN: 9500, Len: 64}}, true
	writeRecord(t, v, img, v.mftRuns, nil, r)
	patchRecord(t, img, recMFT, func(r *Record) {
		d := r.unnamed(attrData)
		d.InitializedSize, d.dirty = 64*v.RecordSize, true
	})
	want, err := mustOpen(t, src).PlanWith(5<<20, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	p, err := mustOpen(t, img).PlanWith(5<<20, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if p.MovedClusters != want.MovedClusters || p.Orphaned != 0 {
		t.Errorf("moved %d (want %d), orphaned %d", p.MovedClusters, want.MovedClusters, p.Orphaned)
	}
}

// Orphaned counts allocated clusters behind the cut that no file owns,
// from the ownership map, so it cannot underflow.
func TestOrphanedClusters(t *testing.T) {
	src := testImage(t)
	img := append([]byte(nil), src...)
	setBitmapBits(t, mustOpen(t, src), img, 9000, 100, true)
	out, p := shrink(t, img, 8<<20, snapshot)
	if p.Orphaned != 100 {
		t.Errorf("orphaned %d, want 100", p.Orphaned)
	}
	if mustOpen(t, out).UsedClusters != mustOpen(t, img).UsedClusters-100 {
		t.Error("orphaned clusters still allocated")
	}
}

// NoMove (images not read from a VSS snapshot): the volume shrinks to
// just after its last used cluster and no data moves.
func TestPlanNoMove(t *testing.T) {
	src := testImage(t)
	orig := fileHashes(t, src)
	v := mustOpen(t, src)
	noMove := PlanOptions{NoMove: true}
	minSize, err := v.MinimumSize(noMove)
	if err != nil {
		t.Fatal(err)
	}
	if want := (v.LastUsedCluster()+1)*v.ClusterSize + v.BytesPerSector; minSize != want {
		t.Errorf("minimum without moving %d, want %d", minSize, want)
	}
	_, err = v.PlanWith(minSize-v.ClusterSize, noMove)
	var tooSmall *TooSmallError
	if !errors.As(err, &tooSmall) || tooSmall.MinSize != minSize || !strings.Contains(err.Error(), "VSS snapshot") {
		t.Fatalf("below the minimum: %v", err)
	}
	for _, size := range []uint64{minSize, minSize + 512, 9 << 20} {
		out, p := shrink(t, src, size, noMove)
		if len(p.Moves) != 0 {
			t.Errorf("%d: %d moves", size, len(p.Moves))
		}
		checkConsistent(t, out)
		sameFiles(t, orig, fileHashes(t, out))
	}
}
