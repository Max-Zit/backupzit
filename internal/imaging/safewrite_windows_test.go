//go:build windows

package imaging

import (
	"os"
	"path/filepath"
	"testing"
)

// Alternate data streams are written into their file below the restore
// folder; a stream of a junction that leads outside is refused.
func TestTargetStreams(t *testing.T) {
	root, outside := filepath.Clean(t.TempDir()), filepath.Clean(t.TempDir())
	tg := openTarget(t, root)
	write := func(p, s string) error {
		f, err := tg.Create(p)
		if err != nil {
			return err
		}
		_, err = f.WriteString(s)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	}
	a := filepath.Join(root, "a.txt")
	if err := write(a, "base"); err != nil {
		t.Fatal(err)
	}
	if err := write(a+":s1", "first try that is longer"); err != nil {
		t.Fatal(err)
	}
	if err := write(a+":s1", "stream"); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(a + ":s1"); err != nil || string(b) != "stream" {
		t.Fatalf("stream: %q %v", b, err)
	}
	if b, _ := os.ReadFile(a); string(b) != "base" {
		t.Fatalf("file: %q", b)
	}
	if err := write(filepath.Join(root, "new.txt:s"), "x"); err != nil {
		t.Fatalf("stream of a file not restored yet: %v", err)
	}
	folderLink(t, outside, filepath.Join(root, "d"))
	if err := write(filepath.Join(root, "d")+":s", "x"); err == nil {
		t.Fatal("stream written on a junction that leads outside")
	}
}
