//go:build !windows

package vss

import (
	"errors"
	"time"
)

// ErrorHandler receives non-fatal errors.
type ErrorHandler func(item string, err error)

var errUnsupported = errors.New("VSS is only available on Windows")

func snapshotVolume(string, time.Duration, ErrorHandler) (string, func() error, error) {
	return "", nil, errUnsupported
}

// Available reports whether snapshots can be created.
func Available() error { return errUnsupported }
