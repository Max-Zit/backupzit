package repo

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

// B11 (round 4): one index file cut in half made every command fail with
// "parse index/...: unexpected end of JSON input", check and repair
// included, although all packs were intact. The packs it listed are now
// recovered from their headers; RebuildIndex replaces the damaged file.
func TestDamagedIndexFileIsRecoveredFromPackHeaders(t *testing.T) {
	for _, pw := range []string{"", "pw"} {
		t.Run(fmt.Sprintf("encrypted=%v", pw != ""), func(t *testing.T) {
			ctx := context.Background()
			be := newMemBackend()
			var opts []Option
			if pw != "" {
				opts = append(opts, Password(pw))
			}
			r := newTestRepo(t, be, 64<<10, opts...)
			var ids []ID
			for i := range 40 {
				id, _, err := r.SaveBlob(ctx, DataBlob, bytes.Repeat([]byte{byte(i), byte(i >> 8), 7}, 10_000+i))
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
				if i == 19 {
					if err := r.Flush(ctx); err != nil { // two index files
						t.Fatal(err)
					}
				}
			}
			if err := r.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			idx := be.objects("index/")
			if len(idx) < 2 {
				t.Fatalf("want at least 2 index files, got %d", len(idx))
			}
			var victim string
			for n := range idx {
				victim = n
				break
			}
			be.objs[victim] = idx[victim][:len(idx[victim])/2]

			r2, err := Open(ctx, be, opts...)
			if err != nil {
				t.Fatalf("open with a damaged index file: %v", err)
			}
			if got := r2.DamagedIndexFiles(); len(got) != 1 || got[0] != victim {
				t.Fatalf("damaged index files: %v, want [%s]", got, victim)
			}
			for _, id := range ids {
				if _, err := r2.LoadBlob(ctx, DataBlob, id); err != nil {
					t.Fatalf("blob %s after recovery: %v", id.Short(), err)
				}
			}
			rb, err := r2.RebuildIndex(ctx)
			if err != nil || rb.Packs == 0 || len(rb.Left) != 0 {
				t.Fatal(rb, err)
			}
			if _, ok := be.objs[victim]; ok {
				t.Error("the damaged index file is still there")
			}
			r3, err := Open(ctx, be, opts...)
			if err != nil {
				t.Fatal(err)
			}
			if got := r3.DamagedIndexFiles(); len(got) != 0 {
				t.Fatalf("still damaged after rebuild: %v", got)
			}
			for _, id := range ids {
				if _, err := r3.LoadBlob(ctx, DataBlob, id); err != nil {
					t.Fatalf("blob %s after rebuild: %v", id.Short(), err)
				}
			}
		})
	}
}

// A backend error while reading an index file is not damage: the open
// still fails instead of working with a partial index.
func TestIndexReadErrorStillFails(t *testing.T) {
	ctx := context.Background()
	be := newMemBackend()
	r := newTestRepo(t, be, 64<<10)
	if _, _, err := r.SaveBlob(ctx, DataBlob, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	failing := &failLoadBackend{memBackend: be, prefix: "index/"}
	if _, err := Open(ctx, failing); err == nil || !strings.Contains(err.Error(), "load index/") {
		t.Fatalf("want a load error, got %v", err)
	}
}

type failLoadBackend struct {
	*memBackend
	prefix string
}

func (f *failLoadBackend) Load(ctx context.Context, name string) ([]byte, error) {
	if strings.HasPrefix(name, f.prefix) {
		return nil, fmt.Errorf("connection reset")
	}
	return f.memBackend.Load(ctx, name)
}

// failHeaderBackend fails ranged reads of packs like a storage that times out.
type failHeaderBackend struct{ *memBackend }

func (f *failHeaderBackend) LoadRange(ctx context.Context, name string, off int64, n int) ([]byte, error) {
	if strings.HasPrefix(name, "data/") {
		return nil, fmt.Errorf("i/o timeout")
	}
	return f.memBackend.LoadRange(ctx, name, off, n)
}

// damagedIndexRepo stores a few blobs in several packs and damages one of
// its index files. It returns the backend, the blobs' IDs and the file.
func damagedIndexRepo(t *testing.T, opts ...Option) (*memBackend, []ID, string, []byte) {
	t.Helper()
	ctx := context.Background()
	be := newMemBackend()
	r := newTestRepo(t, be, 64<<10, opts...)
	var ids []ID
	for i := range 20 {
		id, _, err := r.SaveBlob(ctx, DataBlob, bytes.Repeat([]byte{byte(i), 3, 9}, 30_000+i))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	var victim string
	var orig []byte
	for n, b := range be.objects("index/") {
		victim, orig = n, b
		be.objs[n] = b[:len(b)/2]
	}
	if victim == "" {
		t.Fatal("no index file")
	}
	return be, ids, victim, orig
}

// Review of B11 (1): a header read that fails on the storage must not leave
// the pack out of the index: prune would delete it. Opening fails instead.
func TestIndexRecoveryFailsOnStorageErrorInHeaderRead(t *testing.T) {
	ctx := context.Background()
	be, _, _, _ := damagedIndexRepo(t)
	before := len(be.objs)
	_, err := Open(ctx, &failHeaderBackend{be})
	if err == nil || !strings.Contains(err.Error(), "i/o timeout") {
		t.Fatalf("want the storage error, got %v", err)
	}
	if len(be.objs) != before {
		t.Errorf("objects changed: %d -> %d", before, len(be.objs))
	}
}

// Prune and DropDamagedPacks refuse while an index file is damaged.
func TestPruneRefusesWithDamagedIndex(t *testing.T) {
	ctx := context.Background()
	be, _, _, _ := damagedIndexRepo(t)
	r, err := Open(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	before := len(be.objs)
	if _, err := r.Prune(ctx, PruneOptions{}); err == nil || !strings.Contains(err.Error(), "run repair first") {
		t.Fatalf("prune: %v", err)
	}
	if _, err := r.DropDamagedPacks(ctx, []ID{{1}}); err == nil || !strings.Contains(err.Error(), "run repair first") {
		t.Fatalf("drop: %v", err)
	}
	if len(be.objs) != before {
		t.Errorf("objects changed: %d -> %d", before, len(be.objs))
	}
}

// Review of B11 (2): a pack that repair dropped comes back through a garbage
// index file with its header intact. New backups must store the data again,
// and the rebuilt index must not list the pack.
func TestDroppedPackDoesNotComeBack(t *testing.T) {
	ctx := context.Background()
	be := newMemBackend()
	r := newTestRepo(t, be, 64<<10, Password("pw"))
	data := bytes.Repeat([]byte("dropped data "), 5000)
	id, _, err := r.SaveBlob(ctx, DataBlob, data)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	var packName string
	for n := range be.objects("data/") {
		packName = n
	}
	be.objs[packName][0] ^= 0xff // the blob is damaged, header and trailer are not
	pid, err := ParseID(packName[strings.LastIndex(packName, "/")+1:])
	if err != nil {
		t.Fatal(err)
	}
	if res, err := r.DropDamagedPacks(ctx, []ID{pid}); err != nil || res.Packs != 1 {
		t.Fatal(res, err)
	}
	be.objs["index/"+strings.Repeat("ab", 32)] = []byte("garbage")

	r2, err := Open(ctx, be, Password("pw"))
	if err != nil {
		t.Fatal(err)
	}
	h := BlobHandle{Type: DataBlob, ID: id}
	if !r2.Index().Has(h) || r2.Index().Reusable(h) {
		t.Fatal("recovered blob must be readable but not reusable")
	}
	_, added, err := r2.SaveBlob(ctx, DataBlob, data)
	if err != nil || !added {
		t.Fatalf("data was deduplicated against the dropped pack: added=%v err=%v", added, err)
	}
	if err := r2.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	rb, err := r2.RebuildIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rb.Left) != 1 || rb.Left[0] != pid {
		t.Fatalf("left out: %v, want [%s]", rb.Left, pid.Short())
	}
	if _, ok := be.objs[packName]; !ok {
		t.Error("the damaged pack was deleted")
	}
	r3, err := Open(ctx, be, Password("pw"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r3.Index().Packs()[pid]; ok {
		t.Error("the dropped pack is in the rebuilt index")
	}
	if got, err := r3.LoadBlob(ctx, DataBlob, id); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("blob after rebuild: %v", err)
	}
	if !r3.Index().Reusable(h) {
		t.Error("blob should be reusable after the rebuild")
	}
}

// A pack whose header is garbage is skipped when the index is recovered,
// and the rebuild leaves it out once it is proven damaged.
func TestUnreadablePackHeaderIsSkipped(t *testing.T) {
	ctx := context.Background()
	be, ids, _, _ := damagedIndexRepo(t)
	junk := bytes.Repeat([]byte{0xff}, 100)
	jid := Hash(junk)
	be.objs[packName(jid)] = junk
	r, err := Open(ctx, be)
	if err != nil {
		t.Fatalf("open: %v", err)
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
}

// The damaged list is cleared when the file reads fine again.
func TestDamagedListClearsWhenIndexFileIsRestored(t *testing.T) {
	ctx := context.Background()
	be, ids, victim, orig := damagedIndexRepo(t)
	r, err := Open(ctx, be)
	if err != nil || len(r.DamagedIndexFiles()) != 1 {
		t.Fatal(err, r.DamagedIndexFiles())
	}
	h := BlobHandle{Type: DataBlob, ID: ids[0]}
	if r.Index().Reusable(h) {
		t.Fatal("blob of a recovered pack must not be reusable")
	}
	be.objs[victim] = orig
	l, err := r.Lock(ctx, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	if got := r.DamagedIndexFiles(); len(got) != 0 {
		t.Fatalf("still damaged: %v", got)
	}
	if !r.Index().Reusable(h) {
		t.Error("blob should come from the index again")
	}
	if _, err := r.Prune(ctx, PruneOptions{}); err != nil {
		t.Errorf("prune after the file is fine: %v", err)
	}
}
