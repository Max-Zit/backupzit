package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The agent's LaunchDaemon: a stable label, root, started at boot, kept
// alive, and a definition launchd's parser accepts.
func TestAgentLaunchDaemon(t *testing.T) {
	j := agentJob("/Library/BackupZit/bin/backupzit-agent", []string{"service", "run", "--config", "/Library/Application Support/BackupZit/other.json"})
	b, err := j.Plist()
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"<key>Label</key>\n\t<string>com.backupzit.agent</string>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>KeepAlive</key>\n\t<true/>",
		"<key>UserName</key>\n\t<string>root</string>",
		"<key>ThrottleInterval</key>\n\t<integer>30</integer>",
		"<key>StandardErrorPath</key>\n\t<string>/Library/Logs/BackupZit/launchd.log</string>",
		"\t\t<string>/Library/BackupZit/bin/backupzit-agent</string>\n\t\t<string>service</string>\n\t\t<string>run</string>\n\t\t<string>--config</string>\n\t\t<string>/Library/Application Support/BackupZit/other.json</string>\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("LaunchDaemon lacks %q:\n%s", want, s)
		}
	}
	p := filepath.Join(t.TempDir(), "com.backupzit.agent.plist")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("/usr/bin/plutil", "-lint", p).CombinedOutput(); err != nil {
		t.Errorf("plutil -lint: %v: %s", err, out)
	}
}

func TestRootOnlyPath(t *testing.T) {
	if err := rootOnlyPath("/bin/sh"); err != nil {
		t.Errorf("/bin/sh refused: %v", err)
	}
	if os.Geteuid() == 0 {
		t.Skip("as root, a temporary file belongs to root")
	}
	f := filepath.Join(t.TempDir(), "backupzit-agent")
	if err := os.WriteFile(f, []byte("x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := rootOnlyPath(f); err == nil {
		t.Error("a program the user can replace was accepted")
	}
}

// As a user, status and the other commands that only read must not need
// root, and "not installed" is an answer, not a crash.
func TestStatusAsUser(t *testing.T) {
	svc, _, err := newService(filepath.Join(t.TempDir(), "agent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Status(); err != nil && !strings.Contains(err.Error(), "not installed") {
		t.Errorf("status: %v", err)
	}
	if os.Geteuid() != 0 {
		if err := svc.Install(); err == nil || !strings.Contains(err.Error(), "root") {
			t.Errorf("install as a user: %v", err)
		}
	}
}
