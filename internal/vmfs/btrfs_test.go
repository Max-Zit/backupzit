package vmfs

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash/crc32"
	"io"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/cespare/xxhash/v2"
)

// The btrfs images in testdata were made in the lab (PVE, kernel 7.0,
// btrfs-progs 6.14) by mounting them and writing files, so the trees,
// compressed extents and subvolumes are the kernel's own:
//
//	a: default mkfs (DUP metadata, crc32c, no-holes), 128 MiB: zstd, LZO and
//	   zlib folders (inline, multi-extent and partly overwritten files),
//	   uncompressed files with holes and preallocated space, 400 files in one
//	   folder, links, a Unicode name, subvolumes @home, @home/nested and
//	   data/vol, and a read-only snapshot .snapshots/home-1 of @home.
//	b: --mixed, explicit hole extents, xxhash, LZO.
//	c, d: --mixed, SINGLE metadata, sha256 / blake2, zlib.
//
// btrfs-<x>.sha256 is sha256sum of every file, made on the mounted file
// system.

func btrfsImage(t testing.TB, name string) []byte {
	t.Helper()
	gz, err := os.ReadFile("testdata/btrfs-" + name + ".img.gz")
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	img, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func btrfsManifest(t *testing.T, name string) map[string]string {
	t.Helper()
	f, err := os.Open("testdata/btrfs-" + name + ".sha256")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		sum, p, ok := strings.Cut(sc.Text(), "  ")
		if !ok {
			t.Fatalf("manifest line %q", sc.Text())
		}
		out[strings.TrimPrefix(p, ".")] = sum
	}
	return out
}

func openBtrfsImage(t testing.TB, img []byte) (FS, Volume) {
	t.Helper()
	vols, err := Volumes(bytes.NewReader(img), int64(len(img)))
	if err != nil || len(vols) != 1 {
		t.Fatalf("volumes: %+v %v", vols, err)
	}
	if vols[0].FS != "btrfs" || !vols[0].Browsable() {
		t.Fatalf("volume %+v is not browsable btrfs", vols[0])
	}
	fsys, err := Open(vols[0])
	if err != nil {
		t.Fatal(err)
	}
	return fsys, vols[0]
}

// btrfsTreeWalk lists every folder; it returns the folders and files.
func btrfsTreeWalk(t testing.TB, fsys FS) (dirs []string, files []Entry) {
	var walk func(string, int)
	walk = func(dir string, depth int) {
		if depth > 20 {
			t.Fatalf("%s: too deep", dir)
		}
		es, err := fsys.List(dir)
		if err != nil {
			t.Fatalf("list %s: %v", dir, err)
		}
		for _, e := range es {
			if e.IsDir {
				dirs = append(dirs, e.Path)
				walk(e.Path, depth+1)
			} else {
				files = append(files, e)
			}
		}
	}
	walk("/", 0)
	sort.Strings(dirs)
	return dirs, files
}

func sha256Of(t *testing.T, fsys FS, p string) string {
	t.Helper()
	r, size, err := fsys.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil || n != size {
		t.Fatalf("read %s: %d of %d bytes: %v", p, n, size, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestBtrfsImages(t *testing.T) {
	for _, tc := range []struct{ name, label string }{{"a", "bzdata"}, {"b", "mixed"}, {"c", ""}, {"d", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			fsys, vol := openBtrfsImage(t, btrfsImage(t, tc.name))
			if vol.Label != tc.label {
				t.Errorf("label %q, want %q", vol.Label, tc.label)
			}
			want := btrfsManifest(t, tc.name)
			_, files := btrfsTreeWalk(t, fsys)
			seen := map[string]bool{}
			for _, e := range files {
				seen[e.Path] = true
				if _, ok := want[e.Path]; !ok && !strings.HasPrefix(e.Path, "/link-") {
					t.Errorf("listed %s, which the file system does not have", e.Path)
				}
			}
			for p, sum := range want {
				if !seen[p] {
					t.Errorf("%s is not listed", p)
				}
				if got := sha256Of(t, fsys, p); got != sum {
					t.Errorf("%s: sha256 %s, want %s", p, got, sum)
				}
			}
		})
	}
}

func TestBtrfsSubvolumesAndLinks(t *testing.T) {
	fsys, _ := openBtrfsImage(t, btrfsImage(t, "a"))
	dirs, _ := btrfsTreeWalk(t, fsys)
	// find . -type d on the mounted top level (without ".").
	want := []string{"/.snapshots", "/.snapshots/home-1", "/.snapshots/home-1/nested", "/.snapshots/home-1/user",
		"/@home", "/@home/nested", "/@home/user", "/data", "/data/vol", "/lzo", "/many", "/plain", "/zlib", "/zstd"}
	if strings.Join(dirs, ",") != strings.Join(want, ",") {
		t.Errorf("folders\n%v\nwant\n%v", dirs, want)
	}
	// The snapshot keeps the old notes; the nested subvolume is an empty
	// folder in it (a snapshot does not take subvolumes inside along).
	read := func(p string) string {
		r, _, err := fsys.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		b, _ := io.ReadAll(r)
		return string(b)
	}
	if got := read("/.snapshots/home-1/user/notes.txt"); got != "notes before snapshot\n" {
		t.Errorf("snapshot notes %q", got)
	}
	if got := read("/@home/user/notes.txt"); got != "notes after snapshot\n" {
		t.Errorf("notes %q", got)
	}
	if es, err := fsys.List("/.snapshots/home-1/nested"); err != nil || len(es) != 0 {
		t.Errorf("nested subvolume in the snapshot: %v %v", es, err)
	}
	if got := read("/@home/nested/inner.txt"); got != "nested\n" {
		t.Errorf("nested %q", got)
	}
	// Folder IDs differ between subvolumes (each subvolume's top folder is
	// inode 256), so Visited does not take one for another.
	ids := map[uint64]string{}
	for _, p := range []string{"/zstd", "/@home", "/@home/nested", "/.snapshots/home-1", "/data/vol"} {
		e, err := fsys.Stat(p)
		if err != nil || !e.IsDir || e.ID == 0 {
			t.Fatalf("stat %s: %+v %v", p, e, err)
		}
		if other, ok := ids[e.ID]; ok {
			t.Errorf("%s and %s have the same ID", p, other)
		}
		ids[e.ID] = p
	}
	// Links: relative, absolute, and a hard link with the same contents.
	if read("/link-rel") != read("/zstd/text.txt") || read("/link-abs") != "small zstd\n" || read("/hardlink.txt") != "small zstd\n" {
		t.Error("links read wrong contents")
	}
	if e, err := fsys.Stat("/link-rel"); err != nil || e.IsDir || e.Size != int64(len(read("/zstd/text.txt"))) {
		t.Errorf("stat link: %+v %v", e, err)
	}
	if _, err := fsys.Stat("/no/such/file"); err == nil {
		t.Error("a missing path has no error")
	}
	if _, _, err := fsys.ReadFile("/zstd"); err == nil {
		t.Error("reading a folder has no error")
	}
	e, err := fsys.Stat("/plain/random.bin")
	if err != nil || e.Size != 65536 || e.Perm != 0o644 || e.ModTime.Year() < 2026 {
		t.Errorf("stat: %+v %v", e, err)
	}
}

// The second DUP copy and the superblock copy are used when the first
// ones are damaged.
func TestBtrfsDamagedCopies(t *testing.T) {
	img := btrfsImage(t, "a")
	b, err := openBtrfs(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	t5 := b.top
	c := b.chunkAt(t5.bytenr)
	if c == nil || len(c.stripes) != 2 {
		t.Fatalf("FS tree chunk %+v, want two copies (DUP)", c)
	}
	phys := c.stripes[0] + (t5.bytenr - c.logical)
	clear(img[phys+200 : phys+300])
	copy(img[btrfsSuperOffset:], "damaged")
	fsys, _ := openBtrfsImage(t, img)
	if got := sha256Of(t, fsys, "/hello.txt"); got != "01db95a898efe46228f4a18e3bcaf0b07c96d5303e8634f4c783995c12f70299" {
		t.Errorf("hello.txt: %s", got)
	}
	// Both copies damaged: an error, not a crash.
	phys2 := c.stripes[1] + (t5.bytenr - c.logical)
	clear(img[phys2+200 : phys2+300])
	if fsys, err := Open(Volume{FS: "btrfs", Size: int64(len(img)), r: bytes.NewReader(img)}); err == nil {
		if _, err := fsys.List("/"); err == nil {
			t.Error("a file system tree damaged in both copies reads without an error")
		}
	}
}

func TestBtrfsRefused(t *testing.T) {
	img := btrfsImage(t, "c")
	sb := img[btrfsSuperOffset : btrfsSuperOffset+btrfsSuperSize]
	fix := func() {
		sum := sha256.Sum256(sb[32:])
		copy(sb, sum[:])
	}
	binary.LittleEndian.PutUint64(sb[0x88:], 2)
	fix()
	if _, err := openBtrfs(bytes.NewReader(img), int64(len(img))); err == nil || !strings.Contains(err.Error(), "2 devices") {
		t.Errorf("two devices: %v", err)
	}
	binary.LittleEndian.PutUint64(sb[0x88:], 1)
	binary.LittleEndian.PutUint64(sb[0xbc:], binary.LittleEndian.Uint64(sb[0xbc:])|btrfsExtentTreeV2)
	fix()
	if _, err := openBtrfs(bytes.NewReader(img), int64(len(img))); err == nil || !strings.Contains(err.Error(), "features") {
		t.Errorf("extent tree v2: %v", err)
	}
}

func TestBtrfsNameHash(t *testing.T) {
	// DIR_ITEM keys of the image: every name of a folder must be found
	// through its hash.
	fsys, _ := openBtrfsImage(t, btrfsImage(t, "a"))
	for _, p := range []string{"/many/f000", "/many/f399", "/čćžšđ ünï.txt", "/.snapshots/home-1/user/notes.txt"} {
		if _, err := fsys.Stat(p); err != nil {
			t.Errorf("stat %s: %v", p, err)
		}
	}
}

func TestLZO1X(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
		want string
	}{
		{"literals", append([]byte{17 + 5}, "hello\x11\x00\x00"...), "hello"},
		{"matches", []byte{
			20, 'a', 'b', 'c', // 3 literals
			36, 8, 0, // copy 6 bytes from 3 back
			0x41, 1, 'Z', // copy 3 bytes from 9 back, then 1 literal
			0, 0, // after one literal: copy 2 bytes from 1 back
			0x11, 0, 0}, "abcabcabcabcZZZ"},
		{"long literal run", append(append([]byte{17 + 1, 'x', 0x40, 0, 0, 2}, strings.Repeat("y", 20)...), 0x11, 0, 0), "xxxx" + strings.Repeat("y", 20)},
		{"end only", []byte{0x11, 0, 0}, ""},
	} {
		dst := make([]byte, 64)
		n, err := lzo1xDecompress(tc.in, dst)
		if err != nil || string(dst[:n]) != tc.want {
			t.Errorf("%s: %q %v, want %q", tc.name, dst[:n], err, tc.want)
		}
	}
	for _, bad := range [][]byte{
		{},
		{20, 'a'},                        // literals past the input
		{20, 'a', 'b', 'c', 36, 0x20, 0}, // a match before the output
		{17 + 3, 'a', 'b', 'c'},          // no end marker
		{17, 1, 0, 0, 0},                 // lzo-rle header
		append([]byte{17 + 60}, make([]byte, 60)...), // more than the output holds
	} {
		dst := make([]byte, 32)
		if _, err := lzo1xDecompress(bad, dst); err == nil {
			t.Errorf("%v: no error", bad)
		}
	}
}

// btrfsBlocks finds the tree blocks of an image (their physical offsets).
func btrfsBlocks(t testing.TB, img []byte) (*btrfsFS, []int) {
	b, err := openBtrfs(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	size := int(b.sb.nodeSize)
	var out []int
	for off := 0; off+size <= len(img); off += int(b.sb.sectorSize) {
		blk := img[off : off+size]
		if bytes.Equal(blk[0x20:0x30], b.sb.fsid[:]) && btrfsSumOK(b.sb.csumType, blk) {
			out = append(out, off)
		}
	}
	if len(out) == 0 {
		t.Fatal("no tree blocks found")
	}
	return b, out
}

// btrfsPatch changes a tree block and fixes its checksum, so the change
// reaches the parsers instead of failing the checksum.
func btrfsPatch(b *btrfsFS, blk []byte, at int, patch []byte) {
	copy(blk[32+at%(len(blk)-32):], patch)
	switch b.sb.csumType {
	case 0:
		binary.LittleEndian.PutUint32(blk, crc32.Checksum(blk[32:], castagnoli))
	case 1:
		binary.LittleEndian.PutUint64(blk, xxhash.Sum64(blk[32:]))
	}
}

func btrfsExercise(fsys FS) {
	var walk func(string, int)
	walk = func(dir string, depth int) {
		es, err := fsys.List(dir)
		if err != nil || depth > 8 {
			return
		}
		for i, e := range es {
			if i > 50 {
				return
			}
			if e.IsDir {
				walk(e.Path, depth+1)
				continue
			}
			if r, _, err := fsys.ReadFile(e.Path); err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(r, 4<<20))
			}
		}
	}
	walk("/", 0)
}

// A guest controls its btrfs trees: changed fields in any tree block must
// give errors, never a crash or a hang.
func TestBtrfsCraftedBlocks(t *testing.T) {
	img := btrfsImage(t, "b")
	b, blocks := btrfsBlocks(t, img)
	size := int(b.sb.nodeSize)
	for _, off := range blocks {
		orig := append([]byte(nil), img[off:off+size]...)
		for at := 0x30; at < 600; at += 13 {
			for _, v := range []byte{0x00, 0xff} {
				btrfsPatch(b, img[off:off+size], at-32, []byte{v, v})
				if f, err := Open(Volume{FS: "btrfs", Size: int64(len(img)), r: bytes.NewReader(img)}); err == nil {
					btrfsExercise(f)
				}
				copy(img[off:], orig)
			}
		}
	}
}

func FuzzBtrfsBlock(f *testing.F) {
	img := btrfsImage(f, "b")
	b, blocks := btrfsBlocks(f, img)
	size := int(b.sb.nodeSize)
	f.Add(uint16(0), uint16(0x60), []byte{0xff, 0xff, 0, 0})
	f.Add(uint16(1), uint16(120), []byte{1, 2, 3, 4, 5, 6, 7, 8})
	f.Fuzz(func(t *testing.T, blk, at uint16, patch []byte) {
		off := blocks[int(blk)%len(blocks)]
		orig := append([]byte(nil), img[off:off+size]...)
		defer copy(img[off:], orig)
		btrfsPatch(b, img[off:off+size], int(at), patch)
		if f, err := Open(Volume{FS: "btrfs", Size: int64(len(img)), r: bytes.NewReader(img)}); err == nil {
			btrfsExercise(f)
		}
	})
}

func FuzzLZO1X(f *testing.F) {
	f.Add([]byte{20, 'a', 'b', 'c', 36, 8, 0, 0x41, 1, 'Z', 0, 0, 0x11, 0, 0})
	f.Add([]byte{17 + 1, 'x', 0x40, 0, 0, 2, 'y', 0x11, 0, 0})
	f.Fuzz(func(t *testing.T, src []byte) {
		dst := make([]byte, 4096)
		n, _ := lzo1xDecompress(src, dst)
		if n < 0 || n > len(dst) {
			t.Fatalf("wrote %d bytes", n)
		}
		out := make([]byte, 8192)
		_, _ = btrfsLZO(src, out, 4096)
	})
}
