//go:build !windows

package pgsql

import (
	"os"
	"path/filepath"
	"testing"
)

// H-N4: in a folder postgres can write, a name it replaced with a link is
// never followed by root.
func TestSafeFilesRefuseLinks(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("root only"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "globals.sql")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	if err := writeNew(link, []byte("x"), 0o600); err == nil {
		t.Error("writeNew wrote through a link")
	}
	if b, _ := os.ReadFile(secret); string(b) != "root only" {
		t.Errorf("the link target changed: %q", b)
	}
	if _, err := readNoFollow(link, 0); err == nil {
		t.Error("readNoFollow read through a link")
	}
	if err := writeNew(filepath.Join(dir, "new"), []byte("ok"), 0o600); err != nil {
		t.Errorf("a new file: %v", err)
	}
	if b, err := readNoFollow(filepath.Join(dir, "new"), 0); err != nil || string(b) != "ok" {
		t.Errorf("read back: %q %v", b, err)
	}
	// A folder name taken by a link is refused too.
	if err := os.Symlink(dir, filepath.Join(dir, "wal")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "wal"), 0o700); err == nil {
		t.Error("Mkdir over a link")
	}
}
