package vmlock

import (
	"context"
	"testing"
	"time"
)

func TestLockSerializesOneVM(t *testing.T) {
	Dir = t.TempDir()
	unlock, err := Lock(context.Background(), "pve-101")
	if err != nil {
		t.Fatal(err)
	}
	// Another VM is not held up.
	other, err := Lock(context.Background(), "pve-102")
	if err != nil {
		t.Fatal(err)
	}
	other()
	// The same VM waits, and gives up with its context.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := Lock(ctx, "pve-101"); err == nil {
		t.Fatal("the same VM was locked twice")
	}
	got := make(chan error, 1)
	go func() {
		u, err := Lock(context.Background(), "pve-101")
		if err == nil {
			u()
		}
		got <- err
	}()
	time.Sleep(50 * time.Millisecond)
	unlock()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting run did not get the lock after the release")
	}
}

// TryLock never waits: it gets a free VM and refuses a held one.
func TestTryLock(t *testing.T) {
	Dir = t.TempDir()
	unlock, ok := TryLock("vmware-h-1")
	if !ok {
		t.Fatal("a free VM was refused")
	}
	if _, ok := TryLock("vmware-h-1"); ok {
		t.Fatal("a held VM was taken twice")
	}
	unlock()
	again, ok := TryLock("vmware-h-1")
	if !ok {
		t.Fatal("the VM stayed held after the release")
	}
	again()
}
