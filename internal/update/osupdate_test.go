package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func osApplier(t *testing.T, family string, run func(name string, args ...string) (string, error)) (*Applier, *[]string) {
	root := t.TempDir()
	for _, d := range []string{"etc/apt/apt.conf.d", "etc/dnf", "usr/bin", "var/run", "var/log/unattended-upgrades"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	os.WriteFile(filepath.Join(root, "etc/os-release"), []byte("NAME=\"Debian\"\nPRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\n"), 0o644)
	var calls []string
	a := &Applier{Dir: t.TempDir(), Root: root, OSFamily: family,
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, name+" "+strings.Join(args, " "))
			out, err := run(name, args...)
			return []byte(out), err
		}}
	return a, &calls
}

func request(t *testing.T, a *Applier, r Request) (OSStatus, OSResult) {
	WriteJSON(filepath.Join(a.Dir, "request.json"), r, 0o640)
	if _, err := a.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	var st OSStatus
	var res OSResult
	ReadJSON(filepath.Join(a.Dir, "os-status.json"), &st)
	ReadJSON(filepath.Join(a.Dir, "os-result.json"), &res)
	return st, res
}

const aptSim = `Reading package lists...
Inst libc6 [2.36-9+deb12u7] (2.36-9+deb12u8 Debian-Security:12/stable-security [amd64])
Inst tzdata [2024a-0+deb12u1] (2025b-0+deb12u1 Debian:12.11/stable [all])
Inst openssl [3.0.15-1~deb12u1] (3.0.17-1~deb12u2 Debian-Security:12/stable-security [amd64]) []
Conf libc6 (2.36-9+deb12u8 Debian-Security:12/stable-security [amd64])
`

func TestOSUpdatesApt(t *testing.T) {
	var app *Applier
	a, calls := osApplier(t, "apt", func(name string, args ...string) (string, error) {
		if name == "apt-get" && args[0] == "-s" {
			return aptSim, nil
		}
		if name == "apt-get" && args[0] == "install" {
			os.WriteFile(filepath.Join(app.Root, "usr/bin/unattended-upgrade"), nil, 0o755)
		}
		return "", nil
	})
	app = a
	os.WriteFile(filepath.Join(a.Root, "var/run/reboot-required"), nil, 0o644)
	st, res := request(t, a, Request{Action: ActionOSStatus})
	if res.Status != "done" || st.Pending != 3 || st.Security != 2 || st.AutoSecurity || !st.RebootRequired || st.OS != "Debian GNU/Linux 12 (bookworm)" || st.Manager != "apt" {
		t.Fatalf("status %+v %+v", st, res)
	}
	st, res = request(t, a, Request{Action: ActionOSAuto, Enable: true})
	conf, _ := os.ReadFile(filepath.Join(a.Root, autoUpgradesFile))
	if res.Status != "done" || !st.AutoSecurity || !strings.Contains(string(conf), `Unattended-Upgrade "1"`) || !strings.Contains(strings.Join(*calls, "\n"), "apt-get install -y -q unattended-upgrades") {
		t.Fatalf("auto on: %+v %+v %s", st, res, conf)
	}
	st, _ = request(t, a, Request{Action: ActionOSAuto, Enable: false})
	if st.AutoSecurity {
		t.Error("auto still on")
	}
	_, res = request(t, a, Request{Action: ActionOSUpgrade})
	if res.Status != "done" || !strings.Contains(strings.Join(*calls, "\n"), "DEBIAN_FRONTEND=noninteractive apt-get -y -q -o Dpkg::Options::=--force-confold") {
		t.Errorf("upgrade: %+v", res)
	}
	*calls = nil
	_, res = request(t, a, Request{Action: ActionReboot})
	if res.Status != "done" || (*calls)[len(*calls)-1] != "systemctl reboot" {
		t.Errorf("reboot: %+v %v", res, *calls)
	}
	_, res = request(t, a, Request{Action: "rm -rf /"})
	if res.Status != "failed" {
		t.Errorf("unknown action: %+v", res)
	}
}

func TestOSUpdatesDnf(t *testing.T) {
	a, _ := osApplier(t, "dnf", func(name string, args ...string) (string, error) {
		switch {
		case name == "dnf" && strings.Contains(strings.Join(args, " "), "check-update"):
			return "\nkernel.x86_64    5.14.0-570.el9   baseos\nopenssl.x86_64   1:3.2.2-6.el9    baseos\n", errors.New("dnf: exit status 100")
		case name == "dnf" && strings.Contains(strings.Join(args, " "), "--security"):
			return "RLSA-2025:1 Important/Sec. openssl-1:3.2.2-6.el9.x86_64\n", nil
		case name == "needs-restarting":
			return "", errors.New("needs-restarting: exit status 1")
		case name == "systemctl" && args[0] == "is-enabled":
			return "enabled\n", nil
		}
		return "", nil
	})
	os.WriteFile(filepath.Join(a.Root, dnfAutoConf), []byte("[commands]\nupgrade_type = default\napply_updates = no\n"), 0o644)
	st, _ := request(t, a, Request{Action: ActionOSStatus})
	if st.Pending != 2 || st.Security != 1 || !st.RebootRequired || st.AutoSecurity {
		t.Fatalf("dnf status %+v", st)
	}
	st, res := request(t, a, Request{Action: ActionOSAuto, Enable: true})
	conf, _ := os.ReadFile(filepath.Join(a.Root, dnfAutoConf))
	if res.Status != "done" || !st.AutoSecurity || !strings.Contains(string(conf), "upgrade_type = security") {
		t.Fatalf("dnf auto: %+v %s", res, conf)
	}
}
