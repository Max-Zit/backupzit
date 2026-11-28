package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/api"
)

// fakeShare counts mounts and unmounts of one share.
type fakeShare struct {
	mounts, unmounts atomic.Int32
	mounted          atomic.Int32 // currently mounted
	fail             error
}

func (f *fakeShare) mount() (string, func(), error) {
	if f.fail != nil {
		return "", nil, f.fail
	}
	if f.mounted.Add(1) != 1 {
		panic("mounted twice at the same time")
	}
	f.mounts.Add(1)
	return "/mnt/share", func() {
		f.mounted.Add(-1)
		f.unmounts.Add(1)
	}, nil
}

// M-24: runs on the same share with the same mode and credentials share one
// mount; it is unmounted only when the last of them ends.
func TestShareNASSharedMount(t *testing.T) {
	f := &fakeShare{}
	cfg := nasConfig(false, &api.NASShare{User: "u", Password: "p"})
	ctx := context.Background()
	root1, rel1, err := shareNAS(ctx, "k-shared", cfg, f.mount, nil)
	if err != nil || root1 != "/mnt/share" {
		t.Fatal(root1, err)
	}
	root2, rel2, err := shareNAS(ctx, "k-shared", cfg, f.mount, nil)
	if err != nil || root2 != root1 {
		t.Fatal(root2, err)
	}
	if f.mounts.Load() != 1 {
		t.Fatalf("%d mounts for two runs, want 1", f.mounts.Load())
	}
	rel1()
	rel1() // a second release of the same run changes nothing
	if f.unmounts.Load() != 0 {
		t.Fatal("the first run to end unmounted the share the other one still reads")
	}
	rel2()
	if f.unmounts.Load() != 1 || f.mounted.Load() != 0 {
		t.Fatalf("after the last run: %d unmounts, %d mounted", f.unmounts.Load(), f.mounted.Load())
	}
	// The next run mounts again.
	_, rel3, err := shareNAS(ctx, "k-shared", cfg, f.mount, nil)
	if err != nil || f.mounts.Load() != 2 {
		t.Fatal(f.mounts.Load(), err)
	}
	rel3()
}

// A run that mounts the share differently (a writable restore, other
// credentials) waits until the share is free, then mounts it its way.
func TestShareNASOtherModeWaits(t *testing.T) {
	f := &fakeShare{}
	ctx := context.Background()
	backup := nasConfig(false, &api.NASShare{User: "u", Password: "p"})
	restore := nasConfig(true, &api.NASShare{User: "u", Password: "p"})
	if backup == restore || nasConfig(false, &api.NASShare{User: "u", Password: "other"}) == backup {
		t.Fatal("mode and password do not change the configuration")
	}
	_, rel, err := shareNAS(ctx, "k-wait", backup, f.mount, nil)
	if err != nil {
		t.Fatal(err)
	}
	var waited atomic.Bool
	got := make(chan error, 1)
	go func() {
		_, rel2, err := shareNAS(ctx, "k-wait", restore, f.mount, func() { waited.Store(true) })
		if err == nil {
			rel2()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("the restore did not wait for the backup: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if !waited.Load() {
		t.Error("the waiting run was not told")
	}
	rel()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the restore still waits after the backup ended")
	}
	if f.mounts.Load() != 2 || f.unmounts.Load() != 2 {
		t.Errorf("%d mounts, %d unmounts, want 2 and 2", f.mounts.Load(), f.unmounts.Load())
	}

	// A cancelled run stops waiting.
	_, rel, err = shareNAS(ctx, "k-wait", backup, f.mount, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, _, err := shareNAS(cctx, "k-wait", restore, f.mount, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("cancelled wait: %v", err)
	}
}

// A failed mount is reported to every run that waited for it and
// forgotten: the next run tries again.
func TestShareNASMountFails(t *testing.T) {
	cfg := nasConfig(false, nil)
	f := &fakeShare{fail: errors.New("mount error(13): Permission denied")}
	if _, _, err := shareNAS(context.Background(), "k-fail", cfg, f.mount, nil); err == nil {
		t.Fatal("no error")
	}
	f.fail = nil
	_, rel, err := shareNAS(context.Background(), "k-fail", cfg, f.mount, nil)
	if err != nil {
		t.Fatal(err)
	}
	rel()
}

// Many runs at once on one share: one mount at a time, all unmounted at
// the end (go test -race checks the bookkeeping).
func TestShareNASConcurrent(t *testing.T) {
	f := &fakeShare{}
	cfgs := [][32]byte{nasConfig(false, nil), nasConfig(true, nil)}
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, rel, err := shareNAS(context.Background(), "k-many", cfgs[i%2], f.mount, nil)
			if err != nil {
				t.Error(err)
				return
			}
			time.Sleep(time.Millisecond)
			rel()
		}()
	}
	wg.Wait()
	if f.mounted.Load() != 0 || f.mounts.Load() != f.unmounts.Load() {
		t.Errorf("%d still mounted, %d mounts, %d unmounts", f.mounted.Load(), f.mounts.Load(), f.unmounts.Load())
	}
}

// The run's folder below the share.
func TestNASSubfolder(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Scans", "2026"), 0o700); err != nil {
		t.Fatal(err)
	}
	if p, err := nasSubfolder(root, ""); err != nil || p != root {
		t.Errorf("whole share: %q %v", p, err)
	}
	if p, err := nasSubfolder(root, "Scans/2026"); err != nil || p != filepath.Join(root, "Scans", "2026") {
		t.Errorf("subfolder: %q %v", p, err)
	}
	if _, err := nasSubfolder(root, "missing"); err == nil || err.Error() != "the folder missing does not exist on the share" {
		t.Errorf("missing folder: %v", err)
	}
	if _, err := nasSubfolder(root, "../x"); err == nil {
		t.Error("a folder outside the share was accepted")
	}
}
