package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Checks inside a boot-tested VM (0.61): a VM that boots may still be
// broken or already compromised when the backup was made. A job can name
// what must work in the booted copy: a service that runs, a file that
// exists, a port that listens, or a command of its own that succeeds. They
// run through the QEMU guest agent (qm guest exec) in the isolated copy.

// Check is one check of a boot test.
type Check struct {
	VMID  int    `json:"vmid,omitempty"` // only this VM of the backup; 0: every VM
	Kind  string `json:"kind"`           // service | file | port | command
	Value string `json:"value"`
}

// CheckResult is the outcome of a check.
type CheckResult struct {
	Check  string `json:"check"` // as written in the job ("service nginx")
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"` // why it failed
}

func (c Check) String() string {
	s := c.Kind + " " + c.Value
	if c.VMID != 0 {
		s = strconv.Itoa(c.VMID) + ": " + s
	}
	return s
}

const maxChecks = 20

var (
	serviceName = regexp.MustCompile(`^[A-Za-z0-9_.@:+-]{1,128}$`)
	checkVM     = regexp.MustCompile(`^(\d{1,9}):\s*(.*)$`)
)

// ParseChecks reads the checks of a job, one per line:
//
//	service sshd
//	file /etc/hostname           (Windows: C:\Windows\System32\drivers\etc\hosts)
//	port 443
//	command systemctl is-system-running
//	101: service nginx           (only for VM 101)
//
// Empty lines and lines starting with # are ignored.
func ParseChecks(lines []string) ([]Check, error) {
	var out []Check
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		var c Check
		if m := checkVM.FindStringSubmatch(l); m != nil {
			c.VMID, _ = strconv.Atoi(m[1])
			l = m[2]
		}
		kind, val, _ := strings.Cut(l, " ")
		c.Kind, c.Value = strings.ToLower(kind), strings.TrimSpace(val)
		if strings.ContainsAny(c.Value, "\x00\r\n") {
			return nil, fmt.Errorf("check %q: no control characters", l)
		}
		switch c.Kind {
		case "service":
			if !serviceName.MatchString(c.Value) {
				return nil, fmt.Errorf("check %q: enter a service name (letters, digits and . _ @ : + -)", l)
			}
		case "file":
			if c.Value == "" || len(c.Value) > 1024 {
				return nil, fmt.Errorf("check %q: enter the full path of the file", l)
			}
		case "port":
			if p, err := strconv.Atoi(c.Value); err != nil || p < 1 || p > 65535 {
				return nil, fmt.Errorf("check %q: enter a TCP port from 1 to 65535", l)
			}
		case "command":
			if c.Value == "" || len(c.Value) > 1000 {
				return nil, fmt.Errorf("check %q: enter the command (at most 1000 characters)", l)
			}
		default:
			return nil, fmt.Errorf("check %q: start it with service, file, port or command", l)
		}
		out = append(out, c)
	}
	if len(out) > maxChecks {
		return nil, fmt.Errorf("at most %d checks", maxChecks)
	}
	return out, nil
}

// For picks the checks that apply to VM vmid.
func For(checks []Check, vmid int) []Check {
	var out []Check
	for _, c := range checks {
		if c.VMID == 0 || c.VMID == vmid {
			out = append(out, c)
		}
	}
	return out
}

// psQuote quotes s for a single-quoted PowerShell string.
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// checkArgv is the program and arguments that perform check c in a guest
// (windows: a Windows guest). No shell is used for service and file checks
// on Linux; values are passed as arguments or quoted.
func checkArgv(c Check, windows bool) []string {
	if windows {
		ps := func(script string) []string {
			return []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script}
		}
		switch c.Kind {
		case "service":
			return ps("$s = Get-Service -Name " + psQuote(c.Value) + " -ErrorAction Stop; if ($s.Status -ne 'Running') { Write-Output ('status ' + $s.Status); exit 1 }")
		case "file":
			return ps("if (-not (Test-Path -LiteralPath " + psQuote(c.Value) + ")) { exit 1 }")
		case "port":
			return ps("if (-not (Get-NetTCPConnection -State Listen -LocalPort " + c.Value + " -ErrorAction SilentlyContinue)) { exit 1 }")
		default:
			return []string{"cmd.exe", "/c", c.Value}
		}
	}
	switch c.Kind {
	case "service":
		return []string{"systemctl", "is-active", "--quiet", c.Value}
	case "file":
		return []string{"test", "-e", c.Value}
	case "port": // the port is a validated number
		return []string{"sh", "-c", "ss -Hltn 2>/dev/null | awk '{print $4}' | grep -Eq '[:.]" + c.Value + "$' || netstat -ltn 2>/dev/null | awk '{print $4}' | grep -Eq '[:.]" + c.Value + "$'"}
	default:
		return []string{"sh", "-c", c.Value}
	}
}

// guestExec runs argv in VM id through the QEMU guest agent and returns its
// exit code and output.
func guestExec(ctx context.Context, id string, argv []string) (int, string, error) {
	args := append([]string{"guest", "exec", id, "--timeout", "60", "--"}, argv...)
	out, err := command(ctx, "qm", args...)
	var res struct {
		ExitCode int    `json:"exitcode"`
		Exited   int    `json:"exited"`
		Out      string `json:"out-data"`
		Err      string `json:"err-data"`
	}
	if jerr := json.Unmarshal(out, &res); jerr != nil {
		if err == nil {
			err = jerr
		}
		return -1, strings.TrimSpace(string(out)), err
	}
	if res.Exited != 1 {
		return -1, "", errors.New("the command did not finish within 60 seconds")
	}
	return res.ExitCode, strings.TrimSpace(res.Out + " " + res.Err), nil
}

// guestWindows tells whether the guest in VM id runs Windows.
func guestWindows(ctx context.Context, id string) bool {
	out, err := command(ctx, "qm", "guest", "cmd", id, "get-osinfo")
	if err != nil {
		return false
	}
	var info struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(out, &info)
	return info.ID == "mswindows"
}

// checkPause is the wait between two tries of the checks that failed.
var checkPause = 10 * time.Second

// runChecks runs the checks in the booted VM id until each one passes or
// the deadline comes (services may still be starting after the guest agent
// answers).
func runChecks(ctx context.Context, id string, checks []Check, deadline time.Time) []CheckResult {
	windows := guestWindows(ctx, id)
	res := make([]CheckResult, len(checks))
	for i, c := range checks {
		res[i].Check = c.String()
	}
	for {
		left := 0
		for i, c := range checks {
			if res[i].OK {
				continue
			}
			code, out, err := guestExec(ctx, id, checkArgv(c, windows))
			switch {
			case err != nil:
				res[i].Detail = err.Error()
			case code == 0:
				res[i].OK, res[i].Detail = true, ""
				continue
			default:
				res[i].Detail = fmt.Sprintf("exit code %d", code)
				if out != "" {
					res[i].Detail += ": " + clipText(out, 300)
				}
			}
			left++
		}
		if left == 0 || !time.Now().Add(checkPause).Before(deadline) {
			return res
		}
		select {
		case <-ctx.Done():
			return res
		case <-time.After(checkPause):
		}
	}
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
