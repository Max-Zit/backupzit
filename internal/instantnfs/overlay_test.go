package instantnfs

import (
	"bytes"
	"math/rand"
	"path/filepath"
	"testing"
)

func TestOverlay(t *testing.T) {
	const size = 5*blockSize + 1234
	base := make([]byte, size)
	rand.New(rand.NewSource(1)).Read(base)
	want := append([]byte{}, base...)
	dir := t.TempDir()
	open := func() *Overlay {
		o, err := OpenOverlay(bytes.NewReader(base), size, filepath.Join(dir, "d-flat.vmdk"), filepath.Join(dir, "d.map"))
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	o := open()
	rng := rand.New(rand.NewSource(2))
	for i := 0; i < 200; i++ {
		off := rng.Int63n(size - 1)
		n := 1 + rng.Intn(int(min(3*blockSize, size-off)))
		p := make([]byte, n)
		rng.Read(p)
		if _, err := o.WriteAt(p, off); err != nil {
			t.Fatal(err)
		}
		copy(want[off:], p)
	}
	check := func(o *Overlay) {
		got := make([]byte, size)
		if n, err := o.ReadAt(got, 0); n != size || err != nil {
			t.Fatalf("read %d %v", n, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatal("content differs")
		}
		// Unaligned reads across written and unwritten blocks.
		for i := 0; i < 100; i++ {
			off := rng.Int63n(size)
			p := make([]byte, 1+rng.Intn(2*blockSize))
			n, _ := o.ReadAt(p, off)
			if !bytes.Equal(p[:n], want[off:off+int64(n)]) {
				t.Fatalf("read at %d differs", off)
			}
		}
	}
	check(o)
	o.Close()
	o = open() // changes survive a restart
	defer o.Close()
	check(o)
}
