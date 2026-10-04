package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Operating system updates of the console server, carried out by the same
// root helper: the console asks for a status, for automatic security
// updates to be switched on or off, for all updates to be installed now,
// or for a reboot.

// OS actions of a Request.
const (
	ActionInstall   = ""          // install the console update of the request
	ActionOSStatus  = "os-status" // refresh os-status.json
	ActionOSAuto    = "os-auto"   // automatic security updates on/off (Enable)
	ActionOSUpgrade = "os-upgrade"
	ActionReboot    = "reboot"
)

// OSStatus describes the console server's operating system updates.
type OSStatus struct {
	OS             string    `json:"os"`
	Kernel         string    `json:"kernel"`
	Manager        string    `json:"manager"` // apt or dnf
	Pending        int       `json:"pending"`
	Security       int       `json:"security"`
	Packages       []string  `json:"packages,omitempty"` // first pending packages
	AutoSecurity   bool      `json:"auto_security"`
	LastAutoRun    time.Time `json:"last_auto_run,omitempty"`
	RebootRequired bool      `json:"reboot_required"`
	Checked        time.Time `json:"checked"`
	Error          string    `json:"error,omitempty"`
}

// OSResult reports the last OS action.
type OSResult struct {
	Action   string    `json:"action"`
	Status   string    `json:"status"` // done, failed
	Message  string    `json:"message"`
	Finished time.Time `json:"finished"`
}

// osSystem abstracts the package manager.
type osSystem interface {
	status(ctx context.Context, st *OSStatus) error
	setAuto(ctx context.Context, on bool) error
	upgrade(ctx context.Context) (string, error)
}

func (a *Applier) osSystem() (osSystem, error) {
	if a.OSFamily == "apt" || (a.OSFamily == "" && exists(a.root("/usr/bin/apt-get"))) {
		return &aptSystem{a}, nil
	}
	if a.OSFamily == "dnf" || (a.OSFamily == "" && exists(a.root("/usr/bin/dnf"))) {
		return &dnfSystem{a}, nil
	}
	return nil, errors.New("no supported package manager (apt or dnf) found")
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// root maps an absolute path into a.Root (tests).
func (a *Applier) root(p string) string { return filepath.Join(a.Root, p) }

// osAction runs an OS request and refreshes the status.
func (a *Applier) osAction(ctx context.Context, req Request) (*Result, error) {
	res := OSResult{Action: req.Action, Status: "done"}
	sys, err := a.osSystem()
	if err == nil {
		switch req.Action {
		case ActionOSStatus:
			res.Message = "status refreshed"
		case ActionOSAuto:
			err = sys.setAuto(ctx, req.Enable)
			res.Message = map[bool]string{true: "automatic security updates switched on", false: "automatic security updates switched off"}[req.Enable]
		case ActionOSUpgrade:
			var out string
			out, err = sys.upgrade(ctx)
			res.Message = "all updates installed"
			if out != "" {
				res.Message += "\n\n" + lastN(out, 25)
			}
		case ActionReboot:
			res.Message = "the server restarts"
		default:
			err = fmt.Errorf("unknown action %q", req.Action)
		}
	}
	if err != nil {
		res.Status, res.Message = "failed", err.Error()
	}
	st := OSStatus{Checked: time.Now().UTC()}
	if sys != nil {
		if serr := sys.status(ctx, &st); serr != nil {
			st.Error = serr.Error()
		}
	} else {
		st.Error = err.Error()
	}
	res.Finished = time.Now().UTC()
	a.logf("%s: %s %s", req.Action, res.Status, res.Message)
	WriteJSON(filepath.Join(a.Dir, "os-status.json"), st, 0o644)
	werr := WriteJSON(filepath.Join(a.Dir, "os-result.json"), res, 0o644)
	if req.Action == ActionReboot && res.Status == "done" {
		_, err := a.Run(ctx, "systemctl", "reboot")
		if err != nil {
			res.Status, res.Message = "failed", err.Error()
			WriteJSON(filepath.Join(a.Dir, "os-result.json"), res, 0o644)
		}
	}
	return nil, werr
}

func lastN(s string, n int) string {
	l := strings.Split(strings.TrimSpace(strings.ReplaceAll(s, "\r", "")), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}

func osRelease(a *Applier) string {
	b, err := os.ReadFile(a.root("/etc/os-release"))
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

func kernel(ctx context.Context, a *Applier) string {
	out, err := a.Run(ctx, "uname", "-r")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ---- Debian, Ubuntu

type aptSystem struct{ a *Applier }

const autoUpgradesFile = "/etc/apt/apt.conf.d/20auto-upgrades"

// aptInstRe matches "Inst pkg [old] (new suite [arch])" of apt-get -s.
var aptInstRe = regexp.MustCompile(`^Inst (\S+)(?: \[[^\]]*\])? \((\S+) ([^\[)]*)`)

func (s *aptSystem) status(ctx context.Context, st *OSStatus) error {
	a := s.a
	st.Manager, st.OS, st.Kernel = "apt", osRelease(a), kernel(ctx, a)
	st.RebootRequired = exists(a.root("/var/run/reboot-required"))
	if b, err := os.ReadFile(a.root(autoUpgradesFile)); err == nil {
		st.AutoSecurity = regexp.MustCompile(`APT::Periodic::Unattended-Upgrade\s+"1"`).Match(b) && exists(a.root("/usr/bin/unattended-upgrade"))
	}
	if fi, err := os.Stat(a.root("/var/log/unattended-upgrades/unattended-upgrades.log")); err == nil {
		st.LastAutoRun = fi.ModTime().UTC()
	}
	if _, err := a.Run(ctx, "apt-get", "update", "-qq"); err != nil {
		return fmt.Errorf("refresh package lists: %w", err)
	}
	out, err := a.Run(ctx, "apt-get", "-s", "-o", "Debug::NoLocking=1", "upgrade", "--with-new-pkgs")
	if err != nil {
		return err
	}
	for _, l := range strings.Split(string(out), "\n") {
		m := aptInstRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		st.Pending++
		if strings.Contains(m[3], "security") || strings.Contains(m[3], "Security") {
			st.Security++
		}
		if len(st.Packages) < 50 {
			st.Packages = append(st.Packages, m[1]+" "+m[2])
		}
	}
	return nil
}

func (s *aptSystem) setAuto(ctx context.Context, on bool) error {
	a := s.a
	if on && !exists(a.root("/usr/bin/unattended-upgrade")) {
		if _, err := a.Run(ctx, "apt-get", "install", "-y", "-q", "unattended-upgrades"); err != nil {
			return err
		}
	}
	v := "0"
	if on {
		v = "1"
	}
	// The package's default configuration installs only security updates.
	conf := fmt.Sprintf("// Managed by BackupZit (Settings → Updates)\nAPT::Periodic::Update-Package-Lists \"%s\";\nAPT::Periodic::Unattended-Upgrade \"%s\";\n", v, v)
	return os.WriteFile(a.root(autoUpgradesFile), []byte(conf), 0o644)
}

func (s *aptSystem) upgrade(ctx context.Context) (string, error) {
	a := s.a
	if _, err := a.Run(ctx, "apt-get", "update", "-qq"); err != nil {
		return "", err
	}
	out, err := a.Run(ctx, "env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "-y", "-q",
		"-o", "Dpkg::Options::=--force-confold", "-o", "Dpkg::Options::=--force-confdef", "upgrade", "--with-new-pkgs")
	return string(out), err
}

// ---- AlmaLinux, Rocky Linux, RHEL

type dnfSystem struct{ a *Applier }

const dnfAutoConf = "/etc/dnf/automatic.conf"

func (s *dnfSystem) status(ctx context.Context, st *OSStatus) error {
	a := s.a
	st.Manager, st.OS, st.Kernel = "dnf", osRelease(a), kernel(ctx, a)
	if _, err := a.Run(ctx, "needs-restarting", "-r"); err != nil {
		st.RebootRequired = strings.Contains(err.Error(), "exit status 1")
	}
	if out, err := a.Run(ctx, "systemctl", "is-enabled", "dnf-automatic.timer"); err == nil && strings.TrimSpace(string(out)) == "enabled" {
		b, _ := os.ReadFile(a.root(dnfAutoConf))
		st.AutoSecurity = regexp.MustCompile(`(?m)^apply_updates\s*=\s*yes`).Match(b)
	}
	// dnf check-update exits with 100 when updates are available.
	out, err := a.Run(ctx, "dnf", "-q", "--refresh", "check-update")
	if err != nil && !strings.Contains(err.Error(), "exit status 100") {
		return err
	}
	var pkgs []string
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) == 3 && strings.Contains(f[0], ".") && !strings.HasPrefix(l, " ") {
			st.Pending++
			if len(pkgs) < 50 {
				pkgs = append(pkgs, f[0]+" "+f[1])
			}
		}
	}
	sort.Strings(pkgs)
	st.Packages = pkgs
	if sec, err := a.Run(ctx, "dnf", "-q", "updateinfo", "list", "--security"); err == nil {
		for _, l := range strings.Split(string(sec), "\n") {
			if strings.TrimSpace(l) != "" {
				st.Security++
			}
		}
	}
	return nil
}

func (s *dnfSystem) setAuto(ctx context.Context, on bool) error {
	a := s.a
	if !on {
		_, err := a.Run(ctx, "systemctl", "disable", "--now", "dnf-automatic.timer")
		return err
	}
	if !exists(a.root(dnfAutoConf)) {
		if _, err := a.Run(ctx, "dnf", "install", "-y", "-q", "dnf-automatic"); err != nil {
			return err
		}
	}
	b, err := os.ReadFile(a.root(dnfAutoConf))
	if err != nil {
		return err
	}
	conf := string(b)
	set := func(key, val string) {
		re := regexp.MustCompile(`(?m)^#?\s*` + key + `\s*=.*$`)
		if re.MatchString(conf) {
			conf = re.ReplaceAllString(conf, key+" = "+val)
		} else {
			conf = strings.Replace(conf, "[commands]", "[commands]\n"+key+" = "+val, 1)
		}
	}
	set("upgrade_type", "security")
	set("apply_updates", "yes")
	if err := os.WriteFile(a.root(dnfAutoConf), []byte(conf), 0o644); err != nil {
		return err
	}
	_, err = a.Run(ctx, "systemctl", "enable", "--now", "dnf-automatic.timer")
	return err
}

func (s *dnfSystem) upgrade(ctx context.Context) (string, error) {
	out, err := s.a.Run(ctx, "dnf", "-y", "-q", "upgrade")
	return string(out), err
}
