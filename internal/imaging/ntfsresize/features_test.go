package ntfsresize

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// sectorDisk is a memDisk that refuses writes not on sector boundaries,
// as raw Windows disk devices do.
type sectorDisk struct {
	memDisk
	sector int64
}

func (d *sectorDisk) WriteAt(p []byte, off int64) (int, error) {
	if off%d.sector != 0 || int64(len(p))%d.sector != 0 {
		return 0, fmt.Errorf("unaligned write of %d bytes at %d", len(p), off)
	}
	return d.memDisk.WriteAt(p, off)
}

// withShadowHeader returns img with a VSS volume header pointing to a
// catalog.
func withShadowHeader(img []byte, catalog uint64) []byte {
	out := append([]byte(nil), img...)
	h := out[vssHeaderOffset:]
	copy(h, vssIdentifier)
	binary.LittleEndian.PutUint32(h[16:], 1)
	binary.LittleEndian.PutUint32(h[20:], 1)
	binary.LittleEndian.PutUint64(h[24:], vssHeaderOffset)
	binary.LittleEndian.PutUint64(h[vssCatalogOffset:], catalog)
	binary.LittleEndian.PutUint64(h[56:], 1<<30)
	return out
}

// S1: shadow copy stores refer to volume offsets that change, so a shrink
// clears the catalog offset in the VSS header; the rest of the header
// (storage location, size limit) stays.
func TestShrinkClearsTheShadowCopyCatalog(t *testing.T) {
	src := testImage(t)
	img := withShadowHeader(src, 0x3000000)
	for _, opts := range []PlanOptions{snapshot, {NoMove: true}} {
		out, p := shrink(t, img, 9<<20, opts)
		if !p.ShadowCopiesRemoved {
			t.Error("shadow copies not reported as removed")
		}
		h := out[vssHeaderOffset : vssHeaderOffset+512]
		want := append([]byte(nil), img[vssHeaderOffset:vssHeaderOffset+512]...)
		binary.LittleEndian.PutUint64(want[vssCatalogOffset:], 0)
		if !bytes.Equal(h, want) {
			t.Errorf("header after shrinking:\n% x\nwant\n% x", h[:64], want[:64])
		}
	}
	// No header, or one without a catalog: nothing changes there.
	for _, in := range [][]byte{src, withShadowHeader(src, 0)} {
		out, p := shrink(t, in, 5<<20, snapshot)
		if p.ShadowCopiesRemoved || !bytes.Equal(out[vssHeaderOffset:vssHeaderOffset+512], in[vssHeaderOffset:vssHeaderOffset+512]) {
			t.Error("header changed although there were no shadow copies")
		}
	}
}

// 4Kn disks: 4 KiB sectors, clusters and records. Every write must be
// whole sectors, including the shadow copy header in the middle of one.
func TestShrink4KnVolume(t *testing.T) {
	s := &synthVolume{bps: 4096, cs: 4096, rs: 4096, clusters: 256, mftRecords: 16, vssCatalog: 0x10000, files: []synthFile{
		{num: 9, name: "a.bin", runs: []Run{{VCN: 0, LCN: 100, Len: 40}}},
		{num: 10, name: "b.bin", runs: []Run{{VCN: 0, LCN: 200, Len: 30}, {VCN: 30, LCN: 240, Len: 10}}, size: 40*4096 - 100},
	}}
	img := s.build(t)
	checkConsistent(t, img)
	size := uint64(150*4096 + 4096)
	p, err := mustOpen(t, img).PlanWith(size, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	dst := &sectorDisk{memDisk: memDisk{b: append([]byte(nil), img[:size]...)}, sector: 4096}
	if err := p.Apply(dst, nil); err != nil {
		t.Fatal(err)
	}
	if err := p.Verify(NewAlignedReader(&sectorReader{dst.b, 4096}, 0, int64(size), 4096)); err != nil {
		t.Fatal(err)
	}
	if p.MovedClusters != 40 || !p.ShadowCopiesRemoved {
		t.Errorf("moved %d, shadow copies removed %v", p.MovedClusters, p.ShadowCopiesRemoved)
	}
	for _, f := range s.files {
		if !bytes.Equal(fileData(t, dst.b, f.num), s.wantData(f)) {
			t.Errorf("%s changed", f.name)
		}
	}
}

// sectorReader refuses reads not on sector boundaries, as raw disks do.
type sectorReader struct {
	b      []byte
	sector int64
}

func (r *sectorReader) ReadAt(p []byte, off int64) (int, error) {
	if off%r.sector != 0 || int64(len(p))%r.sector != 0 {
		return 0, fmt.Errorf("unaligned read of %d bytes at %d", len(p), off)
	}
	return (&memDisk{b: r.b}).ReadAt(p, off)
}

// The aligned reader serves any read from sector-sized reads.
func TestAlignedReader(t *testing.T) {
	data := make([]byte, 64<<10)
	for i := range data {
		data[i] = byte(i * 7)
	}
	a := NewAlignedReader(&sectorReader{data, 4096}, 8192, 40000, 4096)
	for _, c := range []struct{ off, n int }{{0, 1}, {1, 4095}, {4000, 200}, {39990, 10}, {39990, 20}, {40000, 1}} {
		p := make([]byte, c.n)
		n, err := a.ReadAt(p, int64(c.off))
		want := min(c.n, max(0, 40000-c.off))
		if n != want || !bytes.Equal(p[:n], data[8192+c.off:8192+c.off+n]) || (n < c.n) != (err != nil) {
			t.Errorf("read %d at %d: %d, %v", c.n, c.off, n, err)
		}
	}
}

// S5, the review's probe 8: a volume already smaller than its new
// partition is restored as it is, without the checks a shrink needs (the
// volume here is dirty from its own earlier shrink).
func TestPlanVolumeAlreadyFits(t *testing.T) {
	src := testImage(t)
	small, _ := shrink(t, src, 8<<20, snapshot)
	part := make([]byte, 10<<20)
	copy(part, small)
	v := mustOpen(t, part)
	for _, size := range []uint64{9 << 20, v.UnchangedSize()} {
		p, err := v.PlanWith(size, PlanOptions{})
		if err != nil || !p.Unchanged {
			t.Fatalf("%d: %v", size, err)
		}
		dst := &countingDisk{memDisk: memDisk{b: append([]byte(nil), part[:size]...)}}
		if err := p.Apply(dst, nil); err != nil || dst.n != 0 {
			t.Errorf("%d: %d writes, %v", size, dst.n, err)
		}
		if err := p.Verify(bytes.NewReader(dst.b)); err != nil {
			t.Error(err)
		}
	}
}

// S5 edge: cutting only the sectors after the last whole cluster changes
// the sector count and the backup boot sector, nothing else.
func TestPlanCutsTrailingSectors(t *testing.T) {
	src := testImage(t)
	v := mustOpen(t, src)
	if v.TotalSectors*v.BytesPerSector == v.Clusters*v.ClusterSize {
		t.Skip("the sample has no sectors after its last cluster")
	}
	size := v.TotalSectors * v.BytesPerSector // one sector less than UnchangedSize
	p, err := v.PlanWith(size, PlanOptions{})
	if err != nil || p.Unchanged || p.NewClusters != v.Clusters {
		t.Fatalf("%v %+v", err, p)
	}
	dst := &countingDisk{memDisk: memDisk{b: append([]byte(nil), src[:size]...)}}
	if err := p.Apply(dst, nil); err != nil || dst.n != 2 {
		t.Fatalf("%d writes, %v", dst.n, err)
	}
	if err := p.Verify(bytes.NewReader(dst.b)); err != nil {
		t.Fatal(err)
	}
	if nv := checkConsistent(t, dst.b); nv.TotalSectors != v.TotalSectors-1 {
		t.Errorf("%d sectors", nv.TotalSectors)
	}
	sameFiles(t, fileHashes(t, src), fileHashes(t, dst.b))
}

// fragmented marks every second free cluster below end as used (with no
// owner), the review's probe 4.
func fragmented(t *testing.T, src []byte, end uint64) []byte {
	t.Helper()
	v := mustOpen(t, src)
	img := append([]byte(nil), src...)
	for c := uint64(0); c < end; c += 2 {
		if !v.used(c) {
			setBitmapBits(t, v, img, c, 1, true)
		}
	}
	return img
}

// S2: with the free space before the cut in 1-cluster holes, $T (1024
// contiguous clusters) cannot move into them: its record would outgrow
// itself. MinimumSize used to promise such sizes (Plan refused the
// minimum + 64 KiB). Now every size from MinimumSize up is accepted, and
// a smaller one names the minimum.
func TestMinimumSizeIsAPromise(t *testing.T) {
	src := testImage(t)
	img := fragmented(t, src, 4600)
	orig := fileHashes(t, img)
	v := mustOpen(t, img)
	oldMin := (v.UsedClusters+max(v.UsedClusters/100, 16))*v.ClusterSize + v.BytesPerSector
	minSize, err := v.MinimumSize(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("old estimate %d, minimum %d", oldMin, minSize)
	_, err = v.PlanWith(minSize-v.ClusterSize, snapshot)
	var tooSmall *TooSmallError
	if !errors.As(err, &tooSmall) || tooSmall.MinSize != minSize {
		t.Fatalf("below the minimum: %v", err)
	}
	t.Log(err)
	fallbacks := 0
	for i, size := 0, minSize; size < uint64(len(img)); i, size = i+1, size+v.ClusterSize*7+512 {
		p, err := v.PlanWith(size, snapshot)
		if err != nil {
			t.Fatalf("size %d (minimum %d) refused: %v", size, minSize, err)
		}
		if p.allocCut < p.NewClusters {
			fallbacks++
		}
		if i%40 == 0 || size < oldMin+128<<10 {
			dst := &memDisk{b: append([]byte(nil), img[:size]...)}
			if err := p.Apply(dst, nil); err != nil {
				t.Fatal(err)
			}
			if err := p.Verify(bytes.NewReader(dst.b)); err != nil {
				t.Fatalf("size %d: %v", size, err)
			}
			if i%40 == 0 {
				sameFiles(t, orig, fileHashes(t, dst.b))
			}
		}
	}
	t.Logf("%d sizes used the fallback", fallbacks)
}

// S3, the review's probe 11: 20 000 requests over 200 000 free extents,
// none large enough, took 27.8 s (O(runs x extents)).
func TestAllocatorScales(t *testing.T) {
	var ext []extent
	for i := uint64(0); i < 200000; i++ {
		ext = append(ext, extent{i * 4, 2})
	}
	a := newAllocator(ext)
	start := time.Now()
	for i := 0; i < 20000; i++ {
		got, err := a.take(3)
		if err != nil || len(got) != 2 || got[0].len+got[1].len != 3 {
			t.Fatalf("take: %v %v", got, err)
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("20000 takes took %v", d)
	}
}

// The allocator against a simple model: first fit by address, otherwise
// the largest extents; never two owners for a cluster.
func TestAllocatorModel(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	for iter := 0; iter < 300; iter++ {
		var ext []extent
		pos := uint64(0)
		for i := rnd.Intn(50); i >= 0; i-- {
			pos += 1 + uint64(rnd.Intn(5))
			n := 1 + uint64(rnd.Intn(20))
			ext = append(ext, extent{pos, n})
			pos += n
		}
		model := append([]extent(nil), ext...)
		a := newAllocator(append([]extent(nil), ext...))
		taken := map[uint64]bool{}
		for k := 0; k < 30; k++ {
			n := 1 + uint64(rnd.Intn(40))
			got, err := a.take(n)
			var free uint64
			for _, e := range model {
				free += e.len
			}
			if err != nil {
				if free >= n {
					t.Fatalf("take %d failed with %d free", n, free)
				}
				break
			}
			// The model: lowest extent that fits, else the largest (lowest of equals) in turn.
			var want []extent
			fit := -1
			for i, e := range model {
				if e.len >= n {
					fit = i
					break
				}
			}
			if fit >= 0 {
				want = append(want, extent{model[fit].start, n})
				model[fit].start += n
				model[fit].len -= n
			} else {
				for left := n; left > 0; {
					big := 0
					for i, e := range model {
						if e.len > model[big].len {
							big = i
						}
					}
					k := min(left, model[big].len)
					want = append(want, extent{model[big].start, k})
					model[big].start += k
					model[big].len -= k
					left -= k
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("take %d: %v, want %v", n, got, want)
			}
			for _, e := range got {
				for c := e.start; c < e.start+e.len; c++ {
					if taken[c] {
						t.Fatalf("cluster %d handed out twice", c)
					}
					taken[c] = true
				}
			}
		}
	}
}

// A crafted bitmap with very many tiny free extents: the shortest are
// left out beyond the limit, the longest kept, in address order.
func TestFreeExtentsLimit(t *testing.T) {
	bm := []byte{0b10101010, 0b00001010, 0b11111111, 0b00000000} // free: 0,2,4,6, 8, 10, 12-15, 24-31
	got := freeExtents(bm, 32, 3)
	want := []extent{{12, 4}, {24, 8}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%v, want %v", got, want)
	}
	if all := freeExtents(bm, 32, 100); len(all) != 8 {
		t.Errorf("%d extents without a limit: %v", len(all), all)
	}
}

// S3: planning reports its progress over the file records.
func TestPlanReportsProgress(t *testing.T) {
	src := testImage(t)
	v := mustOpen(t, src)
	var calls int
	var last, total uint64
	opts := snapshot
	opts.Progress = func(done, all uint64) {
		if done < last || all != v.mftRecords {
			t.Errorf("progress %d of %d after %d", done, all, last)
		}
		calls, last, total = calls+1, done, all
	}
	if _, err := v.PlanWith(5<<20, opts); err != nil {
		t.Fatal(err)
	}
	if calls == 0 || last != total {
		t.Errorf("%d calls, last %d of %d", calls, last, total)
	}
}

// S4: Verify accepts the shrunk volume and finds damage to what the plan
// wrote, and inconsistencies anywhere.
func TestVerifyFindsDamage(t *testing.T) {
	src := testImage(t)
	v := mustOpen(t, src)
	const size = 5 << 20
	p, err := v.PlanWith(size, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	good := &memDisk{b: append([]byte(nil), src[:size]...)}
	if err := p.Apply(good, nil); err != nil {
		t.Fatal(err)
	}
	if err := p.Verify(bytes.NewReader(good.b)); err != nil {
		t.Fatal(err)
	}
	// The restore adjusts the hidden sectors of a moved partition.
	hidden := append([]byte(nil), good.b...)
	for _, off := range []uint64{0, p.NewTotalSectors * 512} {
		binary.LittleEndian.PutUint32(hidden[off+0x1C:], 12345)
	}
	if err := p.Verify(bytes.NewReader(hidden)); err != nil {
		t.Errorf("hidden sectors: %v", err)
	}
	nv := mustOpen(t, good.b)
	recOff := func(num uint64) uint64 {
		lcn, _ := lcnOf(nv.mftRuns, num*nv.RecordSize/nv.ClusterSize)
		return lcn*nv.ClusterSize + num*nv.RecordSize%nv.ClusterSize
	}
	bmLCN, _ := lcnOf(nv.records[recBitmap].unnamed(attrData).Runs, 0)
	mirrLCN, _ := lcnOf(nv.records[recMFTMirr].unnamed(attrData).Runs, 0)
	logLCN, _ := lcnOf(nv.records[recLogFile].unnamed(attrData).Runs, 0)
	var moved uint64 // a record the plan rewrote
	for _, r := range p.dirty {
		if r.Number > recBadClus {
			moved = r.Number
		}
	}
	for name, damage := range map[string]func(b []byte){
		"not applied":        func(b []byte) { copy(b, src[:size]) },
		"backup boot sector": func(b []byte) { b[p.NewTotalSectors*512+0x50] ^= 1 },
		"rewritten record":   func(b []byte) { b[recOff(moved)+0x100] ^= 1 },
		"$Volume record":     func(b []byte) { b[recOff(recVolume)+0x100] ^= 1 },
		"$Bitmap":            func(b []byte) { b[bmLCN*1024+10] ^= 0x10 },
		"$MFTMirr":           func(b []byte) { b[mirrLCN*1024+3*1024+0x100] ^= 1 },
		"$LogFile not reset": func(b []byte) { copy(b[logLCN*1024:], "RSTR") },
		"cross-link elsewhere": func(b []byte) {
			// Record 33 did not move; point it at clusters of record 32.
			r32, _ := nv.readRecord(32)
			patchRecord(t, b, 33, func(r *Record) {
				r.Attrs[2].Runs, r.Attrs[2].dirty = []Run{{VCN: 0, LCN: r32.Attrs[3].Runs[0].LCN, Len: 64}}, true
			})
		},
	} {
		img := append([]byte(nil), good.b...)
		damage(img)
		if err := p.Verify(bytes.NewReader(img)); !errors.Is(err, ErrVerify) {
			t.Errorf("%s: %v", name, err)
		} else {
			t.Logf("%s: %v", name, err)
		}
	}
	shadow := withShadowHeader(src, 0x3000000)
	ps, err := mustOpen(t, shadow).PlanWith(size, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	img := &memDisk{b: append([]byte(nil), shadow[:size]...)}
	if err := ps.Apply(img, nil); err != nil {
		t.Fatal(err)
	}
	copy(img.b[vssHeaderOffset:], shadow[vssHeaderOffset:vssHeaderOffset+512])
	if err := ps.Verify(bytes.NewReader(img.b)); !errors.Is(err, ErrVerify) {
		t.Errorf("shadow copy header left: %v", err)
	}
}

// fullRecordVolume has file x.bin whose record is exactly full: 262
// one-cluster fragments at 100, 102, ... and a last run at 900-919. A cut
// through that last run splits it, which adds a run the record cannot
// hold; a cut before it moves the run whole and the record still fits.
func fullRecordVolume(t *testing.T) (*synthVolume, []byte) {
	t.Helper()
	var runs []Run
	for i := uint64(0); i < 262; i++ {
		runs = append(runs, Run{VCN: i, LCN: 100 + 2*i, Len: 1})
	}
	runs = append(runs, Run{VCN: 262, LCN: 900, Len: 20})
	s := &synthVolume{clusters: 1024, files: []synthFile{{num: 9, name: "x.bin", runs: runs}}}
	return s, s.build(t)
}

// S2 with a cut that is not monotone: planning at a cut through the last
// run fails, so PlanWith falls back to the largest cut below that works
// (and the volume still gets the size asked for). Every size from
// MinimumSize up is accepted.
func TestPlanFallsBackWhenTheCutSplitsAFullRecord(t *testing.T) {
	s, img := fullRecordVolume(t)
	v := checkConsistent(t, img)
	sr, err := v.scan(0, true, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.simulate(sr, 910); err == nil {
		t.Fatal("the fixture does not overflow its record at a cut through the last run")
	}
	if _, err := v.simulate(sr, 900); err != nil {
		t.Fatalf("the fixture does not fit at a cut before the last run: %v", err)
	}
	minSize, err := v.MinimumSize(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("minimum %d bytes (%d clusters)", minSize, minSize/512-1)
	fallbacks := 0
	for size := minSize; size <= v.UnchangedSize(); size += 512 * 3 {
		p, err := v.PlanWith(size, snapshot)
		if err != nil {
			t.Fatalf("size %d (%d clusters) refused: %v", size, size/512-1, err)
		}
		if p.Unchanged {
			continue
		}
		if p.allocCut < p.NewClusters {
			fallbacks++
		}
		dst := &memDisk{b: append([]byte(nil), img[:size]...)}
		if err := p.Apply(dst, nil); err != nil {
			t.Fatal(err)
		}
		if err := p.Verify(bytes.NewReader(dst.b)); err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if !bytes.Equal(fileData(t, dst.b, 9), s.wantData(s.files[0])) {
			t.Fatalf("size %d: x.bin changed", size)
		}
	}
	if fallbacks == 0 {
		t.Error("no size used the fallback")
	}
	t.Logf("%d sizes used the fallback", fallbacks)
}
