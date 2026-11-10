package repo_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/checker"
	"github.com/max-zit/backupzit/internal/diskimg"
	"github.com/max-zit/backupzit/internal/repo"
)

// Record the objects selected for retention without requiring an S3 server.
type guestStateBackend struct {
	backend.Backend
	retained []string
}

func (*guestStateBackend) LockDays() int { return 1 }
func (b *guestStateBackend) KeepLocked(_ context.Context, names []string) (int, error) {
	b.retained = append(b.retained, names...)
	return len(names), nil
}

type guestStateFixture struct {
	r       *repo.Repository
	be      *guestStateBackend
	sn      *repo.Snapshot
	id      repo.ID
	state   []byte
	disk    []byte
	options []repo.Option
}

// Match the format written by Hyper-V 0.48.0/0.48.1, not a new metadata field:
// the VMGS blob ID is inside the guest's serialized configuration. Keep it in
// its own pack so that a missing reference cannot be masked by disk data that
// happens to share its pack.
func newGuestStateFixture(t *testing.T, encrypted bool) *guestStateFixture {
	t.Helper()
	ctx := context.Background()
	local, err := backend.OpenLocal(filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	f := &guestStateFixture{be: &guestStateBackend{Backend: local}}
	if encrypted {
		f.options = []repo.Option{repo.Password("source-recovery-key")}
	}
	f.r, err = repo.Init(ctx, f.be, f.options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.r.Close() })
	f.state = bytes.Repeat([]byte("guest state distinct from virtual disk data\n"), 128)
	f.id, _, err = f.r.SaveBlob(ctx, repo.DataBlob, f.state)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	f.disk = bytes.Repeat([]byte{0x47}, diskimg.BlockSize)
	img, err := diskimg.Store(ctx, f.r, bytes.NewReader(f.disk), uint64(len(f.disk)), 0, "disk", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := f.r.SaveTree(ctx, &repo.Tree{})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	cfg, err := json.Marshal(map[string]any{
		"generation": 2, "tpm": true, "gueststate": f.id.String(), "keyprotector": "dGVzdA==",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.sn = &repo.Snapshot{
		Time: time.Now().UTC(), Tree: tree, ProgramVersion: "0.48.1",
		Guests: []repo.Guest{{Platform: "hyperv", VMID: 1000000001, Config: string(cfg),
			Disks: []repo.GuestDisk{{Key: "scsi0:0", Image: 0}}}},
		Images: []repo.DiskImage{img},
	}
	if _, err := f.r.SaveSnapshot(ctx, f.sn); err != nil {
		t.Fatal(err)
	}
	return f
}

func withGuestStateEncryption(t *testing.T, run func(*testing.T, bool)) {
	t.Helper()
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprintf("encrypted=%v", encrypted), func(t *testing.T) { run(t, encrypted) })
	}
}

type guestStateDisk []byte

func (d guestStateDisk) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off > int64(len(d)) {
		return 0, io.ErrShortWrite
	}
	n := copy(d[int(off):], p)
	if n != len(p) {
		return n, io.ErrShortWrite
	}
	return n, nil
}

func checkGuestStateSnapshot(t *testing.T, r *repo.Repository, sn *repo.Snapshot, f *guestStateFixture) {
	t.Helper()
	state, err := r.LoadBlob(context.Background(), repo.DataBlob, f.id)
	if err != nil || !bytes.Equal(state, f.state) {
		t.Errorf("guest state did not survive: %v", err)
	}
	if sn.Guests[0].Config != f.sn.Guests[0].Config {
		t.Error("guest configuration changed")
	}
	disk := make(guestStateDisk, len(f.disk))
	if err := diskimg.Write(context.Background(), r, &sn.Images[0], disk, true, nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(disk, f.disk) {
		t.Error("virtual disk changed")
	}
}

func TestHyperVGuestStatePrune(t *testing.T) {
	withGuestStateEncryption(t, func(t *testing.T, encrypted bool) {
		ctx := context.Background()
		f := newGuestStateFixture(t, encrypted)
		// Both restore points need the same state; forgetting the old point
		// must not make that state disposable while the newer one remains.
		newer := *f.sn
		newer.ID, newer.Time = repo.ID{}, f.sn.Time.Add(time.Second)
		if _, err := f.r.SaveSnapshot(ctx, &newer); err != nil {
			t.Fatal(err)
		}
		lock, err := f.r.Lock(ctx, true, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Unlock()
		if err := f.r.RemoveSnapshot(ctx, f.sn.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.r.Prune(ctx, repo.PruneOptions{}); err != nil {
			t.Fatal(err)
		}
		r, err := repo.Open(ctx, f.be, f.options...)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		sn, err := r.LoadSnapshot(ctx, newer.ID.String())
		if err != nil {
			t.Fatal(err)
		}
		checkGuestStateSnapshot(t, r, sn, f)
	})
}

func TestHyperVGuestStateCopy(t *testing.T) {
	withGuestStateEncryption(t, func(t *testing.T, encrypted bool) {
		ctx := context.Background()
		f := newGuestStateFixture(t, encrypted)
		be, err := backend.OpenLocal(filepath.Join(t.TempDir(), "copy"))
		if err != nil {
			t.Fatal(err)
		}
		var options []repo.Option
		if encrypted {
			options = []repo.Option{repo.Password("different-destination-recovery-key")}
		}
		dst, err := repo.Init(ctx, be, options...)
		if err != nil {
			t.Fatal(err)
		}
		defer dst.Close()
		srcLock, err := f.r.Lock(ctx, false, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer srcLock.Unlock()
		dstLock, err := dst.Lock(ctx, true, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer dstLock.Unlock()
		_, copied, err := repo.Copy(ctx, f.r, dst, []*repo.Snapshot{f.sn}, nil, nil)
		if err != nil || len(copied) != 1 {
			t.Fatalf("copy: %v, snapshots=%d", err, len(copied))
		}
		r, err := repo.Open(ctx, be, options...)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		sn, err := r.LoadSnapshot(ctx, copied[0].ID.String())
		if err != nil {
			t.Fatal(err)
		}
		checkGuestStateSnapshot(t, r, sn, f)
	})
}

func TestHyperVGuestStateKeepImmutable(t *testing.T) {
	withGuestStateEncryption(t, func(t *testing.T, encrypted bool) {
		f := newGuestStateFixture(t, encrypted)
		loc, ok := f.r.Index().Lookup(repo.BlobHandle{Type: repo.DataBlob, ID: f.id})
		if !ok {
			t.Fatal("state was not stored")
		}
		if _, err := f.r.KeepImmutable(context.Background(), f.sn); err != nil {
			t.Fatal(err)
		}
		pack := "data/" + loc.Pack.String()[:2] + "/" + loc.Pack.String()
		for _, name := range f.be.retained {
			if name == pack {
				return
			}
		}
		t.Error("immutability did not include the pack holding the guest state")
	})
}

func TestHyperVGuestStateCheck(t *testing.T) {
	missing := repo.Hash([]byte("state not in this repository")).String()
	for _, tc := range []struct{ name, config string }{
		{"missing", `{"gueststate":"` + missing + `"}`},
		{"invalid-id", `{"gueststate":"not-a-blob-id"}`},
		{"null-id", `{"gueststate":"` + (repo.ID{}).String() + `"}`},
		{"invalid-json", `{"gueststate":`},
		{"wrong-type", `{"gueststate":17}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGuestStateFixture(t, false)
			f.sn.Guests[0].Config = tc.config
			f.sn.ID = repo.ID{}
			if _, err := f.r.SaveSnapshot(context.Background(), f.sn); err != nil {
				t.Fatal(err)
			}
			for _, readData := range []bool{false, true} {
				result, err := checker.Run(context.Background(), f.r, checker.Options{ReadData: readData})
				if err != nil {
					t.Fatal(err)
				}
				if result.OK() || !strings.Contains(strings.Join(result.Errors, "\n"), "guest") {
					t.Errorf("read-data=%v: missing/invalid guest state was not reported: %+v", readData, result)
				}
			}
		})
	}
}

func TestHyperVGuestStateInvalidReferencesAbortMaintenance(t *testing.T) {
	for _, config := range []string{`{"gueststate":"invalid"}`, `{"gueststate":"` + repo.Hash([]byte("missing")).String() + `"}`} {
		t.Run(config, func(t *testing.T) {
			ctx := context.Background()
			f := newGuestStateFixture(t, false)
			bad := *f.sn
			bad.ID = repo.ID{}
			bad.Guests = append([]repo.Guest(nil), f.sn.Guests...)
			bad.Guests[0].Config = config
			if _, err := f.r.SaveSnapshot(ctx, &bad); err != nil {
				t.Fatal(err)
			}
			lock, err := f.r.Lock(ctx, true, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Unlock()
			before, err := f.be.List(ctx, "data")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.r.Prune(ctx, repo.PruneOptions{}); err == nil {
				t.Error("prune accepted an invalid/missing guest state reference")
			}
			after, err := f.be.List(ctx, "data")
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(before, "\n") != strings.Join(after, "\n") {
				t.Error("prune changed packs before rejecting guest state metadata")
			}
			if _, err := f.r.KeepImmutable(ctx, &bad); err == nil {
				t.Error("immutability accepted an invalid/missing guest state reference")
			}
			be, err := backend.OpenLocal(filepath.Join(t.TempDir(), "copy"))
			if err != nil {
				t.Fatal(err)
			}
			dst, err := repo.Init(ctx, be)
			if err != nil {
				t.Fatal(err)
			}
			defer dst.Close()
			dstLock, err := dst.Lock(ctx, true, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer dstLock.Unlock()
			if _, _, err := repo.Copy(ctx, f.r, dst, []*repo.Snapshot{&bad}, nil, nil); err == nil {
				t.Error("copy accepted an invalid/missing guest state reference")
			}
			snapshots, err := dst.ListSnapshots(ctx)
			if err != nil || len(snapshots) != 0 {
				t.Fatalf("copy published an incomplete snapshot: %d, %v", len(snapshots), err)
			}
		})
	}
}

func TestHyperVGuestStateLegacyConfigurations(t *testing.T) {
	for _, tc := range []struct{ platform, config string }{
		{"hyperv", ""},
		{"hyperv", `{"generation":2,"tpm":false}`},
		{"hyperv", `{"generation":2,"tpm":true}`}, // old backup without saved vTPM
		{"unknown", "opaque configuration"},
	} {
		t.Run(tc.platform+tc.config, func(t *testing.T) {
			f := newGuestStateFixture(t, false)
			f.sn.Guests[0].Platform, f.sn.Guests[0].Config = tc.platform, tc.config
			used, err := f.r.BlobsOf(context.Background(), []*repo.Snapshot{f.sn})
			if err != nil {
				t.Fatal(err)
			}
			if used[repo.BlobHandle{Type: repo.DataBlob, ID: f.id}] {
				t.Error("legacy configuration unexpectedly referenced the state blob")
			}
		})
	}
}
