package imaging

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/imaging/ntfsresize"
	"github.com/max-zit/backupzit/internal/repo"
)

func TestBitmapUsage(t *testing.T) {
	cases := []struct {
		bm   []byte
		n    uint64
		used uint64
		end  uint64
	}{
		{[]byte{0, 0}, 16, 0, 0},
		{[]byte{0x01}, 8, 1, 1},
		{[]byte{0x80}, 8, 1, 8},
		{[]byte{0xFF, 0x03}, 16, 10, 10},
		{[]byte{0x0F, 0xF0}, 12, 4, 4}, // bits past n are ignored
		{[]byte{0x00, 0x00, 0x10}, 21, 1, 21},
		{[]byte{0x00, 0x00, 0x10}, 20, 0, 0},
	}
	for _, c := range cases {
		used, end := bitmapUsage(c.bm, c.n)
		if used != c.used || end != c.end {
			t.Errorf("bitmapUsage(%x, %d) = %d, %d; want %d, %d", c.bm, c.n, used, end, c.used, c.end)
		}
	}
}

func bootSector(bps uint16, spc byte, total uint64) []byte {
	b := make([]byte, 512)
	copy(b[3:], "NTFS    ")
	binary.LittleEndian.PutUint16(b[0x0B:], bps)
	b[0x0D] = spc
	binary.LittleEndian.PutUint64(b[0x28:], total)
	return b
}

func TestParseNTFSBoot(t *testing.T) {
	g, err := ParseNTFSBoot(bootSector(512, 8, 1000))
	if err != nil || g.ClusterSize != 4096 || g.Clusters() != 125 {
		t.Errorf("4K clusters: %+v, %v", g, err)
	}
	g, err = ParseNTFSBoot(bootSector(512, 0xF7, 1<<20)) // 2^9 sectors = 256 KiB
	if err != nil || g.ClusterSize != 256<<10 {
		t.Errorf("large clusters: %+v, %v", g, err)
	}
	if _, err := ParseNTFSBoot(make([]byte, 512)); !errors.Is(err, ErrNotNTFS) {
		t.Errorf("zero sector: %v", err)
	}
	fve := bootSector(512, 8, 1000)
	copy(fve[3:], "-FVE-FS-")
	if _, err := ParseNTFSBoot(fve); !errors.Is(err, ErrNotNTFS) {
		t.Errorf("BitLocker sector: %v", err)
	}
	if _, err := ParseNTFSBoot(bootSector(500, 8, 1000)); !errors.Is(err, ErrNotNTFS) {
		t.Errorf("odd sector size: %v", err)
	}
}

// goNTFSTestImage returns the small NTFS image shipped with go-ntfs.
func goNTFSTestImage(t *testing.T) string {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "www.velocidex.com/golang/go-ntfs").Output()
	if err != nil {
		t.Skip("go-ntfs module directory not available:", err)
	}
	p := filepath.Join(strings.TrimSpace(string(out)), "parser", "test_data", "test.ntfs.dd")
	if _, err := os.Stat(p); err != nil {
		t.Skip("go-ntfs test image not available")
	}
	return p
}

func TestMeasureNTFS(t *testing.T) {
	f, err := os.Open(goNTFSTestImage(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	u, err := MeasureNTFS(f)
	if err != nil {
		t.Fatal(err)
	}
	g := u.Geometry
	vol := g.TotalSectors * uint64(g.BytesPerSector)
	t.Logf("cluster %d, clusters %d, used %d bytes, min size %d of %d", g.ClusterSize, g.Clusters(), u.UsedBytes, u.MinSize, vol)
	if vol > uint64(fi.Size()) {
		t.Errorf("volume (%d) larger than the image (%d)", vol, fi.Size())
	}
	if u.UsedBytes == 0 || u.UsedBytes > vol {
		t.Errorf("used bytes %d", u.UsedBytes)
	}
	if u.MinSize < u.UsedBytes || u.MinSize > vol+uint64(g.BytesPerSector) {
		t.Errorf("min size %d outside [%d, %d]", u.MinSize, u.UsedBytes, vol)
	}
	if u.MinSize%LayoutAlign != 0 && u.MinSize != vol+uint64(g.BytesPerSector) {
		t.Errorf("min size %d not aligned", u.MinSize)
	}
}

type bytesReaderAt []byte

func (b bytesReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(b)) {
		return 0, errors.New("EOF")
	}
	return copy(p, b[off:]), nil
}

func TestIsBitLockerRaw(t *testing.T) {
	fve := make([]byte, 512)
	copy(fve[3:], "-FVE-FS-")
	if !IsBitLockerRaw(bytesReaderAt(fve)) {
		t.Error("BitLocker header not recognised")
	}
	if IsBitLockerRaw(bytesReaderAt(bootSector(512, 8, 1000))) {
		t.Error("NTFS recognised as BitLocker")
	}
	if IsBitLockerRaw(bytesReaderAt(nil)) {
		t.Error("empty reader recognised as BitLocker")
	}
}

func TestLimitsGiveAReasonCode(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		p    repo.PartitionImage
		want LimitReason
	}{
		{repo.PartitionImage{Included: false}, ReasonNotBackedUp},
		{repo.PartitionImage{Included: true, GPTType: GPTTypeEFI}, ReasonFixedSize},
		{repo.PartitionImage{Included: true, GPTType: GPTTypeMSR}, ReasonFixedSize},
		{repo.PartitionImage{Included: true, MBRType: 0x27}, ReasonFixedSize},
	} {
		lim := LimitsForPartition(ctx, nil, &c.p)
		if lim.Resizable || lim.Why != c.want || lim.Reason == "" {
			t.Errorf("%+v: got %+v, want reason %q", c.p, lim, c.want)
		}
	}
}

// Only images read from a VSS snapshot may have data moved; anything else
// (live, offline, images from before the source was recorded) shrinks
// without moving data and needs a clean $LogFile.
func TestShrinkOptions(t *testing.T) {
	for src, want := range map[string]ntfsresize.PlanOptions{
		"vss":  {FromSnapshot: true},
		"live": {NoMove: true},
		"":     {NoMove: true},
	} {
		got := ShrinkOptions(&repo.PartitionImage{Source: src})
		if got.FromSnapshot != want.FromSnapshot || got.NoMove != want.NoMove {
			t.Errorf("%q: %+v, want %+v", src, got, want)
		}
	}
}

// S2: the minimum size shown before a restore is one the restore's
// planning accepts, with the options the restore uses, and so are larger
// sizes.
func TestShrinkLimitsArePromises(t *testing.T) {
	f, err := os.Open(goNTFSTestImage(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	u, err := MeasureNTFS(f)
	if err != nil {
		t.Fatal(err)
	}
	length := u.Geometry.TotalSectors*uint64(u.Geometry.BytesPerSector) + uint64(u.Geometry.BytesPerSector)
	var mins []uint64
	for _, source := range []string{"vss", "live"} {
		p := &repo.PartitionImage{Number: 1, Length: length, Included: true, FileSystem: "NTFS", Source: source}
		lim := shrinkLimits(f, p, u)
		if !lim.Resizable || lim.Reason != "" || lim.MinSize >= length || lim.MinSize > lim.ShrinkableTo {
			t.Fatalf("%s: %+v", source, lim)
		}
		for _, size := range []uint64{lim.MinSize, lim.MinSize + LayoutAlign/2, lim.ShrinkableTo} {
			vol, err := ntfsresize.Open(f)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := vol.PlanWith(size, ShrinkOptions(p)); err != nil {
				t.Errorf("%s: limits say %d, planning refuses %d: %v", source, lim.MinSize, size, err)
			}
		}
		mins = append(mins, lim.MinSize)
	}
	if mins[0] >= mins[1] {
		t.Errorf("moving data (vss) gives no smaller minimum: %d, without moving %d", mins[0], mins[1])
	}
}

// markDirty sets the dirty flag in $VOLUME_INFORMATION of $Volume (file
// record 3) of the NTFS image b, as Windows does for a volume that needs a
// check. It skips the test when the flags would sit on a fixup position.
func markDirty(t *testing.T, b []byte) {
	t.Helper()
	bps := uint64(binary.LittleEndian.Uint16(b[0x0B:]))
	cs := bps * uint64(b[0x0D])
	mft := binary.LittleEndian.Uint64(b[0x30:]) * cs
	rs := uint64(b[0x40]) * cs
	if c := int8(b[0x40]); c < 0 {
		rs = 1 << uint(-c)
	}
	rec := b[mft+3*rs : mft+4*rs]
	for off := int(binary.LittleEndian.Uint16(rec[0x14:])); off+0x18 <= len(rec); {
		typ, n := binary.LittleEndian.Uint32(rec[off:]), int(binary.LittleEndian.Uint32(rec[off+4:]))
		if typ == 0xFFFFFFFF || n <= 0 {
			break
		}
		if typ == 0x70 {
			at := off + int(binary.LittleEndian.Uint16(rec[off+0x14:])) + 0x0A
			if uint64(at+2)%bps == 0 || uint64(at+1)%bps == 0 {
				t.Skip("the volume flags sit on a fixup position of the test image")
			}
			binary.LittleEndian.PutUint16(rec[at:], binary.LittleEndian.Uint16(rec[at:])|1)
			return
		}
		off += n
	}
	t.Fatal("no $VOLUME_INFORMATION in file record 3 of the test image")
}

// #61 finding 1: a volume that cannot be shrunk (here: marked dirty) stays
// resizable, since it can still grow, and its limits say why it keeps its
// size, with a code the recovery program translates.
func TestShrinkLimitsOfAnUncleanVolumeSayWhy(t *testing.T) {
	b, err := os.ReadFile(goNTFSTestImage(t))
	if err != nil {
		t.Fatal(err)
	}
	markDirty(t, b)
	r := bytes.NewReader(b)
	u, err := MeasureNTFS(r)
	if err != nil {
		t.Fatal(err)
	}
	length := u.Geometry.TotalSectors*uint64(u.Geometry.BytesPerSector) + uint64(u.Geometry.BytesPerSector)
	for _, source := range []string{"vss", "live"} {
		p := &repo.PartitionImage{Number: 1, Length: length, Included: true, FileSystem: "NTFS", Source: source}
		lim := shrinkLimits(r, p, u)
		if !lim.Resizable || lim.Why != ReasonNoShrink || !strings.HasPrefix(lim.Reason, NoShrinkPrefix) || !strings.Contains(lim.Reason, "dirty") {
			t.Errorf("%s: %+v", source, lim)
		}
		if lim.MinSize <= u.MinSize || lim.MinSize > length {
			t.Errorf("%s: minimum %d, want the size of the volume (used area ends at %d, partition %d)", source, lim.MinSize, u.MinSize, length)
		}
	}
}
