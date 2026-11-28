package vmfs

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

// hookFS is an FS whose files can be generated (path: content) and that
// calls before ahead of each file it reads.
type hookFS struct {
	FS
	before func(p string)
	gen    map[string]func() io.Reader
}

func (h hookFS) ReadFile(p string) (io.Reader, int64, error) {
	if h.before != nil {
		h.before(Clean(p))
	}
	if g, ok := h.gen[Clean(p)]; ok {
		return g(), 0, nil
	}
	return h.FS.ReadFile(p)
}

// zeroReader reads zeros without end.
type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) {
	clear(b)
	return len(b), nil
}

// sameContent compares the file p with r.
func sameContent(t *testing.T, p string, r io.Reader) {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	a, b := make([]byte, 1<<20), make([]byte, 1<<20)
	for off := 0; ; off += len(a) {
		n, errA := io.ReadFull(f, a)
		m, errB := io.ReadFull(r, b)
		if n != m || !bytes.Equal(a[:n], b[:m]) {
			t.Fatalf("content differs after %d bytes", off)
		}
		if errA != nil || errB != nil {
			return
		}
	}
}

// MED 6a (0.63): a sparse file of the guest was written as zeros. The
// /var/log/lastlog of a guest with a user of UID 4294967294 has an apparent
// size of about 1.2 TB, which filled the agent's disk. Runs of zeros are
// now holes; the file reads back the same.
func TestExtractKeepsFilesSparse(t *testing.T) {
	data := bytes.Repeat([]byte("lastlog!"), 512)
	content := func() io.Reader {
		return io.MultiReader(io.LimitReader(zeroReader{}, sparseMarkRun), bytes.NewReader(data), io.LimitReader(zeroReader{}, sparseMarkRun))
	}
	f := hookFS{FS: &ioFS{fsys: fstest.MapFS{"var/log/lastlog": {Data: []byte("x"), Mode: 0o644}}},
		gen: map[string]func() io.Reader{"/var/log/lastlog": content}}
	dir := t.TempDir()
	st, err := Extract(context.Background(), f, []string{"/var/log/lastlog"}, dir)
	if err != nil || len(st.Errors) > 0 {
		t.Fatal(err, st.Errors)
	}
	size := int64(2*sparseMarkRun + len(data))
	p := filepath.Join(dir, "var", "log", "lastlog")
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != size || st.Bytes != uint64(size) {
		t.Fatalf("size %d, %d bytes counted, want %d", fi.Size(), st.Bytes, size)
	}
	sameContent(t, p, content())
	if used := allocated(t, p); used >= size/2 {
		t.Errorf("%d of %d bytes allocated: the zeros were written", used, size)
	}
}

// A file without zeros, or ending in a short run of them, reads back the
// same through copySparse.
func TestCopySparseContent(t *testing.T) {
	for name, content := range map[string][]byte{
		"empty":      nil,
		"short":      []byte("data"),
		"block":      bytes.Repeat([]byte{7}, sparseBlock),
		"zero tail":  append(bytes.Repeat([]byte{1}, sparseBlock+5), make([]byte, sparseBlock+3)...),
		"only zeros": make([]byte, 3*sparseBlock+1),
	} {
		p := filepath.Join(t.TempDir(), "f")
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		n, err := copySparse(f, bytes.NewReader(content))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil || n != int64(len(content)) {
			t.Fatalf("%s: %d bytes, %v", name, n, err)
		}
		got, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(got, content) {
			t.Errorf("%s: content differs (%d bytes, want %d) %v", name, len(got), len(content), err)
		}
	}
}
