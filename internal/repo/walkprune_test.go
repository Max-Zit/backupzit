package repo

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
)

// objectStore is a local backend that tells the repository it is an object
// store (several reads at a time); reads of the named packs fail.
type objectStore struct {
	backend.Backend
	mu    sync.Mutex
	fail  map[string]bool
	reads map[string]int
}

func (o *objectStore) Concurrency() int { return 4 }

func (o *objectStore) LoadRange(ctx context.Context, name string, off int64, n int) ([]byte, error) {
	o.mu.Lock()
	if o.reads == nil {
		o.reads = map[string]int{}
	}
	o.reads[name]++
	bad := o.fail[name]
	o.mu.Unlock()
	if bad {
		return nil, errors.New("The server did not respond in time.")
	}
	return o.Backend.LoadRange(ctx, name, off, n)
}

func (o *objectStore) setFail(names ...string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.fail = map[string]bool{}
	for _, n := range names {
		o.fail[n] = true
	}
}

// blobsOfOneByOne is BlobsOf as it was before 0.57 (every tree and block
// map read one after another, per snapshot): the reference for the test.
func blobsOfOneByOne(ctx context.Context, r *Repository, sns []*Snapshot) (map[BlobHandle]bool, error) {
	used := map[BlobHandle]bool{}
	var walk func(id ID) error
	walk = func(id ID) error {
		h := BlobHandle{Type: TreeBlob, ID: id}
		if used[h] {
			return nil
		}
		used[h] = true
		t, err := r.LoadTree(ctx, id)
		if err != nil {
			return err
		}
		for _, n := range t.Nodes {
			for _, c := range n.Content {
				used[BlobHandle{Type: DataBlob, ID: c}] = true
			}
			if n.Subtree != nil {
				if err := walk(*n.Subtree); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, sn := range sns {
		if err := walk(sn.Tree); err != nil {
			return nil, err
		}
		for _, img := range sn.Images {
			used[BlobHandle{Type: DataBlob, ID: img.Head}] = true
			for i := range img.Partitions {
				p := &img.Partitions[i]
				if !p.Included {
					continue
				}
				for _, m := range p.Maps {
					used[BlobHandle{Type: MapBlob, ID: m}] = true
				}
				ids, err := r.LoadBlockMap(ctx, p)
				if err != nil {
					return nil, err
				}
				for _, id := range ids {
					if !id.IsNull() {
						used[BlobHandle{Type: DataBlob, ID: id}] = true
					}
				}
			}
		}
	}
	return used, nil
}

// buildWalkRepo saves three snapshots that share subtrees and block map
// chunks, with nested folders, files and disk images whose partitions
// repeat a map chunk (unused space) and span several map blobs.
func buildWalkRepo(t *testing.T, r *Repository) []*Snapshot {
	t.Helper()
	ctx := context.Background()
	blob := func(s string) ID {
		id, _, err := r.SaveBlob(ctx, DataBlob, []byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	tree := func(nodes ...Node) ID {
		id, err := r.SaveTree(ctx, &Tree{Nodes: nodes})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	shared := tree(Node{Name: "a.txt", Type: "file", Content: []ID{blob("shared-1"), blob("shared-2")}})
	var sns []*Snapshot
	for s := 0; s < 3; s++ {
		deep := tree(Node{Name: fmt.Sprintf("f%d", s), Type: "file", Content: []ID{blob(fmt.Sprint("deep-", s))}})
		mid := tree(Node{Name: "deep", Type: "dir", Subtree: &deep}, Node{Name: "shared", Type: "dir", Subtree: &shared})
		root := tree(Node{Name: "mid", Type: "dir", Subtree: &mid}, Node{Name: "same", Type: "dir", Subtree: &shared})
		// A partition of 3 map blobs + a few entries: chunk 0 is the same
		// in every snapshot, chunk 1 is all unused space twice over (a
		// repeated map ID), the tail changes per snapshot.
		n := 3*mapEntriesPerBlob + 5
		ids := make([]ID, n)
		for i := 0; i < mapEntriesPerBlob; i += 97 {
			ids[i] = blob(fmt.Sprint("block-", i))
		}
		for i := 3 * mapEntriesPerBlob; i < n; i++ {
			ids[i] = blob(fmt.Sprint("tail-", s, "-", i))
		}
		maps, err := r.SaveBlockMap(ctx, ids)
		if err != nil {
			t.Fatal(err)
		}
		if maps[1] != maps[2] {
			t.Fatal("test set-up: unused chunks should share one map blob")
		}
		sn := &Snapshot{Time: time.Now(), Hostname: "h", Tree: root, Images: []DiskImage{{
			Head: blob(fmt.Sprint("head-", s)),
			Partitions: []PartitionImage{
				{Number: 1, Included: true, Blocks: uint64(n), Maps: maps},
				{Number: 2, Included: false},
			},
		}}}
		if err := r.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := r.SaveSnapshot(ctx, sn); err != nil {
			t.Fatal(err)
		}
		sns = append(sns, sn)
	}
	return sns
}

// BlobsOf reads trees and block maps several at a time on an object store
// and each one only once, and finds exactly what the old one-by-one walk
// found; a wrong block count is still refused.
func TestBlobsOfParallelSameAsOneByOne(t *testing.T) {
	local, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	be := &objectStore{Backend: local}
	r := newTestRepo(t, be, 64<<10)
	defer r.Close()
	ctx := context.Background()
	sns := buildWalkRepo(t, r)
	if r.Concurrency() < 2 {
		t.Fatalf("concurrency %d", r.Concurrency())
	}
	want, err := blobsOfOneByOne(ctx, r, sns)
	if err != nil {
		t.Fatal(err)
	}
	read := 0
	got, err := r.BlobsOf(WithWalkProgress(ctx, func(n int) { read = n }), sns)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(got, want) {
		t.Fatalf("BlobsOf found %d blobs, the one-by-one walk %d", len(got), len(want))
	}
	// 3 roots + 3 mids + 3 deeps + 1 shared tree; maps: chunk 0 and the
	// unused chunk once, plus one tail per snapshot.
	if read != 10+2+3 {
		t.Errorf("%d trees and maps read, want 15 (each once)", read)
	}
	for _, sn := range sns {
		one, err := r.BlobsOf(ctx, []*Snapshot{sn})
		if err != nil {
			t.Fatal(err)
		}
		ref, _ := blobsOfOneByOne(ctx, r, []*Snapshot{sn})
		if !maps.Equal(one, ref) {
			t.Fatalf("snapshot %s: %d blobs, want %d", sn.ID.Short(), len(one), len(ref))
		}
	}
	bad := *sns[1]
	bad.Images = []DiskImage{{Head: sns[1].Images[0].Head, Partitions: []PartitionImage{sns[1].Images[0].Partitions[0]}}}
	bad.Images[0].Partitions[0].Blocks++
	if _, err := r.BlobsOf(ctx, []*Snapshot{sns[0], &bad}); err == nil || !strings.Contains(err.Error(), "block map has") {
		t.Errorf("wrong block count: %v", err)
	}
}

// A pack that cannot be read while prune repacks it stays as it is (none of
// its blobs copied), the prune finishes and reports it, every kept blob
// stays readable, and the next prune repacks it. After repackGiveUp failed
// packs in a row prune stops repacking and keeps the rest as they are.
func TestPruneKeepsUnreadablePacks(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(fmt.Sprint("all=", all), func(t *testing.T) {
			local, err := backend.OpenLocal(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			be := &objectStore{Backend: local}
			r := newTestRepo(t, be, 64<<10)
			ctx := context.Background()
			blobs := randomBlobs(80, 16<<10)
			var ids, keepIDs []ID
			for i, b := range blobs {
				id, _, err := r.SaveBlob(ctx, DataBlob, b)
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
				if i%4 == 0 {
					keepIDs = append(keepIDs, id)
				}
			}
			save := func(content []ID) *Snapshot {
				root, err := r.SaveTree(ctx, &Tree{Nodes: []Node{{Name: "f", Type: "file", Content: content}}})
				if err != nil {
					t.Fatal(err)
				}
				if err := r.Flush(ctx); err != nil {
					t.Fatal(err)
				}
				sn := &Snapshot{Time: time.Now(), Hostname: "h", Tree: root}
				if _, err := r.SaveSnapshot(ctx, sn); err != nil {
					t.Fatal(err)
				}
				return sn
			}
			old := save(ids)
			save(keepIDs)
			if err := r.RemoveSnapshot(ctx, old.ID); err != nil {
				t.Fatal(err)
			}
			// The packs prune will repack (a quarter of their data is used);
			// they hold data blobs only, the trees are elsewhere.
			used, err := r.BlobsOf(ctx, mustList(t, r))
			if err != nil {
				t.Fatal(err)
			}
			byPack := map[ID][2]int{} // used, all
			r.idx.Each(func(h BlobHandle, l Location) {
				c := byPack[l.Pack]
				if used[h] {
					c[0]++
				}
				c[1]++
				byPack[l.Pack] = c
			})
			var candidates []string
			for p, c := range byPack {
				if c[0] > 0 && c[0] < c[1] {
					candidates = append(candidates, packName(p))
				}
			}
			if len(candidates) < 4 {
				t.Fatalf("test set-up: %d packs to repack", len(candidates))
			}
			failing := candidates[:1]
			if all {
				failing = candidates
			}
			be.setFail(failing...)
			var log []string
			st, err := r.Prune(ctx, PruneOptions{Progress: func(m string) { log = append(log, m) }})
			if err != nil {
				t.Fatalf("prune with an unreadable pack: %v", err)
			}
			wantSkipped := 1
			if all {
				wantSkipped = len(candidates)
			}
			if st.PacksSkipped != wantSkipped || !strings.Contains(st.SkipReason, "did not respond") {
				t.Fatalf("skipped %d (%q), want %d", st.PacksSkipped, st.SkipReason, wantSkipped)
			}
			if all {
				// It stopped after repackGiveUp packs instead of trying all.
				tried := 0
				be.mu.Lock()
				for _, n := range candidates {
					if be.reads[n] > 0 {
						tried++
					}
				}
				be.mu.Unlock()
				// Reads already under way when prune gives up finish: each
				// worker can have one in progress and the one that reported
				// the last failure may have started its next (the race
				// detector widens that window).
				if limit := repackGiveUp + 2*r.Concurrency(); tried > limit || tried == len(candidates) && len(candidates) > limit {
					t.Errorf("%d of %d unreadable packs tried, want it to stop after %d", tried, len(candidates), repackGiveUp)
				}
			}
			if !all {
				// The used blobs of a repacked pack came in one ranged read.
				be.mu.Lock()
				for _, n := range candidates[1:] {
					if be.reads[n] != 1 {
						t.Errorf("pack %s read %d times, want 1", n, be.reads[n])
					}
				}
				be.mu.Unlock()
			}
			if !strings.Contains(strings.Join(log, "\n"), "could not be read and stay as they are") {
				t.Errorf("prune log: %q", log)
			}
			for _, n := range failing {
				if _, err := local.Size(ctx, n); err != nil {
					t.Fatalf("unreadable pack %s was deleted: %v", n, err)
				}
			}
			check := func() {
				r2, err := Open(ctx, local)
				if err != nil {
					t.Fatal(err)
				}
				defer r2.Close()
				for _, id := range keepIDs {
					b, err := r2.LoadBlob(ctx, DataBlob, id)
					if err != nil || Hash(b) != id {
						t.Fatalf("kept blob %s after prune: %v", id.Short(), err)
					}
				}
			}
			check()
			be.setFail()
			st, err = r.Prune(ctx, PruneOptions{})
			if err != nil || st.PacksSkipped != 0 || st.PacksRepacked < wantSkipped {
				t.Fatalf("next prune: %+v, %v", st, err)
			}
			check()
			r.Close()
		})
	}
}

func mustList(t *testing.T, r *Repository) []*Snapshot {
	t.Helper()
	sns, err := r.ListSnapshots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return sns
}
