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
			n, err := r2.RebuildIndex(ctx)
			if err != nil || n == 0 {
				t.Fatal(n, err)
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
