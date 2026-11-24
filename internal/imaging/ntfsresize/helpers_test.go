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

// mustOpen opens an image held in memory.
func mustOpen(t *testing.T, img []byte) *Volume {
	t.Helper()
	v, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return v
}

// snapshot are the options of a VSS image: data may move.
var snapshot = PlanOptions{FromSnapshot: true}

// memDisk is an in-memory WriterAt/ReaderAt of fixed size.
type memDisk struct{ b []byte }

func (m *memDisk) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > int64(len(m.b)) {
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

// countingDisk counts the writes to a memDisk.
type countingDisk struct {
	memDisk
	n int
}

func (c *countingDisk) WriteAt(p []byte, off int64) (int, error) {
	c.n++
	return c.memDisk.WriteAt(p, off)
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

// sameFiles fails the test when the files of two images differ.
func sameFiles(t *testing.T, want, got map[string]string) {
	t.Helper()
	var diff []string
	for k, h := range want {
		if got[k] != h {
			diff = append(diff, fmt.Sprintf("%s: %s -> %s", k, h, got[k]))
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			diff = append(diff, "new: "+k)
		}
	}
	sort.Strings(diff)
	if len(diff) > 0 {
		t.Errorf("%d files differ:\n%s", len(diff), strings.Join(diff, "\n"))
	}
}

// checkConsistent opens an image with this package and fails the test on
// any inconsistency Check finds.
func checkConsistent(t *testing.T, img []byte) *Volume {
	t.Helper()
	v, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("reopen the shrunk volume: %v", err)
	}
	probs, err := v.Check(20)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range probs {
		t.Errorf("inconsistent: %s", p)
	}
	return v
}

// shrink plans (with opts), applies and verifies shrinking src to newSize.
func shrink(t *testing.T, src []byte, newSize uint64, opts PlanOptions) ([]byte, *Plan) {
	t.Helper()
	v := mustOpen(t, src)
	p, err := v.PlanWith(newSize, opts)
	if err != nil {
		t.Fatalf("plan %d: %v", newSize, err)
	}
	// The restore writes the clusters before the new end first.
	dst := &memDisk{b: append([]byte(nil), src[:newSize]...)}
	if err := p.Apply(dst, nil); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := p.Verify(bytes.NewReader(dst.b)); err != nil {
		t.Fatalf("verify: %v", err)
	}
	return dst.b, p
}

// diskOf returns a memDisk over img (changed in place).
func diskOf(img []byte) *memDisk { return &memDisk{b: img} }

// setBitmapBits sets or clears bits of $Bitmap on disk in img.
func setBitmapBits(t *testing.T, v *Volume, img []byte, start, n uint64, on bool) {
	t.Helper()
	bm := v.records[recBitmap].unnamed(attrData)
	for c := start; c < start+n; c++ {
		lcn, err := lcnOf(bm.Runs, c/8/v.ClusterSize)
		if err != nil {
			t.Fatal(err)
		}
		setBit(img[lcn*v.ClusterSize+(c/8)%v.ClusterSize:], c%8, on)
	}
}

// writeRecord writes a (changed) record into img through runs, and into
// $MFTMirr for the first four records.
func writeRecord(t *testing.T, v *Volume, img []byte, mftRuns, mirrRuns []Run, r *Record) {
	t.Helper()
	b, err := r.encode()
	if err != nil {
		t.Fatal(err)
	}
	applyFixups(b)
	if err := v.writeVirtual(diskOf(img), mftRuns, r.Number*v.RecordSize, b); err != nil {
		t.Fatal(err)
	}
	if r.Number < 4 {
		if err := v.writeVirtual(diskOf(img), mirrRuns, r.Number*v.RecordSize, b); err != nil {
			t.Fatal(err)
		}
	}
}

// patchRecord changes file record num of img in place with fn.
func patchRecord(t *testing.T, img []byte, num uint64, fn func(r *Record)) {
	t.Helper()
	v := mustOpen(t, img)
	r, err := v.readRecord(num)
	if err != nil {
		t.Fatal(err)
	}
	fn(r)
	writeRecord(t, v, img, v.mftRuns, v.records[recMFTMirr].unnamed(attrData).Runs, r)
}

// setVolumeFlags sets the $Volume flags of img in place.
func setVolumeFlags(t *testing.T, img []byte, flags uint16) {
	t.Helper()
	patchRecord(t, img, recVolume, func(r *Record) {
		binary.LittleEndian.PutUint16(r.unnamed(attrVolumeInfo).residentValue()[0x0A:], flags)
	})
}

// relocate gives one attribute (record rec, attribute index ai) new runs
// covering the same VCNs, copies its data there, updates $Bitmap and the
// boot sectors, and returns the new image. It builds test volumes without
// using Plan.
func relocate(t *testing.T, src []byte, rec uint64, ai int, newRuns []Run) []byte {
	t.Helper()
	v := mustOpen(t, src)
	r, err := v.readRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	a := r.Attrs[ai]
	img := append([]byte(nil), src...)
	cs := v.ClusterSize
	for _, nr := range newRuns {
		if nr.Sparse {
			continue
		}
		for k := uint64(0); k < nr.Len; k++ {
			from, err := lcnOf(a.Runs, nr.VCN+k)
			if err != nil {
				t.Fatal(err)
			}
			if to := nr.LCN + k; v.used(to) {
				if _, own := findRunAt(a.Runs, to); !own {
					t.Fatalf("target cluster %d in use", to)
				}
			}
			copy(img[(nr.LCN+k)*cs:(nr.LCN+k+1)*cs], src[from*cs:(from+1)*cs])
		}
	}
	for _, or := range a.Runs {
		if !or.Sparse {
			setBitmapBits(t, v, img, or.LCN, or.Len, false)
		}
	}
	for _, nr := range newRuns {
		if !nr.Sparse {
			setBitmapBits(t, v, img, nr.LCN, nr.Len, true)
		}
	}
	c := r.clone()
	c.Attrs[ai].Runs, c.Attrs[ai].dirty = newRuns, true
	mftRuns := v.mftRuns
	if a.Type == attrData && a.Name == "" && (rec == recMFT || (r.Extension && r.Base == recMFT)) {
		// Keep the parts of $MFT's runs outside this segment (Open merges
		// segments that continue each other into one run).
		var runs []Run
		for _, mr := range v.mftRuns {
			if mr.VCN < a.StartVCN {
				runs = append(runs, Run{VCN: mr.VCN, LCN: mr.LCN, Len: min(mr.Len, a.StartVCN-mr.VCN)})
			}
			if end := mr.VCN + mr.Len; end > a.LastVCN+1 {
				s := max(mr.VCN, a.LastVCN+1)
				runs = append(runs, Run{VCN: s, LCN: mr.LCN + s - mr.VCN, Len: end - s})
			}
		}
		runs = append(runs, newRuns...)
		sort.Slice(runs, func(i, j int) bool { return runs[i].VCN < runs[j].VCN })
		mftRuns = runs
	}
	mirrRuns := v.records[recMFTMirr].unnamed(attrData).Runs
	if rec == recMFTMirr && a.Type == attrData {
		mirrRuns = newRuns
	}
	writeRecord(t, v, img, mftRuns, mirrRuns, c)
	mftLCN, _ := lcnOf(mftRuns, 0)
	mirrLCN, _ := lcnOf(mirrRuns, 0)
	for _, off := range []uint64{0, v.TotalSectors * v.BytesPerSector} {
		binary.LittleEndian.PutUint64(img[off+0x30:], mftLCN)
		binary.LittleEndian.PutUint64(img[off+0x38:], mirrLCN)
	}
	return img
}

// findRunAt tells whether logical cluster lcn belongs to one of runs.
func findRunAt(runs []Run, lcn uint64) (Run, bool) {
	for _, r := range runs {
		if !r.Sparse && lcn >= r.LCN && lcn < r.LCN+r.Len {
			return r, true
		}
	}
	return Run{}, false
}

// goView is what go-ntfs, an independent parser, sees in a volume.
type goView struct {
	streams  map[string]string // rec/type/name/id -> size:hash
	owned    map[uint64]string // cluster -> owner
	problems []string
	bitmap   []byte
	clusters uint64
}

// readAllStream reads size bytes of a go-ntfs stream.
func readAllStream(s ntfs.RangeReaderAt, size int64) []byte {
	buf := make([]byte, size)
	var off int64
	for off < size {
		n, err := s.ReadAt(buf[off:], off)
		off += int64(n)
		if n == 0 || err != nil {
			break
		}
	}
	return buf[:off]
}

// goInspect walks every in-use base record with go-ntfs, hashes every
// attribute stream (all VCN segments joined), and builds the cluster
// ownership from go-ntfs' own run list decoder.
func goInspect(t *testing.T, img []byte) *goView {
	t.Helper()
	paged, err := ntfs.NewPagedReader(bytes.NewReader(img), 1024, 64)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ntfs.GetNTFSContext(paged, 0)
	if err != nil {
		t.Fatalf("go-ntfs cannot open: %v", err)
	}
	g := &goView{streams: map[string]string{}, owned: map[uint64]string{}}
	g.clusters = binary.LittleEndian.Uint64(img[0x28:]) * uint64(binary.LittleEndian.Uint16(img[0x0B:])) / uint64(c.ClusterSize)
	mft0, err := c.GetMFT(0)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := ntfs.OpenStream(c, mft0, ntfs.ATTR_TYPE_DATA, ntfs.WILDCARD_STREAM_ID, "")
	if err != nil {
		t.Fatal(err)
	}
	records := ntfs.RangeSize(ms) / c.RecordSize
	for id := int64(0); id < records; id++ {
		e, err := c.GetMFT(id)
		if err != nil || !e.Flags().IsSet("ALLOCATED") || e.Base_record_reference() != 0 {
			continue // extension records are covered through their base
		}
		seen := map[string]bool{}
		for _, a := range e.EnumerateAttributes(c) {
			typ := a.Type().Value
			name := a.Name()
			key := fmt.Sprintf("%d/%#x/%s/%d", id, typ, name, a.Attribute_id())
			if !a.IsResident() && !(id == 8 && name == "$Bad") {
				for i, r := range a.RunList() {
					if r.RelativeUrnOffset == 0 && !(i == 0 && id == 7) {
						continue // sparse
					}
					for cl := uint64(r.Offset); cl < uint64(r.Offset+r.Length); cl++ {
						if cl >= g.clusters {
							g.problems = append(g.problems, fmt.Sprintf("%s: cluster %d past the end (%d)", key, cl, g.clusters))
							break
						}
						if o, ok := g.owned[cl]; ok && o != key {
							g.problems = append(g.problems, fmt.Sprintf("cluster %d owned by %s and %s", cl, o, key))
							break
						}
						g.owned[cl] = key
					}
				}
			}
			if seen[key] || typ == 0x20 {
				continue
			}
			seen[key] = true
			var size int64
			if a.IsResident() {
				size = int64(a.Content_size())
			} else {
				if a.Runlist_vcn_start() != 0 {
					continue // later segment, read through the first
				}
				size = int64(a.Actual_size())
			}
			s, err := ntfs.OpenStream(c, e, typ, a.Attribute_id(), name)
			if err != nil {
				g.streams[key] = "open error: " + err.Error()
				continue
			}
			h := sha256.Sum256(readAllStream(s, size))
			g.streams[key] = fmt.Sprintf("%d:%x", size, h[:8])
		}
	}
	b6, _ := c.GetMFT(6)
	bs, err := ntfs.OpenStream(c, b6, ntfs.ATTR_TYPE_DATA, ntfs.WILDCARD_STREAM_ID, "")
	if err != nil {
		t.Fatal(err)
	}
	g.bitmap = readAllStream(bs, int64((g.clusters+7)/8))
	return g
}

// changedOnPurpose are the streams a shrink changes.
func changedOnPurpose(k string) bool {
	for _, p := range []string{"0/0x80/", "1/0x80/", "2/0x80/", "6/0x80/", "8/0x80/$Bad", "3/0x70/", "7/0x80/"} {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// verifyAgainst compares a shrunk image with its source using go-ntfs:
// every stream except the system files the shrink changes, cluster
// ownership against $Bitmap, $Boot, the boot sectors and $MFTMirr.
func verifyAgainst(t *testing.T, orig, out []byte, newClusters uint64) []string {
	t.Helper()
	var errs []string
	a := goInspect(t, orig)
	b := goInspect(t, out)
	if b.clusters != newClusters {
		errs = append(errs, fmt.Sprintf("boot sector: %d clusters, want %d", b.clusters, newClusters))
	}
	for k, h := range a.streams {
		if !changedOnPurpose(k) && b.streams[k] != h {
			errs = append(errs, fmt.Sprintf("stream %s: %s -> %s", k, h, b.streams[k]))
		}
	}
	for k := range b.streams {
		if _, ok := a.streams[k]; !ok {
			errs = append(errs, "new stream "+k)
		}
	}
	errs = append(errs, b.problems...)
	var extra int
	for cl := uint64(0); cl < b.clusters; cl++ {
		set := b.bitmap[cl/8]&(1<<(cl%8)) != 0
		if _, own := b.owned[cl]; own && !set {
			errs = append(errs, fmt.Sprintf("cluster %d owned by %s but free in $Bitmap", cl, b.owned[cl]))
		} else if set && !own {
			extra++
		}
	}
	if extra > 0 {
		errs = append(errs, fmt.Sprintf("%d clusters set in $Bitmap without owner", extra))
	}
	if !bytes.Equal(orig[512:vssHeaderOffset], out[512:vssHeaderOffset]) || !bytes.Equal(orig[vssHeaderOffset+512:8192], out[vssHeaderOffset+512:8192]) {
		errs = append(errs, "$Boot sectors changed")
	}
	for i := 0; i < 512; i++ {
		if (i < 0x28 || i >= 0x40) && orig[i] != out[i] {
			errs = append(errs, fmt.Sprintf("boot sector byte %#x changed", i))
		}
	}
	if nv, err := Open(bytes.NewReader(out)); err != nil {
		errs = append(errs, "reopen: "+err.Error())
	} else {
		mirr := nv.records[recMFTMirr].unnamed(attrData)
		m, e1 := nv.readVirtual(mirr.Runs, 0, 4*nv.RecordSize)
		f, e2 := nv.readVirtual(nv.mftRuns, 0, 4*nv.RecordSize)
		if e1 != nil || e2 != nil || !bytes.Equal(m, f) {
			errs = append(errs, "$MFTMirr differs from $MFT")
		}
	}
	bps := uint64(binary.LittleEndian.Uint16(out[0x0B:]))
	ts := binary.LittleEndian.Uint64(out[0x28:])
	if ts*bps+bps > uint64(len(out)) {
		errs = append(errs, "backup boot sector outside the partition")
	} else if !bytes.Equal(out[ts*bps:ts*bps+bps], out[:bps]) {
		errs = append(errs, "backup boot sector differs")
	}
	return errs
}
