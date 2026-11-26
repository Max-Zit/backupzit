package repo

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
)

// failingLockWrites fails writing lock files after the first one, as an
// unreachable storage would.
type failingLockWrites struct {
	backend.Backend
	mu   sync.Mutex
	fail bool
}

func (b *failingLockWrites) Save(ctx context.Context, name string, data []byte) error {
	b.mu.Lock()
	fail := b.fail
	b.mu.Unlock()
	if fail && strings.HasPrefix(name, "locks/") {
		return errors.New("storage unreachable")
	}
	return b.Backend.Save(ctx, name, data)
}

// A10: a failed refresh is logged and kept; once the lock is older than
// lockLostAge, no snapshot is saved and prune deletes nothing.
func TestLockLost(t *testing.T) {
	ctx := context.Background()
	local, err := backend.OpenLocal(filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	be := &failingLockWrites{Backend: local}
	r, err := Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var logged []string
	SetLockLog(func(msg string, kv ...any) { logged = append(logged, msg) })
	defer SetLockLog(func(string, ...any) {})

	l, err := r.Lock(ctx, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	be.mu.Lock()
	be.fail = true
	be.mu.Unlock()
	close(l.stop) // no background refresh: refresh by hand below
	l.done.Wait()
	l.stop = make(chan struct{})
	defer func(w time.Duration) { lockRetryWait = w }(lockRetryWait)
	lockRetryWait = 0
	l.refreshOnce()
	if len(logged) != 1 || l.Err() != nil {
		t.Fatalf("after one failed refresh: logged %v, err %v", logged, l.Err())
	}
	later := time.Now().Add(lockLostAge + time.Minute)
	l.now = func() time.Time { return later }
	if err := l.Err(); !errors.Is(err, ErrLockLost) || !strings.Contains(err.Error(), "storage unreachable") {
		t.Fatalf("lost lock: %v", err)
	}
	if _, err := r.SaveSnapshot(ctx, &Snapshot{Time: time.Now()}); !errors.Is(err, ErrLockLost) {
		t.Fatalf("snapshot saved with a lost lock: %v", err)
	}
	l.now = nil
	if _, err := r.SaveSnapshot(ctx, &Snapshot{Time: time.Now()}); err != nil {
		t.Fatalf("snapshot with a fresh lock: %v", err)
	}
	be.mu.Lock()
	be.fail = false
	be.mu.Unlock()
	close(l.stop)
	l.r.dropLock(l)
	_ = l.r.be.Remove(ctx, l.name)
}

// B7: a lock dated far in the future and a huge planted lock file do not
// block the repository.
func TestPlantedLocks(t *testing.T) {
	ctx := context.Background()
	be, err := backend.OpenLocal(filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	future := []byte(`{"time":"2999-01-01T00:00:00Z","exclusive":true,"hostname":"x","pid":1}`)
	if err := be.Save(ctx, "locks/future", future); err != nil {
		t.Fatal(err)
	}
	l, err := r.Lock(ctx, true, 0)
	if err != nil {
		t.Fatalf("a lock dated in 2999 blocks the repository: %v", err)
	}
	_ = l.Unlock()
	if err := be.Save(ctx, "locks/huge", []byte(strings.Repeat("x", maxLockSize+1))); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Lock(ctx, false, 0); err == nil || !errors.Is(err, backend.ErrTooLarge) {
		t.Fatalf("huge lock file: %v", err)
	}
}

// B7: snapshot and config files above their limit are refused before they
// are read whole.
func TestPlantedHugeFiles(t *testing.T) {
	ctx := context.Background()
	be, err := backend.OpenLocal(filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	big := make([]byte, maxConfigSize+10)
	if err := be.Save(ctx, "config", big); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, be); !errors.Is(err, backend.ErrTooLarge) {
		t.Fatalf("huge config: %v", err)
	}
}
