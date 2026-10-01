package repo

import (
	"context"
	"fmt"
	"strings"
)

// CopyOfTag marks a snapshot copied from another repository:
// "copy-of:<source snapshot id>".
const CopyOfTag = "copy-of:"

// CopyStats summarizes a copy.
type CopyStats struct {
	Snapshots   int    `json:"snapshots"`         // snapshots copied
	Skipped     int    `json:"snapshots_skipped"` // already in the destination
	Blobs       int    `json:"blobs"`             // blobs uploaded
	BytesRead   uint64 `json:"bytes_read"`        // plaintext bytes copied
	BytesStored uint64 `json:"bytes_stored"`      // bytes uploaded to the destination
}

// Copy copies snapshots from src to dst (both opened with their own keys).
// Only blobs missing in dst are transferred, so repeated copies upload just
// the new data. Snapshots already copied earlier are skipped. Copies get
// tags (replacing the source's) plus "copy-of:<id>". The caller holds
// locks on both repositories.
func Copy(ctx context.Context, src, dst *Repository, sns []*Snapshot, tags []string, progress func(done, total int)) (*CopyStats, []*Snapshot, error) {
	st := &CopyStats{}
	existing, err := dst.ListSnapshots(ctx)
	if err != nil {
		return nil, nil, err
	}
	have := map[string]bool{}
	for _, sn := range existing {
		for _, t := range sn.Tags {
			if strings.HasPrefix(t, CopyOfTag) {
				have[strings.TrimPrefix(t, CopyOfTag)] = true
			}
		}
	}
	var copied []*Snapshot
	before := dst.Stats()
	for i, sn := range sns {
		if err := ctx.Err(); err != nil {
			return st, copied, err
		}
		if have[sn.ID.String()] {
			st.Skipped++
			continue
		}
		blobs, err := src.BlobsOf(ctx, []*Snapshot{sn})
		if err != nil {
			return st, copied, fmt.Errorf("snapshot %s: %w", sn.ID.Short(), err)
		}
		for h := range blobs {
			if dst.hasBlob(h) {
				continue
			}
			data, err := src.LoadBlob(ctx, h.Type, h.ID)
			if err != nil {
				return st, copied, fmt.Errorf("snapshot %s: read blob %s: %w", sn.ID.Short(), h.ID.Short(), err)
			}
			id, added, err := dst.SaveBlob(ctx, h.Type, data)
			if err != nil {
				return st, copied, err
			}
			if id != h.ID {
				return st, copied, fmt.Errorf("blob %s changed while copying", h.ID.Short())
			}
			if added {
				st.Blobs++
				st.BytesRead += uint64(len(data))
			}
		}
		// Snapshots reference only blobs that are already stored.
		if err := dst.Flush(ctx); err != nil {
			return st, copied, err
		}
		c := *sn
		c.ID = ID{}
		c.Parent = nil
		c.Tags = append(append([]string{}, tags...), CopyOfTag+sn.ID.String())
		if _, err := dst.SaveSnapshot(ctx, &c); err != nil {
			return st, copied, err
		}
		copied = append(copied, &c)
		st.Snapshots++
		if progress != nil {
			progress(i+1, len(sns))
		}
	}
	st.BytesStored = dst.Stats().StoredBytes - before.StoredBytes
	return st, copied, nil
}

func (r *Repository) hasBlob(h BlobHandle) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, pending := r.pending[h]
	return pending || r.idx.Has(h)
}
