package ntfsresize

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"
)

func TestShrinkKeepsEveryFile(t *testing.T) {
	src := testImage(t)
	orig := fileHashes(t, src)
	if len(orig) < 3 {
		t.Fatalf("test image has only %d files", len(orig))
	}
	v := mustOpen(t, src)
	minSize, err := v.MinimumSize(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d clusters of %d bytes, %d used, last used %d, minimum %d bytes, %d files",
		v.Clusters, v.ClusterSize, v.UsedClusters, v.LastUsedCluster(), minSize, len(orig))

	aligned := (minSize + 4095) / 4096 * 4096
	for _, size := range []uint64{9 << 20, 8 << 20, 6 << 20, 5 << 20, aligned, minSize} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			out, p := shrink(t, src, size, snapshot)
			t.Logf("moved %d clusters in %d moves, %d orphaned, %d records changed", p.MovedClusters, len(p.Moves), p.Orphaned, len(p.dirty))
			nv := checkConsistent(t, out)
			if nv.Clusters != p.NewClusters {
				t.Errorf("boot sector says %d clusters, want %d", nv.Clusters, p.NewClusters)
			}
			if nv.LastUsedCluster() >= p.NewClusters {
				t.Errorf("cluster %d used behind the end", nv.LastUsedCluster())
			}
			if got, want := nv.UsedClusters, v.UsedClusters-p.Orphaned; got != want {
				t.Errorf("%d clusters used after shrinking, want %d", got, want)
			}
			if !bytes.Equal(out[len(out)-512:], out[:512]) {
				t.Error("backup boot sector differs from the boot sector")
			}
			if _, _, flags, _ := nv.volumeInfo(); flags&volumeDirty == 0 {
				t.Error("volume not marked dirty")
			}
			sameFiles(t, orig, fileHashes(t, out))
			for _, e := range verifyAgainst(t, src, out, p.NewClusters) {
				t.Error(e)
			}
		})
	}
}

func TestShrinkRefusesTooSmall(t *testing.T) {
	src := testImage(t)
	v := mustOpen(t, src)
	_, err := v.PlanWith(v.UsedClusters*v.ClusterSize/2, snapshot)
	if err == nil {
		t.Fatal("shrinking below the used space accepted")
	}
	t.Log(err)
	// Larger than the volume: nothing to do.
	p, err := v.PlanWith(uint64(len(src))+1<<20, snapshot)
	if err != nil || !p.Unchanged {
		t.Errorf("growing: %v, unchanged %v", err, p != nil && p.Unchanged)
	}
}

// moveToEnd relocates every run of one attribute to free clusters at the
// end of the volume (without shrinking), so a later shrink has to move it
// back: the way to test moving $MFT and $MFTMirr with a small image.
func moveToEnd(t *testing.T, src []byte, rec uint64, attrType uint32) []byte {
	t.Helper()
	v := mustOpen(t, src)
	r, err := v.readRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	ai := -1
	for i, a := range r.Attrs {
		if a.Type == attrType && a.Name == "" {
			ai = i
		}
	}
	a := r.Attrs[ai]
	need, _ := runClusters(a.Runs)
	var start uint64
	forEachFree(v.bitmap, v.Clusters, func(s, n uint64) {
		if n >= need && s > start {
			start = s + n - need
		}
	})
	if start < v.Clusters/2 {
		t.Fatalf("no free space for %d clusters in the second half", need)
	}
	var out []Run
	to := start
	for _, run := range a.Runs {
		out = append(out, Run{VCN: run.VCN, LCN: to, Len: run.Len})
		to += run.Len
	}
	return relocate(t, src, rec, ai, mergeRuns(out))
}

func TestShrinkMovesMFT(t *testing.T) {
	src := testImage(t)
	orig := fileHashes(t, src)
	moved := moveToEnd(t, src, recMFT, attrData)
	moved = moveToEnd(t, moved, recMFTMirr, attrData)
	sameFiles(t, orig, fileHashes(t, moved))
	checkConsistent(t, moved)
	v := mustOpen(t, moved)
	if mftLCN := binary.LittleEndian.Uint64(moved[0x30:]); mftLCN < v.Clusters/2 {
		t.Fatalf("$MFT still at cluster %d", mftLCN)
	}
	out, p := shrink(t, moved, 6<<20, snapshot)
	t.Logf("moved %d clusters, %d records changed, $MFT now at %d", p.MovedClusters, len(p.dirty), binary.LittleEndian.Uint64(out[0x30:]))
	nv := checkConsistent(t, out)
	if l := binary.LittleEndian.Uint64(out[0x30:]); l >= nv.Clusters {
		t.Errorf("$MFT at cluster %d behind the end", l)
	}
	// The mirror holds the same first records as $MFT.
	mirr := nv.records[recMFTMirr].unnamed(attrData)
	m, err := nv.readVirtual(mirr.Runs, 0, 4*nv.RecordSize)
	if err != nil {
		t.Fatal(err)
	}
	f, err := nv.readVirtual(nv.mftRuns, 0, 4*nv.RecordSize)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(m, f) {
		t.Error("$MFTMirr differs from the first records of $MFT")
	}
	sameFiles(t, orig, fileHashes(t, out))
}

// Base records have a zero base reference just like extensions of $MFT
// (record 0) have record number 0 in theirs: only the sequence number
// tells them apart. Plan must keep only $MFT's own runs as $MFT.
func TestPlanKeepsMFTRuns(t *testing.T) {
	src := testImage(t)
	v := mustOpen(t, src)
	want := fmt.Sprint(v.mftRuns)
	p, err := v.PlanWith(5<<20, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if p.MovedClusters == 0 {
		t.Fatal("test needs moved clusters")
	}
	if got := fmt.Sprint(p.mftRuns); got != want {
		t.Errorf("$MFT runs changed although $MFT did not move:\n got %s\nwant %s", got, want)
	}
	if got := fmt.Sprint(v.mftRuns); got != want {
		t.Errorf("Plan changed the volume's $MFT runs: %s", got)
	}
}

// Windows marks the bits past the last cluster in $Bitmap as used; chkdsk
// reports "the volume bitmap is incorrect" when they are clear.
func TestShrinkMarksBitsPastTheEnd(t *testing.T) {
	src := testImage(t)
	out, p := shrink(t, src, 5<<20+3*4096, snapshot) // a cluster count that is not a multiple of 8
	if p.NewClusters%8 == 0 {
		t.Fatalf("test needs a partial last byte, got %d clusters", p.NewClusters)
	}
	v := mustOpen(t, out)
	for c := v.Clusters; c < uint64(len(v.bitmap))*8; c++ {
		if !v.used(c) {
			t.Fatalf("bit %d past the end (%d clusters) is clear", c, v.Clusters)
		}
	}
}

// Plan does not change the volume, so the same volume can be planned at
// several sizes, with the same result each time.
func TestPlanIsRepeatable(t *testing.T) {
	src := testImage(t)
	v := mustOpen(t, src)
	used := v.UsedClusters
	a, err := v.PlanWith(5<<20, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.PlanWith(6<<20, snapshot); err != nil {
		t.Fatal(err)
	}
	b, err := v.PlanWith(5<<20, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(a.Moves) != fmt.Sprint(b.Moves) || v.UsedClusters != used || countOnes(v.bitmap, 0, v.Clusters) != used {
		t.Error("planning twice gave different moves or changed the volume")
	}
}

// N7: the moves are copied in source order, which keeps the reads of the
// backup sequential.
func TestMovesSortedBySource(t *testing.T) {
	src := testImage(t)
	p, err := mustOpen(t, src).PlanWith(5<<20, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(p.Moves); i++ {
		if p.Moves[i].From < p.Moves[i-1].From {
			t.Fatalf("move %d (%d) before move %d (%d)", i, p.Moves[i].From, i-1, p.Moves[i-1].From)
		}
	}
}

// The probe's case: a fragmented file (8 pieces, one straddling the cut)
// is split, kept and moved parts merge, and the content is the same.
func TestShrinkFragmentedFileAcrossTheCut(t *testing.T) {
	src := testImage(t)
	orig := fileHashes(t, src)
	var runs []Run
	for i, s := range []uint64{7800, 7900, 8000, 8100, 8996, 9100, 9200, 9300} {
		runs = append(runs, Run{VCN: uint64(i * 8), LCN: s, Len: 8})
	}
	frag := relocate(t, src, 33, 2, runs) // record 33: 64 clusters of data
	sameFiles(t, orig, fileHashes(t, frag))
	for _, cut := range []uint64{9000, 8104, 7804, 4200} {
		size := cut*1024 + 512
		out, p := shrink(t, frag, size, snapshot)
		checkConsistent(t, out)
		sameFiles(t, orig, fileHashes(t, out))
		for _, e := range verifyAgainst(t, frag, out, p.NewClusters) {
			t.Errorf("cut %d: %s", cut, e)
		}
	}
}

// The probe's sweep, coarser: many sizes from the minimum to the full
// size, each checked with go-ntfs.
func TestShrinkSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	src := testImage(t)
	v := mustOpen(t, src)
	minSize, err := v.MinimumSize(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for s := minSize; s < uint64(len(src))-2048; s += 251 * 1024 {
		for _, size := range []uint64{s, s + 512, s + 1536} {
			out, p := shrink(t, src, size, snapshot)
			for _, e := range verifyAgainst(t, src, out, p.NewClusters) {
				t.Errorf("size %d: %s", size, e)
			}
		}
	}
}
