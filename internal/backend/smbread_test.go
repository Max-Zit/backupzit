package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeFile is an open file of fakeShare.
type fakeFile struct {
	sh   *fakeShare
	name string
	data []byte
	size int64 // what Stat reports
	once sync.Once
}

func (f *fakeFile) ReadAt(p []byte, off int64) (int, error) {
	if f.sh.failRead {
		return 0, errors.New("connection reset")
	}
	if off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *fakeFile) Stat() (fs.FileInfo, error) { return fakeInfo(f.size), nil }

func (f *fakeFile) Close() error {
	f.once.Do(func() {
		f.sh.mu.Lock()
		f.sh.open[f.name]--
		f.sh.mu.Unlock()
	})
	return nil
}

type fakeInfo int64

func (i fakeInfo) Name() string       { return "f" }
func (i fakeInfo) Size() int64        { return int64(i) }
func (i fakeInfo) Mode() fs.FileMode  { return 0 }
func (i fakeInfo) ModTime() time.Time { return time.Time{} }
func (i fakeInfo) IsDir() bool        { return false }
func (i fakeInfo) Sys() any           { return nil }

// fakeShare counts the opens and the handles open per name.
type fakeShare struct {
	mu       sync.Mutex
	files    map[string][]byte
	sizes    map[string]int64 // a size Stat reports instead of the real one
	opens    map[string]int
	open     map[string]int
	failRead bool
}

func newFakeSMB() (*SMB, *fakeShare) {
	sh := &fakeShare{files: map[string][]byte{}, sizes: map[string]int64{}, opens: map[string]int{}, open: map[string]int{}}
	s := &SMB{}
	s.openRO = func(name string) (smbFile, error) {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		d, ok := sh.files[name]
		if !ok {
			return nil, fs.ErrNotExist
		}
		size, ok := sh.sizes[name]
		if !ok {
			size = int64(len(d))
		}
		sh.opens[name]++
		sh.open[name]++
		return &fakeFile{sh: sh, name: name, data: d, size: size}, nil
	}
	return s, sh
}

func (sh *fakeShare) counts(name string) (opens, open int) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.opens[name], sh.open[name]
}

// Blocks of one pack are read through one open handle; other objects are
// opened per read; at most smbHandleCache packs stay open, and an idle
// handle is closed soon, so a prune elsewhere can delete the pack (#71).
func TestSMBPackHandles(t *testing.T) {
	ctx := context.Background()
	s, sh := newFakeSMB()
	s.handles.idleFor = 30 * time.Millisecond
	pack := bytes.Repeat([]byte("0123456789"), 100)
	for i := range 12 {
		sh.files[fmt.Sprintf("data/p%02d", i)] = pack
	}
	sh.files["index/x"] = []byte("index")
	for off := 0; off < 1000; off += 100 {
		b, err := s.LoadRange(ctx, "data/p00", int64(off), 100)
		if err != nil || !bytes.Equal(b, pack[off:off+100]) {
			t.Fatalf("read at %d: %v", off, err)
		}
	}
	if b, err := s.LoadRange(ctx, "data/p00", -10, 10); err != nil || string(b) != "0123456789" {
		t.Fatalf("read from the end: %q %v", b, err)
	}
	if opens, open := sh.counts("data/p00"); opens != 1 || open != 1 {
		t.Errorf("one pack read 11 times: %d opens, %d open; want 1 and 1", opens, open)
	}
	for range 2 {
		if _, err := s.LoadRange(ctx, "index/x", 0, 5); err != nil {
			t.Fatal(err)
		}
	}
	if opens, open := sh.counts("index/x"); opens != 2 || open != 0 {
		t.Errorf("an index file: %d opens, %d open; want 2 and 0", opens, open)
	}
	for i := 1; i < 12; i++ {
		if _, err := s.LoadRange(ctx, fmt.Sprintf("data/p%02d", i), 0, 10); err != nil {
			t.Fatal(err)
		}
	}
	total := 0
	for i := range 12 {
		_, open := sh.counts(fmt.Sprintf("data/p%02d", i))
		total += open
	}
	if total != smbHandleCache {
		t.Errorf("%d pack handles open, want %d", total, smbHandleCache)
	}
	if _, open := sh.counts("data/p00"); open != 0 {
		t.Error("the oldest pack handle is still open")
	}
	deadline := time.Now().Add(5 * time.Second)
	for total > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		total = 0
		for i := range 12 {
			_, open := sh.counts(fmt.Sprintf("data/p%02d", i))
			total += open
		}
	}
	if total != 0 {
		t.Errorf("%d idle pack handles still open", total)
	}
}

// A handle in use is closed after its read, also when it is forgotten
// meanwhile; a failed read drops the handle, and Close closes all.
func TestSMBPackHandleLifetimes(t *testing.T) {
	ctx := context.Background()
	s, sh := newFakeSMB()
	sh.files["data/a"] = []byte("aaaa")
	sh.files["data/b"] = []byte("bbbb")
	h, err := s.acquire("data/a")
	if err != nil {
		t.Fatal(err)
	}
	s.forget("data/a") // as Remove and Save do
	if _, open := sh.counts("data/a"); open != 1 {
		t.Error("a handle in use was closed")
	}
	s.release(h)
	if _, open := sh.counts("data/a"); open != 0 {
		t.Error("a forgotten handle stays open after its read")
	}
	sh.failRead = true
	if _, err := s.LoadRange(ctx, "data/b", 0, 4); err == nil {
		t.Fatal("no error")
	}
	sh.failRead = false
	if _, open := sh.counts("data/b"); open != 0 {
		t.Error("the handle of a failed read stays open")
	}
	if _, err := s.LoadRange(ctx, "data/b", 0, 4); err != nil {
		t.Fatal(err)
	}
	s.closeHandles()
	if _, open := sh.counts("data/b"); open != 0 {
		t.Error("Close left a handle open")
	}
	if _, err := s.LoadRange(ctx, "data/b", 0, 4); err != nil {
		t.Fatal(err)
	}
	if _, open := sh.counts("data/b"); open != 0 {
		t.Error("a read after Close keeps its handle")
	}
	if _, err := s.LoadRange(ctx, "data/missing", 0, 4); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing pack: %v", err)
	}
}

// Load reads with one ReadAt sized from the file; a size the file does not
// have, a huge or negative one, is no allocation of that size (#71 review).
func TestSMBLoadSizes(t *testing.T) {
	ctx := context.Background()
	s, sh := newFakeSMB()
	sh.files["data/ok"] = []byte("whole pack")
	sh.files["data/huge"] = []byte("small really")
	sh.sizes["data/huge"] = 1 << 50
	sh.files["data/neg"] = []byte("negative")
	sh.sizes["data/neg"] = -1
	sh.files["data/short"] = []byte("short")
	sh.sizes["data/short"] = 100
	if b, err := s.Load(ctx, "data/ok"); err != nil || string(b) != "whole pack" {
		t.Errorf("load: %q %v", b, err)
	}
	if b, err := s.Load(ctx, "data/huge"); err != nil || string(b) != "small really" {
		t.Errorf("huge size: %q %v", b, err)
	}
	if b, err := s.Load(ctx, "data/neg"); err != nil || string(b) != "negative" {
		t.Errorf("negative size: %q %v", b, err)
	}
	if _, err := s.Load(ctx, "data/short"); err == nil || !strings.Contains(err.Error(), "unexpected EOF") {
		t.Errorf("a file shorter than its size: %v", err)
	}
	if _, err := s.LoadMax(ctx, "data/huge", 1<<20); !errors.Is(err, ErrTooLarge) {
		t.Errorf("LoadMax of a huge size: %v", err)
	}
	if b, err := s.LoadMax(ctx, "data/neg", 4); !errors.Is(err, ErrTooLarge) {
		t.Errorf("LoadMax of a negative size and more data: %q %v", b, err)
	}
	if b, err := s.LoadMax(ctx, "data/ok", 100); err != nil || string(b) != "whole pack" {
		t.Errorf("LoadMax: %q %v", b, err)
	}
	for name, o := range map[string]int{"data/ok": 0, "data/huge": 0, "data/neg": 0, "data/short": 0} {
		if _, open := sh.counts(name); open != o {
			t.Errorf("%s: %d handles open after Load", name, open)
		}
	}
}

// On a real share: a pack this backend read (its handle cached) can be
// deleted by another client once the handle has been idle, and saving
// over it through this backend works at once (#71 review).
func TestSMBCachedHandleLetsOthersDelete(t *testing.T) {
	be, u, opts := testSMB(t)
	other, err := OpenSMB(context.Background(), u, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	ctx := context.Background()
	data := bytes.Repeat([]byte("pack "), 20000)
	if err := be.Save(ctx, "data/aa/pack", data); err != nil {
		t.Fatal(err)
	}
	if b, err := be.LoadRange(ctx, "data/aa/pack", 5, 10); err != nil || string(b) != "pack pack " {
		t.Fatalf("read: %q %v", b, err)
	}
	if err := be.Save(ctx, "data/aa/pack", data); err != nil {
		t.Fatalf("save over a pack this backend has open: %v", err)
	}
	if _, err := be.LoadRange(ctx, "data/aa/pack", 0, 10); err != nil {
		t.Fatal(err)
	}
	if err := other.Remove(ctx, "data/aa/pack"); err != nil {
		t.Logf("another client removing the pack while the handle is open: %v", err)
	}
	time.Sleep(smbHandleIdle + 2*time.Second)
	if err := other.Remove(ctx, "data/aa/pack"); err != nil && !errors.Is(err, ErrNotFound) {
		t.Fatalf("another client cannot remove the pack after the handle was idle: %v", err)
	}
	if _, err := other.Size(ctx, "data/aa/pack"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the pack is still there: %v", err)
	}
}
