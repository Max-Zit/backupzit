//go:build linux || darwin || freebsd

package vmlock

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// pollEvery is how often a file lock held by another process is tried.
var pollEvery = 500 * time.Millisecond

// warn reports a lock file that cannot be used; tests replace it. The
// package has no logger of its own: the default one writes to stderr, which
// systemd puts into the agent's journal.
var warn = func(msg string, args ...any) { slog.Warn(msg, args...) }

func lockFile(ctx context.Context, key string) (func(), error) {
	// Without the file lock only this process is kept out of the VM, not a
	// command line run as root at the same time. The operation still runs
	// (tests and a user's command line have no lock folder), but the reason
	// is logged: a planted link (O_NOFOLLOW) or a broken folder must not go
	// unnoticed (L-N12).
	noFile := func(err error) (func(), error) {
		warn("vmlock: the lock file cannot be used, so another BackupZit process (a command line run) is not kept out of this VM", "key", key, "err", err)
		return func() {}, nil
	}
	if err := os.MkdirAll(Dir, 0o700); err != nil {
		return noFile(err)
	}
	f, err := os.OpenFile(filepath.Join(Dir, key+".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600) // #nosec G304 -- key is reduced to [A-Za-z0-9._-]
	if err != nil {
		return noFile(err)
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
			return noFile(err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(pollEvery):
		}
	}
}
