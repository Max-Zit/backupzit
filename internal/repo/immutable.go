package repo

import (
	"context"
	"fmt"

	"github.com/backupzit/backupzit/internal/backend"
)

// KeepImmutable extends the immutability of everything the given snapshots
// need (their packs, the snapshot files, the index, keys and config) on
// storage with object lock. Data is deduplicated, so a new snapshot often
// reuses packs written long ago whose lock would otherwise end before the
// lock period of the new snapshot. Call it after saving a snapshot; it is a
// no-op on storage without object lock.
func (r *Repository) KeepImmutable(ctx context.Context, sns ...*Snapshot) (int, error) {
	ret, ok := r.be.(backend.Retainer)
	if !ok || ret.LockDays() <= 0 || len(sns) == 0 {
		return 0, nil
	}
	used, err := r.BlobsOf(ctx, sns)
	if err != nil {
		return 0, err
	}
	names := []string{"config"}
	packs := map[ID]bool{}
	for h := range used {
		l, ok := r.idx.Lookup(h)
		if !ok {
			return 0, fmt.Errorf("blob %s not in index", h.ID.Short())
		}
		packs[l.Pack] = true
	}
	for p := range packs {
		names = append(names, packName(p))
	}
	for _, sn := range sns {
		names = append(names, "snapshots/"+sn.ID.String())
	}
	for _, dir := range []string{"keys", "index"} {
		files, err := r.be.List(ctx, dir)
		if err != nil {
			return 0, err
		}
		names = append(names, files...)
	}
	return ret.KeepLocked(ctx, names)
}
