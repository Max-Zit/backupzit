package repo

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// A pack upload that fails on a sequential backend (local, SFTP, SMB) must
// fail everything after it: before, its blobs stayed "pending", a later
// save of the same data counted as stored (the next VM of the run) and
// Flush succeeded, so a snapshot could reference data that was never
// uploaded.
func TestFailedSequentialUploadIsSticky(t *testing.T) {
	ctx := context.Background()
	be := newMemBackend()
	r := newTestRepo(t, be, 1<<20)
	if r.maxUploads > 1 {
		t.Fatalf("memBackend should be sequential, got %d uploads", r.maxUploads)
	}
	be.failPacks = 1 // the first pack upload fails
	data := bytes.Repeat([]byte("vm1 block "), 200000)
	if _, _, err := r.SaveBlob(ctx, DataBlob, data); err == nil {
		if err = r.Flush(ctx); err == nil {
			t.Fatal("the failed upload was not reported")
		}
	}
	be.failPacks = 0 // the storage works again
	if _, added, err := r.SaveBlob(ctx, DataBlob, data); err == nil && !added {
		if err := r.Flush(ctx); err == nil {
			t.Fatal("the same data counted as stored after its upload failed, and Flush succeeded")
		}
	}
	if err := r.Flush(ctx); err == nil || !errors.Is(err, errUpload) {
		t.Fatalf("Flush after a failed upload: %v, want the upload error", err)
	}
}
