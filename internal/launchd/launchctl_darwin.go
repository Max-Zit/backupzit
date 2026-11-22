package launchd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// launchctl exit statuses of bootout and print (see wait.go for the others).
const (
	// errInProgress (EINPROGRESS, "Operation now in progress") is what
	// bootout returns when the job's process does not exit at once: the
	// unload goes on in the background.
	errInProgress = 36
	// errNotFound (ESRCH, "Could not find service") is the exit status
	// when the domain has no such job.
	errNotFound = 113
)

// pollInterval is how often the state of a job is read while waiting for
// launchd to finish unloading or loading it.
const pollInterval = 500 * time.Millisecond

// launchctl is the system's launchctl: by absolute path, so the agent never
// runs another program of that name found in PATH.
const launchctl = "/bin/launchctl"

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

// Bootout stops a system job and unloads it, and returns once launchd no
// longer lists it, or with an error after wait. launchd sends SIGTERM, then
// SIGKILL after the job's ExitTimeOut, so wait should be a little longer
// than that. launchctl does not always wait itself: a process that takes a
// while to exit makes it fail with EINPROGRESS while the unload goes on,
// and the label stays listed ("not running") for a moment after the
// process is gone. A bootstrap in that window fails, so the caller must
// not go on before the job is really unloaded.
func Bootout(label string, wait time.Duration) error {
	err := run("bootout", SystemTarget(label))
	switch exitCode(err) {
	case 0, errInProgress:
	case errNotFound:
		return nil // unloaded by someone else in the meantime
	default:
		return err
	}
	return waitNotLoaded(func() (State, error) { return SystemState(label) }, wait, pollInterval, time.Sleep)
}

// Start starts a system job: it loads the job from its property list when
// launchd does not know it (it starts at once: RunAtLoad), or starts a
// loaded job that is not running. Right after a bootout, launchd may still
// list the label while it finishes unloading; bootstrap then fails with
// EIO or EALREADY and kickstart fails too, so Start reads the state again
// and retries for up to wait.
func Start(label, plistPath string, wait time.Duration) error {
	return startJob(jobOps{
		state:     func() (State, error) { return SystemState(label) },
		bootstrap: func() error { return Bootstrap(label, plistPath) },
		kickstart: func() error { return Kickstart(label) },
		sleep:     time.Sleep,
	}, wait, pollInterval)
}

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
