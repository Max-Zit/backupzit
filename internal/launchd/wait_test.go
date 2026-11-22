package launchd

import (
	"errors"
	"os/exec"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// states returns a state reader that answers the given states in turn (the
// last one repeated) and counts its calls.
func states(calls *int, seq ...State) func() (State, error) {
	return func() (State, error) {
		i := *calls
		*calls++
		if i >= len(seq) {
			i = len(seq) - 1
		}
		return seq[i], nil
	}
}

// fakeSleep records the time "slept" instead of sleeping.
func fakeSleep(total *time.Duration) func(time.Duration) {
	return func(d time.Duration) { *total += d }
}

// exitErr makes a real *exec.ExitError with the given status, as launchctl
// would return.
func exitErr(t *testing.T, code int) error {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	err := exec.Command("/bin/sh", "-c", "exit "+strconv.Itoa(code)).Run()
	if exitCode(err) != code {
		t.Fatalf("exit status %d not reproduced: %v", code, err)
	}
	return err
}

func TestWaitNotLoadedAfterAsyncBootout(t *testing.T) {
	var calls int
	var slept time.Duration
	// Still running (the agent finishes a backup), then listed as not
	// running for a moment, then gone.
	read := states(&calls, Running, Running, Stopped, NotLoaded)
	if err := waitNotLoaded(read, 100*time.Second, 500*time.Millisecond, fakeSleep(&slept)); err != nil {
		t.Fatal(err)
	}
	if calls != 4 || slept != 1500*time.Millisecond {
		t.Errorf("calls=%d slept=%s", calls, slept)
	}
}

func TestWaitNotLoadedDeadline(t *testing.T) {
	var calls int
	var slept time.Duration
	err := waitNotLoaded(states(&calls, Running), 2*time.Second, 500*time.Millisecond, fakeSleep(&slept))
	if err == nil {
		t.Fatal("a job that never unloads was accepted")
	}
	if slept != 2*time.Second || calls != 5 {
		t.Errorf("calls=%d slept=%s", calls, slept)
	}
}

func TestWaitNotLoadedReadErrorsAreRetried(t *testing.T) {
	boom := errors.New("boom")
	n := 0
	read := func() (State, error) {
		n++
		if n < 3 {
			return Running, boom
		}
		return NotLoaded, nil
	}
	var slept time.Duration
	if err := waitNotLoaded(read, time.Minute, time.Second, fakeSleep(&slept)); err != nil {
		t.Fatal(err)
	}
	// Past the deadline, the last read error is reported.
	n = -100
	if err := waitNotLoaded(read, time.Second, time.Second, fakeSleep(&slept)); !errors.Is(err, boom) {
		t.Errorf("got %v, want the read error", err)
	}
}

func TestStartJobRetriesWhileUnloading(t *testing.T) {
	eio := exitErr(t, errIO)
	var calls, boots, kicks int
	var slept time.Duration
	// Listed but not running just after a bootout (kickstart fails), then
	// not listed but bootstrap still refused once, then accepted.
	o := jobOps{
		state: states(&calls, Stopped, NotLoaded),
		kickstart: func() error {
			kicks++
			return errors.New("kickstart failed")
		},
		bootstrap: func() error {
			boots++
			if boots == 1 {
				return eio
			}
			return nil
		},
		sleep: fakeSleep(&slept),
	}
	if err := startJob(o, 10*time.Second, 500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if kicks != 1 || boots != 2 || slept != time.Second {
		t.Errorf("kicks=%d boots=%d slept=%s", kicks, boots, slept)
	}
}

func TestStartJobStates(t *testing.T) {
	var slept time.Duration
	never := func() error { t.Fatal("unexpected launchctl call"); return nil }
	var calls int
	if err := startJob(jobOps{state: states(&calls, Running), bootstrap: never, kickstart: never, sleep: fakeSleep(&slept)}, time.Second, time.Second); err != nil {
		t.Errorf("running job: %v", err)
	}
	kicked := false
	calls = 0
	o := jobOps{state: states(&calls, Stopped), bootstrap: never, kickstart: func() error { kicked = true; return nil }, sleep: fakeSleep(&slept)}
	if err := startJob(o, time.Second, time.Second); err != nil || !kicked {
		t.Errorf("stopped job: kicked=%v err=%v", kicked, err)
	}
	if slept != 0 {
		t.Errorf("slept %s without a failure", slept)
	}
}

func TestStartJobFinalErrors(t *testing.T) {
	bad := exitErr(t, 22) // EINVAL: a property list launchd refuses
	var calls, boots int
	var slept time.Duration
	o := jobOps{
		state:     states(&calls, NotLoaded),
		bootstrap: func() error { boots++; return bad },
		sleep:     fakeSleep(&slept),
	}
	if err := startJob(o, time.Minute, time.Second); !errors.Is(err, bad) || boots != 1 {
		t.Errorf("err=%v boots=%d: a non-transient failure must not be retried", err, boots)
	}
	// A transient failure that lasts past the deadline is returned.
	eio := exitErr(t, errAlready)
	calls, boots, slept = 0, 0, 0
	o.bootstrap = func() error { boots++; return eio }
	if err := startJob(o, 2*time.Second, time.Second); !errors.Is(err, eio) || boots != 3 {
		t.Errorf("err=%v boots=%d", err, boots)
	}
}
