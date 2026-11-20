// Package vmlock serializes BackupZit operations on one virtual machine on
// this host: two runs of the same VM (two jobs, or a job and a command line
// backup) would otherwise remove each other's live snapshot when they clean
// up what an interrupted backup left behind (M-13).
package vmlock

import (
	"context"
	"regexp"
	"sync"
	"time"
)

var unsafeKey = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

var (
	mu   sync.Mutex
	held = map[string]chan struct{}{}
)

// Lock waits until no other operation of this process (and, where the
// system allows, of another BackupZit process) uses the VM named by key,
// e.g. "pve-101" or "vmware-<host>-<uuid>". The returned function releases
// it; ctx ends the wait.
func Lock(ctx context.Context, key string) (func(), error) {
	key = unsafeKey.ReplaceAllString(key, "_")
	// In this process.
	for {
		mu.Lock()
		ch, busy := held[key]
		if !busy {
			ch = make(chan struct{})
			held[key] = ch
			mu.Unlock()
			break
		}
		mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ch:
		}
	}
	release := func() {
		mu.Lock()
		close(held[key])
		delete(held, key)
		mu.Unlock()
	}
	// Across processes (the service and the command line).
	unlockFile, err := lockFile(ctx, key)
	if err != nil {
		release()
		return nil, err
	}
	return func() {
		unlockFile()
		release()
	}, nil
}

// pollEvery is how often a file lock held by another process is tried.
var pollEvery = 500 * time.Millisecond

// Dir holds the lock files on Linux; only root uses it (the agent's own
// folder).
var Dir = "/var/lib/backupzit-agent/locks"
