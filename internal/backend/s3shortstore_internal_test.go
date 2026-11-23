package backend

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/testutil"
)

// A storage that answers success but keeps a shorter object (seen at a
// provider, #38): the upload checks the stored size and uploads again.
func TestS3UploadAgainWhenStoredShort(t *testing.T) {
	old := s3Backoff
	s3Backoff = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond}
	defer func() { s3Backoff = old }()
	srv, err := testutil.StartS3Server("bkt")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ctx := context.Background()
	be, err := Open(ctx, "s3://"+srv.Host+"/bkt/p?tls=false", Options{S3AccessKey: "k", S3SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("pack data "), 400000)
	srv.TruncateNextPuts("data/77", 1)
	if err := be.Save(ctx, "data/77/77e0", data); err != nil {
		t.Fatalf("save: %v", err)
	}
	if n := srv.Puts("data/77"); n != 2 {
		t.Errorf("%d uploads, want 2 (the short one and the repeat)", n)
	}
	got, err := be.Load(ctx, "data/77/77e0")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("stored object: %d bytes, %v; want %d", len(got), err, len(data))
	}
	// A storage that keeps losing data fails the upload after the retries.
	srv.TruncateNextPuts("data/88", 100)
	if err := be.Save(ctx, "data/88/88aa", data); err == nil || !strings.Contains(err.Error(), "different size") {
		t.Fatalf("always short: %v", err)
	}
}
