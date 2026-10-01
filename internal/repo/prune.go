package repo

import (
	"context"
	"fmt"
	"strings"
)

// PruneOptions control how aggressively space is reclaimed.
type PruneOptions struct {
	// RepackUnusedRatio: packs whose unused share (by stored bytes) is at
	// least this are rewritten with only their used blobs. Packs that are
	// entirely unused are always deleted. Default 0.3.
	RepackUnusedRatio float64
	// Progress reports phases for logging.
	Progress func(msg string)
}

// PruneStats summarises a prune run.
type PruneStats struct {
	Snapshots      int    `json:"snapshots"`
	PacksTotal     int    `json:"packs_total"`
	PacksDeleted   int    `json:"packs_deleted"`
	PacksRepacked  int    `json:"packs_repacked"`
	BlobsRepacked  int    `json:"blobs_repacked"`
	BytesFreed     uint64 `json:"bytes_freed"`
	BytesRemaining uint64 `json:"bytes_remaining"`
}

// Each calls fn for every indexed blob.
func (x *Index) Each(fn func(h BlobHandle, l Location)) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	for h, l := range x.blobs {
		fn(h, l)
	}
}

// UsedBlobs returns every blob referenced by the repository's snapshots.
func (r *Repository) UsedBlobs(ctx context.Context) (map[BlobHandle]bool, int, error) {
	sns, err := r.ListSnapshots(ctx)
	if err != nil {
		return nil, 0, err
	}
	used, err := r.BlobsOf(ctx, sns)
	return used, len(sns), err
}

// BlobsOf returns every blob referenced by the given snapshots.
func (r *Repository) BlobsOf(ctx context.Context, sns []*Snapshot) (map[BlobHandle]bool, error) {
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
			return nil, fmt.Errorf("snapshot %s: %w", sn.ID.Short(), err)
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
					return nil, fmt.Errorf("snapshot %s: %w", sn.ID.Short(), err)
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

// Prune deletes data no longer referenced by any snapshot. The caller must
// hold an exclusive lock. The order of operations keeps the repository
// consistent if prune is interrupted: new packs and the new index are
// written before old index files and packs are deleted.
func (r *Repository) Prune(ctx context.Context, opts PruneOptions) (*PruneStats, error) {
	if opts.RepackUnusedRatio <= 0 {
		opts.RepackUnusedRatio = 0.3
	}
	say := func(f string, a ...any) {
		if opts.Progress != nil {
			opts.Progress(fmt.Sprintf(f, a...))
		}
	}
	st := &PruneStats{}

	used, nsn, err := r.UsedBlobs(ctx)
	if err != nil {
		return nil, err
	}
	st.Snapshots = nsn
	oldIndexes, err := r.be.List(ctx, "index")
	if err != nil {
		return nil, err
	}
	onDisk, err := r.ListPacks(ctx)
	if err != nil {
		return nil, err
	}
	st.PacksTotal = len(onDisk)

	// Group indexed blobs by pack.
	type packInfo struct {
		blobs                 []PackedBlob
		usedBytes, totalBytes uint64
	}
	packs := map[ID]*packInfo{}
	r.idx.Each(func(h BlobHandle, l Location) {
		pi := packs[l.Pack]
		if pi == nil {
			pi = &packInfo{}
			packs[l.Pack] = pi
		}
		pb := PackedBlob{Type: h.Type, ID: h.ID, Offset: l.Offset, Length: l.Length, Raw: l.Raw}
		pi.blobs = append(pi.blobs, pb)
		pi.totalBytes += uint64(l.Length)
		if used[h] {
			pi.usedBytes += uint64(l.Length)
		}
	})

	var deletePacks []ID
	var keep []indexPack
	var repack []ID
	for _, p := range onDisk {
		pi := packs[p]
		switch {
		case pi == nil || pi.usedBytes == 0:
			deletePacks = append(deletePacks, p) // unreferenced or fully unused
			if pi != nil {
				st.BytesFreed += pi.totalBytes
			}
		case float64(pi.totalBytes-pi.usedBytes)/float64(pi.totalBytes) >= opts.RepackUnusedRatio:
			repack = append(repack, p)
		default:
			keep = append(keep, indexPack{ID: p, Blobs: pi.blobs})
			st.BytesRemaining += pi.totalBytes
		}
	}
	if len(deletePacks) == 0 && len(repack) == 0 {
		say("nothing to prune")
		return st, nil
	}

	// Copy used blobs out of packs that are mostly garbage.
	say("repacking %d packs", len(repack))
	for _, p := range repack {
		pi := packs[p]
		for _, b := range pi.blobs {
			if !used[BlobHandle{Type: b.Type, ID: b.ID}] {
				continue
			}
			stored, err := r.be.LoadRange(ctx, packName(p), int64(b.Offset), int(b.Length))
			if err != nil {
				return nil, fmt.Errorf("repack %s: %w", p.Short(), err)
			}
			if _, err := r.decodeBlob(b.ID, b.Type, stored, b.Raw); err != nil {
				return nil, fmt.Errorf("repack %s: %w", p.Short(), err)
			}
			if err := r.addStoredBlob(ctx, b.Type, b.ID, stored, b.Raw); err != nil {
				return nil, err
			}
			st.BlobsRepacked++
			st.BytesRemaining += uint64(b.Length)
		}
		st.BytesFreed += pi.totalBytes - pi.usedBytes
		st.PacksRepacked++
		deletePacks = append(deletePacks, p)
	}

	// Write the new index: kept packs plus the repacked ones (Flush writes
	// those), then drop the old index files and the obsolete packs.
	if err := r.Flush(ctx); err != nil {
		return nil, err
	}
	// Index files are named by their content, so a new file can have the
	// same name as an old one; those must not be deleted below.
	written := map[string]bool{}
	for start := 0; start < len(keep); start += indexFlushPacks * 8 {
		end := min(start+indexFlushPacks*8, len(keep))
		name, err := r.saveIndexFile(ctx, keep[start:end])
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
	say("deleting %d packs", len(deletePacks))
	for _, p := range deletePacks {
		if err := r.be.Remove(ctx, packName(p)); err != nil && !strings.Contains(err.Error(), "not found") {
			return nil, fmt.Errorf("remove pack %s: %w", p.Short(), err)
		}
		st.PacksDeleted++
	}
	st.PacksDeleted -= st.PacksRepacked // reported separately

	// Reload the in-memory index from what is now on disk.
	r.mu.Lock()
	r.idx = newIndex()
	r.mu.Unlock()
	if err := r.loadIndex(ctx); err != nil {
		return nil, err
	}
	return st, nil
}

// addStoredBlob appends an already encoded blob to the current pack,
// bypassing deduplication against the index (used by repacking, where the
// blob is indexed in a pack that is about to be deleted).
func (r *Repository) addStoredBlob(ctx context.Context, t BlobType, id ID, stored []byte, raw uint32) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := BlobHandle{Type: t, ID: id}
	if _, ok := r.pending[h]; ok {
		return nil
	}
	r.pack.add(t, id, stored, int(raw))
	r.pending[h] = struct{}{}
	if r.pack.size() >= r.cfg.PackSize {
		return r.flushPackLocked(ctx)
	}
	return nil
}
