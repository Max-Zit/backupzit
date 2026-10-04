package backend_test

import (
	"bytes"
	"context"
	"math/rand"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/testutil"
)

func TestS3RoundTrip(t *testing.T) {
	srv, err := testutil.StartS3Server("bkt")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ctx := context.Background()
	be, err := backend.Open(ctx, "s3://"+srv.Host+"/bkt/p?tls=false", backend.Options{S3AccessKey: "k", S3SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{10, 70000, 200000, 5 << 20, 20 << 20} {
		data := make([]byte, size)
		rand.New(rand.NewSource(int64(size))).Read(data)
		if err := be.Save(ctx, "data/x", data); err != nil {
			t.Fatal(err)
		}
		got, err := be.Load(ctx, "data/x")
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("size %d: full load differs (got %d bytes, err %v)", size, len(got), err)
		}
		if size > 100 {
			part, err := be.LoadRange(ctx, "data/x", int64(size/3), 50)
			if err != nil || !bytes.Equal(part, data[size/3:size/3+50]) {
				t.Fatalf("size %d: range differs: %v", size, err)
			}
			tail, err := be.LoadRange(ctx, "data/x", -8, 8)
			if err != nil || !bytes.Equal(tail, data[size-8:]) {
				t.Fatalf("size %d: suffix differs: %v", size, err)
			}
		}
		if n, err := be.Size(ctx, "data/x"); err != nil || n != int64(size) {
			t.Fatalf("size %d: Size=%d %v", size, n, err)
		}
	}
	names, err := be.List(ctx, "data")
	if err != nil || len(names) != 1 || names[0] != "data/x" {
		t.Fatalf("list: %v %v", names, err)
	}
	if err := be.Remove(ctx, "data/x"); err != nil {
		t.Fatal(err)
	}
	if _, err := be.Load(ctx, "data/x"); err == nil {
		t.Fatal("removed object still loads")
	}
}
