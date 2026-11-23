package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
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
	// One snapshot that cannot be read (after the backend's retries) must not
	// cost all the others their lock extension: then the snapshots are read
	// one by one, everything readable is extended and the rest is reported.
	logf, _ := ctx.Value(keepLogKey{}).(func(msg string, kv ...any))
	if logf == nil {
		logf = func(string, ...any) {}
	}
	start := time.Now()
	logf("lock extension: reading the kept backups", "snapshots", len(sns))
	var problems []error
	used, err := r.BlobsOf(ctx, sns)
	if err != nil {
		if ctx.Err() != nil {
			return 0, err
		}
		used = map[BlobHandle]bool{}
		bad := 0
		for _, sn := range sns {
			u, err := r.BlobsOf(ctx, []*Snapshot{sn})
			if err != nil {
				if ctx.Err() != nil {
					return 0, err
				}
				if bad == 0 {
					problems = append(problems, err)
				}
				bad++
				continue
			}
			for h := range u {
				used[h] = true
			}
		}
		if bad > 0 {
			problems[0] = fmt.Errorf("%d of %d snapshots could not be read, so their data keeps its current lock: %w", bad, len(sns), problems[0])
		}
	}
	// The small files first: without config, keys, index and snapshots the
	// packs cannot be read at all, so their locks must not wait behind
	// 10,000 packs (or be left out when the step stops early).
	names := []string{"config"}
	for _, dir := range []string{"keys", "index"} {
		files, err := r.be.List(ctx, dir)
		if err != nil {
			return 0, errors.Join(append(problems, err)...)
		}
		names = append(names, files...)
	}
	for _, sn := range sns {
		names = append(names, "snapshots/"+sn.ID.String())
	}
	packs := map[ID]bool{}
	missing := 0
	for h := range used {
		l, ok := r.idx.Lookup(h)
		if !ok {
			if missing == 0 {
				problems = append(problems, fmt.Errorf("blob %s not in index", h.ID.Short()))
			}
			missing++
			continue
		}
		packs[l.Pack] = true
	}
	for p := range packs {
		names = append(names, packName(p))
	}
	logf("lock extension: kept backups read", "blobs", len(used), "objects", len(names), "took", time.Since(start).Round(time.Second).String())
	start = time.Now()
	defer func() {
		logf("lock extension: locks checked", "objects", len(names), "took", time.Since(start).Round(time.Second).String())
	}()
	// In batches, so that the run shows how far it is (a repository with
	// 10,000 packs on a slow provider takes a while, #38).
	progress, _ := ctx.Value(keepProgressKey{}).(func(done, total int))
	n := 0
	for start := 0; start < len(names); start += keepBatch {
		end := min(start+keepBatch, len(names))
		k, err := ret.KeepLocked(ctx, names[start:end])
		n += k
		if progress != nil {
			progress(end, len(names))
		}
		if err == nil {
			continue
		}
		problems = append(problems, err)
		// A few objects that fail keep their current lock and the others
		// are extended; only a storage that keeps failing (the backend
		// says so) stops the step. The next run tries again.
		if ctx.Err() != nil || errors.Is(err, backend.ErrKeepStopped) {
			break
		}
	}
	return n, errors.Join(problems...)
}

// keepBatch is how many objects KeepImmutable hands to the storage at once.
const keepBatch = 500

type keepProgressKey struct{}

type keepLogKey struct{}

// WithKeepLog makes KeepImmutable log its phases (reading the kept
// backups, checking the locks) with their duration, so a slow step shows
// in the agent log (#38).
func WithKeepLog(ctx context.Context, logf func(msg string, kv ...any)) context.Context {
	return context.WithValue(ctx, keepLogKey{}, logf)
}

// WithKeepProgress makes KeepImmutable report how many of the objects it
// has handled so far.
func WithKeepProgress(ctx context.Context, progress func(done, total int)) context.Context {
	return context.WithValue(ctx, keepProgressKey{}, progress)
}
