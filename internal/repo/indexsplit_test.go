package repo

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
)

// U1 (0.63): index files are read with a size limit (B7, 0.62) while prune,
// repair and the index recovery wrote 256 packs per file with no bound on
// the entries. With many small blobs a file passed the limit, and the
// repository could no longer be opened. Every writer now splits at
// maxIndexFileBlobs entries too, and the read limit is 1 GiB.

func setIndexLimits(t *testing.T, blobs int, size int64) {
	t.Helper()
	oldBlobs, oldSize := maxIndexFileBlobs, maxIndexSize
	if blobs > 0 {
		maxIndexFileBlobs = blobs
	}
	if size > 0 {
		maxIndexSize = size
	}
	t.Cleanup(func() { maxIndexFileBlobs, maxIndexSize = oldBlobs, oldSize })
}

type indexFileInfo struct {
	name         string
	size         int
	packs, blobs int
}

func indexFileInfos(t *testing.T, r *Repository, mem *memBackend) []indexFileInfo {
	t.Helper()
	var out []indexFileInfo
	for n, sealed := range mem.objects("index/") {
		f, err := r.parseIndexFile(n, sealed)
		if err != nil {
			t.Fatal(err)
		}
		fi := indexFileInfo{name: n, size: len(sealed), packs: len(f.Packs)}
		for _, p := range f.Packs {
			fi.blobs += len(p.Blobs)
		}
		out = append(out, fi)
	}
	return out
}

// requireBoundedIndex fails when an index file holds more than the caps and
// returns the size of the largest one.
func requireBoundedIndex(t *testing.T, r *Repository, mem *memBackend, what string) int {
	t.Helper()
	largest := 0
	infos := indexFileInfos(t, r, mem)
	if len(infos) < 2 {
		t.Fatalf("%s: %d index files, want them split", what, len(infos))
	}
	for _, fi := range infos {
		if fi.packs > maxIndexFilePacks || (fi.blobs > maxIndexFileBlobs && fi.packs > 1) {
			t.Errorf("%s: %s holds %d packs and %d blobs (caps %d and %d)", what, fi.name, fi.packs, fi.blobs, maxIndexFilePacks, maxIndexFileBlobs)
		}
		largest = max(largest, fi.size)
	}
	return largest
}

// smallBlobRepo stores two groups of tiny blobs in tiny packs: keep is used
// by both snapshots, drop only by the first, which is removed. It returns
// the blobs of keep. A prune then deletes the packs of drop and writes the
// index for the packs of keep.
func smallBlobRepo(t *testing.T, mem *memBackend, opts ...Option) []ID {
	t.Helper()
	ctx := context.Background()
	r := newTestRepo(t, mem, 2<<10, opts...)
	group := func(seed int64) []ID {
		var ids []ID
		for i := range 300 {
			id, _, err := r.SaveBlob(ctx, DataBlob, randBlob(seed+int64(i), 48))
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		if err := r.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		return ids
	}
	keep, drop := group(1000), group(5000)
	snap := func(ids []ID) ID {
		tree, err := r.SaveTree(ctx, &Tree{Nodes: []Node{{Name: "f", Type: NodeFile, Content: ids}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		id, err := r.SaveSnapshot(ctx, &Snapshot{Time: time.Now(), Tree: tree})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := snap(append(append([]ID(nil), keep...), drop...))
	snap(keep)
	if err := r.RemoveSnapshot(ctx, first); err != nil {
		t.Fatal(err)
	}
	return keep
}

func requireBlobs(t *testing.T, r *Repository, ids []ID) {
	t.Helper()
	for _, id := range ids {
		if _, err := r.LoadBlob(context.Background(), DataBlob, id); err != nil {
			t.Fatalf("blob %s: %v", id.Short(), err)
		}
	}
}

func TestSplitIndexPacks(t *testing.T) {
	setIndexLimits(t, 50, 0)
	mk := func(counts ...int) []indexPack {
		var out []indexPack
		for _, c := range counts {
			out = append(out, indexPack{Blobs: make([]PackedBlob, c)})
		}
		return out
	}
	sizes := func(groups [][]indexPack) (packs, blobs []int) {
		for _, g := range groups {
			n := 0
			for _, p := range g {
				n += len(p.Blobs)
			}
			packs, blobs = append(packs, len(g)), append(blobs, n)
		}
		return
	}
	// Entries: 10+30 fit, 45 starts a new file, 60 is alone (a pack is
	// never split), 1+49 fit.
	_, blobs := sizes(splitIndexPacks(mk(10, 30, 45, 60, 1, 49, 2)))
	if want := []int{40, 45, 60, 50, 2}; !equalInts(blobs, want) {
		t.Errorf("blob groups %v, want %v", blobs, want)
	}
	// Packs: at most maxIndexFilePacks per file.
	setIndexLimits(t, 1<<20, 0)
	packs, _ := sizes(splitIndexPacks(mk(make([]int, 2*maxIndexFilePacks+3)...)))
	if want := []int{maxIndexFilePacks, maxIndexFilePacks, 3}; !equalInts(packs, want) {
		t.Errorf("pack groups %v, want %v", packs, want)
	}
	if g := splitIndexPacks(nil); len(g) != 0 {
		t.Errorf("no packs: %d groups", len(g))
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The largest file a writer produces stays far below the read limit, and
// files of 256 packs written up to 0.62.2 open again down to an average
// stored blob of about 450 bytes.
func TestIndexReadLimitCoversWrittenFiles(t *testing.T) {
	if maxIndexSize < 1<<30 {
		t.Fatalf("read limit %d, want at least 1 GiB", maxIndexSize)
	}
	// Entries with the longest numbers (compressed, offsets near 4 GiB).
	const n = 1000
	blobs := make([]PackedBlob, n)
	for i := range blobs {
		blobs[i] = PackedBlob{Type: DataBlob, ID: Hash(randBlob(int64(i), 8)), Offset: 4000000000, Length: 4000000000, Raw: 4000000000}
	}
	b, err := encodeIndexFile([]indexPack{{ID: Hash([]byte("p")), Blobs: blobs}})
	if err != nil {
		t.Fatal(err)
	}
	perEntry := float64(len(b)) / n
	// Plus a pack ID line per pack and the sealing (nonce and tag).
	biggest := perEntry*float64(maxIndexFileBlobs) + float64(maxIndexFilePacks)*100 + 64
	if biggest > float64(maxIndexSize)/8 {
		t.Errorf("a full index file is about %.0f MiB, more than an eighth of the read limit", biggest/(1<<20))
	}
	// 0.62.2: 256 packs of 16 MiB, an average stored blob of 450 bytes,
	// about 111 bytes per entry (as measured in the review).
	old := 256 * float64(DefaultPackSize) / 450 * 111
	if old > float64(maxIndexSize) {
		t.Errorf("an index file of 0.62.2 with 450-byte blobs (%.0f MiB) does not fit the read limit", old/(1<<20))
	}
}

// Backups, prune, repair and the index rebuild all write bounded files, and
// the repository opens under a limit an unsplit file would exceed.
func TestIndexFilesSplitWithManySmallBlobs(t *testing.T) {
	for _, enc := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "encrypted"}[enc], func(t *testing.T) {
			setIndexLimits(t, 50, 0)
			ctx := context.Background()
			mem := newMemBackend()
			var opts []Option
			if enc {
				opts = append(opts, Password("secret"))
			}
			keep := smallBlobRepo(t, mem, opts...)
			r0, err := Open(ctx, mem, opts...)
			if err != nil {
				t.Fatal(err)
			}
			requireBoundedIndex(t, r0, mem, "backup")

			// Prune: the packs of the removed snapshot go, the rest is indexed
			// anew.
			st, err := r0.Prune(ctx, PruneOptions{RepackUnusedRatio: 0.99})
			if err != nil {
				t.Fatal(err)
			}
			if st.PacksDeleted == 0 {
				t.Fatalf("setup: nothing deleted: %+v", st)
			}
			largest := requireBoundedIndex(t, r0, mem, "prune")
			// An index of all kept packs in one file, as 0.62.2 wrote it, is
			// larger than the limit set from here on; the split files fit.
			var all []indexPack
			for p := range r0.idx.Packs() {
				all = append(all, indexPack{ID: p})
			}
			r0.idx.Each(func(h BlobHandle, l Location) {
				for i := range all {
					if all[i].ID == l.Pack {
						all[i].Blobs = append(all[i].Blobs, PackedBlob{Type: h.Type, ID: h.ID, Offset: l.Offset, Length: l.Length, Raw: l.Raw})
					}
				}
			})
			whole, err := encodeIndexFile(all)
			if err != nil {
				t.Fatal(err)
			}
			if len(whole) <= 2*largest {
				t.Fatalf("setup: one file %d bytes, largest split file %d", len(whole), largest)
			}
			setIndexLimits(t, 0, int64(len(whole)-1))
			r1, err := Open(ctx, mem, opts...)
			if err != nil {
				t.Fatalf("open after prune: %v", err)
			}
			requireBlobs(t, r1, keep)

			// Repair: one pack taken out, the new index split as well.
			var victim ID
			for p := range r1.idx.Packs() {
				victim = p
				break
			}
			if _, err := r1.DropDamagedPacks(ctx, []ID{victim}); err != nil {
				t.Fatal(err)
			}
			requireBoundedIndex(t, r1, mem, "repair")

			// Index rebuild (repair of damaged index files) writes all of it.
			if _, err := r1.RebuildIndex(ctx); err != nil {
				t.Fatal(err)
			}
			requireBoundedIndex(t, r1, mem, "rebuild")
			if _, err := Open(ctx, mem, opts...); err != nil {
				t.Fatalf("open after rebuild: %v", err)
			}
		})
	}
}

// A big index file that 0.62.2 wrote (here: above a lowered limit, for the
// real one between 128 MiB and 1 GiB) made the repository fail to open.
// Under the read limit it opens, and the next prune rewrites it into files
// that fit any limit above the cap.
func TestBigIndexFileOfEarlierVersionsOpens(t *testing.T) {
	setIndexLimits(t, 50, 0)
	ctx := context.Background()
	mem := newMemBackend()
	keep := smallBlobRepo(t, mem)
	r, err := Open(ctx, mem)
	if err != nil {
		t.Fatal(err)
	}
	// One index file for all packs, as prune wrote it before 0.63.
	var all []indexPack
	for p := range r.idx.Packs() {
		all = append(all, indexPack{ID: p})
	}
	r.idx.Each(func(h BlobHandle, l Location) {
		for i := range all {
			if all[i].ID == l.Pack {
				all[i].Blobs = append(all[i].Blobs, PackedBlob{Type: h.Type, ID: h.ID, Offset: l.Offset, Length: l.Length, Raw: l.Raw})
			}
		}
	})
	for n := range mem.objects("index/") {
		delete(mem.objs, n)
	}
	name, err := r.saveIndexFile(ctx, all)
	if err != nil {
		t.Fatal(err)
	}
	big := len(mem.objects("index/")[name])

	// The limit of 0.62.x below the file: the repository does not open.
	setIndexLimits(t, 0, int64(big-1))
	if _, err := Open(ctx, mem); !errors.Is(err, backend.ErrTooLarge) {
		t.Fatalf("open with the file above the limit: %v", err)
	}
	// The limit above it (1 GiB for real files): it opens, and prune
	// rewrites the index.
	setIndexLimits(t, 0, int64(big))
	r2, err := Open(ctx, mem)
	if err != nil {
		t.Fatal(err)
	}
	requireBlobs(t, r2, keep)
	if _, err := r2.Prune(ctx, PruneOptions{RepackUnusedRatio: 0.99}); err != nil {
		t.Fatal(err)
	}
	if _, ok := mem.objects("index/")[name]; ok {
		t.Fatal("prune kept the big index file")
	}
	requireBoundedIndex(t, r2, mem, "prune")
	setIndexLimits(t, 0, int64(big-1))
	r3, err := Open(ctx, mem)
	if err != nil {
		t.Fatalf("open after prune with the lower limit: %v", err)
	}
	requireBlobs(t, r3, keep)
}
