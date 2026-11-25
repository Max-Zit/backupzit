package pve

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseChecks(t *testing.T) {
	checks, err := ParseChecks([]string{"service sshd", "", "# a note", "FILE /etc/hostname", "port 443", "101: command systemctl is-system-running", "102:service nginx.service"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Check{{0, "service", "sshd"}, {0, "file", "/etc/hostname"}, {0, "port", "443"}, {101, "command", "systemctl is-system-running"}, {102, "service", "nginx.service"}}
	if len(checks) != len(want) {
		t.Fatalf("%+v", checks)
	}
	for i := range want {
		if checks[i] != want[i] {
			t.Errorf("check %d: %+v, want %+v", i, checks[i], want[i])
		}
	}
	if got := For(checks, 101); len(got) != 4 || got[3].VMID != 101 {
		t.Errorf("for VM 101: %+v", got)
	}
	for _, bad := range [][]string{{"service ssh; reboot"}, {"service"}, {"port 0"}, {"port 70000"}, {"port 44x"}, {"file "}, {"ping 8.8.8.8"}, {"command "}, {"file /x\x00y"}} {
		if _, err := ParseChecks(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	many := make([]string, maxChecks+1)
	for i := range many {
		many[i] = "port 22"
	}
	if _, err := ParseChecks(many); err == nil {
		t.Error("accepted too many checks")
	}
}

// Service and file checks pass their value as an argument (no shell on
// Linux); on Windows the value is quoted for PowerShell.
func TestCheckArgv(t *testing.T) {
	if a := checkArgv(Check{Kind: "service", Value: "sshd"}, false); strings.Join(a, " ") != "systemctl is-active --quiet sshd" {
		t.Errorf("linux service: %q", a)
	}
	if a := checkArgv(Check{Kind: "file", Value: "/etc/my file"}, false); len(a) != 3 || a[2] != "/etc/my file" {
		t.Errorf("linux file: %q", a)
	}
	if a := checkArgv(Check{Kind: "port", Value: "443"}, false); a[0] != "sh" || !strings.Contains(a[2], "[:.]443$") {
		t.Errorf("linux port: %q", a)
	}
	a := checkArgv(Check{Kind: "file", Value: `C:\it's\x.txt`}, true)
	if a[0] != "powershell.exe" || !strings.Contains(a[len(a)-1], `-LiteralPath 'C:\it''s\x.txt'`) {
		t.Errorf("windows file: %q", a)
	}
	if a := checkArgv(Check{Kind: "command", Value: "sc query spooler"}, true); strings.Join(a, " ") != "cmd.exe /c sc query spooler" {
		t.Errorf("windows command: %q", a)
	}
}

// runChecks tries the failing checks again until they pass or time is up:
// a service still starting passes on the second try, a missing file fails
// with its exit code.
func TestRunChecks(t *testing.T) {
	old, oldPause := command, checkPause
	t.Cleanup(func() { command, checkPause = old, oldPause })
	checkPause = 10 * time.Millisecond
	tries := map[string]int{}
	command = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "qm" || len(args) < 3 || args[0] != "guest" {
			t.Fatalf("unexpected %s %q", name, args)
		}
		if args[1] == "cmd" { // get-osinfo
			return []byte(`{"id":"debian","name":"Debian GNU/Linux"}`), nil
		}
		argv := strings.Join(args[6:], " ")
		tries[argv]++
		code := 0
		switch {
		case strings.Contains(argv, "nginx") && tries[argv] < 2:
			code = 3
		case strings.Contains(argv, "/missing"):
			code = 1
		}
		b, _ := json.Marshal(map[string]any{"exited": 1, "exitcode": code, "out-data": ""})
		return b, nil
	}
	res := runChecks(context.Background(), "9001", []Check{{Kind: "service", Value: "nginx"}, {Kind: "file", Value: "/missing"}, {Kind: "port", Value: "22"}}, time.Now().Add(200*time.Millisecond))
	if !res[0].OK || res[1].OK || !res[2].OK {
		t.Fatalf("%+v", res)
	}
	if res[1].Check != "file /missing" || !strings.Contains(res[1].Detail, "exit code 1") {
		t.Errorf("failed check: %+v", res[1])
	}
	if tries["systemctl is-active --quiet nginx"] != 2 {
		t.Errorf("nginx tried %d times", tries["systemctl is-active --quiet nginx"])
	}
}
