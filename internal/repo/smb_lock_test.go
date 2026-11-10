package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
)

func smbLockFixture(t *testing.T) (*Repository, *Repository, *backend.SMB) {
	t.Helper()
	location := os.Getenv("BACKUPZIT_TEST_SMB_URL")
	if location == "" {
		t.Skip("set BACKUPZIT_TEST_SMB_URL to a disposable SMB test root")
	}
	u, err := url.Parse(location)
	if err != nil {
		t.Fatal("invalid SMB test URL")
	}
	u.Path = path.Join(u.Path, fmt.Sprintf("lock-test-%d", time.Now().UnixNano()))
	ctx := context.Background()
	opts := backend.Options{SMBPassword: os.Getenv("BACKUPZIT_TEST_SMB_PASSWORD"), SMBDomain: os.Getenv("BACKUPZIT_TEST_SMB_DOMAIN")}
	be, err := backend.OpenSMB(ctx, u, opts)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Init(ctx, be)
	if err != nil {
		_ = be.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		names, err := be.List(ctx, "")
		if err != nil {
			t.Error(err)
		} else {
			for _, name := range names {
				if err := be.Remove(ctx, name); err != nil {
					t.Error(err)
				}
			}
		}
		_ = be.RemoveDirs(ctx)
		_ = r.Close()
	})
	peerBackend, err := backend.OpenSMB(ctx, u, opts)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := Open(ctx, peerBackend)
	if err != nil {
		_ = peerBackend.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	return r, peer, peerBackend
}

func TestSMBLockRefreshPersists(t *testing.T) {
	r, peer, peerBackend := smbLockFixture(t)
	ctx := context.Background()
	// Exercise the exact write path used by the heartbeat, without waiting
	// 30 minutes: the initial timestamp is stale, the refresh must replace it.
	l := &Lock{r: r, name: "locks/heartbeat", info: lockFile{Time: time.Now().Add(-staleLockAge - time.Minute)}}
	if err := l.write(ctx); err != nil {
		t.Fatal(err)
	}
	l.info.Time = time.Now().UTC()
	if err := l.write(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := peerBackend.Load(ctx, l.name)
	if err != nil {
		t.Fatal(err)
	}
	var persisted lockFile
	if err := json.Unmarshal(data, &persisted); err != nil || !persisted.Time.Equal(l.info.Time) {
		t.Errorf("refresh timestamp was not persisted: got %s want %s error %v", persisted.Time, l.info.Time, err)
	}
	maintenance, err := peer.Lock(ctx, true, 0)
	if maintenance != nil {
		_ = maintenance.Unlock()
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("exclusive maintenance accepted a refreshed lock: %v", err)
	}
	if err := r.be.Remove(ctx, l.name); err != nil {
		t.Fatal(err)
	}
	maintenance, err = peer.Lock(ctx, true, 0)
	if err != nil {
		t.Fatalf("lock removal did not release maintenance: %v", err)
	}
	if err := maintenance.Unlock(); err != nil {
		t.Fatal(err)
	}
}

func TestSMBLockHeartbeat(t *testing.T) {
	if os.Getenv("BACKUPZIT_TEST_SMB_LONG") != "1" {
		t.Skip("set BACKUPZIT_TEST_SMB_LONG=1 for the real five-minute heartbeat")
	}
	r, peer, be := smbLockFixture(t)
	ctx := context.Background()
	l, err := r.Lock(ctx, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	// Read the persisted initial value, avoiding any read of fields mutated
	// by the heartbeat goroutine. The lock's filename is stable.
	name := l.name
	readTime := func() time.Time {
		data, err := be.Load(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		var info lockFile
		if err := json.Unmarshal(data, &info); err != nil {
			t.Fatal(err)
		}
		return info.Time
	}
	initial := readTime()
	deadline := time.NewTimer(lockRefreshInterval + time.Minute)
	defer deadline.Stop()
	poll := time.NewTicker(11 * time.Second)
	defer poll.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("real heartbeat did not update the persisted lock timestamp")
		case <-poll.C:
			maintenance, err := peer.Lock(ctx, true, 0)
			if maintenance != nil {
				_ = maintenance.Unlock()
			}
			if !errors.Is(err, ErrLocked) {
				t.Fatalf("exclusive maintenance acquired during live heartbeat: %v", err)
			}
			if current := readTime(); current.After(initial) {
				t.Logf("real heartbeat persisted after %s", current.Sub(initial).Round(time.Second))
				if err := l.Unlock(); err != nil {
					t.Fatal(err)
				}
				maintenance, err := peer.Lock(ctx, true, 0)
				if err != nil {
					t.Fatalf("maintenance after unlock: %v", err)
				}
				if err := maintenance.Unlock(); err != nil {
					t.Fatal(err)
				}
				return
			}
		}
	}
}
