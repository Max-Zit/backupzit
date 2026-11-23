package fsutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func dirLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		return
	}
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Clean(link), filepath.Clean(target)).CombinedOutput(); err == nil {
			return
		} else {
			t.Skipf("cannot create a junction: %v %s", err, out)
		}
	}
	t.Skip("cannot create a link to a folder")
}

func trusting(t *testing.T, on bool) {
	old := trustLink
	trustLink = func(bool) bool { return on }
	t.Cleanup(func() { trustLink = old })
}

func TestOpenOriginalCreatesAndFollowsTrustedLinks(t *testing.T) {
	base, outside := filepath.Clean(t.TempDir()), filepath.Clean(t.TempDir())
	d, err := OpenOriginal(filepath.Join(base, "a", "b"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := d.CreateExcl("x.bzpart")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("data")
	_ = f.Close()
	if err := d.Rename("x.bzpart", "x"); err != nil {
		t.Fatal(err)
	}
	_ = d.Close()
	if b, err := os.ReadFile(filepath.Join(base, "a", "b", "x")); err != nil || string(b) != "data" {
		t.Fatalf("file: %q %v", b, err)
	}

	dirLink(t, outside, filepath.Join(base, "L"))
	trusting(t, false)
	if _, err := OpenOriginal(filepath.Join(base, "L", "sub")); err == nil || !strings.Contains(err.Error(), "link of another user") {
		t.Fatalf("link of another user: %v", err)
	}
	if names, _ := os.ReadDir(outside); len(names) != 0 {
		t.Fatalf("created through the link: %v", names)
	}
	trusting(t, true)
	d, err = OpenOriginal(filepath.Join(base, "L", "sub"))
	if err != nil {
		t.Fatalf("trusted link: %v", err)
	}
	_ = d.Close()
	if fi, err := os.Stat(filepath.Join(outside, "sub")); err != nil || !fi.IsDir() {
		t.Fatalf("trusted link not followed: %v", err)
	}
}

// A folder of the path swapped for a link after it was opened changes
// nothing: the writes go into the folder that was opened.
func TestDirSwappedAfterOpen(t *testing.T) {
	base, outside := filepath.Clean(t.TempDir()), filepath.Clean(t.TempDir())
	trusting(t, false)
	d, err := OpenOriginal(filepath.Join(base, "C"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := os.Rename(filepath.Join(base, "C"), filepath.Join(base, "C2")); err != nil {
		t.Skipf("the opened folder cannot be moved away (%v): no swap possible", err)
	}
	dirLink(t, outside, filepath.Join(base, "C"))
	f, err := d.CreateExcl("x")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if names, _ := os.ReadDir(outside); len(names) != 0 {
		t.Fatalf("written through the swapped link: %v", names)
	}
	if _, err := os.Stat(filepath.Join(base, "C2", "x")); err != nil {
		t.Fatalf("not in the opened folder: %v", err)
	}
	// Opening anew refuses the link of another user.
	if _, err := OpenOriginal(filepath.Join(base, "C")); err == nil {
		t.Fatal("the swapped-in link was followed")
	}
}

// Below a restore folder a link where a folder belongs is replaced by a
// real folder.
func TestSubReplacesLinksBelowRestoreFolder(t *testing.T) {
	base, outside := filepath.Clean(t.TempDir()), filepath.Clean(t.TempDir())
	dirLink(t, outside, filepath.Join(base, "C"))
	d, err := OpenFolder(base)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if l, err := d.IsLink("C"); err != nil || !l {
		t.Fatalf("IsLink: %v %v", l, err)
	}
	s, err := d.Sub("C", false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	f, err := s.CreateExcl("x")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if names, _ := os.ReadDir(outside); len(names) != 0 {
		t.Fatalf("written through the link: %v", names)
	}
	if fi, err := os.Lstat(filepath.Join(base, "C")); err != nil || !fi.IsDir() || fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		t.Fatalf("C: %v %v", fi.Mode(), err)
	}
	if _, err := d.Sub("..", false); err == nil {
		t.Error("'..' accepted as a folder name")
	}
	if err := os.WriteFile(filepath.Join(base, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Sub("file", false); err == nil {
		t.Error("a file accepted as a folder")
	}
}

func TestOpenOriginalRelativeSymlink(t *testing.T) {
	base := filepath.Clean(t.TempDir())
	if err := os.MkdirAll(filepath.Join(base, "real", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(base, "rel")); err != nil {
		t.Skipf("cannot create a relative symbolic link: %v", err)
	}
	trusting(t, true)
	d, err := OpenOriginal(filepath.Join(base, "rel", "x"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if !strings.EqualFold(d.Path(), filepath.Join(base, "real", "x")) {
		t.Errorf("path %s", d.Path())
	}
}
