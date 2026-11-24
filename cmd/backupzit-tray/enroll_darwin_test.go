package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/localipc"
)

func TestOsascriptError(t *testing.T) {
	for _, c := range []struct {
		in       string
		want     string
		canceled bool
	}{
		{"0:153: execution error: User canceled. (-128)\n", "", true},
		{"0:153: execution error: error: agent is already enrolled (/Library/Application Support/BackupZit/agent.json); use --force to enroll again (1)\n",
			"agent is already enrolled (/Library/Application Support/BackupZit/agent.json); use --force to enroll again", false},
		{"0:153: execution error: error: enroll: invalid or expired enrollment token\nmore (1)", "enroll: invalid or expired enrollment token more", false},
		{"something else", "something else", false},
	} {
		got, canceled := osascriptError(c.in)
		if got != c.want || canceled != c.canceled {
			t.Errorf("osascriptError(%q) = %q, %v; want %q, %v", c.in, got, canceled, c.want, c.canceled)
		}
	}
}

func TestCleanCodeAndSentence(t *testing.T) {
	if got := cleanCode("  BZ1-abc\ndef \n"); got != "BZ1-abcdef" {
		t.Errorf("cleanCode = %q", got)
	}
	if got := sentence("not a BackupZit enrollment code (it starts with BZ1-)"); got != "Not a BackupZit enrollment code (it starts with BZ1-)." {
		t.Errorf("sentence = %q", got)
	}
}

func TestShouldAskEnroll(t *testing.T) {
	for _, c := range []struct {
		st   *localipc.Status
		want bool
	}{
		{nil, false}, // the agent does not answer: its own hint first
		{&localipc.Status{}, true},
		{&localipc.Status{Enrolled: true}, false},
		{&localipc.Status{ServerURL: "https://c:8443"}, false}, // a code from the installation, still trying
		{&localipc.Status{ServerURL: "https://c:8443", LastError: "invalid or expired enrollment token"}, true},
	} {
		if got := shouldAskEnroll(c.st); got != c.want {
			t.Errorf("shouldAskEnroll(%+v) = %v", c.st, got)
		}
	}
}

// The scripts compile (osacompile checks the syntax without running
// them), and the code reaches the shell only through "quoted form of".
func TestEnrollScripts(t *testing.T) {
	if !strings.Contains(enrollScript, `quoted form of (item 1 of argv)`) || strings.Contains(enrollScript, "%") {
		t.Error("the code is not quoted for the shell")
	}
	if !strings.HasPrefix(enrollScript, "on run argv\ndo shell script \""+agentProgram+" enroll --code \"") {
		t.Errorf("enroll script: %s", enrollScript)
	}
	if _, err := exec.LookPath("osacompile"); err != nil {
		t.Skip("no osacompile")
	}
	for name, s := range map[string]string{"ask": askCodeScript, "enroll": enrollScript, "info": infoScript} {
		out := filepath.Join(t.TempDir(), name+".scpt")
		if b, err := exec.Command("osacompile", "-o", out, "-e", s).CombinedOutput(); err != nil {
			t.Errorf("%s script does not compile: %v: %s", name, err, b)
		}
		_ = os.Remove(out)
	}
}
