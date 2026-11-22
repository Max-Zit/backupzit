package agent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/max-zit/backupzit/internal/launchd"
)

// uninstallerCommand removes the agent installed by the .pkg, in a
// one-shot launchd job (see prepareDetached), because the first thing it
// does is stopping the agent's own LaunchDaemon. The job is loaded before
// the run is reported (a refusal fails it), and started afterwards. As with a package removal on
// Linux, the configuration (/Library/Application Support/BackupZit) and the
// log (/Library/Logs/BackupZit) stay; the uninstall log is written there.
func uninstallerCommand() (func() error, error) {
	if !installedFromPkg(fileExists, func() bool {
		return exec.Command("/usr/sbin/pkgutil", "--pkg-info", pkgID).Run() == nil // #nosec G204 -- fixed program and package identifier
	}) {
		return nil, errors.New("the agent was not installed from the BackupZit package (.pkg); uninstall it by hand")
	}
	dir := stateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := protectDir(dir); err != nil {
		return nil, err
	}
	return prepareDetached(dir, "uninstall", uninstallScript())
}

func fileExists(p string) bool { _, err := os.Lstat(p); return err == nil }

// installedFromPkg: the LaunchDaemon or the package receipt is there.
func installedFromPkg(exists func(string) bool, receipt func() bool) bool {
	return exists(LaunchdPlist) || receipt()
}

// uninstallScript stops and removes the agent's LaunchDaemon, the menu bar
// app's LaunchAgent in every open session, the installed files and the
// package receipt. Each step goes on when the one before fails (an item
// already removed by hand must not keep the rest), and everything is
// logged.
func uninstallScript() string {
	logDir := filepath.Dir(LaunchdLogPath)
	log := filepath.Join(logDir, "uninstall.log")
	q := shellQuote
	lines := []string{
		// The agent reports "uninstalling" to the console before it is
		// stopped.
		"sleep 2",
		"mkdir -p -m 700 " + q(logDir),
		"exec >>" + q(log) + " 2>&1",
		`echo "$(date): uninstalling the BackupZit agent"`,
		"/bin/launchctl bootout " + q(launchd.SystemTarget(LaunchdLabel)),
		"rm -f " + q(LaunchdPlist),
		// The menu bar app runs in each user's GUI session (one loginwindow
		// per signed-in user, fast user switching included).
		`for uid in $(/bin/ps -axo uid=,comm= | /usr/bin/awk '$2 ~ /\/loginwindow$/ {print $1}' | /usr/bin/sort -u); do`,
		`  /bin/launchctl bootout "gui/$uid/` + trayLabel + `"`,
		"done",
		"rm -f " + q(trayLaunchAgent),
		"/usr/bin/pkill -f " + q("^"+trayApp+"/"),
		"rm -rf " + q(InstallPrefix) + " " + q(trayApp),
		// Only the package's own link: a file of the same name that points
		// elsewhere is not the agent's.
		`case "$(/usr/bin/readlink ` + q(cliLink) + `)" in ` + InstallPrefix + "/*) rm -f " + q(cliLink) + ";; esac",
		"/usr/sbin/pkgutil --forget " + q(pkgID),
		`echo "$(date): BackupZit agent removed (configuration and log kept)"`,
	}
	return strings.Join(lines, "\n")
}
