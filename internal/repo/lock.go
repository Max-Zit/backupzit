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
)

// Repository locks prevent prune from deleting data that a concurrent
// backup or restore relies on. Backups and restores take shared locks;
// prune takes an exclusive lock. A lock that has not been refreshed for
// staleLockAge is ignored (its holder crashed).
const (
	lockRefreshInterval = 5 * time.Minute
	staleLockAge        = 30 * time.Minute
)

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
		b, err := r.be.Load(ctx, n)
		if err != nil {
			continue // removed meanwhile
		}
		var lf lockFile
		if json.Unmarshal(b, &lf) != nil {
			continue
		}
		if time.Since(lf.Time) > staleLockAge {
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
	// Two clients may have checked at the same time; re-check now that our
	// lock is visible and back off if there is a conflict.
	if c, err := r.conflicting(ctx, exclusive, l.name); err != nil || c != "" {
		r.be.Remove(ctx, l.name)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s", ErrLocked, c)
	}
	l.done.Add(1)
	go l.refresh()
	return l, nil
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
			l.info.Time = time.Now().UTC()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			l.write(ctx)
			cancel()
		}
	}
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
	close(l.stop)
	l.done.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := l.r.be.Remove(ctx, l.name)
	if err != nil && strings.Contains(err.Error(), "not found") {
		return nil
	}
	return err
}
