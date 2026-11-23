package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/max-zit/backupzit/internal/launchd"
)

// Where the macOS package (.pkg) installs the agent. The service, the
// self-update and the self-uninstall share these names, so they must match
// the packaging.
const (
	// LaunchdLabel is the agent's launchd job label, and the package
	// identifier of its .pkg (what "pkgutil --pkgs" lists).
	LaunchdLabel = "com.backupzit.agent"
	// LaunchdPlist is the agent's LaunchDaemon definition.
	LaunchdPlist = "/Library/LaunchDaemons/" + LaunchdLabel + ".plist"
	// LaunchdLogPath receives what the agent writes to stdout and stderr
	// outside its own log (a crash trace): the agent's log is LogPath.
	LaunchdLogPath = "/Library/Logs/BackupZit/launchd.log"
	// InstallPrefix holds the binaries (InstallPrefix/bin).
	InstallPrefix = "/Library/BackupZit"

	pkgID           = LaunchdLabel
	trayLabel       = "com.backupzit.tray"
	trayLaunchAgent = "/Library/LaunchAgents/" + trayLabel + ".plist"
	trayApp         = "/Applications/BackupZit.app"
	// cliLink is a convenience link the package may add, so that
	// "backupzit-agent" works in a Terminal (/usr/local/bin is in PATH).
	cliLink = "/usr/local/bin/backupzit-agent"
	// macInstaller is Apple's package installer.
	macInstaller = "/usr/sbin/installer"
)

// detachedLabel is a unique label for a one-shot job of the agent
// ("update", "uninstall").
func detachedLabel(kind string) string {
	return LaunchdLabel + "." + kind + "." + strconv.FormatInt(time.Now().UnixNano(), 10)
}

// oneShotJob is the launchd job that runs script once, outside the agent's
// own job, then removes itself: its property list (plistPath) is deleted
// and the job booted out, so nothing of it is left loaded or starts again
// at the next boot. The last command is launchd unloading the job it runs
// in, which ends the job's shell as expected. It does not start when it is
// loaded (RunAtLoad off): prepareDetached loads it before the agent reports
// the run, and starts it only afterwards.
func oneShotJob(label, plistPath, script string) launchd.Job {
	cleanup := "rm -f " + shellQuote(plistPath) + "\nexec /bin/launchctl bootout " + shellQuote(launchd.SystemTarget(label)) + "\n"
	return launchd.Job{
		Label:            label,
		ProgramArguments: []string{"/bin/sh", "-c", script + "\n" + cleanup},
		UserName:         "root",
		RunAtLoad:        false,
		// Once: never restarted when the script fails (an installer run in a
		// loop), nor if launchd restarts.
		LaunchOnlyOnce:   true,
		WorkingDirectory: "/",
	}
}

// prepareDetached loads script as a one-shot launchd system job that is not
// running yet, and returns the function that starts it. The property list
// lives in dir, a folder only root can change (the update folder, checked
// by privateDir, or the agent's state folder): launchd runs what that file
// says as root.
//
// Why in two steps: the agent must report the run before the job starts,
// because the installation (or uninstallation) stops the agent. Loading the
// job is where launchd checks it and can refuse it; done before the report,
// such a refusal fails the run in the console instead of being written only
// to the agent's own log while the console shows a success.
// Starting a loaded job (kickstart) is all that is left for afterwards.
//
// Why a launchd job and not a child process (Setsid, like Linux without
// systemd-run): launchd ends every process of a job it stops, and the
// package's postinstall restarts the agent's job; an installer started by
// the agent would be killed in the middle of the installation. A job of its
// own belongs to launchd, not to the agent, like a systemd-run unit on
// Linux. "launchctl submit" was not used: it asks launchd to keep the
// program alive, so a finished installer would be started again and again.
func prepareDetached(dir, kind, script string) (start func() error, err error) {
	return prepareJob(dir, kind, script)
}

// prepareJob is prepareDetached's work, replaced by tests: loading a real
// launchd system job needs root and would run the script.
var prepareJob = loadDetached

func loadDetached(dir, kind, script string) (start func() error, err error) {
	label := detachedLabel(kind)
	plist := filepath.Join(dir, label+".plist")
	if err := launchd.WritePlist(plist, oneShotJob(label, plist, script)); err != nil {
		return nil, err
	}
	if err := launchd.BootstrapNew(plist); err != nil {
		_ = os.Remove(plist)
		return nil, fmt.Errorf("launchd refused the %s job: %w", kind, err)
	}
	return func() error {
		if err := launchd.Kickstart(label); err != nil {
			// Never started: unloaded and removed, so it cannot start by
			// surprise later; the next request prepares a new one.
			_ = launchd.Bootout(label, detachedBootoutWait)
			_ = os.Remove(plist)
			return err
		}
		return nil
	}, nil
}

// detachedBootoutWait bounds the wait for launchd to unload a one-shot job
// that could not be started (it runs nothing, so it unloads at once).
const detachedBootoutWait = 10 * time.Second
