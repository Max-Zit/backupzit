package e2e

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/backupzit/backupzit/internal/archiver"
	"github.com/backupzit/backupzit/internal/backend"
	"github.com/backupzit/backupzit/internal/checker"
	"github.com/backupzit/backupzit/internal/repo"
	"github.com/backupzit/backupzit/internal/restorer"
)

// TestCopy copies snapshots between two repositories with different keys
// (e.g. NAS → S3) and restores from the copy.
func TestCopy(t *testing.T) {
	ctx := context.Background()
	open := func(dir, pw string, create bool) *repo.Repository {
		be, err := backend.OpenLocal(dir)
		if err != nil {
			t.Fatal(err)
		}
		var r *repo.Repository
		if create {
			r, err = repo.Init(ctx, be, repo.Password(pw))
		} else {
			r, err = repo.Open(ctx, be, repo.Password(pw))
		}
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	srcDir, dstDir := filepath.Join(tempDir(t), "src"), filepath.Join(tempDir(t), "dst")
	src := open(srcDir, "source-key", true)
	dst := open(dstDir, "other-key", true)

	data := filepath.Join(tempDir(t), "data")
	makeSource(t, data, rand.New(rand.NewSource(3)))
	want := manifest(t, data)
	sn1, err := archiver.Run(ctx, src, archiver.Options{Paths: []string{data}, Tags: []string{"job:1"}})
	if err != nil {
		t.Fatal(err)
	}
	st, copied, err := repo.Copy(ctx, src, dst, []*repo.Snapshot{sn1}, []string{"job:7"}, nil)
	if err != nil || st.Snapshots != 1 || len(copied) != 1 {
		t.Fatalf("copy 1: %+v %v", st, err)
	}
	t.Logf("first copy: %+v", st)

	// Second backup changes a little; copying it transfers only new data.
	writeFile(t, filepath.Join(data, "new.txt"), []byte("added after the first copy"))
	sn2, err := archiver.Run(ctx, src, archiver.Options{Paths: []string{data}, Tags: []string{"job:1"}})
	if err != nil {
		t.Fatal(err)
	}
	st2, _, err := repo.Copy(ctx, src, dst, []*repo.Snapshot{sn1, sn2}, []string{"job:7"}, nil)
	if err != nil || st2.Snapshots != 1 || st2.Skipped != 1 || st2.BytesRead > 64*1024 {
		t.Fatalf("copy 2: %+v %v", st2, err)
	}
	t.Logf("second copy: %+v", st2)
	dst.Close()

	// The copy is a complete, independent repository.
	dst = open(dstDir, "other-key", false)
	if res, err := checker.Run(ctx, dst, checker.Options{ReadData: true}); err != nil || !res.OK() {
		t.Fatalf("check copy: %v %v", err, res.Errors)
	}
	sns, _ := dst.ListSnapshots(ctx)
	if len(sns) != 2 {
		t.Fatalf("copied snapshots: %d", len(sns))
	}
	var first *repo.Snapshot
	for _, sn := range sns {
		if hasTagT(sn, repo.CopyOfTag+sn1.ID.String()) && hasTagT(sn, "job:7") && !hasTagT(sn, "job:1") {
			first = sn
		}
	}
	if first == nil {
		t.Fatal("copy of sn1 not found or wrong tags")
	}
	tgt := filepath.Join(tempDir(t), "restore")
	if st, err := restorer.Run(ctx, dst, first, restorer.Options{Target: tgt, Verify: true}); err != nil || len(st.Errors) > 0 {
		t.Fatalf("restore from copy: %v %v", err, st)
	}
	compareManifests(t, want, manifest(t, restoredRoot(t, tgt, data)))
	os.RemoveAll(srcDir) // the original is gone; the copy still works
	if _, err := repo.Open(ctx, mustLocal(t, dstDir), repo.Password("other-key")); err != nil {
		t.Fatal(err)
	}
}

func hasTagT(sn *repo.Snapshot, tag string) bool {
	for _, t := range sn.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

func mustLocal(t *testing.T, dir string) backend.Backend {
	be, err := backend.OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	return be
}
