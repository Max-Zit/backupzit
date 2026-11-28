package repo

import (
	"context"
	"fmt"
	"strings"
)

// RepairResult is what DropDamagedPacks did.
type RepairResult struct {
	// Packs taken out of the index, and the blobs they held. Their files
	// stay until the next cleanup (prune) removes them as unreferenced.
	Packs, Blobs int
	// Snapshots that need data of the dropped packs: they cannot be
	// restored completely. Later backups store that data again.
	Snapshots []*Snapshot
	// Unchecked snapshots could not be read completely for other reasons
	// (data this agent cannot read now): kept, not counted as affected.
	Unchecked []*Snapshot
}

// Merge adds the result of another step (the packs RebuildIndex left out and
// the ones DropDamagedPacks took out): each backup is listed once.
func (rr *RepairResult) Merge(o *RepairResult) {
	rr.Packs += o.Packs
	rr.Blobs += o.Blobs
	seen := map[ID]bool{}
	for _, sn := range rr.Snapshots {
		seen[sn.ID] = true
	}
	for _, sn := range o.Snapshots {
		if !seen[sn.ID] {
			seen[sn.ID] = true
			rr.Snapshots = append(rr.Snapshots, sn)
		}
	}
	// A backup one step proved affected is no longer unchecked.
	un := rr.Unchecked
	rr.Unchecked = nil
	for _, sn := range append(un, o.Unchecked...) {
		dup := false
		for _, x := range rr.Unchecked {
			dup = dup || x.ID == sn.ID
		}
		if !seen[sn.ID] && !dup {
			rr.Unchecked = append(rr.Unchecked, sn)
		}
	}
}

// needsLost tells whether sn references a blob in lost. Blobs in lost are
// never loaded: a lost folder list or block map proves the snapshot
// affected. Any other load error is returned (affected unknown). trees
// remembers the verdict of folder lists already walked.
func (r *Repository) needsLost(ctx context.Context, sn *Snapshot, lost map[BlobHandle]bool, trees map[ID]bool) (bool, error) {
	var walk func(id ID) (bool, error)
	walk = func(id ID) (bool, error) {
		if v, ok := trees[id]; ok {
			return v, nil
		}
		if lost[BlobHandle{Type: TreeBlob, ID: id}] {
			trees[id] = true
			return true, nil
		}
		t, err := r.LoadTree(ctx, id)
		if err != nil {
			return false, err
		}
		trees[id] = false // a cycle cannot make it affected twice
		for _, n := range t.Nodes {
			for _, c := range n.Content {
				if lost[BlobHandle{Type: DataBlob, ID: c}] {
					trees[id] = true
					return true, nil
				}
			}
			if n.Subtree != nil {
				v, err := walk(*n.Subtree)
				if err != nil {
					delete(trees, id)
					return false, err
				}
				if v {
					trees[id] = true
					return true, nil
				}
			}
		}
		return false, nil
	}
	if v, err := walk(sn.Tree); err != nil || v {
		return v, err
	}
	data := func(id ID) bool { return !id.IsNull() && lost[BlobHandle{Type: DataBlob, ID: id}] }
	for _, g := range sn.Guests {
		ids, err := g.StateBlobs()
		if err != nil {
			return false, err
		}
		for _, id := range ids {
			if data(id) || !r.idx.Has(BlobHandle{Type: DataBlob, ID: id}) {
				return true, nil
			}
		}
	}
	if sys := sn.System; sys != nil {
		for _, d := range sys.Disks {
			if data(d.Head) {
				return true, nil
			}
			for _, p := range d.Partitions {
				for _, id := range p.Raw {
					if data(id) {
						return true, nil
					}
				}
			}
		}
	}
	for _, img := range sn.Images {
		if data(img.Head) {
			return true, nil
		}
		for i := range img.Partitions {
			p := &img.Partitions[i]
			if !p.Included {
				continue
			}
			for _, m := range p.Maps {
				if lost[BlobHandle{Type: MapBlob, ID: m}] {
					return true, nil
				}
			}
			ids, err := r.LoadBlockMap(ctx, p)
			if err != nil {
				return false, err
			}
			for _, id := range ids {
				if data(id) {
					return true, nil
				}
			}
		}
	}
	return false, nil
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
	// The new index would list the recovered, unverified packs as well.
	if err := r.errDamagedIndexFiles(); err != nil {
		return nil, err
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
	trees := map[ID]bool{}
	for _, sn := range sns {
		affected, err := r.needsLost(ctx, sn, lost, trees)
		switch {
		case err != nil:
			// A folder list or block map that is not lost could not be read
			// (a pack this agent cannot read now, #38): nothing proves the
			// backup needs lost data, so it is not counted as affected (and
			// never removed for it).
			res.Unchecked = append(res.Unchecked, sn)
		case affected:
			res.Snapshots = append(res.Snapshots, sn)
		}
	}

	// New index without the dropped packs, then the old index files go.
	list := make([]indexPack, 0, len(keep))
	for p, blobs := range keep {
		list = append(list, indexPack{ID: p, Blobs: blobs})
	}
	written := map[string]bool{}
	names, err := r.saveIndexFiles(ctx, list)
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		written[n] = true
	}
	for _, n := range oldIndexes {
		if written[n] {
			continue
		}
		if err := r.be.Remove(ctx, n); err != nil && !strings.Contains(err.Error(), "not found") {
			return nil, fmt.Errorf("remove %s: %w", n, err)
		}
	}
	// The pack files themselves stay: prune removes them later as
	// unreferenced, and until then a wrong verdict can still be undone by
	// restoring the old index files from a copy of the storage.
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
