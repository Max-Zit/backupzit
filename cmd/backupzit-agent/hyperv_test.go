package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/hyperv"
	"github.com/max-zit/backupzit/internal/repo"
)

type restoreLockBackend struct {
	backend.Backend
	onSnapshots func()
	snapshotErr error
}

func (b *restoreLockBackend) List(ctx context.Context, dir string) ([]string, error) {
	if dir == "snapshots" {
		if b.onSnapshots != nil {
			b.onSnapshots()
		}
		if b.snapshotErr != nil {
			return nil, b.snapshotErr
		}
	}
	return b.Backend.List(ctx, dir)
}

func restoreLockFixture(t *testing.T) (*repo.Repository, *repo.Repository, *restoreLockBackend, *repo.Snapshot, repo.ID) {
	t.Helper()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "repo")
	local, err := backend.OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	be := &restoreLockBackend{Backend: local}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	id, _, err := r.SaveBlob(ctx, repo.DataBlob, []byte("restore data"))
	if err != nil {
		t.Fatal(err)
	}
	tree, err := r.SaveTree(ctx, &repo.Tree{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	sn := &repo.Snapshot{Time: time.Now().UTC(), Tree: tree}
	if _, err := r.SaveSnapshot(ctx, sn); err != nil {
		t.Fatal(err)
	}
	otherBackend, err := backend.OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	other, err := repo.Open(ctx, otherBackend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	return r, other, be, sn, id
}

func requireRestoreLock(t *testing.T, other *repo.Repository) {
	t.Helper()
	// Prune takes this exclusive lock before deleting packs. A restore must
	// exclude it, while still permitting other readers/backups to proceed.
	lock, err := other.Lock(context.Background(), true, 0)
	if lock != nil {
		_ = lock.Unlock()
	}
	if !errors.Is(err, repo.ErrLocked) {
		t.Errorf("exclusive maintenance lock during restore: got %v, want ErrLocked", err)
	}
	shared, err := other.Lock(context.Background(), false, 0)
	if err != nil {
		t.Fatalf("concurrent shared lock: %v", err)
	}
	if err := shared.Unlock(); err != nil {
		t.Fatal(err)
	}
}

func requireRestoreUnlocked(t *testing.T, other *repo.Repository) {
	t.Helper()
	lock, err := other.Lock(context.Background(), true, 0)
	if err != nil {
		t.Fatalf("restore left a lock behind: %v", err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
}

func TestHyperVRestoreRepositoryLock(t *testing.T) {
	for _, selector := range []string{"id", "prefix", "latest"} {
		for _, outcome := range []string{"success", "error", "cancel"} {
			t.Run(selector+"/"+outcome, func(t *testing.T) {
				r, other, be, expected, dataID := restoreLockFixture(t)
				ref := expected.ID.String()
				if selector == "prefix" {
					ref = expected.ID.Short()
				} else if selector == "latest" {
					ref = "latest"
				}
				loads := 0
				be.onSnapshots = func() {
					loads++
					requireRestoreLock(t, other)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				failure := errors.New("restore failed")
				var want error
				called := false
				err := withHyperVRestoreSnapshot(ctx, r, ref, func(sn *repo.Snapshot) error {
					called = true
					if sn.ID != expected.ID {
						t.Errorf("snapshot = %s, want %s", sn.ID, expected.ID)
					}
					requireRestoreLock(t, other)
					data, err := r.LoadBlob(ctx, repo.DataBlob, dataID)
					if err != nil || !bytes.Equal(data, []byte("restore data")) {
						t.Fatalf("restore data: %q, %v", data, err)
					}
					if outcome == "error" {
						want = failure
					} else if outcome == "cancel" {
						cancel()
						want = ctx.Err()
					}
					// The lock must remain held until the restore returns,
					// including after cancellation and the last data read.
					requireRestoreLock(t, other)
					return want
				})
				if !called || loads == 0 || !errors.Is(err, want) {
					t.Fatalf("called=%v loads=%d error=%v, want %v", called, loads, err, want)
				}
				requireRestoreUnlocked(t, other)
			})
		}
	}
}

func TestHyperVRestoreRepositoryLockBlocksBeforeSnapshot(t *testing.T) {
	r, other, be, sn, _ := restoreLockFixture(t)
	lock, err := other.Lock(context.Background(), true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	be.onSnapshots = func() { t.Error("snapshot accessed while exclusive lock held") }
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Stop a conflicting wait without depending on sleeps/timeouts.
	err = withHyperVRestoreSnapshot(ctx, r, sn.ID.String(), func(*repo.Snapshot) error {
		t.Error("restore started while exclusive lock held")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	requireRestoreUnlocked(t, other)
}

func TestHyperVRestoreRepositoryLockSnapshotError(t *testing.T) {
	for _, ref := range []string{"latest", "missing"} {
		t.Run(ref, func(t *testing.T) {
			r, other, be, _, _ := restoreLockFixture(t)
			failure := errors.New("snapshot read failed")
			be.snapshotErr = failure
			be.onSnapshots = func() { requireRestoreLock(t, other) }
			err := withHyperVRestoreSnapshot(context.Background(), r, ref, func(*repo.Snapshot) error {
				t.Error("restore started after snapshot read failed")
				return nil
			})
			if !errors.Is(err, failure) {
				t.Fatalf("got %v, want snapshot read error", err)
			}
			requireRestoreUnlocked(t, other)
		})
	}
}

func TestHyperVRestoreCLILock(t *testing.T) {
	if !hyperv.Available() {
		t.Skip("CLI requires a Hyper-V host; portable lock tests run everywhere")
	}
	_, other, be, _, _ := restoreLockFixture(t)
	lock, err := other.Lock(context.Background(), true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A deliberately missing selector prevents any VM operation even if the
	// CLI regresses and bypasses the lock. Only a temporary repository is used.
	err = cmdHyperV(ctx, []string{"restore", "--repo", be.Location(), "--vmid", "1000000001", "--new", "missing"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CLI got %v, want canceled lock wait before snapshot lookup", err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	requireRestoreUnlocked(t, other)
}
