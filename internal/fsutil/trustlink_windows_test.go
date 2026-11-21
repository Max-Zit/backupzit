//go:build windows

package fsutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// A junction of the system (C:\Documents and Settings, owned by SYSTEM) is
// trusted; one the test user makes is trusted only when that user is an
// administrator.
func TestTrustedLinkOwner(t *testing.T) {
	sys := `C:\Documents and Settings`
	if fi, err := os.Lstat(sys); err == nil && fi.Mode()&os.ModeIrregular != 0 {
		if !TrustedLink(sys, fi) {
			t.Errorf("%s (the system's junction) is not trusted", sys)
		}
	}
	dir := filepath.Clean(t.TempDir()) // mklink needs backslashes
	link := filepath.Join(dir, "j")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, dir).CombinedOutput(); err != nil {
		t.Skipf("mklink /J: %v %s", err, out)
	}
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	tok := windows.GetCurrentProcessToken()
	u, err := tok.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	admin := trustedOwner(u.User.Sid)
	t.Logf("test user is an administrator: %v", admin)
	if got := TrustedLink(link, fi); got != admin {
		t.Errorf("junction of the test user (administrator: %v): trusted %v", admin, got)
	}
	if TrustedLink(filepath.Join(dir, "missing"), fi) {
		t.Error("a missing link is trusted")
	}
}

// Owners other than the system and the administrators are not trusted.
func TestTrustedOwner(t *testing.T) {
	for s, want := range map[string]bool{
		"S-1-5-18":       true, // SYSTEM
		"S-1-5-32-544":   true, // Administrators
		trustedInstaller: true,
		"S-1-5-32-545":   false, // Users
		"S-1-1-0":        false, // Everyone
		"S-1-5-21-1111111111-2222222222-3333333333-1001": false, // a user of no domain here
	} {
		sid, err := windows.StringToSid(s)
		if err != nil {
			t.Fatal(err)
		}
		if got := trustedOwner(sid); got != want {
			t.Errorf("%s: trusted %v, want %v", s, got, want)
		}
	}
	if len(localAdmins()) == 0 {
		t.Error("no members of the local Administrators group found")
	}
}
