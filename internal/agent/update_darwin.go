package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// installerCommand installs the verified package with Apple's installer
// in a one-shot launchd job of its own (see prepareDetached): the package's
// postinstall restarts the agent's LaunchDaemon, which must not stop the
// installation. The job is loaded here, before the agent reports the update
// run, so a job launchd refuses fails the run; the returned function only
// starts it. The installer's output goes to <package>.log in the update
// folder, which "send the agent log" includes.
func installerCommand(path string) (func() error, error) {
	if !strings.HasSuffix(path, ".pkg") {
		return nil, fmt.Errorf("%s is not a macOS installer package (.pkg)", filepath.Base(path))
	}
	if _, err := os.Stat(macInstaller); err != nil {
		return nil, fmt.Errorf("macOS package, but %s is not available on this system", macInstaller)
	}
	return prepareDetached(filepath.Dir(path), "update", pkgInstallScript(path))
}

// pkgInstallScript installs a package on the startup volume. The package
// is the update folder's own file (checked by privateDir, verified against
// the signed release); its path is quoted for the shell.
func pkgInstallScript(path string) string {
	pkg, log := shellQuote(path), shellQuote(path+".log")
	return "printf '%s: installing %s\\n' \"$(date)\" " + pkg + " >" + log + "\n" +
		macInstaller + " -verbose -pkg " + pkg + " -target / >>" + log + " 2>&1\n" +
		"rc=$?\n" +
		"printf '%s: installer exited with status %s\\n' \"$(date)\" \"$rc\" >>" + log
}
