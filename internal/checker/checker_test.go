package checker_test

import (
	"context"
	mrand "math/rand"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/checker"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/testutil"
)

// The data check reads packs in parallel on S3 and names the damaged pack
// (here one the storage kept short, #38), reading all the others.
func TestReadDataFindsDamagedPack(t *testing.T) {
	ctx := context.Background()
	srv, err := testutil.StartS3Server("bkt")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	be, err := backend.Open(ctx, "s3://"+srv.Host+"/bkt/r?tls=false", backend.Options{S3AccessKey: "k", S3SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	if r.Concurrency() < 2 {
		t.Fatalf("S3 should read in parallel, concurrency %d", r.Concurrency())
	}
	var ids []repo.ID
	for i := 0; i < 8; i++ { // random 6 MiB blocks: several ~16 MiB packs
		data := make([]byte, 6<<20)
		mrand.New(mrand.NewSource(int64(i))).Read(data)
		id, _, err := r.SaveBlob(ctx, repo.DataBlob, data)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	tree, err := r.SaveTree(ctx, &repo.Tree{Nodes: []repo.Node{{Name: "f", Type: repo.NodeFile, Content: ids}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SaveSnapshot(ctx, &repo.Snapshot{Time: time.Now(), Tree: tree}); err != nil {
		t.Fatal(err)
	}
	packs, err := r.ListPacks(ctx)
	if err != nil || len(packs) < 2 {
		t.Fatalf("packs: %v %v", packs, err)
	}
	bad := packs[1]
	key := "r/data/" + bad.String()[:2] + "/" + bad.String()
	full, err := be.Load(ctx, "data/"+bad.String()[:2]+"/"+bad.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Replace("bkt", key, full[:len(full)/2]); err != nil {
		t.Fatal(err)
	}
	var progress int
	res, err := checker.Run(ctx, r, checker.Options{ReadData: true, Progress: func(done, total int) { progress = done }})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK() || len(res.Damaged) != 1 || res.Damaged[0] != bad {
		t.Fatalf("damaged %v (want %s), errors %v", res.Damaged, bad.Short(), res.Errors)
	}
	if res.PacksRead != len(packs)-1 || progress != len(packs) {
		t.Errorf("read %d of %d packs, progress %d", res.PacksRead, len(packs), progress)
	}
}
