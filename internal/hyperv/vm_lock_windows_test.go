package hyperv

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func testVMGUID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func holdTestVMLock(t *testing.T, id string) func() {
	t.Helper()
	ready := make(chan error, 1)
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		unlock, err := lockVM(context.Background(), id, true)
		ready <- err
		if err != nil {
			return
		}
		<-release
		unlock()
	}()
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	return func() { close(release); <-done }
}

func TestVMLockExclusionAndCancellation(t *testing.T) {
	id := testVMGUID(t)
	release := holdTestVMLock(t, id)
	defer release()
	unlock, err := lockVM(context.Background(), strings.ToUpper(id), false)
	if unlock != nil {
		unlock()
	}
	if !errors.Is(err, errVMBusy) {
		t.Fatalf("same GUID in another goroutine: %v", err)
	}
	other, err := lockVM(context.Background(), testVMGUID(t), false)
	if err != nil {
		t.Fatalf("different VM was blocked: %v", err)
	}
	other()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	unlock, err = lockVM(ctx, id, true)
	if unlock != nil {
		unlock()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled wait: %v", err)
	}
}

func TestVMLockInvalidAndCanceled(t *testing.T) {
	if unlock, err := lockVM(context.Background(), "invalid GUID", false); err == nil || unlock != nil {
		t.Fatal("invalid GUID accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if unlock, err := lockVM(ctx, testVMGUID(t), false); !errors.Is(err, context.Canceled) || unlock != nil {
		t.Fatalf("pre-canceled lock: %v", err)
	}
}

func TestVMLockProcessExit(t *testing.T) {
	if id := os.Getenv("BACKUPZIT_TEST_VMLOCK_CHILD"); id != "" {
		if _, err := lockVM(context.Background(), id, true); err != nil {
			t.Fatal(err)
		}
		os.Exit(0) // intentionally abandon ownership, without calling unlock
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	id := testVMGUID(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestVMLockProcessExit$")
	cmd.Env = append(os.Environ(), "BACKUPZIT_TEST_VMLOCK_CHILD="+id)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %s %v", output, err)
	}
	unlock, err := lockVM(ctx, id, false)
	if err != nil {
		t.Fatalf("lock leaked after process exit: %v", err)
	}
	unlock()
}
