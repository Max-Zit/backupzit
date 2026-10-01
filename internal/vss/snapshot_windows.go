//go:build windows

package vss

import (
	"runtime"
	"time"

	"github.com/go-ole/go-ole"
)

// snapshotVolume creates a snapshot of volume ("c:\") with the default
// provider and returns its device path. Mounted folders inside the volume
// are not included.
func snapshotVolume(volume string, timeout time.Duration, onErr ErrorHandler) (string, func() error, error) {
	// COM is initialized per OS thread; keep each VSS conversation on one.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	snap, err := newVssSnapshot("", volume, timeout, nil, onErr)
	if err != nil {
		return "", nil, err
	}
	return snap.GetSnapshotDeviceObject(), func() error {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED) // S_FALSE if already initialized
		return snap.Delete()
	}, nil
}

// Available reports whether this process may create snapshots (it needs
// administrator or backup operator rights).
func Available() error { return HasSufficientPrivilegesForVSS() }
