package e2e

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/backupzit/backupzit/internal/archiver"
	"github.com/backupzit/backupzit/internal/backend"
	"github.com/backupzit/backupzit/internal/repo"
	"github.com/backupzit/backupzit/internal/restorer"
)

// TestImmutableS3 runs against a real bucket with Object Lock, e.g. MinIO:
//
//	mc mb --with-lock lab/immutable-backups
//	BACKUPZIT_TEST_S3LOCK=s3://host:9000/immutable-backups?tls=false \
//	BACKUPZIT_TEST_S3_ACCESS_KEY=... BACKUPZIT_TEST_S3_SECRET_KEY=... go test ./internal/e2e -run Immutable
//
// Test objects stay locked for one day.
func TestImmutableS3(t *testing.T) {
	loc := os.Getenv("BACKUPZIT_TEST_S3LOCK")
	if loc == "" {
		t.Skip("BACKUPZIT_TEST_S3LOCK not set")
	}
	ctx := context.Background()
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("e2e-%d", time.Now().UnixNano())
	u.Path = strings.TrimRight(u.Path, "/") + "/" + prefix
	loc = u.String()
	bucket := strings.Split(strings.Trim(u.Path, "/"), "/")[0]
	opts := backend.Options{S3AccessKey: os.Getenv("BACKUPZIT_TEST_S3_ACCESS_KEY"), S3SecretKey: os.Getenv("BACKUPZIT_TEST_S3_SECRET_KEY"), S3LockDays: 1}
	open := func(o backend.Options) backend.Backend {
		t.Helper()
		be, err := backend.Open(ctx, loc, o)
		if err != nil {
			t.Fatal(err)
		}
		return be
	}
	pw := repo.Password("immutable-test")

	// --- two backups to the locked bucket
	src := filepath.Join(tempDir(t), "source data")
	makeSource(t, src, rand.New(rand.NewSource(7)))
	want := manifest(t, src)
	r, err := repo.Init(ctx, open(opts), pw)
	if err != nil {
		t.Fatal(err)
	}
	sn1, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(src, "later.txt"), []byte("after the first backup"))
	sn2, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := r.KeepImmutable(ctx, sn2); err != nil || n != 0 {
		t.Errorf("fresh objects should need no extension: n=%d err=%v", n, err)
	}
	r.Close()

	// A longer period extends every object sn2 needs, once.
	o2 := opts
	o2.S3LockDays = 2
	r2, err := repo.Open(ctx, open(o2), pw)
	if err != nil {
		t.Fatal(err)
	}
	n, err := r2.KeepImmutable(ctx, sn2)
	if err != nil || n == 0 {
		t.Fatalf("expected extended locks: n=%d err=%v", n, err)
	}
	if n2, err := r2.KeepImmutable(ctx, sn2); err != nil || n2 != 0 {
		t.Errorf("second extension: n=%d err=%v", n2, err)
	}
	t.Logf("extended %d objects", n)
	r2.Close()

	// --- the attacker has the storage keys
	mc, err := minio.New(u.Host, &minio.Options{Creds: credentials.NewStaticV4(opts.S3AccessKey, opts.S3SecretKey, ""), Secure: u.Query().Get("tls") != "false"})
	if err != nil {
		t.Fatal(err)
	}
	var asOf time.Time
	var victim minio.ObjectInfo
	for obj := range mc.ListObjects(ctx, bucket, minio.ListObjectsOptions{Prefix: prefix + "/", Recursive: true, WithVersions: true}) {
		if obj.Err != nil {
			t.Fatal(obj.Err)
		}
		if obj.LastModified.After(asOf) {
			asOf = obj.LastModified // server clock, independent of skew
		}
		if strings.Contains(obj.Key, "/data/") {
			victim = obj
		}
	}
	time.Sleep(2 * time.Second)
	if err := mc.RemoveObject(ctx, bucket, victim.Key, minio.RemoveObjectOptions{VersionID: victim.VersionID}); err == nil {
		t.Fatal("deleting a locked object version succeeded")
	} else {
		t.Logf("permanent delete refused: %v", err)
	}
	if err := mc.RemoveObject(ctx, bucket, victim.Key, minio.RemoveObjectOptions{VersionID: victim.VersionID, GovernanceBypass: true}); err == nil {
		t.Fatal("governance bypass deleted a compliance-locked object")
	}
	// Delete everything (only adds delete markers) and overwrite the config.
	be := open(opts)
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
	if err := be.Save(ctx, "config", []byte("encrypted by ransomware")); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Open(ctx, be, pw); err == nil {
		t.Fatal("repository still opens after the attack")
	}

	// --- point-in-time view before the attack
	view := opts
	view.AsOf = asOf
	vbe := open(view)
	if err := vbe.Save(ctx, "x", []byte("x")); !errors.Is(err, backend.ErrReadOnly) {
		t.Errorf("save to point-in-time view: %v", err)
	}
	rv, err := repo.Open(ctx, vbe, pw)
	if err != nil {
		t.Fatal(err)
	}
	sns, err := rv.ListSnapshots(ctx)
	if err != nil || len(sns) != 2 {
		t.Fatalf("snapshots before the attack: %d, %v", len(sns), err)
	}
	sn, err := rv.LoadSnapshot(ctx, sn1.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	tgt := filepath.Join(tempDir(t), "restore")
	st, err := restorer.Run(ctx, rv, sn, restorer.Options{Target: tgt, Verify: true})
	if err != nil || len(st.Errors) > 0 {
		t.Fatalf("restore: %v %v", err, st.Errors)
	}
	compareManifests(t, want, manifest(t, restoredRoot(t, tgt, src)))
}
