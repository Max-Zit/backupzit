package vmfs

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"sync"
	"testing"
)

// Browse and download requests of the same cached ext4 volume run at once
// (the console caches the opened file system); they shared one reader
// without a lock and could get each other's blocks.
func TestExt4ConcurrentReads(t *testing.T) {
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
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				if _, err := fsys.List("/a"); err != nil {
					errs <- "list: " + err.Error()
					return
				}
				for _, p := range []string{"/a/f", "/a/l", "/abs"} {
					r, _, err := fsys.ReadFile(p)
					if err != nil {
						errs <- p + ": " + err.Error()
						return
					}
					b, _ := io.ReadAll(r)
					if string(b) != "hello from a/f\n" {
						errs <- p + " read " + string(b)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
