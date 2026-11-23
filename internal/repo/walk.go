package repo

import (
	"context"
	"fmt"
	"sync"
)

// blobReaders is how many trees and block maps BlobsOf reads at once from
// an object store. Each is a small ranged read, so latency dominates: the
// lock extension after a backup spent over two hours walking the five kept
// snapshots of a job one blob after another on a slow provider (#38).
const blobReaders = 16

type walkProgressKey struct{}

// WithWalkProgress makes BlobsOf report how many trees and block maps it
// has read so far (the total is not known in advance). It is called
// once at a time, with growing counts.
func WithWalkProgress(ctx context.Context, progress func(read int)) context.Context {
	return context.WithValue(ctx, walkProgressKey{}, progress)
}

// forEach calls fn for 0..n-1 with up to workers calls at a time and
// returns the first error (the other calls then see a cancelled context).
func forEach(ctx context.Context, workers, n int, fn func(ctx context.Context, i int) error) error {
	if n == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	jobs := make(chan int)
	for w := 0; w < min(max(workers, 1), n); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if err := fn(ctx, i); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
						cancel()
					}
					mu.Unlock()
				}
			}
		}()
	}
feed:
	for i := 0; i < n; i++ {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr == nil && ctx.Err() != nil {
		firstErr = ctx.Err()
	}
	return firstErr
}

// BlobsOf returns every blob referenced by the given snapshots.
//
// Every tree and every block map is read once, however many snapshots
// share it: kept snapshots of a job share most of their trees and map
// chunks. On an object store they are read several at a time.
func (r *Repository) BlobsOf(ctx context.Context, sns []*Snapshot) (map[BlobHandle]bool, error) {
	return r.blobsOf(ctx, sns, nil)
}

// blobsOf is BlobsOf that does not read the folder lists and block maps in
// skip (blobs that are lost): they count as used, what they reference is
// not known.
func (r *Repository) blobsOf(ctx context.Context, sns []*Snapshot, skip map[BlobHandle]bool) (map[BlobHandle]bool, error) {
	used := map[BlobHandle]bool{}
	workers := 1
	if r.Concurrency() > 1 {
		workers = blobReaders
	}
	progress, _ := ctx.Value(walkProgressKey{}).(func(read int))
	// One call at a time and in order, although the readers run at once.
	var pmu sync.Mutex
	read := 0
	counted := func() {
		pmu.Lock()
		defer pmu.Unlock()
		read++
		if progress != nil {
			progress(read)
		}
	}

	// Trees, one level of all snapshots at a time. origin names the
	// snapshot a tree was first reached from, for the error message.
	origin := map[ID]*Snapshot{}
	var level []ID
	for _, sn := range sns {
		h := BlobHandle{Type: TreeBlob, ID: sn.Tree}
		if used[h] {
			continue
		}
		used[h] = true
		if skip[h] {
			continue
		}
		origin[sn.Tree] = sn
		level = append(level, sn.Tree)
	}
	// Each tree is merged into the result as soon as it is read, so only
	// the trees being read are in memory, not a whole level of them.
	var tmu sync.Mutex
	for len(level) > 0 {
		var next []ID
		err := forEach(ctx, workers, len(level), func(ctx context.Context, i int) error {
			t, err := r.LoadTree(ctx, level[i])
			tmu.Lock()
			defer tmu.Unlock()
			if err != nil {
				return fmt.Errorf("snapshot %s: %w", origin[level[i]].ID.Short(), err)
			}
			for _, n := range t.Nodes {
				for _, c := range n.Content {
					used[BlobHandle{Type: DataBlob, ID: c}] = true
				}
				if n.Subtree == nil {
					continue
				}
				h := BlobHandle{Type: TreeBlob, ID: *n.Subtree}
				if used[h] {
					continue
				}
				used[h] = true
				if skip[h] {
					continue
				}
				origin[*n.Subtree] = origin[level[i]]
				next = append(next, *n.Subtree)
			}
			counted()
			return nil
		})
		if err != nil {
			return nil, err
		}
		level = next
	}

	// Guest state, system disks and the block maps of disk images.
	type part struct {
		sn *Snapshot
		p  *PartitionImage
	}
	var parts []part
	mapIndex := map[ID]int{}
	var mapIDs []ID
	var mapOrigin []*Snapshot
	for _, sn := range sns {
		for _, g := range sn.Guests {
			ids, err := g.StateBlobs()
			if err != nil {
				return nil, fmt.Errorf("snapshot %s guest %d: %w", sn.ID.Short(), g.VMID, err)
			}
			for _, id := range ids {
				h := BlobHandle{Type: DataBlob, ID: id}
				if !r.idx.Has(h) {
					return nil, fmt.Errorf("snapshot %s guest %d: state blob %s missing", sn.ID.Short(), g.VMID, id.Short())
				}
				used[h] = true
			}
		}
		if sys := sn.System; sys != nil {
			for _, d := range sys.Disks {
				if !d.Head.IsNull() {
					used[BlobHandle{Type: DataBlob, ID: d.Head}] = true
				}
				for _, p := range d.Partitions {
					for _, id := range p.Raw {
						if !id.IsNull() {
							used[BlobHandle{Type: DataBlob, ID: id}] = true
						}
					}
				}
			}
		}
		for _, img := range sn.Images {
			used[BlobHandle{Type: DataBlob, ID: img.Head}] = true
			for i := range img.Partitions {
				p := &img.Partitions[i]
				if !p.Included {
					continue
				}
				lostMap := false
				for _, m := range p.Maps {
					lostMap = lostMap || skip[BlobHandle{Type: MapBlob, ID: m}]
				}
				if !lostMap {
					parts = append(parts, part{sn, p})
				}
				for _, m := range p.Maps {
					used[BlobHandle{Type: MapBlob, ID: m}] = true
					if skip[BlobHandle{Type: MapBlob, ID: m}] {
						continue
					}
					if _, ok := mapIndex[m]; !ok {
						mapIndex[m] = len(mapIDs)
						mapIDs = append(mapIDs, m)
						mapOrigin = append(mapOrigin, sn)
					}
				}
			}
		}
	}
	entries := make([]int, len(mapIDs))
	var mu sync.Mutex
	err := forEach(ctx, workers, len(mapIDs), func(ctx context.Context, i int) error {
		m := mapIDs[i]
		b, err := r.LoadBlob(ctx, MapBlob, m)
		if err != nil {
			return fmt.Errorf("snapshot %s: block map %s: %w", mapOrigin[i].ID.Short(), m.Short(), err)
		}
		if len(b)%len(ID{}) != 0 {
			return fmt.Errorf("snapshot %s: block map %s: bad length %d", mapOrigin[i].ID.Short(), m.Short(), len(b))
		}
		entries[i] = len(b) / len(ID{})
		mu.Lock()
		for j := 0; j < len(b); j += len(ID{}) {
			var id ID
			copy(id[:], b[j:])
			if !id.IsNull() {
				used[BlobHandle{Type: DataBlob, ID: id}] = true
			}
		}
		mu.Unlock()
		counted()
		return nil
	})
	if err != nil {
		return nil, err
	}
	// The same check as LoadBlockMap: the maps of a partition hold one
	// entry per block (a map chunk may occur more than once).
	for _, pt := range parts {
		n := 0
		for _, m := range pt.p.Maps {
			n += entries[mapIndex[m]]
		}
		if uint64(n) != pt.p.Blocks { // #nosec G115 -- a count, not negative
			return nil, fmt.Errorf("snapshot %s: block map has %d entries, partition has %d blocks", pt.sn.ID.Short(), n, pt.p.Blocks)
		}
	}
	return used, nil
}
