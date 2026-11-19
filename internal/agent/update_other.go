//go:build !windows

package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// installerCommand installs the package outside the service's control
// group (systemd-run), so stopping the service during the upgrade does not
// stop the package manager.
func installerCommand(path string) (func() error, error) {
	var script string
	switch {
	case strings.HasSuffix(path, ".deb"):
		if _, err := exec.LookPath("dpkg"); err != nil {
			return nil, fmt.Errorf("deb package, but dpkg is not available on this system")
		}
		script = "dpkg -i " + shellQuote(path)
	case strings.HasSuffix(path, ".rpm"):
		if _, err := exec.LookPath("rpm"); err != nil {
			return nil, fmt.Errorf("rpm package, but rpm is not available on this system")
		}
		script = "rpm -U --force " + shellQuote(path)
	default:
		return nil, fmt.Errorf("%s is not a deb or rpm package", filepath.Base(path))
	}
	script += " >" + shellQuote(path+".log") + " 2>&1"
	return func() error {
		if _, err := exec.LookPath("systemd-run"); err == nil {
			unit := "backupzit-agent-update-" + strconv.FormatInt(time.Now().Unix(), 10)
			return exec.Command("systemd-run", "--unit", unit, "--collect", "--quiet", "/bin/sh", "-c", script).Run() // #nosec G204 -- script and package built by the agent from fixed text and its verified update
		}
		cmd := exec.Command("/bin/sh", "-c", script) // #nosec G204 -- script and package built by the agent from fixed text and its verified update
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// privateDir makes sure the update folder is root's own: a real folder
// (not a link), owned by the agent's user, writable by nobody else, so
// that nobody can swap the checked package before it is installed.
func privateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is not a folder owned by the agent's user; refusing to install from it", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return os.Chmod(dir, 0o700) // #nosec G302 -- a folder: 0700 is root only
	}
	return nil
}
