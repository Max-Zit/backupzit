package pve

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/vmlock"
)

// P7/P8 (0.55 review): a backup, an in-place restore and a replica update of
// a guest wait for the guest's lock before they read its state or change
// it: a backup that read the state before a long wait treated a guest
// started meanwhile as stopped, and a restore over the guest deleted the
// snapshot of a running backup.
func TestGuestOperationsWaitForTheGuestLock(t *testing.T) {
	oldDir, oldCmd, oldLocks := ConfigDir, command, vmlock.Dir
	defer func() { ConfigDir, command, vmlock.Dir = oldDir, oldCmd, oldLocks }()
	ConfigDir, vmlock.Dir = t.TempDir(), t.TempDir()
	var ran []string
	command = func(_ context.Context, name string, args ...string) ([]byte, error) {
		ran = append(ran, name+" "+strings.Join(args, " "))
		return nil, errors.New("not in this test")
	}
	unlock, err := vmlock.Lock(context.Background(), vmLockKey(101))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	waiting := func() context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		t.Cleanup(cancel)
		return ctx
	}
	sn := &repo.Snapshot{Guests: []repo.Guest{{Platform: "proxmox", VMID: 100, Type: "qemu", Name: "web"}}}
	if _, _, _, err := backupGuest(waiting(), nil, nil, Guest{VMID: 101, Type: "qemu"}, sn, guestRun{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("backup did not wait for the lock: %v", err)
	}
	if _, err := Restore(waiting(), nil, sn, RestoreOptions{VMID: 100, NewVMID: 101, Overwrite: true}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("restore over guest 101 did not wait for its lock: %v", err)
	}
	if _, err := Replicate(waiting(), nil, sn, ReplicaOptions{VMID: 100, ReplicaVMID: 101}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("replica update of guest 101 did not wait for its lock: %v", err)
	}
	if len(ran) > 0 {
		t.Errorf("commands ran before the lock: %q", ran)
	}
}
