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
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Clean(link), filepath.Clean(target)).CombinedOutput(); err == nil {
			return
		} else {
			t.Skipf("cannot create a junction: %v %s", err, out)
		}
	}
	t.Skip("cannot create a link to a folder")
}

func openTarget(t *testing.T, dir string) *Target {
	t.Helper()
	tg, err := OpenTarget(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tg.Close() })
	return tg
}

func TestTargetReplacesLinks(t *testing.T) {
	root, outside := filepath.Clean(t.TempDir()), filepath.Clean(t.TempDir())
	folderLink(t, outside, filepath.Join(root, "C"))
	tg := openTarget(t, root)
	dir := filepath.Join(root, "C", "Users", "ana")
	if err := tg.Mkdir(dir); err != nil {
		t.Fatal(err)
	}
	if names, _ := os.ReadDir(outside); len(names) > 0 {
		t.Fatalf("created through the link: %v", names)
	}
	fi, err := os.Lstat(filepath.Join(root, "C"))
	if err != nil || !fi.IsDir() || isLink(fi) {
		t.Fatalf("C: %v %v", fi.Mode(), err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatal(err)
	}
}

func TestTargetRefusesFilesOutsideAndVolumeRoots(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	if err := os.WriteFile(filepath.Join(root, "f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	tg := openTarget(t, root)
	if err := tg.Mkdir(filepath.Join(root, "f", "x")); err == nil {
		t.Error("a file where a folder belongs was accepted")
	}
	if err := tg.Mkdir(filepath.Dir(root)); err == nil {
		t.Error("a folder outside the restore folder was accepted")
	}
	if _, err := tg.Create(filepath.Join(filepath.Dir(root), "x")); err == nil {
		t.Error("a file outside the restore folder was accepted")
	}
	vol := "/"
	if runtime.GOOS == "windows" {
		vol = filepath.VolumeName(root) + `\`
	}
	if _, err := OpenTarget(vol); err == nil {
		t.Errorf("%s accepted as restore folder", vol)
	}
	// Without a restore folder (original location): plain folders.
	orig := openTarget(t, "")
	if err := orig.Mkdir(filepath.Join(root, "a", "b")); err != nil {
		t.Error(err)
	}
}

func TestTargetCreateRemovesLink(t *testing.T) {
	dir := filepath.Clean(t.TempDir())
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "r")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(sub, "x.bzpart")
	if err := os.Symlink(victim, part); err != nil {
		t.Skipf("cannot create a symbolic link: %v", err)
	}
	for _, tg := range []*Target{openTarget(t, sub), openTarget(t, "")} {
		f, err := tg.Create(part)
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
		_ = os.Remove(part)
		_ = os.Symlink(victim, part)
	}
}

// A folder swapped for a link after it was checked (while the restore
// runs) does not let anything be written outside the restore folder: every
// step goes through the folder.
func TestTargetLinkSwappedInAfterTheCheck(t *testing.T) {
	root, outside := filepath.Clean(t.TempDir()), filepath.Clean(t.TempDir())
	tg := openTarget(t, root)
	dir := filepath.Join(root, "C", "Users")
	if err := tg.Mkdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "C")); err != nil {
		t.Fatal(err)
	}
	folderLink(t, outside, filepath.Join(root, "C"))
	if err := os.Mkdir(filepath.Join(outside, "Users"), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := tg.Create(filepath.Join(dir, "x.bzpart"))
	if err == nil {
		_ = f.Close()
		t.Fatal("created through a link swapped in after the check")
	}
	if err := tg.Replace(filepath.Join(root, "y"), filepath.Join(dir, "y")); err == nil {
		t.Error("renamed through a link swapped in after the check")
	}
	if err := tg.Chmod(filepath.Join(dir), 0o777); err == nil {
		t.Error("chmod through a link swapped in after the check")
	}
	if names, _ := os.ReadDir(filepath.Join(outside, "Users")); len(names) > 0 {
		t.Fatalf("written outside: %v", names)
	}
}

// At the original location a link of another user in the middle of the
// path is not followed, also when the folder at its end exists.
func TestOriginalLocationLinkAbovePath(t *testing.T) {
	root, outside := filepath.Clean(t.TempDir()), filepath.Clean(t.TempDir())
	if err := os.MkdirAll(filepath.Join(outside, "Windows", "System32"), 0o755); err != nil {
		t.Fatal(err)
	}
	folderLink(t, outside, filepath.Join(root, "P"))
	old := trustedLink
	trustedLink = func(string, os.FileInfo) bool { return false }
	defer func() { trustedLink = old }()
	orig := openTarget(t, "")
	if err := orig.Mkdir(filepath.Join(root, "P", "Windows", "System32")); err == nil {
		t.Fatal("a link of another user above the path was followed")
	}
	trustedLink = func(string, os.FileInfo) bool { return true }
	if err := orig.Mkdir(filepath.Join(root, "P", "Windows", "System32", "new")); err != nil {
		t.Fatalf("a trusted link was not followed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "Windows", "System32", "new")); err != nil {
		t.Fatal(err)
	}
}
