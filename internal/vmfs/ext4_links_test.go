package vmfs

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"os"
	"strings"
	"testing"
)

// H-N5 (0.56): an ext4 guest with a link to itself (`ln -s x /x`) and two
// links pointing at each other must not overflow the stack (no recover()
// catches that); a relative link in a subfolder resolves in that folder.
// testdata/ext4-links.img.gz: mkfs.ext4 -O ^has_journal, 2 MiB, with
// x -> x, loop1 -> loop2 -> loop1, a/f, a/l -> f, abs -> /a/f.
func TestExt4SymlinkLoops(t *testing.T) {
	gz, err := os.ReadFile("testdata/ext4-links.img.gz")
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
	vols, err := Volumes(bytes.NewReader(img), int64(len(img)))
	if err != nil || len(vols) == 0 {
		t.Fatalf("volumes: %v %v", vols, err)
	}
	fsys, err := Open(vols[0])
	if err != nil {
		t.Fatal(err)
	}
	ents, err := fsys.List("/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name)
	}
	if got := strings.Join(names, ","); !strings.Contains(got, "x") || !strings.Contains(got, "loop1") {
		t.Errorf("root lists %s", got)
	}
	for _, p := range []string{"/x", "/loop1", "/loop2"} {
		_, _ = fsys.Stat(p) // must return, not crash
		if _, _, err := fsys.ReadFile(p); err == nil {
			t.Errorf("%s: read a link loop without an error", p)
		}
	}
	for _, p := range []string{"/a/l", "/abs"} {
		r, _, err := fsys.ReadFile(p)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		b, _ := io.ReadAll(r)
		if string(b) != "hello from a/f\n" {
			t.Errorf("%s reads %q", p, b)
		}
	}
}

// M-N8 (0.56): a crafted superblock (a 1 GiB block size, no inodes per
// group) is refused instead of allocating gigabytes or dividing by zero.
func TestExt4CraftedSuperblock(t *testing.T) {
	gz, err := os.ReadFile("testdata/ext4-links.img.gz")
	if err != nil {
		t.Fatal(err)
	}
	zr, _ := gzip.NewReader(bytes.NewReader(gz))
	orig, _ := io.ReadAll(zr)
	for name, patch := range map[string]func([]byte){
		"block size":       func(b []byte) { binary.LittleEndian.PutUint32(b[1024+24:], 20) },
		"inodes per group": func(b []byte) { binary.LittleEndian.PutUint32(b[1024+40:], 0) },
		"blocks per group": func(b []byte) { binary.LittleEndian.PutUint32(b[1024+32:], 0) },
	} {
		img := append([]byte(nil), orig...)
		patch(img)
		vols, err := Volumes(bytes.NewReader(img), int64(len(img)))
		if err != nil || len(vols) == 0 {
			continue // not even recognised: fine
		}
		if _, err := Open(vols[0]); err == nil {
			t.Errorf("%s: crafted superblock accepted", name)
		}
	}
}
