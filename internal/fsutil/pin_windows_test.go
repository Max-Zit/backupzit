package fsutil

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// B1 (0.59): while a folder is pinned, none of the folders of its path can
// be renamed (to put a junction in its place) or removed.
func TestPinnedFoldersCannotBeSwapped(t *testing.T) {
	base := t.TempDir()
	deep := filepath.Join(base, "a", "b")
	p, err := PinFolder(deep, true)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(deep); err != nil || !fi.IsDir() {
		t.Fatalf("missing folders were not created: %v", err)
	}
	if p.Path() != deep {
		t.Errorf("path %q", p.Path())
	}
	for _, f := range []string{filepath.Join(base, "a"), deep} {
		if err := os.Rename(f, f+"-moved"); err == nil {
			t.Fatalf("%s was renamed while pinned", f)
		}
	}
	if err := os.Remove(deep); err == nil {
		t.Fatal("a pinned folder was removed")
	}
	_ = p.Close()
	_ = p.Close() // twice is fine
	if err := os.Rename(filepath.Join(base, "a"), filepath.Join(base, "a-moved")); err != nil {
		t.Fatalf("after Close: %v", err)
	}
	if _, err := PinFolder(filepath.Join(base, "missing"), false); err == nil {
		t.Fatal("a missing folder was pinned without create")
	}
}

// A junction in a folder where everybody may remove and add entries is a
// link others may change (they could have put it there): not followed, and
// nothing is created where it points. (The test user may be an
// administrator, whose own links count as trusted.)
func TestPinRefusesAUsersJunction(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("mklink /J: %v %s", err, out)
	}
	if out, err := exec.Command("icacls", base, "/grant", "*S-1-1-0:(F)").CombinedOutput(); err != nil {
		t.Skipf("icacls: %v %s", err, out)
	}
	if _, err := PinFolder(filepath.Join(link, "vm"), true); err == nil || !strings.Contains(err.Error(), "not followed") {
		t.Fatalf("pinned through a user's junction: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "vm")); err == nil {
		t.Fatal("a folder was created where the junction points")
	}
	p, err := PinFolder(base, false)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, _, err := p.Sub("link", true, false); err == nil {
		t.Fatal("Sub followed a user's junction")
	}
}

// A link of the system is followed ("C:\Documents and Settings" leads to
// C:\Users), and the path stays as given.
func TestPinFollowsASystemJunction(t *testing.T) {
	sysdrive := os.Getenv("SystemDrive")
	link := sysdrive + `\Documents and Settings`
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeIrregular == 0 {
		t.Skipf("no system junction %s: %v", link, err)
	}
	p, err := PinFolder(filepath.Join(link, "Public"), false)
	if err != nil {
		t.Fatalf("the system's junction is not followed: %v", err)
	}
	defer p.Close()
	if p.Path() != filepath.Join(link, "Public") {
		t.Errorf("path %q", p.Path())
	}
}

// A private folder does not inherit what its parent allows others, and
// Sub on an existing folder opens it without creating anything.
func TestCreatePrivateFolder(t *testing.T) {
	base := t.TempDir()
	p, err := PinFolder(base, false)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	priv, err := p.CreatePrivate("vm", true)
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetSecurityInfo(priv.h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	s := sd.String()
	if !strings.HasPrefix(s, "D:P") || strings.Contains(s, ";ID;") || !strings.Contains(s, ";;;SY)") || !strings.Contains(s, ";;;BA)") || !strings.Contains(s, ";;;S-1-5-32-578)") && !strings.Contains(s, ";;;HA)") {
		t.Errorf("private folder's permissions: %s", s)
	}
	_ = priv.Close()
	if _, err := p.CreatePrivate("vm", true); !errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) {
		t.Fatalf("second create: %v", err)
	}
	sub, created, err := p.Sub("vm", true, true)
	if err != nil || created {
		t.Fatalf("Sub of the existing folder: created=%v %v", created, err)
	}
	_ = sub.Close()
	sub, created, err = p.Sub("new", true, false)
	if err != nil || !created {
		t.Fatalf("Sub creating: created=%v %v", created, err)
	}
	_ = sub.Close()
	for _, bad := range []string{"", ".", "..", `a\b`, "c:"} {
		if _, err := p.CreatePrivate(bad, true); err == nil {
			t.Errorf("CreatePrivate(%q)", bad)
		}
	}
}
