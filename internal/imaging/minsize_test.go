package imaging

import (
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBitmapUsage(t *testing.T) {
	cases := []struct {
		bm   []byte
		n    uint64
		used uint64
		last int64
	}{
		{[]byte{0, 0}, 16, 0, -1},
		{[]byte{0x01}, 8, 1, 0},
		{[]byte{0x80}, 8, 1, 7},
		{[]byte{0xFF, 0x03}, 16, 10, 9},
		{[]byte{0x0F, 0xF0}, 12, 4, 3}, // bits past n are ignored
		{[]byte{0x00, 0x00, 0x10}, 21, 1, 20},
	}
	for _, c := range cases {
		used, last := bitmapUsage(c.bm, c.n)
		if used != c.used || last != c.last {
			t.Errorf("bitmapUsage(%x, %d) = %d, %d; want %d, %d", c.bm, c.n, used, last, c.used, c.last)
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
