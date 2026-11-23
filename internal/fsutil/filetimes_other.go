//go:build !linux && !windows

package fsutil

import (
	"os"
	"time"
)

// SetTimes sets the access and modification times of the open file f (by
// its name on other systems).
func SetTimes(f *os.File, t time.Time) error { return os.Chtimes(f.Name(), t, t) }
