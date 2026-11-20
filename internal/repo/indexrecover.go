package repo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// errIndexDamaged marks an index file that was read but cannot be opened or
// parsed (cut short, overwritten, wrong key). Reading it again does not help.
var errIndexDamaged = errors.New("index file damaged")

// DamagedIndexFiles returns the index files that could not be read when the
// repository was opened or its index refreshed. Their packs were found from
// the pack headers instead (see recoverIndex); RebuildIndex replaces them.
func (r *Repository) DamagedIndexFiles() []string {
	r.indexMu.Lock()
	defer r.indexMu.Unlock()
	out := append([]string(nil), r.damagedIndex...)
	sort.Strings(out)
	return out
}

func (r *Repository) noteDamagedIndex(names []string) {
	r.indexMu.Lock()
	defer r.indexMu.Unlock()
	for _, n := range names {
		dup := false
		for _, d := range r.damagedIndex {
			if d == n {
				dup = true
				break
			}
		}
		if !dup {
			r.damagedIndex = append(r.damagedIndex, n)
		}
	}
}

// recoverIndex adds the packs that no readable index file lists, from their
// own headers: the index is only an accelerator, every pack carries the
// list of its blobs. Without this, one damaged index file made the whole
// repository unusable, check and repair included, although all data was
// there (B11). A pack whose header cannot be read stays out; the data
// check reports it.
func (r *Repository) recoverIndex(ctx context.Context) error {
	packs, err := r.ListPacks(ctx)
	if err != nil {
		return fmt.Errorf("list packs to recover the damaged index: %w", err)
	}
	indexed := r.idx.Packs()
	for _, p := range packs {
		if _, ok := indexed[p]; ok {
			continue
		}
		blobs, err := r.ReadPackHeader(ctx, p)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		r.idx.addPack(p, blobs)
	}
	return nil
}

// RebuildIndex writes the index anew from what is in memory (the readable
// index files plus the packs recovered from their headers) and removes the
// old index files, the damaged ones included. The caller holds the
// exclusive lock.
func (r *Repository) RebuildIndex(ctx context.Context) (int, error) {
	if err := r.Flush(ctx); err != nil {
		return 0, err
	}
	keep := map[ID][]PackedBlob{}
	r.idx.Each(func(h BlobHandle, l Location) {
		keep[l.Pack] = append(keep[l.Pack], PackedBlob{Type: h.Type, ID: h.ID, Offset: l.Offset, Length: l.Length, Raw: l.Raw})
	})
	if err := r.rewriteIndex(ctx, keep); err != nil {
		return 0, err
	}
	r.indexMu.Lock()
	r.damagedIndex = nil
	r.indexMu.Unlock()
	return len(keep), nil
}

// rewriteIndex replaces all index files by new ones listing keep, then
// loads the index again.
func (r *Repository) rewriteIndex(ctx context.Context, keep map[ID][]PackedBlob) error {
	oldIndexes, err := r.be.List(ctx, "index")
	if err != nil {
		return fmt.Errorf("list index: %w", err)
	}
	list := make([]indexPack, 0, len(keep))
	for p, blobs := range keep {
		list = append(list, indexPack{ID: p, Blobs: blobs})
	}
	written := map[string]bool{}
	for start := 0; start < len(list); start += indexFlushPacks * 8 {
		end := min(start+indexFlushPacks*8, len(list))
		name, err := r.saveIndexFile(ctx, list[start:end])
		if err != nil {
			return err
		}
		written[name] = true
	}
	for _, n := range oldIndexes {
		if written[n] {
			continue
		}
		if err := r.be.Remove(ctx, n); err != nil && !strings.Contains(err.Error(), "not found") {
			return fmt.Errorf("remove %s: %w", n, err)
		}
	}
	r.mu.Lock()
	r.idx = newIndex()
	r.mu.Unlock()
	r.indexMu.Lock()
	r.indexFiles = map[string]bool{}
	r.indexMu.Unlock()
	return r.loadIndex(ctx, nil)
}
