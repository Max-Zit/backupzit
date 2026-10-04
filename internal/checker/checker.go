// Package checker verifies repository consistency and data integrity.
package checker

import (
	"context"
	"fmt"

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
}

// OK reports whether no errors were found.
func (r *Result) OK() bool { return len(r.Errors) == 0 }

// Options control how deep the check goes.
type Options struct {
	// ReadData downloads every pack and verifies every blob hash.
	ReadData bool
	// Progress is called after each pack is read (ReadData only).
	Progress func(done, total int)
}

// Run checks r.
func Run(ctx context.Context, r *repo.Repository, opts Options) (*Result, error) {
	res := &Result{}
	errf := func(f string, a ...any) { res.Errors = append(res.Errors, fmt.Sprintf(f, a...)) }

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
	for p := range indexed {
		if !present[p] {
			errf("pack %s is referenced by the index but missing from storage", p.Short())
		}
	}
	for _, p := range onDisk {
		if _, ok := indexed[p]; !ok {
			res.Warnings = append(res.Warnings, fmt.Sprintf("pack %s is not indexed (left over from an interrupted backup)", p.Short()))
		}
	}

	// 2. Pack headers must match the index.
	for p := range indexed {
		if !present[p] {
			continue
		}
		blobs, err := r.ReadPackHeader(ctx, p)
		if err != nil {
			errf("pack %s: read header: %v", p.Short(), err)
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
	}
	res.DataBlobs = len(seenData)

	// 4. Optionally read everything.
	if opts.ReadData {
		total := len(onDisk)
		for i, p := range onDisk {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			n, err := r.VerifyPack(ctx, p)
			if err != nil {
				errf("%v", err)
			} else {
				res.PacksRead++
				res.BlobsRead += n
			}
			if opts.Progress != nil {
				opts.Progress(i+1, total)
			}
		}
	}
	return res, nil
}
