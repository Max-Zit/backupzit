//go:build linux

package fsutil

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// SetTimes sets the access and modification times of the open file f
// through its handle (not by name).
func SetTimes(f *os.File, t time.Time) error {
	ts := []unix.Timespec{unix.NsecToTimespec(t.UnixNano()), unix.NsecToTimespec(t.UnixNano())}
	fd := int(f.Fd()) // #nosec G115 -- a file descriptor
	err := unix.UtimesNanoAt(fd, "", ts, unix.AT_EMPTY_PATH)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
		// Kernels before 5.8: by the descriptor's own path in /proc.
		err = unix.UtimesNanoAt(unix.AT_FDCWD, fmt.Sprintf("/proc/self/fd/%d", fd), ts, 0)
	}
	return err
}
