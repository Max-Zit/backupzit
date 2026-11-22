package launchd

import (
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// launchctl exit statuses the agent reacts to. launchctl exits with an
// errno value (or one of launchd's own codes above 100).
const (
	// errIO (EIO, "Input/output error") is bootstrap's catch-all failure:
	// among other causes, the label is still listed in the domain because
	// a bootout has not finished yet.
	errIO = 5
	// errAlready (EALREADY, "Operation already in progress") is bootstrap's
	// answer while a bootout of the same label is still running.
	errAlready = 37
)

// exitCode is launchctl's exit status in an error from run: 0 for no error,
// -1 when launchctl did not run or did not exit normally.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// waitNotLoaded reads the job's state every interval until launchd no
// longer lists it, for up to timeout. A failed read is not final (launchctl
// can fail while the domain changes), it is only reported if the deadline
// passes. The state reader and sleep are parameters so the logic is tested
// without launchd.
func waitNotLoaded(state func() (State, error), timeout, interval time.Duration, sleep func(time.Duration)) error {
	var waited time.Duration
	for {
		st, err := state()
		if err == nil && st == NotLoaded {
			return nil
		}
		if waited >= timeout {
			if err != nil {
				return fmt.Errorf("the job is still loaded after %s: %w", timeout, err)
			}
			return fmt.Errorf("the job is still loaded after %s (its process did not exit)", timeout)
		}
		sleep(interval)
		waited += interval
	}
}

// jobOps are the launchctl operations startJob uses, injected for tests.
type jobOps struct {
	state     func() (State, error)
	bootstrap func() error
	kickstart func() error
	sleep     func(time.Duration)
}

// startJob starts a job whatever launchd's state is: nothing to do when it
// runs, kickstart when it is loaded but not running, bootstrap when it is
// not loaded. A job that was just booted out can still be listed for a
// moment ("not running"): kickstart then fails, and bootstrap fails with
// EIO or EALREADY. Such failures are retried, reading the state again each
// time, every interval for up to timeout; any other bootstrap failure (a
// bad property list, for one) is final.
func startJob(o jobOps, timeout, interval time.Duration) error {
	var waited time.Duration
	for {
		st, err := o.state()
		if err == nil {
			switch st {
			case Running:
				return nil
			case Stopped:
				err = o.kickstart()
			default:
				err = o.bootstrap()
				if c := exitCode(err); err != nil && c != errIO && c != errAlready {
					return err
				}
			}
			if err == nil {
				return nil
			}
		}
		if waited >= timeout {
			return err
		}
		o.sleep(interval)
		waited += interval
	}
}
