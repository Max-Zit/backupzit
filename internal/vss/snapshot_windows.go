//go:build windows

package vss

import (
	"errors"
	"runtime"
	"strings"
	"time"

	"github.com/go-ole/go-ole"
)

// snapshotVolume creates a snapshot of volume ("c:\") with the default
// provider and returns its device path. Mounted folders inside the volume
// are not included.
func snapshotVolume(volume string, timeout time.Duration, onErr ErrorHandler) (string, func() error, error) {
	devs, closeFn, err := snapshotVolumes([]string{volume}, timeout, onErr)
	if err != nil {
		return "", nil, err
	}
	return devs[strings.ToLower(volume)], closeFn, nil
}

// snapshotVolumes snapshots all volumes in one snapshot set, so they are
// consistent with each other (a database on D: with its log on E:). It
// returns the device of each snapshotted volume (keyed in lower case); a
// volume VSS does not support is reported through onErr and left out.
func snapshotVolumes(volumes []string, timeout time.Duration, onErr ErrorHandler) (map[string]string, func() error, error) {
	if len(volumes) == 0 {
		return nil, nil, errors.New("no volume to snapshot")
	}
	// COM is initialized per OS thread; keep each VSS conversation on one.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	snap, err := newVssSnapshot("", volumes[0], volumes[1:], timeout, nil, onErr)
	if err != nil {
		return nil, nil, err
	}
	devs := map[string]string{strings.ToLower(volumes[0]): snap.GetSnapshotDeviceObject()}
	for v, x := range snap.extra {
		devs[strings.ToLower(v)] = x.snapshotDeviceObject
	}
	return devs, func() error {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		_ = ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED) // S_FALSE if already initialized
		return snap.Delete()
	}, nil
}

// Available reports whether this process may create snapshots (it needs
// administrator or backup operator rights).
func Available() error { return HasSufficientPrivilegesForVSS() }
