package checker_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/checker"
	"github.com/max-zit/backupzit/internal/repo"
)

// L-25 (0.55): the blobs of a Linux system backup outside its tree (each
// disk's start, raw partition blocks) are checked too.
func TestSystemBackupBlobsChecked(t *testing.T) {
	ctx := context.Background()
	be, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	head, _, err := r.SaveBlob(ctx, repo.DataBlob, []byte("boot code and partition table"))
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
	missing := repo.Hash([]byte("a raw partition block that was never stored"))
	sys := &repo.SystemLayout{Disks: []repo.SystemDisk{{Head: head, Partitions: []repo.SystemPartition{{Raw: []repo.ID{missing}}}}}}
	if _, err := r.SaveSnapshot(ctx, &repo.Snapshot{Time: time.Now(), Tree: tree, System: sys}); err != nil {
		t.Fatal(err)
	}
	res, err := checker.Run(ctx, r, checker.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "system disk 0") || !strings.Contains(res.Errors[0], missing.Short()) {
		t.Fatalf("errors: %v", res.Errors)
	}
}
