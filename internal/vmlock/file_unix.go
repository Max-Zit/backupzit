//go:build linux || darwin || freebsd

package vmlock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// pollEvery is how often a file lock held by another process is tried.
var pollEvery = 500 * time.Millisecond

func lockFile(ctx context.Context, key string) (func(), error) {
	if err := os.MkdirAll(Dir, 0o700); err != nil {
		// Not the service (tests, a user's command line): the in-process
		// lock still applies.
		return func() {}, nil
	}
	f, err := os.OpenFile(filepath.Join(Dir, key+".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600) // #nosec G304 -- key is reduced to [A-Za-z0-9._-]
	if err != nil {
		return func() {}, nil
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) // #nosec G115 -- a file descriptor
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) // #nosec G115 -- a file descriptor
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return func() {}, nil
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(pollEvery):
		}
	}
}
