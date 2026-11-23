package repo

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
)

// Fixes of the third review of the damaged-index recovery (0.59, PR #53
// taken over).

// twoDataPacks returns two data packs of the snapshot other than the one
// with its folder list.
func twoDataPacks(t *testing.T, r *Repository, ids []ID) (ID, ID) {
	t.Helper()
	sns := mustList(t, r)
	treeLoc, _ := r.idx.Lookup(BlobHandle{Type: TreeBlob, ID: sns[0].Tree})
	var packs []ID
	for _, id := range ids {
		l, _ := r.idx.Lookup(BlobHandle{Type: DataBlob, ID: id})
		if l.Pack != treeLoc.Pack && (len(packs) == 0 || packs[len(packs)-1] != l.Pack) {
			packs = append(packs, l.Pack)
		}
		if len(packs) == 2 {
			return packs[0], packs[1]
		}
	}
	t.Fatal("setup: fewer than two data packs")
	return ID{}, ID{}
}

// A backup that needs a pack proven damaged is affected, but its other blobs
// still count: one in an intact pack the listing hid must stop the rebuild,
// or the next prune deletes that pack.
func TestRebuildChecksTheBlobsOfAffectedBackupsToo(t *testing.T) {
	ctx := context.Background()
	mem := newMemBackend()
	ids := snapshotRepo(t, mem, 20)
	r0, err := Open(ctx, mem)
	if err != nil {
		t.Fatal(err)
	}
	damaged, hidden := twoDataPacks(t, r0, ids)
	truncateIndexFiles(mem)
	mem.objs[packName(damaged)][0] ^= 0xff
	be := &listHidingBackend{memBackend: mem, hide: map[string]bool{packName(hidden): true}}
	r, err := Open(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	before := copyObjects(mem)
	if _, err := r.RebuildIndex(ctx); err == nil || !strings.Contains(err.Error(), "in no pack of the new index") || !strings.Contains(err.Error(), "not touched") {
		t.Fatalf("rebuild with a hidden pack an affected backup needs: %v", err)
	}
	requireUntouched(t, mem, before)
}

// Data that is really gone: the rebuild names the backups that use it and
// stops; with "remove affected backups" (AcceptLost) they count as affected
// and the rebuild goes on.
func TestRebuildNamesBackupsWithLostDataAndAcceptsTheLoss(t *testing.T) {
	ctx := context.Background()
	mem := newMemBackend()
	ids := snapshotRepo(t, mem, 20)
	r0, err := Open(ctx, mem)
	if err != nil {
		t.Fatal(err)
	}
	sn := mustList(t, r0)[0]
	gone, _ := twoDataPacks(t, r0, ids)
	truncateIndexFiles(mem)
	delete(mem.objs, packName(gone))
	r, err := Open(ctx, mem)
	if err != nil {
		t.Fatal(err)
	}
	before := copyObjects(mem)
	_, err = r.RebuildIndex(ctx)
	if err == nil || !strings.Contains(err.Error(), sn.ID.Short()) || !strings.Contains(err.Error(), "remove affected backups") {
		t.Fatalf("rebuild with lost data: %v", err)
	}
	requireUntouched(t, mem, before)
	rb, err := r.RebuildIndexWith(ctx, RebuildOptions{AcceptLost: true})
	if err != nil {
		t.Fatal(err)
	}
	if rb.Missing == 0 || len(rb.Snapshots) != 1 || rb.Snapshots[0].ID != sn.ID {
		t.Fatalf("missing %d, affected %v", rb.Missing, rb.Snapshots)
	}
	if len(r.DamagedIndexFiles()) != 0 {
		t.Errorf("still damaged: %v", r.DamagedIndexFiles())
	}
}

// A storage that lists differently each time proves nothing: the loss is
// not accepted.
func TestRebuildAcceptsNoLossOnAnUnstableListing(t *testing.T) {
	ctx := context.Background()
	mem := newMemBackend()
	ids := snapshotRepo(t, mem, 20)
	r0, err := Open(ctx, mem)
	if err != nil {
		t.Fatal(err)
	}
	gone, other := twoDataPacks(t, r0, ids)
	truncateIndexFiles(mem)
	delete(mem.objs, packName(gone))
	be := &flipListBackend{memBackend: mem, sometimes: packName(other)}
	r, err := Open(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RebuildIndexWith(ctx, RebuildOptions{AcceptLost: true}); err == nil || !strings.Contains(err.Error(), "lists its packs differently") {
		t.Fatalf("rebuild on an unstable listing: %v", err)
	}
}

// flipListBackend shows the pack sometimes only every other listing.
type flipListBackend struct {
	*memBackend
	sometimes string
	n         int
}

func (f *flipListBackend) List(ctx context.Context, dir string) ([]string, error) {
	names, err := f.memBackend.List(ctx, dir)
	if err != nil || dir != "data" {
		return names, err
	}
	f.n++
	if f.n%2 == 1 {
		return names, nil
	}
	var out []string
	for _, n := range names {
		if n != f.sometimes {
			out = append(out, n)
		}
	}
	return out, nil
}

// A pack that was listed and is gone a moment later (a data folder unmounted
// during the repair) stops the rebuild instead of counting as damaged.
func TestRebuildStopsWhenAListedPackVanishes(t *testing.T) {
	ctx := context.Background()
	mem := newMemBackend()
	snapshotRepo(t, mem, 20)
	truncateIndexFiles(mem)
	be := &vanishingBackend{memBackend: mem}
	r, err := Open(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	before := copyObjects(mem)
	be.vanish = true
	if _, err := r.RebuildIndex(ctx); err == nil || !strings.Contains(err.Error(), "is gone now") || !strings.Contains(err.Error(), "not touched") {
		t.Fatalf("rebuild while packs vanish: %v", err)
	}
	requireUntouched(t, mem, before)
}

// vanishingBackend lists every pack, but once vanish is set, loading one
// whole says it is not there.
type vanishingBackend struct {
	*memBackend
	vanish bool
}

func (v *vanishingBackend) Load(ctx context.Context, name string) ([]byte, error) {
	if v.vanish && strings.HasPrefix(name, "data/") {
		return nil, backend.ErrNotFound
	}
	return v.memBackend.Load(ctx, name)
}

// The copy of a damaged index file is sealed: whatever the storage handed
// over as that file is not written back readable.
func TestDamagedIndexCopyIsSealed(t *testing.T) {
	ctx := context.Background()
	mem := newMemBackend()
	snapshotRepo(t, mem, 6, Password("pw"))
	planted := "index/" + strings.Repeat("ab", 32)
	mem.objs[planted] = []byte("root:secret:of the host")
	r, err := Open(ctx, mem, Password("pw"))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.DamagedIndexFiles()) == 0 {
		t.Fatal("the planted file does not count as damaged")
	}
	if _, err := r.RebuildIndex(ctx); err != nil {
		t.Fatal(err)
	}
	copied, ok := mem.objs[damagedIndexDir+"/"+strings.Repeat("ab", 32)]
	if !ok {
		t.Fatalf("no copy; objects %v", mem.objects(damagedIndexDir))
	}
	if bytes.Contains(copied, []byte("secret")) {
		t.Fatal("the copy is readable")
	}
	if plain, err := r.open(copied); err != nil || string(plain) != "root:secret:of the host" {
		t.Fatalf("the copy does not open with the repository key: %q %v", plain, err)
	}
}

// One corrupt download of a healthy index file (the same length, so not a
// cut download) is read again before the file counts as damaged.
func TestIndexFileReadAgainOnAHashMismatch(t *testing.T) {
	ctx := context.Background()
	mem := newMemBackend()
	ids := snapshotRepo(t, mem, 6)
	be := &corruptOnceBackend{memBackend: mem, done: map[string]bool{}}
	r, err := Open(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	if bad := r.DamagedIndexFiles(); len(bad) != 0 {
		t.Fatalf("one corrupt download marked %v as damaged", bad)
	}
	if be.corrupted == 0 {
		t.Fatal("setup: nothing was corrupted")
	}
	for _, id := range ids {
		if !r.idx.Reusable(BlobHandle{Type: DataBlob, ID: id}) {
			t.Fatalf("blob %s is not reusable", id.Short())
		}
	}
}

// corruptOnceBackend flips a byte of the first download of each index file.
type corruptOnceBackend struct {
	*memBackend
	done      map[string]bool
	corrupted int
}

func (c *corruptOnceBackend) Load(ctx context.Context, name string) ([]byte, error) {
	b, err := c.memBackend.Load(ctx, name)
	if err != nil || !strings.HasPrefix(name, "index/") || c.done[name] {
		return b, err
	}
	c.done[name] = true
	c.corrupted++
	b = append([]byte(nil), b...)
	b[len(b)/2] ^= 0x01
	return b, nil
}

var _ = errors.New
