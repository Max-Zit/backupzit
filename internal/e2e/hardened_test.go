package e2e

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/backupzit/backupzit/internal/archiver"
	"github.com/backupzit/backupzit/internal/backend"
	"github.com/backupzit/backupzit/internal/checker"
	"github.com/backupzit/backupzit/internal/repo"
	"github.com/backupzit/backupzit/internal/restorer"
	"github.com/backupzit/backupzit/internal/testutil"
)

func startHardened(t *testing.T) (*testutil.HardenedServer, backend.Options) {
	t.Helper()
	srv, err := testutil.StartHardened(filepath.Join(tempDir(t), "hardened"), 7)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return srv, backend.Options{HardenedKey: srv.Key, HardenedFingerprint: srv.Fingerprint}
}

func TestBackupRestoreHardened(t *testing.T) {
	srv, opts := startHardened(t)
	ctx := context.Background()
	loc := "hardened://" + srv.Host + "/office/pc1"
	if _, err := backend.Open(ctx, loc, backend.Options{HardenedKey: "bzr_wrong", HardenedFingerprint: srv.Fingerprint}); err == nil {
		t.Error("wrong key accepted")
	}
	if _, err := backend.Open(ctx, loc, backend.Options{HardenedKey: srv.Key, HardenedFingerprint: "SHA256:wrong"}); err == nil {
		t.Error("wrong certificate accepted")
	}
	runCycle(t, func() backend.Backend {
		be, err := backend.Open(ctx, loc, opts)
		if err != nil {
			t.Fatal(err)
		}
		return be
	}, false, repo.Password("hardened-test"))
}

// TestHardenedAttack: a client with the access key tries to destroy the
// backups; a point-in-time view and undelete recover everything.
func TestHardenedAttack(t *testing.T) {
	srv, opts := startHardened(t)
	ctx := context.Background()
	loc := "hardened://" + srv.Host + "/pc1"
	open := func(o backend.Options) backend.Backend {
		t.Helper()
		be, err := backend.Open(ctx, loc, o)
		if err != nil {
			t.Fatal(err)
		}
		return be
	}
	pw := repo.Password("attack-test")
	be := open(opts)
	if ret, ok := be.(backend.Retainer); !ok || ret.LockDays() != 7 {
		t.Fatal("hardened backend does not report its lock period")
	}
	r, err := repo.Init(ctx, be, pw)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(11))
	src := filepath.Join(tempDir(t), "src")
	for i := 0; i < 4; i++ {
		writeFile(t, filepath.Join(src, fmt.Sprintf("f%d.bin", i)), randBytes(rng, 3*mib))
	}
	want1 := manifest(t, src)
	sn1, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}})
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(src, "f0.bin"))
	writeFile(t, filepath.Join(src, "g.bin"), randBytes(rng, 2*mib))
	sn2, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.KeepImmutable(ctx, sn2); err != nil {
		t.Fatal(err)
	}

	// Retention and prune still work: sn1's data is only hidden.
	lock, err := r.Lock(ctx, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveSnapshot(ctx, sn1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Prune(ctx, repo.PruneOptions{}); err != nil {
		t.Fatal(err)
	}
	lock.Unlock()
	r.Close()
	r, err = repo.Open(ctx, open(opts), pw)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := checker.Run(ctx, r, checker.Options{ReadData: true}); err != nil || !res.OK() {
		t.Fatalf("check after prune: %v %v", err, res.Errors)
	}
	time.Sleep(1100 * time.Millisecond) // point-in-time views have 1 s resolution
	beforeAttack := time.Now()
	time.Sleep(1100 * time.Millisecond)

	// --- attack: overwrite and delete everything
	be = open(opts)
	if err := be.Save(ctx, "config", []byte("ransom")); err == nil {
		t.Error("config overwritten")
	}
	for _, dir := range []string{"data", "index", "snapshots", "keys"} {
		files, err := be.List(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if err := be.Remove(ctx, f); err != nil {
				t.Fatal(err)
			}
		}
	}
	be.Remove(ctx, "config")
	if _, err := repo.Open(ctx, be, pw); err == nil {
		t.Fatal("repository still opens after the attack")
	}
	if st, _ := srv.Store.Stats(); st.Files != 0 || st.Hidden == 0 {
		t.Errorf("after attack: %+v", st)
	}

	// --- point-in-time view before the attack (and before the prune for sn1)
	view := opts
	view.AsOf = beforeAttack
	vbe := open(view)
	if err := vbe.Save(ctx, "x", []byte("x")); !errors.Is(err, backend.ErrReadOnly) {
		t.Errorf("save to view: %v", err)
	}
	rv, err := repo.Open(ctx, vbe, pw)
	if err != nil {
		t.Fatal(err)
	}
	sns, err := rv.ListSnapshots(ctx)
	if err != nil || len(sns) != 1 || sns[0].ID != sn2.ID {
		t.Fatalf("snapshots before the attack: %v %v", sns, err)
	}
	view.AsOf = time.Unix(0, 0).Add(time.Second) // before everything was deleted
	rOld, err := repo.Open(ctx, open(view), pw)
	if err != nil {
		t.Fatal(err)
	}
	old, err := rOld.LoadSnapshot(ctx, sn1.ID.String())
	if err != nil {
		t.Fatalf("pruned snapshot not readable as of before prune: %v", err)
	}
	tgt := filepath.Join(tempDir(t), "restore")
	if st, err := restorer.Run(ctx, rOld, old, restorer.Options{Target: tgt, Verify: true}); err != nil || len(st.Errors) > 0 {
		t.Fatalf("restore pruned snapshot: %v %v", err, st)
	}
	compareManifests(t, want1, manifest(t, restoredRoot(t, tgt, src)))

	// --- the administrator undeletes on the server; backups continue
	n, err := srv.Store.Undelete(beforeAttack)
	if err != nil || n == 0 {
		t.Fatalf("undelete: %d %v", n, err)
	}
	r, err = repo.Open(ctx, open(opts), pw)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := checker.Run(ctx, r, checker.Options{ReadData: true}); err != nil || !res.OK() {
		t.Fatalf("check after undelete: %v %v", err, res.Errors)
	}
	if _, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}}); err != nil {
		t.Fatalf("backup after undelete: %v", err)
	}
}
