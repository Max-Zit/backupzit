package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
)

// Second review of #53: cases where the rebuild of a damaged index could
// still lose data, each with its test.

func randBlob(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// snapshotRepo stores n incompressible blobs in several packs and one
// snapshot that uses all of them; it returns the blob IDs.
func snapshotRepo(t *testing.T, be backend.Backend, n int, opts ...Option) []ID {
	t.Helper()
	ctx := context.Background()
	r, err := Init(ctx, be, opts...)
	if err != nil {
		t.Fatal(err)
	}
	r.cfg.PackSize = 64 << 10
	var ids []ID
	for i := range n {
		id, _, err := r.SaveBlob(ctx, DataBlob, randBlob(int64(i)+1, 12<<10))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	tree, err := r.SaveTree(ctx, &Tree{Nodes: []Node{{Name: "f", Type: NodeFile, Content: ids, Size: uint64(n) * 12 << 10}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SaveSnapshot(ctx, &Snapshot{Time: time.Now(), Tree: tree}); err != nil {
		t.Fatal(err)
	}
	return ids
}

// truncateIndexFiles cuts every index file in half and returns their names.
func truncateIndexFiles(be *memBackend) []string {
	var names []string
	for n, b := range be.objects("index/") {
		be.objs[n] = b[:len(b)/2]
		names = append(names, n)
	}
	return names
}

// copyObjects is the content of the whole storage, to compare before and after.
func copyObjects(be *memBackend) map[string]string {
	be.mu.Lock()
	defer be.mu.Unlock()
	out := make(map[string]string, len(be.objs))
	for n, b := range be.objs {
		out[n] = string(b)
	}
	return out
}

func requireUntouched(t *testing.T, be *memBackend, before map[string]string) {
	t.Helper()
	if after := copyObjects(be); !reflect.DeepEqual(before, after) {
		t.Errorf("the storage was touched: %d objects before, %d after", len(before), len(after))
		for n := range before {
			if _, ok := after[n]; !ok {
				t.Errorf("  gone: %s", n)
			}
		}
	}
}

// listHidingBackend lists fewer data packs than there are, without an error,
// like an unmounted data folder or an SFTP listing that skips a folder.
type listHidingBackend struct {
	*memBackend
	hide    map[string]bool
	hideAll bool
}

func (h *listHidingBackend) List(ctx context.Context, dir string) ([]string, error) {
	names, err := h.memBackend.List(ctx, dir)
	if err != nil || dir != "data" {
		return names, err
	}
	var out []string
	for _, n := range names {
		if !h.hideAll && !h.hide[n] {
			out = append(out, n)
		}
	}
	return out, nil
}

// (1) A pack that the listing does not show is neither recovered nor marked
// unreadable. The rebuild must refuse, because a backup uses its blobs and
// the next prune would delete the pack; it touches nothing.
func TestRebuildRefusesWhenListingHidesAPackBackupsNeed(t *testing.T) {
	ctx := context.Background()
	mem := newMemBackend()
	ids := snapshotRepo(t, mem, 12)
	// The pack of a data blob that is not the one with the folder list.
	r0, err := Open(ctx, mem)
	if err != nil {
		t.Fatal(err)
	}
	sns := mustList(t, r0)
	treeLoc, _ := r0.idx.Lookup(BlobHandle{Type: TreeBlob, ID: sns[0].Tree})
	var hidden ID
	for _, id := range ids {
		if l, _ := r0.idx.Lookup(BlobHandle{Type: DataBlob, ID: id}); l.Pack != treeLoc.Pack {
			hidden = l.Pack
			break
		}
	}
	if hidden == (ID{}) {
		t.Fatal("setup: all blobs in one pack")
	}
	truncateIndexFiles(mem)
	be := &listHidingBackend{memBackend: mem, hide: map[string]bool{packName(hidden): true}}
	r, err := Open(ctx, be)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	before := copyObjects(mem)
	_, err = r.RebuildIndex(ctx)
	if err == nil || !strings.Contains(err.Error(), "in no pack of the new index") || !strings.Contains(err.Error(), "not touched") {
		t.Fatalf("want a refusal, got %v", err)
	}
	requireUntouched(t, mem, before)
	if len(r.DamagedIndexFiles()) != 1 {
		t.Errorf("damaged state changed: %v", r.DamagedIndexFiles())
	}
	// The listing is complete again: the rebuild goes through and every blob reads.
	be.hide = nil
	if _, err := r.RebuildIndex(ctx); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(ctx, mem)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if _, err := r2.LoadBlob(ctx, DataBlob, id); err != nil {
			t.Fatalf("blob %s after rebuild: %v", id.Short(), err)
		}
	}
}

// (1) An empty listing although the index names packs: the checker's H-N6
// guard, in recovery and in the rebuild.
func TestRecoveryAndRebuildRefuseWhenNoPacksAreListed(t *testing.T) {
	ctx := context.Background()
	mem := newMemBackend()
	r := newTestRepo(t, mem, 64<<10)
	for batch := range 2 { // two index files
		for i := range 10 {
			if _, _, err := r.SaveBlob(ctx, DataBlob, randBlob(int64(batch*100+i), 12<<10)); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var victim string
	for n, b := range mem.objects("index/") {
		victim = n
		mem.objs[n] = b[:len(b)/2]
		break
	}
	be := &listHidingBackend{memBackend: mem}
	r2, err := Open(ctx, be)
	if err != nil || len(r2.DamagedIndexFiles()) != 1 {
		t.Fatal(err, r2.DamagedIndexFiles())
	}
	be.hideAll = true
	before := copyObjects(mem)
	if _, err := r2.RebuildIndex(ctx); err == nil || !strings.Contains(err.Error(), "lists no data packs at all") {
		t.Fatalf("rebuild: %v", err)
	}
	requireUntouched(t, mem, before)
	if _, err := Open(ctx, be); err == nil || !strings.Contains(err.Error(), "lists no data packs at all") {
		t.Fatalf("open: %v", err)
	}
	if _, ok := mem.objs[victim]; !ok {
		t.Error("the damaged file is gone")
	}
}

// (1) The damaged index file is moved aside, not deleted, and nothing that
// lists or loads index/ sees it afterwards.
func TestRebuildMovesDamagedIndexAside(t *testing.T) {
	ctx := context.Background()
	be, ids, victim, _ := damagedIndexRepo(t)
	damaged := append([]byte(nil), be.objs[victim]...)
	r, err := Open(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := r.RebuildIndex(ctx)
	if err != nil || len(rb.Moved) != 1 || rb.Moved[0] != victim {
		t.Fatal(rb, err)
	}
	aside := damagedIndexDir + "/" + victim[len("index/"):]
	if got, ok := be.objs[aside]; !ok || !bytes.Equal(got, damaged) {
		t.Fatalf("%s: kept=%v", aside, ok)
	}
	// The rebuilt index lists the same packs, so it has the damaged file's
	// name (the hash of its content) and replaces it in place.
	if got, ok := be.objs[victim]; ok && bytes.Equal(got, damaged) {
		t.Error("the damaged file is still in index/")
	}
	for n := range be.objects("index/") {
		if strings.Contains(n, "damaged") {
			t.Errorf("index/ lists %s", n)
		}
	}
	r2, err := Open(ctx, be)
	if err != nil || len(r2.DamagedIndexFiles()) != 0 {
		t.Fatal(err, r2.DamagedIndexFiles())
	}
	for _, id := range ids {
		if _, err := r2.LoadBlob(ctx, DataBlob, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r2.Prune(ctx, PruneOptions{}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, ok := be.objs[aside]; !ok {
		t.Error("prune removed the moved file")
	}
}

// cutLoadBackend serves a cut-short download of one file.
type cutLoadBackend struct {
	*memBackend
	name string
}

func (c *cutLoadBackend) Load(ctx context.Context, name string) ([]byte, error) {
	b, err := c.memBackend.Load(ctx, name)
	if err == nil && name == c.name {
		b = b[:len(b)/2]
	}
	return b, err
}

// (1) A damaged file is only removed from index/ after a copy that holds
// every byte the storage has.
func TestCopyAsideNeedsTheWholeFile(t *testing.T) {
	ctx := context.Background()
	be, _, victim, _ := damagedIndexRepo(t)
	r, err := Open(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	r.be = &cutLoadBackend{memBackend: be, name: victim}
	before := copyObjects(be)
	if _, err := r.copyAside(ctx, victim); err == nil || !strings.Contains(err.Error(), "not moved aside") {
		t.Fatalf("moveAside: %v", err)
	}
	requireUntouched(t, be, before)
}

// flakyPackBackend lets the reads of one pack fail in a scripted way:
// "bad" returns it with a flipped byte (a corrupt download), "err" a storage
// error, anything else the real content.
type flakyPackBackend struct {
	*memBackend
	pack  string
	steps []string
	calls int
}

func (f *flakyPackBackend) Load(ctx context.Context, name string) ([]byte, error) {
	b, err := f.memBackend.Load(ctx, name)
	if err != nil || name != f.pack {
		return b, err
	}
	step := ""
	if f.calls < len(f.steps) {
		step = f.steps[f.calls]
	}
	f.calls++
	switch step {
	case "bad":
		b[0] ^= 0xff
	case "err":
		return nil, fmt.Errorf("connection reset")
	}
	return b, nil
}

func onePackName(be *memBackend) string {
	for n := range be.objects("data/") {
		return n
	}
	return ""
}

// (2) One corrupt download during a repair must not take a healthy pack out
// of the index (the next prune would delete it): a pack is left out only
// when every try says it is damaged.
func TestRebuildUsesThreeTries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []string
		left  bool
		fail  bool
		calls int
	}{
		{"one bad download", []string{"bad"}, false, false, 2},
		{"two bad downloads", []string{"bad", "bad"}, false, false, 3},
		{"damaged every time", []string{"bad", "bad", "bad"}, true, false, 3},
		{"damaged and a storage error", []string{"bad", "err", "bad"}, false, true, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			mem, ids, _, _ := damagedIndexRepo(t)
			pack := onePackName(mem)
			f := &flakyPackBackend{memBackend: mem, pack: pack, steps: tc.steps}
			r, err := Open(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			before := copyObjects(mem)
			rb, err := r.RebuildIndex(ctx)
			if f.calls != tc.calls {
				t.Errorf("the pack was read %d times, want %d", f.calls, tc.calls)
			}
			if tc.fail {
				if err == nil || !strings.Contains(err.Error(), "could not be verified") {
					t.Fatalf("want a refusal, got %v", err)
				}
				requireUntouched(t, mem, before)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := len(rb.Left) == 1; got != tc.left {
				t.Fatalf("left out: %v", rb.Left)
			}
			r2, err := Open(ctx, mem)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, id := range ids {
				if _, err := r2.LoadBlob(ctx, DataBlob, id); err == nil {
					n++
				}
			}
			if tc.left && n == len(ids) || !tc.left && n != len(ids) {
				t.Errorf("%d of %d blobs readable after the rebuild", n, len(ids))
			}
		})
	}
}

// (3) Two hosts that backed up at once both stored the same blob. The copy
// that the in-memory index points at is in a damaged pack: the rebuild must
// still list the blob through the other, verified pack.
func TestRebuildListsBlobThroughTheOtherPack(t *testing.T) {
	ctx := context.Background()
	be := newMemBackend()
	r1 := newTestRepo(t, be, 1<<20)
	r2, err := Open(ctx, be) // opened before r1 stored anything: does not see its blobs
	if err != nil {
		t.Fatal(err)
	}
	shared := randBlob(1, 20<<10)
	var fillers []ID
	for i, r := range []*Repository{r1, r2} {
		if _, _, err := r.SaveBlob(ctx, DataBlob, shared); err != nil {
			t.Fatal(err)
		}
		id, _, err := r.SaveBlob(ctx, DataBlob, randBlob(int64(10+i), 20<<10))
		if err != nil {
			t.Fatal(err)
		}
		fillers = append(fillers, id)
		if err := r.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(be.objects("data/")); n != 2 {
		t.Fatalf("setup: %d packs", n)
	}
	truncateIndexFiles(be)
	r3, err := Open(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	h := BlobHandle{Type: DataBlob, ID: Hash(shared)}
	loc, ok := r3.idx.Lookup(h)
	if !ok {
		t.Fatal("setup: blob not recovered")
	}
	// Damage the copy the index points at (the blob itself, not the header).
	be.objs[packName(loc.Pack)][loc.Offset+3] ^= 0xff
	rb, err := r3.RebuildIndex(ctx)
	if err != nil || len(rb.Left) != 1 || rb.Left[0] != loc.Pack {
		t.Fatal(rb, err)
	}
	r4, err := Open(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := r4.LoadBlob(ctx, DataBlob, h.ID); err != nil || !bytes.Equal(got, shared) {
		t.Fatalf("shared blob after the rebuild: %v", err)
	}
	other := 0
	for _, id := range fillers {
		if l, ok := r4.idx.Lookup(BlobHandle{Type: DataBlob, ID: id}); ok && l.Pack != loc.Pack {
			other++
		}
	}
	if other != 1 {
		t.Errorf("the verified pack's other blob is listed %d times", other)
	}
}

// (4) A header length beyond the pack's size, under the maximum: bad header
// content, not a storage error that fails opening (repair included).
func TestHeaderLengthBeyondPackIsBadContent(t *testing.T) {
	ctx := context.Background()
	be, ids, _, _ := damagedIndexRepo(t)
	junk := append(bytes.Repeat([]byte{0x5a}, 196), 0xe8, 0x03, 0, 0) // length 1000 in 200 bytes
	jid := Hash(junk)
	be.objs[packName(jid)] = junk
	r, err := Open(ctx, be)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := r.ReadPackHeader(ctx, jid); !errors.Is(err, errPackHeader) {
		t.Fatalf("ReadPackHeader: %v", err)
	}
	for _, id := range ids {
		if _, err := r.LoadBlob(ctx, DataBlob, id); err != nil {
			t.Fatal(err)
		}
	}
	rb, err := r.RebuildIndex(ctx)
	if err != nil || len(rb.Left) != 1 || rb.Left[0] != jid {
		t.Fatal(rb, err)
	}
	// A storage error on the same read stays a storage error.
	if _, err := (&Repository{be: &failHeaderBackend{be}}).ReadPackHeader(ctx, ID{9}); errors.Is(err, errPackHeader) {
		t.Errorf("a storage error was taken for bad content: %v", err)
	}
}

// shortIndexBackend serves the index file cut short while the storage
// holds it whole: a download that was cut on its way here (#38).
type shortIndexBackend struct {
	*memBackend
	name string
}

func (s *shortIndexBackend) Load(ctx context.Context, name string) ([]byte, error) {
	b, err := s.memBackend.Load(ctx, name)
	if err == nil && name == s.name {
		b = b[:len(b)/2]
	}
	return b, err
}

// (5) A cut-short download of a healthy index file is not damage.
func TestCutShortDownloadOfIndexFileIsNotDamage(t *testing.T) {
	ctx := context.Background()
	be := newMemBackend()
	snapshotRepo(t, be, 6)
	var name string
	for n := range be.objects("index/") {
		name = n
	}
	short := &shortIndexBackend{memBackend: be, name: name}
	_, err := Open(ctx, short)
	if err == nil || errors.Is(err, errIndexDamaged) || !strings.Contains(err.Error(), "bytes arrived") {
		t.Fatalf("want a storage error, got %v", err)
	}
	// The same cut in a refresh, after the repository was open.
	r, err := Open(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	r.be = short
	r.indexMu.Lock()
	delete(r.indexFiles, name)
	r.indexMu.Unlock()
	if err := r.refreshIndex(ctx); err == nil || len(r.DamagedIndexFiles()) != 0 {
		t.Fatalf("refresh: %v, damaged %v", err, r.DamagedIndexFiles())
	}
	// A file that really is cut short on the storage is still damaged.
	be.objs[name] = be.objs[name][:len(be.objs[name])/2]
	r2, err := Open(ctx, be)
	if err != nil || len(r2.DamagedIndexFiles()) != 1 {
		t.Fatal(err, r2.DamagedIndexFiles())
	}
	// A file that is gone since the listing is not judged either.
	if err := r2.indexFileBad(ctx, "index/"+strings.Repeat("0", 64), []byte("x"), errIndexDamaged); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("a file that is gone: %v", err)
	}
}

// (7) A damaged index file that disappears must not clear the damaged
// state: the packs only it listed are recovered until the rebuild.
func TestDamagedFileDisappearingKeepsRecovering(t *testing.T) {
	ctx := context.Background()
	be, ids, victim, _ := damagedIndexRepo(t)
	r, err := Open(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	delete(be.objs, victim)
	l, err := r.Lock(ctx, true, 0) // refreshes the index
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	if got := r.DamagedIndexFiles(); len(got) != 1 || got[0] != victim {
		t.Fatalf("damaged after the file vanished: %v", got)
	}
	h := BlobHandle{Type: DataBlob, ID: ids[0]}
	if !r.Index().Has(h) || r.Index().Reusable(h) {
		t.Error("the packs of the vanished file must stay recovered, not reusable")
	}
	if _, err := r.Prune(ctx, PruneOptions{}); err == nil || !strings.Contains(err.Error(), "run repair first") {
		t.Fatalf("prune: %v", err)
	}
	// A new open does not know the file; this handle does, and the rebuild ends it.
	rb, err := r.RebuildIndex(ctx)
	if err != nil || rb.Packs == 0 {
		t.Fatal(rb, err)
	}
	if got := r.DamagedIndexFiles(); len(got) != 0 {
		t.Fatalf("damaged after the rebuild: %v", got)
	}
	if !r.Index().Reusable(h) {
		t.Error("blob should be reusable after the rebuild")
	}
	for _, id := range ids {
		if _, err := r.LoadBlob(ctx, DataBlob, id); err != nil {
			t.Fatal(err)
		}
	}
}

// failAfterBackend fails reads of packs and writes once armed, like a
// storage that drops out during the repair.
type failAfterBackend struct {
	*memBackend
	armed bool
}

func (f *failAfterBackend) Load(ctx context.Context, name string) ([]byte, error) {
	if f.armed && strings.HasPrefix(name, "data/") {
		return nil, fmt.Errorf("i/o timeout")
	}
	return f.memBackend.Load(ctx, name)
}

func (f *failAfterBackend) Save(ctx context.Context, name string, data []byte) error {
	if f.armed {
		return fmt.Errorf("storage full")
	}
	return f.memBackend.Save(ctx, name, data)
}

// Extra: RebuildIndex fails without touching anything (index files, packs)
// when the storage fails, while it reads the packs and while it writes.
func TestRebuildFailsWithoutTouchingAnythingOnStorageError(t *testing.T) {
	ctx := context.Background()
	mem := newMemBackend()
	snapshotRepo(t, mem, 8)
	names := truncateIndexFiles(mem)
	f := &failAfterBackend{memBackend: mem}
	r, err := Open(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	before := copyObjects(mem)
	f.armed = true
	if _, err := r.RebuildIndex(ctx); err == nil || !strings.Contains(err.Error(), "i/o timeout") {
		t.Fatalf("read error: %v", err)
	}
	requireUntouched(t, mem, before)
	// Reads work, the first write fails.
	f2 := &writeFailBackend{memBackend: mem}
	r.be = f2
	f2.armed = true
	if _, err := r.RebuildIndex(ctx); err == nil || !strings.Contains(err.Error(), "storage full") {
		t.Fatalf("write error: %v", err)
	}
	requireUntouched(t, mem, before)
	if got := r.DamagedIndexFiles(); !reflect.DeepEqual(got, names) {
		t.Errorf("damaged state: %v, want %v", got, names)
	}
}

type writeFailBackend struct {
	*memBackend
	armed bool
}

func (w *writeFailBackend) Save(ctx context.Context, name string, data []byte) error {
	if w.armed {
		return fmt.Errorf("storage full")
	}
	return w.memBackend.Save(ctx, name, data)
}
