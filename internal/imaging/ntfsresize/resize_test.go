package ntfsresize

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	ntfs "www.velocidex.com/golang/go-ntfs/parser"
)

// testImage returns the NTFS image shipped with go-ntfs (10 MiB, 1 KiB
// clusters, $MFT in the middle).
func testImage(t *testing.T) []byte {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "www.velocidex.com/golang/go-ntfs").Output()
	if err != nil {
		t.Skip("go-ntfs module not available")
	}
	b, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(out)), "parser", "test_data", "test.ntfs.dd"))
	if err != nil {
		t.Skip("go-ntfs test image not available")
	}
	return b
}

// memDisk is a growable in-memory WriterAt/ReaderAt.
type memDisk struct{ b []byte }

func (m *memDisk) WriteAt(p []byte, off int64) (int, error) {
	if int(off)+len(p) > len(m.b) {
		return 0, fmt.Errorf("write past the end: %d+%d > %d", off, len(p), len(m.b))
	}
	return copy(m.b[off:], p), nil
}

func (m *memDisk) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(m.b)) {
		return 0, io.EOF
	}
	n := copy(p, m.b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// fileHashes walks the file system with go-ntfs (an independent parser)
// and hashes every file's default data stream.
func fileHashes(t *testing.T, img []byte) map[string]string {
	t.Helper()
	paged, err := ntfs.NewPagedReader(bytes.NewReader(img), 1024, 64)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ntfs.GetNTFSContext(paged, 0)
	if err != nil {
		t.Fatalf("go-ntfs cannot open the volume: %v", err)
	}
	out := map[string]string{}
	var walk func(dir string, e *ntfs.MFT_ENTRY, depth int)
	walk = func(dir string, e *ntfs.MFT_ENTRY, depth int) {
		if depth > 20 {
			return
		}
		for _, fi := range ntfs.ListDir(c, e) {
			if fi.IsSlack || fi.Name == "." || fi.Name == ".." || strings.HasPrefix(fi.Name, "$") {
				continue
			}
			p := dir + "/" + fi.Name
			id, _, _, _, err := ntfs.ParseMFTId(fi.MFTId)
			if err != nil {
				t.Errorf("%s: %v", p, err)
				continue
			}
			child, err := c.GetMFT(id)
			if err != nil {
				t.Errorf("%s: %v", p, err)
				continue
			}
			if fi.IsDir {
				out[p] = "dir"
				walk(p, child, depth+1)
				continue
			}
			s, err := ntfs.OpenStream(c, child, ntfs.ATTR_TYPE_DATA, ntfs.WILDCARD_STREAM_ID, "")
			if err != nil {
				out[p] = "nodata"
				continue
			}
			data := make([]byte, fi.Size)
			n, _ := s.ReadAt(data, 0)
			h := sha256.Sum256(data[:n])
			out[p] = fmt.Sprintf("%d:%x", n, h[:8])
		}
	}
	root, err := c.GetMFT(5)
	if err != nil {
		t.Fatal(err)
	}
	walk("", root, 0)
	return out
}

// checkConsistent opens the shrunk volume with this package and checks
// that every run lies inside the volume and is marked in the bitmap,
// and that no two runs share a cluster.
func checkConsistent(t *testing.T, img []byte) *Volume {
	t.Helper()
	v, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("reopen the shrunk volume: %v", err)
	}
	owner := map[uint64]string{}
	bad := false
	err = v.scanRecords(func(rec *Record) error {
		if bad {
			return nil
		}
		for _, a := range rec.Attrs {
			if !a.NonResident || (rec.Number == recBadClus && a.Name == "$Bad") {
				continue
			}
			for _, r := range a.Runs {
				if r.Sparse {
					continue
				}
				if r.LCN+r.Len > v.Clusters {
					t.Errorf("record %d attr %#x: run %d+%d past the end (%d clusters)", rec.Number, a.Type, r.LCN, r.Len, v.Clusters)
					continue
				}
				for c := r.LCN; c < r.LCN+r.Len; c++ {
					if !v.used(c) {
						t.Errorf("record %d: cluster %d in use but free in $Bitmap", rec.Number, c)
						bad = true
						return nil
					}
					who := fmt.Sprintf("%d/%#x", rec.Number, a.Type)
					if o, ok := owner[c]; ok {
						t.Errorf("cluster %d used by %s and %s", c, o, who)
						bad = true
						return nil
					}
					owner[c] = who
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func shrink(t *testing.T, src []byte, newSize uint64) ([]byte, *Plan) {
	t.Helper()
	v, err := Open(bytes.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	p, err := v.Plan(newSize)
	if err != nil {
		t.Fatalf("plan %d: %v", newSize, err)
	}
	// The restore writes the clusters before the new end first.
	dst := &memDisk{b: append([]byte(nil), src[:newSize]...)}
	if err := p.Apply(dst, nil); err != nil {
		t.Fatalf("apply: %v", err)
	}
	return dst.b, p
}

func TestShrinkKeepsEveryFile(t *testing.T) {
	src := testImage(t)
	orig := fileHashes(t, src)
	if len(orig) < 3 {
		t.Fatalf("test image has only %d files", len(orig))
	}
	v, err := Open(bytes.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d clusters of %d bytes, %d used, last used %d, minimum %d bytes, %d files",
		v.Clusters, v.ClusterSize, v.UsedClusters, v.LastUsedCluster(), v.MinimumSize(), len(orig))

	min := (v.MinimumSize() + 4095) / 4096 * 4096
	for _, size := range []uint64{9 << 20, 8 << 20, 6 << 20, 5 << 20, min} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			if size < v.MinimumSize() {
				t.Skip("below the minimum")
			}
			out, p := shrink(t, src, size)
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
			vi := nv.records[recVolume].unnamed(attrVolumeInfo).residentValue()
			if binary.LittleEndian.Uint16(vi[0x0A:])&1 == 0 {
				t.Error("volume not marked dirty")
			}
			got := fileHashes(t, out)
			var diff []string
			for k, h := range orig {
				if got[k] != h {
					diff = append(diff, fmt.Sprintf("%s: %s -> %s", k, h, got[k]))
				}
			}
			for k := range got {
				if _, ok := orig[k]; !ok {
					diff = append(diff, "new: "+k)
				}
			}
			sort.Strings(diff)
			if len(diff) > 0 {
				t.Errorf("%d files differ:\n%s", len(diff), strings.Join(diff, "\n"))
			}
		})
	}
}

func TestShrinkRefusesTooSmall(t *testing.T) {
	src := testImage(t)
	v, err := Open(bytes.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Plan(v.UsedClusters * v.ClusterSize / 2); err == nil {
		t.Error("shrinking below the used space accepted")
	}
	v, _ = Open(bytes.NewReader(src))
	if _, err := v.Plan(uint64(len(src)) + 1<<20); err == nil {
		t.Error("growing accepted")
	}
}

func TestRunlistRoundTrip(t *testing.T) {
	cases := [][]Run{
		{{VCN: 0, LCN: 3413, Len: 64}},
		{{VCN: 0, LCN: 100, Len: 10}, {VCN: 10, Len: 5, Sparse: true}, {VCN: 15, LCN: 50, Len: 300}, {VCN: 315, LCN: 1 << 33, Len: 1}},
		{{VCN: 0, Len: 1 << 40, Sparse: true}},
	}
	for _, runs := range cases {
		got, err := decodeRuns(encodeRuns(runs), 0)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(got) != fmt.Sprint(runs) {
			t.Errorf("round trip %v -> %v", runs, got)
		}
	}
	if _, err := decodeRuns([]byte{0x11, 5}, 0); err == nil {
		t.Error("unterminated mapping pairs accepted")
	}
}

func TestFixupsRoundTrip(t *testing.T) {
	b := make([]byte, 1024)
	copy(b, "FILE")
	binary.LittleEndian.PutUint16(b[4:], 0x30)
	binary.LittleEndian.PutUint16(b[6:], 3)
	b[510], b[511], b[1022], b[1023] = 0xAA, 0xBB, 0xCC, 0xDD
	orig := append([]byte(nil), b...)
	applyFixups(b)
	if b[510] == 0xAA {
		t.Fatal("fixup not applied")
	}
	if err := removeFixups(b); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b[0x36:], orig[0x36:]) || !bytes.Equal(b[:0x30], orig[:0x30]) {
		t.Error("fixups did not round-trip")
	}
	b[1022] ^= 1
	applyFixups(b)
	b[1022] ^= 0xFF
	if err := removeFixups(b); err == nil {
		t.Error("torn record accepted")
	}
}

// moveToEnd relocates every run of one attribute to free clusters at the
// end of the volume (without shrinking), so a later shrink has to move it
// back: the way to test moving $MFT and $MFTMirr with a small image.
func moveToEnd(t *testing.T, src []byte, rec uint64, attrType uint32) []byte {
	t.Helper()
	v, err := Open(bytes.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	a := v.records[rec].unnamed(attrType)
	var need uint64
	for _, r := range a.Runs {
		need += r.Len
	}
	var start uint64
	for _, e := range v.freeExtents(v.Clusters) {
		if e.len >= need && e.start > start {
			start = e.start + e.len - need
		}
	}
	if start < v.Clusters/2 {
		t.Fatalf("no free space for %d clusters in the second half", need)
	}
	p := &Plan{v: v, NewSize: uint64(len(src)), NewTotalSectors: v.TotalSectors, NewClusters: v.Clusters}
	var out []Run
	to := start
	for _, r := range a.Runs {
		p.Moves = append(p.Moves, Move{From: r.LCN, To: to, Len: r.Len})
		for c := uint64(0); c < r.Len; c++ {
			v.setUsed(r.LCN+c, false)
			v.setUsed(to+c, true)
		}
		out = append(out, Run{VCN: r.VCN, LCN: to, Len: r.Len})
		to += r.Len
	}
	a.Runs, a.dirty, v.records[rec].dirty = mergeRuns(out), true, true
	if rec == recMFT {
		v.mftRuns = append([]Run(nil), a.Runs...)
	}
	v.records[recBitmap].dirty = true
	for _, num := range sortedKeys(v.records) {
		if r := v.records[num]; r.dirty {
			p.dirty = append(p.dirty, r)
		}
	}
	dst := &memDisk{b: append([]byte(nil), src...)}
	if err := p.Apply(dst, nil); err != nil {
		t.Fatal(err)
	}
	return dst.b
}

func TestShrinkMovesMFT(t *testing.T) {
	src := testImage(t)
	orig := fileHashes(t, src)
	moved := moveToEnd(t, src, recMFT, attrData)
	moved = moveToEnd(t, moved, recMFTMirr, attrData)
	if got := fileHashes(t, moved); fmt.Sprint(got) != fmt.Sprint(orig) {
		t.Fatalf("moving $MFT to the end broke the volume:\n%v\n%v", orig, got)
	}
	checkConsistent(t, moved)
	v, _ := Open(bytes.NewReader(moved))
	mftLCN := binary.LittleEndian.Uint64(moved[0x30:])
	if mftLCN < v.Clusters/2 {
		t.Fatalf("$MFT still at cluster %d", mftLCN)
	}
	out, p := shrink(t, moved, 6<<20)
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
	if got := fileHashes(t, out); fmt.Sprint(got) != fmt.Sprint(orig) {
		t.Errorf("files differ after moving $MFT back:\n%v\n%v", orig, got)
	}
}

// Base records have a zero base reference just like extensions of $MFT
// (record 0) have record number 0 in theirs: only the sequence number
// tells them apart. Plan must keep only $MFT's own runs as $MFT.
func TestPlanKeepsMFTRuns(t *testing.T) {
	src := testImage(t)
	v, err := Open(bytes.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprint(v.mftRuns)
	p, err := v.Plan(5 << 20)
	if err != nil {
		t.Fatal(err)
	}
	if p.MovedClusters == 0 {
		t.Fatal("test needs moved clusters")
	}
	if got := fmt.Sprint(v.mftRuns); got != want {
		t.Errorf("$MFT runs changed although $MFT did not move:\n got %s\nwant %s", got, want)
	}
}

// NTFS reads run lengths as signed numbers: a length whose top stored
// byte has bit 7 set is negative, so 0x8982 needs three bytes (82 89 00).
// Found on a real Windows volume (chkdsk: "extent list could not be
// initialized").
func TestRunLengthIsSigned(t *testing.T) {
	enc := encodeRuns([]Run{{VCN: 0, LCN: 3475731, Len: 0x8982}})
	ls := int(enc[0] & 0x0F)
	if top := enc[ls]; top&0x80 != 0 {
		t.Errorf("length 0x8982 stored as % x: top byte %#x reads as negative", enc[1:1+ls], top)
	}
	if _, err := decodeRuns([]byte{0x32, 0x82, 0x89, 0x13, 0x09, 0x35, 0x00}, 0); err == nil {
		t.Error("a negative run length was accepted")
	}
	if runs, err := decodeRuns([]byte{0x33, 0x82, 0x89, 0x00, 0x7c, 0x66, 0x3f, 0x00}, 0); err != nil || runs[0].Len != 0x8982 || runs[0].LCN != 4155004 {
		t.Errorf("Windows' encoding: %v, %v", runs, err)
	}
}

// Windows marks the bits past the last cluster in $Bitmap as used; chkdsk
// reports "the volume bitmap is incorrect" when they are clear.
func TestShrinkMarksBitsPastTheEnd(t *testing.T) {
	src := testImage(t)
	out, p := shrink(t, src, 5<<20+3*4096) // a cluster count that is not a multiple of 8
	if p.NewClusters%8 == 0 {
		t.Fatalf("test needs a partial last byte, got %d clusters", p.NewClusters)
	}
	v, err := Open(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	for c := v.Clusters; c < uint64(len(v.bitmap))*8; c++ {
		if !v.used(c) {
			t.Fatalf("bit %d past the end (%d clusters) is clear", c, v.Clusters)
		}
	}
}
