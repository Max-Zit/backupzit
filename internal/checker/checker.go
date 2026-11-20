// Package checker verifies repository consistency and data integrity.
package checker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/max-zit/backupzit/internal/repo"
)

// Result of a check run.
type Result struct {
	Snapshots int
	Trees     int
	DataBlobs int
	Packs     int
	PacksRead int
	BlobsRead int
	Errors    []string
	Warnings  []string
	// Damaged lists the indexed packs proven damaged on the storage
	// (missing, or a read of all their bytes does not verify): repair takes
	// them out of the index, so that the next backups store their data
	// again.
	Damaged []repo.ID
	// Unreadable lists packs this machine could not read even after a few
	// tries while the storage holds them (a download cut short, timeouts):
	// it may be the network path, so repair leaves them alone (#38).
	Unreadable []repo.ID
}

// OK reports whether no errors were found.
func (r *Result) OK() bool { return len(r.Errors) == 0 }

// Options control how deep the check goes.
type Options struct {
	// ReadData downloads every pack and verifies every blob hash.
	ReadData bool
	// Progress is called after each pack is read (ReadData only).
	Progress func(done, total int)
	// RetryPause is the wait before reading a pack that failed once more
	// (default 10 s, doubled for the third try): a dropped connection or a
	// download cut short is not a damaged pack.
	RetryPause time.Duration
}

// DefaultRetryPause is RetryPause when Options leave it 0 (tests shorten it).
var DefaultRetryPause = 10 * time.Second

// maxFailStreak packs in a row that cannot be read (after their retries)
// mean the storage or the way to it is down: the check stops with an error
// instead of trying every pack for days (and holding the lock of a repair).
const maxFailStreak = 20

// errStorageDown is the error of a check stopped by maxFailStreak.
var errStorageDown = errors.New("the storage stopped answering")

// streak counts reads that failed in a row; any success resets it.
type streak struct {
	mu   sync.Mutex
	n    int
	last error
}

// add records one read; it reports true once maxFailStreak is reached.
func (s *streak) add(err error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil || errors.Is(err, repo.ErrPackDamaged) {
		s.n = 0 // the storage answered
		return false
	}
	s.n++
	s.last = err
	return s.n >= maxFailStreak
}

func (s *streak) err() error {
	return fmt.Errorf("%w: %d data packs in a row could not be read (last: %v); nothing was judged, check the storage and the network path and run the check again", errStorageDown, s.n, s.last)
}

// tries runs read up to three times, pausing in between, while it fails.
// A proven verdict (repo.ErrPackDamaged) is read again too: over plain
// HTTP a corrupt download can be a one-off.
func tries(ctx context.Context, pause time.Duration, read func() error) error {
	if pause <= 0 {
		pause = DefaultRetryPause
	}
	var err error
	for i := 0; i < 3; i++ {
		if err = read(); err == nil || ctx.Err() != nil {
			return err
		}
		if i < 2 {
			select {
			case <-ctx.Done():
				return err
			case <-time.After(pause << i):
			}
		}
	}
	return err
}

// Run checks r.
func Run(ctx context.Context, r *repo.Repository, opts Options) (*Result, error) {
	res := &Result{}
	errf := func(f string, a ...any) { res.Errors = append(res.Errors, fmt.Sprintf(f, a...)) }
	bad := map[repo.ID]bool{}
	damaged := func(p repo.ID) {
		if !bad[p] {
			bad[p] = true
			res.Damaged = append(res.Damaged, p)
		}
	}

	// 0. Index files that could not be read: their packs were taken from
	// the pack headers when the repository was opened; repair rewrites the
	// index.
	for _, n := range r.DamagedIndexFiles() {
		errf("index file %s is damaged; its packs were read from their own headers instead (repair writes a new index)", n)
	}

	// 1. Packs on storage vs. packs in the index.
	onDisk, err := r.ListPacks(ctx)
	if err != nil {
		return nil, fmt.Errorf("list packs: %w", err)
	}
	res.Packs = len(onDisk)
	present := map[repo.ID]bool{}
	for _, p := range onDisk {
		present[p] = true
	}
	indexed := r.Index().Packs()
	// A pack absent from the listing is asked for directly before it counts
	// as missing: SFTP and SMB listings skip folders they cannot read
	// without an error, and repair would drop healthy data (H-N6).
	if len(onDisk) == 0 && len(indexed) > 0 {
		return res, fmt.Errorf("the storage lists no data packs at all although the index names %d: a folder may be missing or not mounted; nothing was judged", len(indexed))
	}
	var gone []repo.ID
	var ls streak
	for p := range indexed {
		if present[p] {
			continue
		}
		var stored bool
		err := tries(ctx, opts.RetryPause, func() (err error) {
			stored, err = r.PackStored(ctx, p)
			return err
		})
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if ls.add(err) {
			return res, ls.err()
		}
		switch {
		case err != nil:
			errf("pack %s is not in the storage listing and could not be looked up after 3 tries (the storage may be fine): %v", p.Short(), err)
			res.Unreadable = append(res.Unreadable, p)
		case stored:
			res.Warnings = append(res.Warnings, fmt.Sprintf("pack %s is on the storage but was missing from its listing (the storage listed its folders incompletely)", p.Short()))
			present[p] = true
			onDisk = append(onDisk, p)
		default:
			gone = append(gone, p)
		}
	}
	// Most of the data gone at once is more likely a folder that is not
	// mounted or the wrong location than lost data: no verdict.
	if len(gone) > 10 && len(gone)*2 > len(indexed) {
		return res, fmt.Errorf("%d of %d data packs are missing on the storage: more likely a folder that is not mounted or a wrong location than lost data; nothing was judged", len(gone), len(indexed))
	}
	for _, p := range gone {
		errf("pack %s is referenced by the index but missing from storage", p.Short())
		damaged(p)
	}
	for _, p := range onDisk {
		if _, ok := indexed[p]; !ok {
			res.Warnings = append(res.Warnings, fmt.Sprintf("pack %s is not indexed (left over from an interrupted backup)", p.Short()))
		}
	}

	// 2. Pack headers must match the index.
	var hs streak
	for p := range indexed {
		if !present[p] {
			continue
		}
		var blobs []repo.PackedBlob
		var err error
		if opts.ReadData {
			// The whole pack is read (with retries) below anyway.
			blobs, err = r.ReadPackHeader(ctx, p)
		} else {
			err = tries(ctx, opts.RetryPause, func() (err error) {
				blobs, err = r.ReadPackHeader(ctx, p)
				return err
			})
		}
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if hs.add(err) {
			return res, hs.err()
		}
		if err != nil {
			// With ReadData the whole pack is read below, which tells a
			// damaged pack from one that only does not arrive here.
			if !opts.ReadData {
				errf("pack %s: read header: %v", p.Short(), err)
			}
			continue
		}
		for _, b := range blobs {
			loc, ok := r.Index().Lookup(repo.BlobHandle{Type: b.Type, ID: b.ID})
			if ok && loc.Pack == p && (loc.Offset != b.Offset || loc.Length != b.Length) {
				errf("pack %s: blob %s location differs between header and index", p.Short(), b.ID.Short())
			}
		}
	}

	// 3. Every snapshot's trees and data blobs must be present.
	sns, err := r.ListSnapshots(ctx)
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	res.Snapshots = len(sns)
	seenTree := map[repo.ID]bool{}
	seenData := map[repo.ID]bool{}
	var walk func(id repo.ID, path string)
	walk = func(id repo.ID, path string) {
		if seenTree[id] {
			return
		}
		seenTree[id] = true
		t, err := r.LoadTree(ctx, id)
		if err != nil {
			errf("tree %s (%s): %v", id.Short(), path, err)
			return
		}
		res.Trees++
		for _, n := range t.Nodes {
			p := path + "/" + n.Name
			switch n.Type {
			case repo.NodeDir:
				if n.Subtree == nil {
					errf("%s: directory without subtree", p)
					continue
				}
				walk(*n.Subtree, p)
			case repo.NodeFile:
				for _, c := range n.Content {
					if seenData[c] {
						continue
					}
					seenData[c] = true
					if !r.Index().Has(repo.BlobHandle{Type: repo.DataBlob, ID: c}) {
						errf("%s: data blob %s missing", p, c.Short())
					}
				}
			}
		}
	}
	for _, sn := range sns {
		walk(sn.Tree, "snapshot "+sn.ID.Short())
		for _, g := range sn.Guests {
			// Container volumes stored as files are in the tree just walked;
			// a guest must not point to a directory that is not there.
			for _, d := range g.Disks {
				if !d.Files() {
					continue
				}
				if n, err := r.FindNode(ctx, sn.Tree, d.Path); err != nil {
					errf("snapshot %s guest %d volume %s: %v", sn.ID.Short(), g.VMID, d.Key, err)
				} else if n.Type != repo.NodeDir || n.Subtree == nil {
					errf("snapshot %s guest %d volume %s: %s is not a directory", sn.ID.Short(), g.VMID, d.Key, d.Path)
				}
			}
			ids, err := g.StateBlobs()
			if err != nil {
				errf("snapshot %s guest %d: %v", sn.ID.Short(), g.VMID, err)
				continue
			}
			for _, id := range ids {
				if seenData[id] {
					continue
				}
				seenData[id] = true
				if !r.Index().Has(repo.BlobHandle{Type: repo.DataBlob, ID: id}) {
					errf("snapshot %s guest %d: state blob %s missing", sn.ID.Short(), g.VMID, id.Short())
				}
			}
		}
		for _, img := range sn.Images {
			where := fmt.Sprintf("snapshot %s disk %d", sn.ID.Short(), img.Number)
			if !seenData[img.Head] {
				seenData[img.Head] = true
				if !r.Index().Has(repo.BlobHandle{Type: repo.DataBlob, ID: img.Head}) {
					errf("%s: disk head blob %s missing", where, img.Head.Short())
				}
			}
			for i := range img.Partitions {
				p := &img.Partitions[i]
				if !p.Included {
					continue
				}
				ids, err := r.LoadBlockMap(ctx, p)
				if err != nil {
					errf("%s partition %d: %v", where, p.Number, err)
					continue
				}
				for _, id := range ids {
					if id.IsNull() || seenData[id] {
						continue
					}
					seenData[id] = true
					if !r.Index().Has(repo.BlobHandle{Type: repo.DataBlob, ID: id}) {
						errf("%s partition %d: data blob %s missing", where, p.Number, id.Short())
					}
				}
			}
		}
		// Linux system backups keep each disk's start and the raw parts of
		// partitions as blobs of their own (prune counts them as used too).
		if sys := sn.System; sys != nil {
			for di, d := range sys.Disks {
				need := []repo.ID{d.Head}
				for _, p := range d.Partitions {
					need = append(need, p.Raw...)
				}
				for _, id := range need {
					if id.IsNull() || seenData[id] {
						continue
					}
					seenData[id] = true
					if !r.Index().Has(repo.BlobHandle{Type: repo.DataBlob, ID: id}) {
						errf("snapshot %s system disk %d: data blob %s missing", sn.ID.Short(), di, id.Short())
					}
				}
			}
		}
	}
	res.DataBlobs = len(seenData)

	// 4. Optionally read everything: several packs at once on storage that
	// serves parallel requests well (S3, Azure, hardened), one by one on
	// local disks and SFTP/SMB. On a distant S3 one stream read about 3 MB/s
	// (855 GB in about three days, #38).
	if opts.ReadData {
		type verified struct {
			blobs int
			err   error
		}
		out := make([]verified, len(onDisk))
		jobs := make(chan int)
		var (
			wg   sync.WaitGroup
			mu   sync.Mutex
			done int
			ds   streak
		)
		rctx, stop := context.WithCancel(ctx)
		defer stop()
		down := false
		for w := 0; w < min(max(r.Concurrency(), 1), max(len(onDisk), 1)); w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range jobs {
					var n int
					err := tries(rctx, opts.RetryPause, func() (err error) {
						n, err = r.VerifyPack(rctx, onDisk[i])
						return err
					})
					out[i] = verified{n, err}
					if rctx.Err() == nil && ds.add(err) {
						mu.Lock()
						down = true
						mu.Unlock()
						stop()
					}
					if opts.Progress != nil {
						mu.Lock()
						done++
						done := done
						opts.Progress(done, len(onDisk))
						mu.Unlock()
					}
				}
			}()
		}
		for i := range onDisk {
			if rctx.Err() != nil {
				break
			}
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if down {
			return res, ds.err()
		}
		for i, v := range out {
			if v.err != nil {
				_, inIndex := indexed[onDisk[i]]
				switch {
				case !inIndex:
					// A pack no backup uses (taken out by a repair, left over
					// from an interrupted backup): the next cleanup removes it.
					res.Warnings = append(res.Warnings, fmt.Sprintf("%v (not in the index: no backup uses it, the next cleanup removes it)", v.err))
				case errors.Is(v.err, repo.ErrPackDamaged):
					errf("%v", v.err)
					damaged(onDisk[i])
				default:
					// Only this machine could not read it: not a verdict on
					// the pack (repair leaves it alone).
					errf("pack %s could not be read from this agent after 3 tries (the storage may be fine; check the network path: proxy, firewall, IPS, NAT): %v", onDisk[i].Short(), v.err)
					res.Unreadable = append(res.Unreadable, onDisk[i])
				}
				continue
			}
			res.PacksRead++
			res.BlobsRead += v.blobs
		}
	}
	return res, nil
}
