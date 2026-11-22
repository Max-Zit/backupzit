package launchd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// launchctl is the system's launchctl: by absolute path, so the agent never
// runs another program of that name found in PATH.
const launchctl = "/bin/launchctl"

// errNotFound is launchctl's exit status when the domain has no such job
// (ESRCH, "Could not find service").
const errNotFound = 113

// SystemTarget is the launchctl service target of a system daemon.
func SystemTarget(label string) string { return "system/" + label }

// SystemState asks launchd for the state of a system job. Reading the
// system domain works without root, so "service status" run by a user
// reports the daemon correctly (the legacy "launchctl list" used by
// kardianos/service only looks into the caller's own domain).
func SystemState(label string) (State, error) {
	out, err := exec.Command(launchctl, "print", SystemTarget(label)).CombinedOutput() // #nosec G204 -- fixed program, label is the agent's own constant
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == errNotFound {
			return NotLoaded, nil
		}
		return NotLoaded, fmt.Errorf("launchctl print %s: %w: %s", SystemTarget(label), err, bytes.TrimSpace(out))
	}
	return ParsePrint(string(out)), nil
}

// Bootstrap loads a system job from its property list; launchd starts it
// at once when it has RunAtLoad. A job disabled earlier ("launchctl
// disable") is enabled first, otherwise bootstrap refuses it.
func Bootstrap(label, plistPath string) error {
	_ = run("enable", SystemTarget(label))
	return run("bootstrap", "system", plistPath)
}

// Kickstart starts a loaded job that is not running (no -k: a running job
// is left alone).
func Kickstart(label string) error { return run("kickstart", SystemTarget(label)) }

// Bootout stops a system job and unloads it; launchd waits for the process
// to exit (SIGTERM, then SIGKILL after the job's ExitTimeOut).
func Bootout(label string) error { return run("bootout", SystemTarget(label)) }

func run(args ...string) error {
	out, err := exec.Command(launchctl, args...).CombinedOutput() // #nosec G204 -- fixed program; arguments are the agent's own labels and paths
	if err != nil {
		return fmt.Errorf("launchctl %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

// WritePlist writes a job definition where launchd accepts it: owned by
// root and writable by nobody else (launchd refuses a group- or
// world-writable property list). The file is replaced atomically, so a
// reinstall over a loaded job never leaves a half-written definition.
func WritePlist(path string, j Job) error {
	b, err := j.Plist()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil { // #nosec G302 -- launchd property lists are world-readable, like every file in /Library/LaunchDaemons
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
