package repo

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
)

type failingLockReadBackend struct {
	backend.Backend
	readError error
}

func (b *failingLockReadBackend) List(ctx context.Context, dir string) ([]string, error) {
	if dir == "locks" {
		return []string{"locks/other"}, nil
	}
	return b.Backend.List(ctx, dir)
}

func (b *failingLockReadBackend) Load(ctx context.Context, name string) ([]byte, error) {
	if name == "locks/other" {
		return nil, b.readError
	}
	return b.Backend.Load(ctx, name)
}

func TestLockUnreadableHolder(t *testing.T) {
	for _, exclusive := range []bool{false, true} {
		for _, cause := range []error{errors.New("SMB sharing violation"), errors.New("permission denied"), context.Canceled} {
			t.Run(fmt.Sprintf("exclusive=%v/%v", exclusive, cause), func(t *testing.T) {
				local, err := backend.OpenLocal(filepath.Join(t.TempDir(), "repo"))
				if err != nil {
					t.Fatal(err)
				}
				be := &failingLockReadBackend{Backend: local, readError: cause}
				r, err := Init(context.Background(), be)
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				lock, err := r.Lock(context.Background(), exclusive, 0)
				if lock != nil {
					_ = lock.Unlock()
				}
				if !errors.Is(err, cause) {
					t.Fatalf("unreadable holder accepted: got %v, want %v", err, cause)
				}
			})
		}
	}
}

func TestLockHolderRemovedDuringRead(t *testing.T) {
	local, err := backend.OpenLocal(filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	be := &failingLockReadBackend{Backend: local, readError: fmt.Errorf("removed: %w", backend.ErrNotFound)}
	r, err := Init(context.Background(), be)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	lock, err := r.Lock(context.Background(), true, 0)
	if err != nil {
		t.Fatalf("concurrently removed lock should be ignored: %v", err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
}
