package repo_test

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/backupzit/backupzit/internal/backend"
	"github.com/backupzit/backupzit/internal/checker"
	"github.com/backupzit/backupzit/internal/repo"
)

// TestImageSnapshotRoundTrip stores a fake partition image across several
// map blobs, reloads it, and checks that the checker follows block maps.
func TestImageSnapshotRoundTrip(t *testing.T) {
	ctx := context.Background()
	be, err := backend.OpenLocal(filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}

	// 20000 blocks span three map blobs; every third block is unused.
	const blocks = 20000
	ids := make([]repo.ID, blocks)
	var unique []repo.ID
	for i := range ids {
		if i%3 == 2 {
			continue
		}
		data := bytes.Repeat([]byte{byte(i % 7)}, 4096) // only 7 distinct blocks
		id, isNew, err := r.SaveBlob(ctx, repo.DataBlob, data)
		if err != nil {
			t.Fatal(err)
		}
		if isNew {
			unique = append(unique, id)
		}
		ids[i] = id
	}
	if len(unique) != 7 {
		t.Fatalf("dedup: want 7 unique blocks, got %d", len(unique))
	}
	maps, err := r.SaveBlockMap(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(maps) != 3 {
		t.Fatalf("want 3 map blobs, got %d", len(maps))
	}
	head, _, _ := r.SaveBlob(ctx, repo.DataBlob, make([]byte, 512))
	tree, _ := r.SaveTree(ctx, &repo.Tree{})
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	sn := &repo.Snapshot{Paths: []string{`\\.\PhysicalDrive0`}, Tree: tree, Images: []repo.DiskImage{{
		Number: 0, Size: 1 << 30, SectorSize: 512, Style: "gpt", GPTDiskID: "C6552194-B882-49DD-884D-1100B1A5FE52", Head: head,
		Partitions: []repo.PartitionImage{
			{Number: 1, Offset: 1 << 20, Length: blocks * 4096, Included: true, BlockSize: 4096, Blocks: blocks, Maps: maps},
			{Number: 2, Offset: 1 << 29, Length: 1 << 20}, // layout only
		},
	}}}
	if _, err := r.SaveSnapshot(ctx, sn); err != nil {
		t.Fatal(err)
	}

	r2, err := repo.Open(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := r2.LoadSnapshot(ctx, "latest")
	if err != nil {
		t.Fatal(err)
	}
	got, err := r2.LoadBlockMap(ctx, &loaded.Images[0].Partitions[0])
	if err != nil {
		t.Fatal(err)
	}
	for i := range ids {
		if got[i] != ids[i] {
			t.Fatalf("block %d differs after reload", i)
		}
	}
	res, err := checker.Run(ctx, r2, checker.Options{ReadData: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() || res.DataBlobs != 8 { // 7 blocks + disk head
		t.Fatalf("check: ok=%v blobs=%d errors=%v", res.OK(), res.DataBlobs, res.Errors)
	}

	// A wrong block count must be detected.
	bad := loaded.Images[0].Partitions[0]
	bad.Blocks++
	if _, err := r2.LoadBlockMap(ctx, &bad); err == nil {
		t.Error("block count mismatch not detected")
	}
}
