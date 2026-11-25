package backend

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"sync"
	"testing"
	"time"
)

type fakeSMBFile struct {
	*bytes.Reader
	name   string
	fs     *fakeSMBShare
	closed bool
}

func (f *fakeSMBFile) Close() error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if f.closed {
		f.fs.doubleClose++
	}
	f.closed = true
	f.fs.open--
	return nil
}

func (f *fakeSMBFile) Stat() (fs.FileInfo, error) { return nil, fmt.Errorf("not needed") }

type fakeSMBShare struct {
	mu          sync.Mutex
	opens       int
	open        int
	doubleClose int
}

func (f *fakeSMBShare) openRO(name string) (smbFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens++
	f.open++
	return &fakeSMBFile{Reader: bytes.NewReader(bytes.Repeat([]byte(name[len(name)-1:]), 100)), name: name, fs: f}, nil
}

// Ranged reads of a pack reuse one open handle; at most smbHandleCache
// packs stay open, the least recently used is closed first, and Remove
// (forget) and Close close what is cached. Other objects are opened per read.
func TestSMBHandleCache(t *testing.T) {
	ctx := context.Background()
	share := &fakeSMBShare{}
	s := &SMB{openRO: share.openRO}
	read := func(name string) {
		t.Helper()
		b, err := s.LoadRange(ctx, name, 10, 5)
		if err != nil || !bytes.Equal(b, bytes.Repeat([]byte(name[len(name)-1:]), 5)) {
			t.Fatalf("%s: %q %v", name, b, err)
		}
	}
	for range 5 {
		read("data/ab/p0")
	}
	if share.opens != 1 || share.open != 1 {
		t.Fatalf("5 reads of one pack: %d opens, %d open", share.opens, share.open)
	}
	for i := range smbHandleCache + 3 {
		read(fmt.Sprintf("data/ab/p%d", i+1))
	}
	if share.open != smbHandleCache {
		t.Errorf("%d handles open, want %d", share.open, smbHandleCache)
	}
	read("data/ab/p0") // evicted long ago: opened again
	if share.opens != smbHandleCache+5 {
		t.Errorf("%d opens", share.opens)
	}
	s.forget("data/ab/p0") // what Remove does before deleting on the share
	if share.open != smbHandleCache-1 {
		t.Errorf("after Remove %d handles open, want %d", share.open, smbHandleCache-1)
	}
	read("index/x1")
	if share.open != smbHandleCache-1 {
		t.Errorf("an index read left its handle open")
	}
	s.closeHandles()
	if share.open != 0 || share.doubleClose != 0 {
		t.Errorf("after close %d open, %d closed twice", share.open, share.doubleClose)
	}
}

// Reads running at once share a handle; one that is evicted while a read
// uses it is closed when that read ends, once.
func TestSMBHandleCacheConcurrent(t *testing.T) {
	ctx := context.Background()
	share := &fakeSMBShare{}
	s := &SMB{openRO: share.openRO}
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				name := fmt.Sprintf("data/ab/p%d", (g+i)%20)
				if _, err := s.LoadRange(ctx, name, 0, 10); err != nil {
					t.Error(err)
					return
				}
				if i%50 == 0 {
					time.Sleep(time.Millisecond)
				}
			}
		}()
	}
	wg.Wait()
	if share.open > smbHandleCache {
		t.Errorf("%d handles open", share.open)
	}
	s.closeHandles()
	if share.open != 0 || share.doubleClose != 0 {
		t.Errorf("after close %d open, %d closed twice", share.open, share.doubleClose)
	}
}
