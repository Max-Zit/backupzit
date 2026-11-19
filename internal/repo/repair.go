package repo

import (
	"context"
	"fmt"
	"strings"
)

// RepairResult is what DropDamagedPacks did.
type RepairResult struct {
	// Packs taken out of the index, and the blobs they held.
	Packs, Blobs int
	// PackFilesRemoved counts the damaged pack files deleted from the
	// storage (immutable storage keeps them until their lock expires).
	PackFilesRemoved int
	// Snapshots that need data of the dropped packs: they cannot be
	// restored completely. Later backups store that data again.
	Snapshots []*Snapshot
}

// DropDamagedPacks takes damaged packs (from a data check) out of the
// index, so that later backups no longer reuse their blobs but store the
// data again, and lists the snapshots that referenced them. The caller
// holds the exclusive lock. Without this, a damaged pack stays indexed and
// every new backup keeps deduplicating against its lost data.
func (r *Repository) DropDamagedPacks(ctx context.Context, packs []ID) (*RepairResult, error) {
	res := &RepairResult{}
	drop := map[ID]bool{}
	for _, p := range packs {
		drop[p] = true
	}
	if len(drop) == 0 {
		return res, nil
	}
	if err := r.Flush(ctx); err != nil {
		return nil, err
	}
	oldIndexes, err := r.be.List(ctx, "index")
	if err != nil {
		return nil, fmt.Errorf("list index: %w", err)
	}
	keep := map[ID][]PackedBlob{}
	lost := map[BlobHandle]bool{}
	r.idx.Each(func(h BlobHandle, l Location) {
		if drop[l.Pack] {
			lost[h] = true
			return
		}
		keep[l.Pack] = append(keep[l.Pack], PackedBlob{Type: h.Type, ID: h.ID, Offset: l.Offset, Length: l.Length, Raw: l.Raw})
	})
	for p := range drop {
		if _, ok := r.idx.Packs()[p]; ok {
			res.Packs++
		}
	}
	res.Blobs = len(lost)
	if res.Packs == 0 {
		return res, nil
	}

	// Snapshots that need lost blobs, found before the index changes (the
	// trees may live in a dropped pack: then the snapshot is affected too).
	sns, err := r.ListSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	for _, sn := range sns {
		used, err := r.BlobsOf(ctx, []*Snapshot{sn})
		if err != nil {
			res.Snapshots = append(res.Snapshots, sn) // unreadable: affected
			continue
		}
		for h := range used {
			if lost[h] {
				res.Snapshots = append(res.Snapshots, sn)
				break
			}
		}
	}

	// New index without the dropped packs, then the old index files go.
	list := make([]indexPack, 0, len(keep))
	for p, blobs := range keep {
		list = append(list, indexPack{ID: p, Blobs: blobs})
	}
	written := map[string]bool{}
	for start := 0; start < len(list); start += indexFlushPacks * 8 {
		end := min(start+indexFlushPacks*8, len(list))
		name, err := r.saveIndexFile(ctx, list[start:end])
		if err != nil {
			return nil, err
		}
		written[name] = true
	}
	for _, n := range oldIndexes {
		if written[n] {
			continue
		}
		if err := r.be.Remove(ctx, n); err != nil && !strings.Contains(err.Error(), "not found") {
			return nil, fmt.Errorf("remove %s: %w", n, err)
		}
	}
	for p := range drop {
		// A pack that cannot be removed now (immutable storage, a lock that
		// has not expired) is out of the index already: prune removes it later.
		if err := r.be.Remove(ctx, packName(p)); err == nil {
			res.PackFilesRemoved++
		}
	}
	r.mu.Lock()
	r.idx = newIndex()
	r.mu.Unlock()
	r.indexMu.Lock()
	r.indexFiles = map[string]bool{}
	r.indexMu.Unlock()
	if err := r.loadIndex(ctx, nil); err != nil {
		return nil, err
	}
	return res, nil
}
