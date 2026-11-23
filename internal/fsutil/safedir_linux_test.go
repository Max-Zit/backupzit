package fsutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A link of root in a folder others may write to (not sticky) is not
// trusted: another user could have renamed it there, or replace it.
func TestLinkInWritableFolderNotTrusted(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (links of root)")
	}
	base := t.TempDir()
	open := filepath.Join(base, "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(open, "l")); err != nil {
		t.Fatal(err)
	}
	check := func() bool {
		d, err := OpenOriginal(open)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		e, err := d.lstat("l")
		if err != nil {
			t.Fatal(err)
		}
		return e.trusted
	}
	if !check() {
		t.Error("a link of root in a folder of root (0755) is not trusted")
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if check() {
		t.Error("a link in a folder everybody may write to is trusted")
	}
	if err := os.Chmod(open, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if !check() {
		t.Error("a link of root in a sticky folder is not trusted")
	}
}

// B2 (0.59): a link with a second name (a hard link of it, which anyone may
// make in a sticky folder when fs.protected_hardlinks is off) is not
// trusted under either name. The agent's own links count as trusted, so
// this runs without root.
func TestHardLinkedLinkNotTrusted(t *testing.T) {
	base := t.TempDir()
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(base, "l")); err != nil {
		t.Fatal(err)
	}
	trusted := func(name string) bool {
		d, err := OpenOriginal(base)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		e, err := d.lstat(name)
		if err != nil {
			t.Fatal(err)
		}
		if e.kind != entryLink {
			t.Fatalf("%s is no link", name)
		}
		return e.trusted
	}
	if !trusted("l") {
		t.Fatal("the agent's own link in its own folder is not trusted")
	}
	// link(2) on Linux links the symbolic link itself.
	if err := os.Link(filepath.Join(base, "l"), filepath.Join(base, "second")); err != nil {
		t.Skipf("hard link of a link: %v", err)
	}
	if trusted("l") || trusted("second") {
		t.Error("a link with two names is trusted")
	}
}

// A trusted link to a folder that does not exist (a disk that is not
// mounted) is an error; nothing is created where it points.
func TestLinkToMissingFolderNotCreated(t *testing.T) {
	base := filepath.Clean(t.TempDir())
	missing := filepath.Join(base, "disk2", "data")
	if err := os.Symlink(missing, filepath.Join(base, "data")); err != nil {
		t.Fatal(err)
	}
	trusting(t, true)
	if _, err := OpenOriginal(filepath.Join(base, "data", "x")); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("link to a missing folder: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(base, "disk2")); err == nil {
		t.Fatal("the missing target was created")
	}
}

// A hard link is made only to the file the restore wrote, not to what was
// put at its name since.
func TestLinkFromChecksTheFile(t *testing.T) {
	base := filepath.Clean(t.TempDir())
	d, err := OpenFolder(base)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	f, err := d.CreateExcl("first")
	if err != nil {
		t.Fatal(err)
	}
	id, ok := IDOf(f)
	_ = f.Close()
	if !ok {
		t.Fatal("no file identity")
	}
	if err := d.LinkFrom(d, "first", "second", id); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(base, "first")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(base, "first")); err != nil {
		t.Fatal(err)
	}
	if err := d.LinkFrom(d, "first", "third", id); err == nil {
		t.Fatal("a link was made to what was put in place of the restored file")
	}
	if _, err := os.Lstat(filepath.Join(base, "third")); err == nil {
		t.Fatal("third exists")
	}
}
