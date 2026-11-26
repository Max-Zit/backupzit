package update

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A17: copyRegular copies regular files only, with their size.
func TestCopyRegular(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.bin")
	if err := os.WriteFile(src, []byte("package data"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "b.bin")
	if err := copyRegular(src, dst); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "package data" {
		t.Fatalf("copy: %q %v", b, err)
	}
	if err := copyRegular(dir, filepath.Join(dir, "c")); err == nil {
		t.Error("folder copied")
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(dir, "link")
		if err := os.Symlink(src, link); err != nil {
			t.Fatal(err)
		}
		if err := copyRegular(link, filepath.Join(dir, "d")); err == nil {
			t.Error("symbolic link copied")
		}
	}
	if _, err := os.Stat(dst + ".tmp"); err == nil {
		t.Error("temporary file left")
	}
}
