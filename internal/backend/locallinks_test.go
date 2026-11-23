package backend

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// B3 (0.59): the local storage reads and writes nothing outside the
// repository folder through links put into it.

func localRepo(t *testing.T) (*Local, string, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("host file"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := OpenLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	return l, root, outside
}

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symbolic links need a privilege on this Windows: %v", err)
		}
		t.Fatal(err)
	}
}

// Regular objects behave as before.
func TestLocalRegularObjects(t *testing.T) {
	ctx := context.Background()
	l, _, _ := localRepo(t)
	if err := l.Save(ctx, "data/ab/abcd", []byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	if err := l.Save(ctx, "config", []byte("cfg")); err != nil {
		t.Fatal(err)
	}
	if b, err := l.Load(ctx, "data/ab/abcd"); err != nil || string(b) != "0123456789" {
		t.Fatalf("load: %q %v", b, err)
	}
	if b, err := l.LoadRange(ctx, "data/ab/abcd", 2, 3); err != nil || string(b) != "234" {
		t.Fatalf("range: %q %v", b, err)
	}
	if b, err := l.LoadRange(ctx, "data/ab/abcd", -4, 4); err != nil || string(b) != "6789" {
		t.Fatalf("range from the end: %q %v", b, err)
	}
	if n, err := l.Size(ctx, "data/ab/abcd"); err != nil || n != 10 {
		t.Fatalf("size: %d %v", n, err)
	}
	if n, err := l.Size(ctx, "data/ab"); err != nil || n < 0 {
		t.Fatalf("size of a folder still answers: %d %v", n, err)
	}
	if got, err := l.List(ctx, "data"); err != nil || !slices.Equal(got, []string{"data/ab/abcd"}) {
		t.Fatalf("list: %v %v", got, err)
	}
	if got, err := l.List(ctx, "missing"); err != nil || len(got) != 0 {
		t.Fatalf("list of a missing folder: %v %v", got, err)
	}
	for _, f := range []func() error{
		func() error { _, err := l.Load(ctx, "data/cd/nothing"); return err },
		func() error { _, err := l.LoadRange(ctx, "data/ab/nothing", 0, 1); return err },
		func() error { _, err := l.Size(ctx, "nothing"); return err },
		func() error { return l.Remove(ctx, "data/ab/nothing") },
	} {
		if err := f(); !errors.Is(err, ErrNotFound) {
			t.Errorf("missing object: %v", err)
		}
	}
	if err := l.Remove(ctx, "data/ab/abcd"); err != nil {
		t.Fatal(err)
	}
}

func TestLocalLinkToAFileOutsideIsNotRead(t *testing.T) {
	ctx := context.Background()
	l, root, outside := localRepo(t)
	if err := l.Save(ctx, "data/ab/abcd", []byte("pack")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "index"), 0o700); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, filepath.Join(outside, "secret"), filepath.Join(root, "index", "planted"))
	symlinkOrSkip(t, filepath.Join(root, "data", "ab", "abcd"), filepath.Join(root, "index", "inside"))
	symlinkOrSkip(t, filepath.Join(root, "gone"), filepath.Join(root, "index", "dangling"))

	got, err := l.List(ctx, "index")
	if err != nil || !slices.Equal(got, []string{"index/inside"}) {
		t.Fatalf("list: %v %v (only the link inside the repository)", got, err)
	}
	if b, err := l.Load(ctx, "index/inside"); err != nil || string(b) != "pack" {
		t.Fatalf("a link inside the repository is read: %q %v", b, err)
	}
	if b, err := l.Load(ctx, "index/planted"); err == nil || errors.Is(err, ErrNotFound) || strings.Contains(string(b), "host") {
		t.Fatalf("a link out of the repository was read: %q %v", b, err)
	}
	if _, err := l.LoadRange(ctx, "index/planted", 0, 4); err == nil {
		t.Fatal("range through a link out of the repository")
	}
	if _, err := l.Size(ctx, "index/planted"); err == nil {
		t.Fatal("size through a link out of the repository")
	}
	if _, err := l.Load(ctx, "index/dangling"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a dangling link: %v", err)
	}
	// Removing the link removes the link, not the file.
	if err := l.Remove(ctx, "index/planted"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "secret")); err != nil {
		t.Fatalf("the file the link pointed to: %v", err)
	}
}

// A folder of the repository that is a link (Unix) or a junction (Windows,
// which needs no privilege) is neither listed, read, nor written through.
func TestLocalLinkedFolderIsNotFollowed(t *testing.T) {
	ctx := context.Background()
	l, root, outside := localRepo(t)
	link := filepath.Join(root, "index")
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
			t.Skipf("mklink /J: %v %s", err, out)
		}
	} else {
		symlinkOrSkip(t, outside, link)
	}
	if got, err := l.List(ctx, "index"); err == nil || len(got) != 0 {
		t.Fatalf("list through a linked folder: %v %v", got, err)
	}
	if b, err := l.Load(ctx, "index/secret"); err == nil || strings.Contains(string(b), "host") {
		t.Fatalf("read through a linked folder: %q %v", b, err)
	}
	if err := l.Save(ctx, "index/written", []byte("x")); err == nil {
		t.Fatal("saved through a linked folder")
	}
	if _, err := os.Stat(filepath.Join(outside, "written")); err == nil {
		t.Fatal("a file was written outside the repository")
	}
	if err := l.Remove(ctx, "index/secret"); err == nil {
		t.Fatal("removed through a linked folder")
	}
	if _, err := os.Stat(filepath.Join(outside, "secret")); err != nil {
		t.Fatalf("the file outside: %v", err)
	}
	// A deeper folder too (data/ab).
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "data", "ab")
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", sub, outside).CombinedOutput(); err != nil {
			t.Fatalf("mklink /J: %v %s", err, out)
		}
	} else {
		symlinkOrSkip(t, outside, sub)
	}
	if got, err := l.List(ctx, "data"); err != nil || len(got) != 0 {
		t.Fatalf("list of data with a linked subfolder: %v %v", got, err)
	}
	if _, err := l.Load(ctx, "data/ab/secret"); err == nil {
		t.Fatal("read through a linked subfolder")
	}
	if err := l.Save(ctx, "data/ab/abcd", []byte("x")); err == nil {
		t.Fatal("saved through a linked subfolder")
	}
}
