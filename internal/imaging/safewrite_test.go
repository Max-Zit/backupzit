package imaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func folderLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		return
	}
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err == nil {
			return
		} else {
			t.Skipf("cannot create a junction: %v %s", err, out)
		}
	}
	t.Skip("cannot create a link to a folder")
}

func TestMkdirBelowReplacesLinks(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	folderLink(t, outside, filepath.Join(root, "C"))
	dir := filepath.Join(root, "C", "Users", "ana")
	if err := MkdirBelow(root, dir); err != nil {
		t.Fatal(err)
	}
	if names, _ := os.ReadDir(outside); len(names) > 0 {
		t.Fatalf("created through the link: %v", names)
	}
	fi, err := os.Lstat(filepath.Join(root, "C"))
	if err != nil || !fi.IsDir() || fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		t.Fatalf("C: %v %v", fi.Mode(), err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatal(err)
	}
}

func TestMkdirBelowRefusesFilesAndOutside(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MkdirBelow(root, filepath.Join(root, "f", "x")); err == nil {
		t.Error("a file where a folder belongs was accepted")
	}
	if err := MkdirBelow(root, filepath.Dir(root)); err == nil {
		t.Error("a folder outside the restore folder was accepted")
	}
	// Without a restore folder (original location): plain MkdirAll.
	if err := MkdirBelow("", filepath.Join(root, "a", "b")); err != nil {
		t.Error(err)
	}
}

func TestCreateFreshRemovesLink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(dir, "x.bzpart")
	if err := os.Symlink(victim, part); err != nil {
		t.Skipf("cannot create a symbolic link: %v", err)
	}
	f, err := CreateFresh(part)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("restored")
	_ = f.Close()
	if b, _ := os.ReadFile(victim); string(b) != "original" {
		t.Fatalf("written through the link: %q", b)
	}
	if fi, err := os.Lstat(part); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("part file: %v %v", fi.Mode(), err)
	}
}
