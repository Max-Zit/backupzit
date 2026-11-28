package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// currentUser is the SID of the account the test runs as.
func currentUser(t *testing.T) *windows.SID {
	t.Helper()
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return tu.User.Sid
}

// openUp gives the test's account full access to a folder made with the
// agent's access list (as its owner it may change the list), so the test
// can look inside and remove it.
func openUp(t *testing.T, dir string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString("D:(A;OICI;FA;;;" + currentUser(t).String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, _ := sd.DACL()
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

func junction(t *testing.T, link, target string) {
	t.Helper()
	// Junctions need no privilege: what any user can do before the agent
	// is installed.
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Clean(link), filepath.Clean(target)).CombinedOutput(); err != nil { // #nosec G204 -- test paths
		t.Skipf("mklink /J: %v %s", err, out)
	}
}

// HIGH 6: the agent's folder in ProgramData may have been made by any user
// before the installation, or be a junction to a folder of his own. It is
// set aside unread and replaced by a protected folder; the agent's own
// folder (owned by SYSTEM or Administrators) stays as it is.
func TestTrustDir(t *testing.T) {
	base := t.TempDir()
	elevated := windows.GetCurrentProcessToken().IsElevated()

	t.Run("missing folder is created protected", func(t *testing.T) {
		dir := filepath.Join(base, "new")
		aside, err := trustDir(dir)
		if err != nil || aside != "" {
			t.Fatalf("trustDir: %q %v", aside, err)
		}
		t.Cleanup(func() { openUp(t, dir) })
		sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		if got := sd.String(); got != configDirSDDL {
			t.Errorf("access list %s, want %s", got, configDirSDDL)
		}
	})

	t.Run("folder of another user is set aside unread", func(t *testing.T) {
		if elevated {
			t.Skip("elevated: the test's folders belong to Administrators, which the agent trusts")
		}
		dir := filepath.Join(base, "planted")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "agent.json"), []byte(`{"server_url":"https://attacker"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		aside, err := trustDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { openUp(t, dir) })
		if !strings.HasPrefix(aside, dir+".untrusted-") {
			t.Fatalf("set aside as %q", aside)
		}
		if b, err := os.ReadFile(filepath.Join(aside, "agent.json")); err != nil || !strings.Contains(string(b), "attacker") {
			t.Errorf("the planted file did not move with its folder: %q %v", b, err)
		}
		openUp(t, dir)
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Errorf("new folder: %v %v", entries, err)
		}
		if link, _ := reparsePoint(dir); link {
			t.Error("the new folder is a link")
		}
	})

	t.Run("junction is set aside, its target untouched", func(t *testing.T) {
		target := filepath.Join(base, "target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(target, "agent.json"), []byte("planted"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "link")
		junction(t, link, target)
		aside, err := trustDir(link)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { openUp(t, link) })
		if isLink, err := reparsePoint(aside); err != nil || !isLink {
			t.Errorf("%s should be the junction itself: %v %v", aside, isLink, err)
		}
		if isLink, err := reparsePoint(link); err != nil || isLink {
			t.Errorf("%s is still a link: %v", link, err)
		}
		if b, err := os.ReadFile(filepath.Join(target, "agent.json")); err != nil || string(b) != "planted" {
			t.Errorf("the junction's target changed: %q %v", b, err)
		}
		sd, err := windows.GetNamedSecurityInfo(target, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		if sd.String() == configDirSDDL {
			t.Error("the junction's target got the agent's access list")
		}
	})

	t.Run("protectDir refuses a junction", func(t *testing.T) {
		target := filepath.Join(base, "target2")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "updates")
		junction(t, link, target)
		if err := protectDir(link); err == nil || !strings.Contains(err.Error(), "link") {
			t.Errorf("protectDir on a junction: %v", err)
		}
	})

	t.Run("the agent's own folder stays as it is", func(t *testing.T) {
		me := currentUser(t)
		orig := trustedOwner
		trustedOwner = func(sid *windows.SID) bool { return orig(sid) || sid.Equals(me) }
		defer func() { trustedOwner = orig }()
		dir := filepath.Join(base, "own")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "agent.json"), []byte("mine"), 0o600); err != nil {
			t.Fatal(err)
		}
		aside, err := trustDir(dir)
		if err != nil || aside != "" {
			t.Fatalf("trustDir: %q %v", aside, err)
		}
		if b, err := os.ReadFile(filepath.Join(dir, "agent.json")); err != nil || string(b) != "mine" {
			t.Errorf("own folder changed: %q %v", b, err)
		}
		// A file where the folder should be is not the agent's folder.
		file := filepath.Join(base, "file")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		aside, err = trustDir(file)
		if err != nil || !strings.HasPrefix(aside, file+".untrusted-") {
			t.Fatalf("file: %q %v", aside, err)
		}
		t.Cleanup(func() { openUp(t, file) })
	})
}
