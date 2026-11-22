package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// shellSyntax fails the test when sh cannot parse script.
func shellSyntax(t *testing.T, script string) {
	t.Helper()
	if out, err := exec.Command("/bin/sh", "-n", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("script does not parse: %v: %s\n%s", err, out, script)
	}
}

func TestMacPaths(t *testing.T) {
	if got := DefaultConfigPath(); got != "/Library/Application Support/BackupZit/agent.json" {
		t.Errorf("configuration %s", got)
	}
	if got := LogPath(DefaultConfigPath()); got != "/Library/Logs/BackupZit/agent.log" {
		t.Errorf("log %s", got)
	}
	if got := LogPath("/tmp/x/agent.json"); got != "/tmp/x/agent.log" {
		t.Errorf("log of another configuration %s", got)
	}
	if got := RetainCachePath(); got != "/Library/Application Support/BackupZit/retain-cache.json" {
		t.Errorf("retain cache %s", got)
	}
	if got := LeftoverPath(); got != "/Library/Application Support/BackupZit/leftovers.json" {
		t.Errorf("leftovers %s", got)
	}
}

// protectDir leaves the folder to its owner only, ACL entries included.
func TestProtectDirClearsACL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("/bin/chmod", "+a", "everyone allow list,search", dir).CombinedOutput(); err != nil {
		t.Skipf("cannot set an ACL here: %v: %s", err, out)
	}
	if err := protectDir(dir); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	out, err := exec.Command("/bin/ls", "-lde", dir).Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "allow") {
		t.Errorf("ACL left on the folder:\n%s", out)
	}
	// Again on a folder without entries.
	if err := protectDir(dir); err != nil {
		t.Errorf("folder without ACL: %v", err)
	}
}

func TestPkgInstallScript(t *testing.T) {
	p := "/Library/Application Support/BackupZit/updates/backupzit-agent-0.59.0-darwin-arm64'$(id).pkg"
	s := pkgInstallScript(p)
	shellSyntax(t, s)
	q := shellQuote(p)
	if !strings.Contains(s, "/usr/sbin/installer -verbose -pkg "+q+" -target / >>"+shellQuote(p+".log")+" 2>&1") {
		t.Errorf("installer command:\n%s", s)
	}
	if strings.Contains(s, " "+p) {
		t.Errorf("unquoted package path:\n%s", s)
	}
}

func TestInstallerCommandNeedsPkg(t *testing.T) {
	for _, name := range []string{"backupzit-agent.deb", "backupzit-agent.rpm", "backupzit-agent.msi", "backupzit-agent.pkg.sh"} {
		if _, err := installerCommand(filepath.Join(t.TempDir(), name)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := os.Stat(macInstaller); err == nil {
		if _, err := installerCommand("/x/backupzit-agent-0.59.0.pkg"); err != nil {
			t.Errorf("pkg refused: %v", err)
		}
	}
}

// The one-shot job runs once, as root, and removes itself.
func TestOneShotJob(t *testing.T) {
	plist := "/Library/Application Support/BackupZit/updates/com.backupzit.agent.update.1.plist"
	j := oneShotJob("com.backupzit.agent.update.1", plist, "echo hi")
	if !j.RunAtLoad || j.KeepAlive || !j.LaunchOnlyOnce || j.UserName != "root" {
		t.Errorf("job %+v", j)
	}
	if len(j.ProgramArguments) != 3 || j.ProgramArguments[0] != "/bin/sh" || j.ProgramArguments[1] != "-c" {
		t.Fatalf("program %q", j.ProgramArguments)
	}
	script := j.ProgramArguments[2]
	shellSyntax(t, script)
	for _, want := range []string{"echo hi\n", "rm -f " + shellQuote(plist), "exec /bin/launchctl bootout 'system/com.backupzit.agent.update.1'"} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
	if b, err := j.Plist(); err != nil {
		t.Fatal(err)
	} else if plutil, err := exec.LookPath("plutil"); err == nil {
		p := filepath.Join(t.TempDir(), "job.plist")
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(plutil, "-lint", p).CombinedOutput(); err != nil {
			t.Errorf("plutil: %v: %s", err, out)
		}
	}
	if a, b := detachedLabel("update"), detachedLabel("update"); a == b || !strings.HasPrefix(a, "com.backupzit.agent.update.") {
		t.Errorf("labels %s, %s", a, b)
	}
}

func TestUninstallScriptDarwin(t *testing.T) {
	s := uninstallScript()
	shellSyntax(t, s)
	for _, want := range []string{
		"/bin/launchctl bootout 'system/com.backupzit.agent'",
		"rm -f '/Library/LaunchDaemons/com.backupzit.agent.plist'",
		`/bin/launchctl bootout "gui/$uid/com.backupzit.tray"`,
		"rm -f '/Library/LaunchAgents/com.backupzit.tray.plist'",
		"rm -rf '/Library/BackupZit' '/Applications/BackupZit.app'",
		"/usr/sbin/pkgutil --forget 'com.backupzit.agent'",
		"exec >>'/Library/Logs/BackupZit/uninstall.log' 2>&1",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
	// Configuration and log are kept.
	for _, kept := range []string{"Application Support", "rm -rf '/Library/Logs"} {
		if strings.Contains(s, kept) {
			t.Errorf("script touches %q:\n%s", kept, s)
		}
	}
	// The daemon is stopped before its files are removed.
	if strings.Index(s, "bootout 'system/") > strings.Index(s, "rm -rf") {
		t.Error("files removed before the agent is stopped")
	}
}

func TestInstalledFromPkg(t *testing.T) {
	no := func(string) bool { return false }
	yes := func(string) bool { return true }
	if installedFromPkg(no, func() bool { return false }) {
		t.Error("nothing installed, but accepted")
	}
	if !installedFromPkg(yes, func() bool { return false }) || !installedFromPkg(no, func() bool { return true }) {
		t.Error("LaunchDaemon or receipt not recognized")
	}
}
