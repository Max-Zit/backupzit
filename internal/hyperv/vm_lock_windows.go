package hyperv

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

var errVMBusy = errors.New("another BackupZit operation is using this Hyper-V VM")

// lockVM serializes cooperating processes on this host, across Windows
// sessions. A Windows mutex is owned by an OS thread: callers must release it
// on this goroutine, after all disk reads and checkpoint cleanup finish.
// Windows abandons the mutex if its owning process dies.
func lockVM(ctx context.Context, id string, wait bool) (func(), error) {
	if !guidRe.MatchString(id) {
		return nil, fmt.Errorf("invalid Hyper-V VM GUID %q", id)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(`Global\BackupZit.HyperV.` + strings.ToLower(id))
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateMutexEx(nil, name, 0, windows.SYNCHRONIZE|windows.MUTEX_MODIFY_STATE)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return nil, fmt.Errorf("open Hyper-V VM lock: %w", err)
	}
	runtime.LockOSThread()
	for {
		if err := ctx.Err(); err != nil {
			_ = windows.CloseHandle(h)
			runtime.UnlockOSThread()
			return nil, err
		}
		state, err := windows.WaitForSingleObject(h, 0)
		if err != nil {
			_ = windows.CloseHandle(h)
			runtime.UnlockOSThread()
			return nil, fmt.Errorf("wait for Hyper-V VM lock: %w", err)
		}
		switch state {
		case windows.WAIT_OBJECT_0, windows.WAIT_ABANDONED:
			return func() {
				_ = windows.ReleaseMutex(h)
				_ = windows.CloseHandle(h)
				runtime.UnlockOSThread()
			}, nil
		case uint32(windows.WAIT_TIMEOUT):
			if !wait {
				_ = windows.CloseHandle(h)
				runtime.UnlockOSThread()
				return nil, errVMBusy
			}
		default:
			_ = windows.CloseHandle(h)
			runtime.UnlockOSThread()
			return nil, fmt.Errorf("unexpected Hyper-V VM lock wait status %d", state)
		}
		select {
		case <-ctx.Done():
		case <-time.After(50 * time.Millisecond):
		}
	}
}
