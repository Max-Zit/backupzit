package repo

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
)

// Repository locks prevent prune from deleting data that a concurrent
// backup or restore relies on. Backups and restores take shared locks;
// prune takes an exclusive lock. A lock that has not been refreshed for
// staleLockAge is ignored (its holder crashed).
const (
	lockRefreshInterval = 5 * time.Minute
	staleLockAge        = 30 * time.Minute
	// lockLostAge: a lock whose refreshes have failed for this long is
	// treated as lost (Lock.Err) before others may ignore it as stale, so
	// a holder stops before a concurrent prune can start.
	lockLostAge = 20 * time.Minute
	// lockRetries and lockRetryWait: a failed refresh is tried again a few
	// times within the refresh interval.
	lockRetries = 3
)

// lockRetryWait is the pause between refresh attempts (a variable for tests).
var lockRetryWait = 20 * time.Second

// lockLog reports failed lock refreshes; SetLockLog connects it to the
// agent's or the console's log.
var (
	lockLogMu sync.Mutex
	lockLog   = func(string, ...any) {}
)

// SetLockLog sets where failed repository lock refreshes are logged.
func SetLockLog(f func(msg string, kv ...any)) {
	lockLogMu.Lock()
	lockLog = f
	lockLogMu.Unlock()
}

func logLock(msg string, kv ...any) {
	lockLogMu.Lock()
	f := lockLog
	lockLogMu.Unlock()
	f(msg, kv...)
}

// ErrLockLost: the repository lock could not be refreshed for too long;
// another client may already ignore it.
var ErrLockLost = errors.New("the repository lock could not be refreshed")

type lockFile struct {
	Time      time.Time `json:"time"`
	Exclusive bool      `json:"exclusive"`
	Hostname  string    `json:"hostname"`
	PID       int       `json:"pid"`
}

// Lock is a held repository lock.
type Lock struct {
	r    *Repository
	name string
	info lockFile
	stop chan struct{}
	done sync.WaitGroup
	once sync.Once

	// lastOK is when the lock file was last written; lastErr the error of
	// the last failed refresh.
	mu      sync.Mutex
	lastOK  time.Time
	lastErr error
	now     func() time.Time
}

// Err tells whether the lock still holds: an error once its refreshes have
// failed for lockLostAge (the storage was unreachable), so a backup does
// not save a snapshot and a prune deletes nothing that a concurrent run
// may rely on.
func (l *Lock) Err() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now
	if l.now != nil {
		now = l.now
	}
	if since := now().Sub(l.lastOK); since > lockLostAge {
		return fmt.Errorf("%w for %s (%v); stopped so that data a concurrent prune may have removed is not relied on", ErrLockLost, since.Round(time.Minute), l.lastErr)
	}
	return nil
}

// ErrLocked is returned when the repository is locked by someone else.
var ErrLocked = errors.New("repository is locked")

// Lock acquires a shared or exclusive lock, waiting up to wait for
// conflicting locks to go away.
func (r *Repository) Lock(ctx context.Context, exclusive bool, wait time.Duration) (*Lock, error) {
	host, _ := os.Hostname()
	deadline := time.Now().Add(wait)
	for {
		l, err := r.tryLock(ctx, exclusive, host)
		if err == nil {
			if err := r.refreshIndex(ctx); err != nil {
				_ = l.Unlock()
				return nil, err
			}
			return l, nil
		}
		if !errors.Is(err, ErrLocked) || wait <= 0 || time.Now().After(deadline) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

// conflicting returns a description of a live lock that conflicts with
// the requested kind, ignoring ignore.
func (r *Repository) conflicting(ctx context.Context, exclusive bool, ignore string) (string, error) {
	names, err := r.be.List(ctx, "locks")
	if err != nil {
		return "", err
	}
	for _, n := range names {
		if n == ignore {
			continue
		}
		b, err := backend.LoadMax(ctx, r.be, n, maxLockSize)
		if err != nil {
			if errors.Is(err, backend.ErrNotFound) {
				continue // removed meanwhile
			}
			return "", fmt.Errorf("read repository lock %s: %w", n, err)
		}
		var lf lockFile
		if json.Unmarshal(b, &lf) != nil {
			continue
		}
		if time.Since(lf.Time) > staleLockAge {
			continue
		}
		// A lock dated more than staleLockAge ahead never came from a
		// working client (clocks are not that far apart): a planted one
		// would otherwise block the repository until that date (B7).
		if time.Until(lf.Time) > staleLockAge {
			continue
		}
		if exclusive || lf.Exclusive {
			kind := "shared"
			if lf.Exclusive {
				kind = "exclusive"
			}
			return fmt.Sprintf("%s lock held by %s (pid %d) since %s", kind, lf.Hostname, lf.PID, lf.Time.Local().Format(time.DateTime)), nil
		}
	}
	return "", nil
}

func (r *Repository) tryLock(ctx context.Context, exclusive bool, host string) (*Lock, error) {
	if c, err := r.conflicting(ctx, exclusive, ""); err != nil {
		return nil, err
	} else if c != "" {
		return nil, fmt.Errorf("%w: %s", ErrLocked, c)
	}
	l := &Lock{r: r, info: lockFile{Time: time.Now().UTC(), Exclusive: exclusive, Hostname: host, PID: os.Getpid()}, stop: make(chan struct{})}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return nil, err
	}
	l.name = fmt.Sprintf("locks/%x", rnd)
	if err := l.write(ctx); err != nil {
		return nil, err
	}
	l.lastOK = time.Now()
	// Two clients may have checked at the same time; re-check now that our
	// lock is visible and back off if there is a conflict.
	if c, err := r.conflicting(ctx, exclusive, l.name); err != nil || c != "" {
		_ = r.be.Remove(ctx, l.name)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s", ErrLocked, c)
	}
	l.done.Add(1)
	go l.refresh() // #nosec G118 -- background work that outlives the request on purpose
	r.addLock(l)
	return l, nil
}

// addLock and dropLock track the locks this repository holds (lockErr).
func (r *Repository) addLock(l *Lock) {
	r.locksMu.Lock()
	defer r.locksMu.Unlock()
	if r.locks == nil {
		r.locks = map[*Lock]bool{}
	}
	r.locks[l] = true
}

func (r *Repository) dropLock(l *Lock) {
	r.locksMu.Lock()
	defer r.locksMu.Unlock()
	delete(r.locks, l)
}

// lockErr is the first error of the locks this repository holds (Err).
func (r *Repository) lockErr() error {
	r.locksMu.Lock()
	defer r.locksMu.Unlock()
	for l := range r.locks {
		if err := l.Err(); err != nil {
			return err
		}
	}
	return nil
}

func (l *Lock) write(ctx context.Context) error {
	b, _ := json.Marshal(l.info)
	return l.r.be.Save(ctx, l.name, b)
}

func (l *Lock) refresh() {
	defer l.done.Done()
	t := time.NewTicker(lockRefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			l.refreshOnce()
		}
	}
}

// refreshOnce writes the lock file again, with a few retries; a failure is
// logged and kept for Err.
func (l *Lock) refreshOnce() {
	var err error
	for try := 0; try < lockRetries; try++ {
		if try > 0 {
			select {
			case <-l.stop:
				return
			case <-time.After(lockRetryWait):
			}
		}
		l.info.Time = time.Now().UTC()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		err = l.write(ctx)
		cancel()
		if err == nil {
			l.mu.Lock()
			l.lastOK, l.lastErr = time.Now(), nil
			l.mu.Unlock()
			return
		}
	}
	l.mu.Lock()
	l.lastErr = err
	since := time.Since(l.lastOK).Round(time.Second)
	l.mu.Unlock()
	logLock("repository lock refresh failed", "lock", l.name, "location", l.r.be.Location(), "last_written", since.String()+" ago", "err", err)
}

// Unlock releases the lock. It is safe to call more than once.
func (l *Lock) Unlock() error {
	if l == nil {
		return nil
	}
	first := false
	l.once.Do(func() { first = true })
	if !first {
		return nil
	}
	// Packs still uploading (a run that failed before Flush) must land
	// while the lock is held: a prune right after it would take them for
	// leftovers and delete them, while an index written later names them.
	// The lock is kept fresh meanwhile.
	l.r.waitUploads()
	close(l.stop)
	l.done.Wait()
	l.r.dropLock(l)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := l.r.be.Remove(ctx, l.name)
	if err != nil && strings.Contains(err.Error(), "not found") {
		return nil
	}
	return err
}
