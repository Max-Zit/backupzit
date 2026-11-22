package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// lockWait bounds how long a run waits for a conflicting repository lock
// (e.g. a prune started by the same agent on another job).
const lockWait = 30 * time.Minute

func jobTag(id int64) string { return fmt.Sprintf("job:%d", id) }

func hasTag(sn *repo.Snapshot, tag string) bool {
	for _, t := range sn.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// partPrefix starts the tag of a part of a split VMware backup.
const partPrefix = "proxy:"

// partOf is the part tag of sn: the proxy of a VMware backup split over
// several proxies ("" for every other backup).
func partOf(sn *repo.Snapshot) string {
	for _, t := range sn.Tags {
		if strings.HasPrefix(t, partPrefix) {
			return t
		}
	}
	return ""
}

// ownSnapshot tells whether retention of this run deals with sn: a
// snapshot of the run's job and, for a part of a split VMware backup, of
// that part.
func ownSnapshot(sn *repo.Snapshot, run api.Run) bool {
	return hasTag(sn, jobTag(run.JobID)) && inPart(sn, run)
}

// inPart tells whether sn belongs to the run's part (a run without a part
// takes all). Copies of a split backup run per part too.
func inPart(sn *repo.Snapshot, run api.Run) bool {
	return run.Part == "" || partOf(sn) == run.Part
}

// applyRetention removes snapshots of the run's job that the policy no
// longer keeps and frees their data. It returns the removed snapshot IDs
// and a summary for the run message. Failures here do not fail the
// backup that just succeeded: they are reported as text, and the run ends
// with a warning (0.58: also when the repository could not be listed or
// locked, so a retention that never runs does not go unnoticed).
func (a *Agent) applyRetention(ctx context.Context, r *repo.Repository, run api.Run) (forgotten []string, note string, warn bool) {
	if run.Retention == nil || run.Retention.Empty() || run.JobID == 0 {
		return nil, "", false
	}
	sns, err := r.ListSnapshots(ctx)
	if err != nil {
		return nil, "retention skipped: " + err.Error(), true
	}
	var mine []*repo.Snapshot
	for _, sn := range sns {
		if ownSnapshot(sn, run) {
			mine = append(mine, sn)
		}
	}
	remove := retentionRemove(mine, *run.Retention)
	// Never remove a backup a VM currently runs from (instant recovery).
	keep := map[string]bool{}
	for _, id := range run.KeepSnapshots {
		keep[id] = true
	}
	var filtered []*repo.Snapshot
	for _, sn := range remove {
		if !keep[sn.ID.String()] {
			filtered = append(filtered, sn)
		}
	}
	remove = filtered
	if len(remove) == 0 {
		return nil, "", false
	}

	lock, err := r.Lock(ctx, true, lockWait)
	if err != nil {
		return nil, "retention skipped: " + err.Error(), true
	}
	defer lock.Unlock()
	for _, sn := range remove {
		if err := r.RemoveSnapshot(ctx, sn.ID); err != nil {
			return forgotten, "retention stopped: " + err.Error(), true
		}
		forgotten = append(forgotten, sn.ID.String())
	}
	st, err := r.Prune(ctx, repo.PruneOptions{Progress: func(m string) { a.log.Info("prune", "run", run.ID, "msg", m) }})
	if err != nil {
		return forgotten, fmt.Sprintf("removed %s; freeing space failed: %v", count(len(forgotten), "old backup", "old backups"), err), true
	}
	msg := fmt.Sprintf("removed %s, freed %s", count(len(forgotten), "old backup", "old backups"), humanSize(st.BytesFreed))
	if st.PacksSkipped > 0 {
		return forgotten, msg + fmt.Sprintf("; %s could not be read and stay as they are, the next run tries again (%s)", count(st.PacksSkipped, "pack", "packs"), st.SkipReason), true
	}
	return forgotten, msg, false
}

func humanSize(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// withRetention runs retention after a successful backup and merges its
// outcome into the result.
func (a *Agent) withRetention(ctx context.Context, r *repo.Repository, run api.Run, res api.RunResult) api.RunResult {
	if res.Status == api.StatusFailed {
		return res
	}
	if err := a.keepImmutable(ctx, r, run, res.SnapshotID); err != nil {
		a.log.Warn("extend immutability", "run", run.ID, "err", err)
		res.Status = api.StatusWarning
		res.Message = strings.TrimSpace(res.Message + " Could not extend immutability of reused data: " + err.Error())
	}
	forgotten, msg, warn := a.applyRetention(ctx, r, run)
	res.Forgotten = forgotten
	if warn && res.Status == api.StatusSuccess {
		res.Status = api.StatusWarning
	}
	if msg != "" {
		if res.Message != "" {
			res.Message += ". "
		}
		res.Message += msg
	}
	// Storage usage for the console's charts and forecast.
	res.RepoBytes = r.Index().StoredBytes()
	if u, ok := backend.UsageOf(ctx, r.Backend()); ok {
		res.StorageTotal, res.StorageFree = u.Total, u.Free
	}
	return res
}

// keepImmutable extends the object lock of all data that the job's restore
// points still need (no-op on storage without object lock): the new
// snapshot and every older snapshot of the job that the retention policy
// keeps. Extending only the new snapshot would let data used only by older
// kept snapshots (e.g. monthly ones) lose its lock after the lock period
// although the policy still keeps them. Backends extend only locks that end
// within the lock period, so data locked far enough ahead costs one check.
func (a *Agent) keepImmutable(ctx context.Context, r *repo.Repository, run api.Run, snapshotID string) error {
	if ret, ok := r.Backend().(backend.Retainer); !ok || ret.LockDays() <= 0 {
		return nil
	}
	lock, err := r.Lock(ctx, false, lockWait)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	var mine []*repo.Snapshot
	if run.JobID != 0 {
		all, err := r.ListSnapshots(ctx)
		if err != nil {
			return err
		}
		for _, sn := range all {
			if ownSnapshot(sn, run) {
				mine = append(mine, sn)
			}
		}
	}
	sns, err := keepSet(mine, run, snapshotID, func() (*repo.Snapshot, error) { return r.LoadSnapshot(ctx, snapshotID) })
	if err != nil || len(sns) == 0 {
		return err
	}
	// The console shows "Extending object locks N / M" instead of a silent
	// 100 % (a large repository on a slow provider takes a while, #38).
	kctx := repo.WithKeepProgress(ctx, func(done, total int) {
		a.progressStage(ctx, "locks", uint64(done), uint64(total)) // #nosec G115 -- counts, not negative
	})
	// Before the locks: reading the kept backups, which took longer than
	// the locks themselves on a slow provider (#38).
	kctx = repo.WithWalkProgress(kctx, func(read int) {
		a.progressStage(ctx, "locks-read", uint64(read), 0) // #nosec G115 -- a count, not negative
	})
	kctx = repo.WithKeepLog(kctx, func(msg string, kv ...any) {
		a.log.Info(msg, append([]any{"run", run.ID}, kv...)...)
	})
	n, err := r.KeepImmutable(kctx, sns...)
	a.progressStage(ctx, "", 0, 0)
	if n > 0 {
		a.log.Info("extended immutability", "run", run.ID, "snapshots", len(sns), "objects", n)
	}
	return err
}

// keepSet returns the snapshots whose data must stay locked: those of the
// job (mine) that its retention policy keeps, all of them without a policy,
// plus the snapshot just written (snapshotID, read with load when it is not
// among mine). Snapshots a VM runs from (run.KeepSnapshots) are always kept.
func keepSet(mine []*repo.Snapshot, run api.Run, snapshotID string, load func() (*repo.Snapshot, error)) ([]*repo.Snapshot, error) {
	drop := map[repo.ID]bool{}
	if run.Retention != nil && !run.Retention.Empty() {
		for _, sn := range retentionRemove(mine, *run.Retention) {
			drop[sn.ID] = true
		}
	}
	pinned := map[string]bool{snapshotID: snapshotID != ""}
	for _, id := range run.KeepSnapshots {
		pinned[id] = true
	}
	var out []*repo.Snapshot
	found := snapshotID == ""
	for _, sn := range mine {
		id := sn.ID.String()
		if id == snapshotID {
			found = true
		}
		if pinned[id] || !drop[sn.ID] {
			out = append(out, sn)
		}
	}
	if !found {
		sn, err := load()
		if err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	return out, nil
}

// retentionRemove selects the snapshots a policy removes (see
// repo.RetentionRemove), for each part of a split VMware backup on its
// own: the parts back up different VMs, so one part's backups must never
// take the place of another's (keep 7 daily = 7 days of every part). The
// backups of a proxy that left the connection stay a group of their own.
func retentionRemove(sns []*repo.Snapshot, p repo.RetentionPolicy) []*repo.Snapshot {
	groups := map[string][]*repo.Snapshot{}
	var keys []string
	for _, sn := range sns {
		k := partOf(sn)
		if _, seen := groups[k]; !seen {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], sn)
	}
	if len(keys) <= 1 {
		return repo.RetentionRemove(sns, p)
	}
	var out []*repo.Snapshot
	for _, k := range keys {
		out = append(out, repo.RetentionRemove(groups[k], p)...)
	}
	return out
}
