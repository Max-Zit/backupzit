package fsutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// A symbolic link of a trusted owner in a protected folder is followed,
// but not when it has a second name: macOS lets any user hard-link someone
// else's symbolic link (there is no protected_hardlinks rule), so a second
// name may have been put there by another user.
func TestHardLinkedSymlinkNotTrusted(t *testing.T) {
	base := realTempDir(t)
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/private/var/db", filepath.Join(base, "l")); err != nil {
		t.Fatal(err)
	}
	d, err := OpenFolder(base)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	e, err := d.lstat("l")
	if err != nil || e.kind != entryLink || !e.trusted {
		t.Fatalf("own link with one name: %+v %v", e, err)
	}
	// linkat without AT_SYMLINK_FOLLOW: the second name is the link itself
	// (os.Link would link what it points to).
	if err := unix.Linkat(unix.AT_FDCWD, filepath.Join(base, "l"), unix.AT_FDCWD, filepath.Join(base, "l2"), 0); err != nil {
		t.Skipf("cannot hard-link a symbolic link here: %v", err)
	}
	for _, n := range []string{"l", "l2"} {
		if e, err := d.lstat(n); err != nil || e.kind != entryLink || e.trusted {
			t.Errorf("%s with two names: %+v %v", n, e, err)
		}
	}
}

// A named pipe is created relative to the opened folder (mkfifoat): a folder
// above it swapped for a link after it was opened changes nothing.
func TestMknodThroughOpenedFolder(t *testing.T) {
	base, outside := realTempDir(t), realTempDir(t)
	if err := os.Mkdir(filepath.Join(base, "C"), 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := OpenFolder(filepath.Join(base, "C"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := os.Rename(filepath.Join(base, "C"), filepath.Join(base, "C2")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base, "C")); err != nil {
		t.Fatal(err)
	}
	if err := d.Mknod("pipe", unix.S_IFIFO|0o640, 0); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(base, "C2", "pipe"))
	if err != nil || fi.Mode()&os.ModeNamedPipe == 0 || fi.Mode().Perm() != 0o640 {
		t.Fatalf("pipe in the opened folder: %v %v", fi, err)
	}
	if names, _ := os.ReadDir(outside); len(names) != 0 {
		t.Fatalf("created through the link: %v", names)
	}
	if err := d.Mknod("pipe", unix.S_IFIFO|0o600, 0); err != nil {
		t.Fatalf("again over the old one: %v", err)
	}
}

// An ACL entry that lets others add or delete entries makes a folder
// unprotected whatever its mode; deny entries change nothing.
func TestParentProtectedACL(t *testing.T) {
	dir := realTempDir(t)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	protected := func() bool {
		f, err := os.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		return parentProtected(int(f.Fd()))
	}
	if !protected() {
		t.Fatal("own 0755 folder without ACL not protected")
	}
	chmod := func(args ...string) {
		if out, err := exec.Command("/bin/chmod", append(args, dir)...).CombinedOutput(); err != nil {
			t.Skipf("chmod %v: %v %s", args, err, out)
		}
	}
	// The temporary folder is removed after the test: without its ACL,
	// whose deny entry would refuse that.
	t.Cleanup(func() { _ = exec.Command("/bin/chmod", "-N", dir).Run() })
	chmod("+a", "everyone deny delete")
	if !protected() {
		t.Error("a deny entry made the folder unprotected")
	}
	chmod("+a", "everyone allow add_file,delete_child")
	if protected() {
		t.Error("an allow add_file entry left the folder protected")
	}
}

func TestACLLetsOthersChange(t *testing.T) {
	for text, want := range map[string]bool{
		"": false,
		"!#acl 1\ngroup:ABCDEFAB-CDEF-ABCD-EFAB-CDEF0000000C:everyone:12:deny:delete\n":        false,
		"!#acl 1\ngroup:ABCDEFAB-CDEF-ABCD-EFAB-CDEF0000000C:everyone:12:allow:read,search\n":  false,
		"!#acl 1\ngroup:ABCDEFAB-CDEF-ABCD-EFAB-CDEF0000000C:everyone:12:allow:add_file\n":     true,
		"!#acl 1\nuser:FFFFEEEE-DDDD-CCCC-BBBB-AAAA000001F5:bob:501:allow:list,delete_child\n": true,
		"!#acl 1\nuser:FFFFEEEE-DDDD-CCCC-BBBB-AAAA000001F5:bob:501:allow:add_subdirectory\n":  true,
	} {
		if got := aclLetsOthersChange(text); got != want {
			t.Errorf("%q: %v", text, got)
		}
	}
}
