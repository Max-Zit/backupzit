package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/backupzit/backupzit/internal/api"
	"github.com/backupzit/backupzit/internal/repo"
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

// applyRetention removes snapshots of the run's job that the policy no
// longer keeps and frees their data. It returns the removed snapshot IDs
// and a summary for the run message. Failures here do not fail the
// backup that just succeeded; they are reported as text.
func (a *Agent) applyRetention(ctx context.Context, r *repo.Repository, run api.Run) ([]string, string) {
	if run.Retention == nil || run.Retention.Empty() || run.JobID == 0 {
		return nil, ""
	}
	sns, err := r.ListSnapshots(ctx)
	if err != nil {
		return nil, "retention skipped: " + err.Error()
	}
	var mine []*repo.Snapshot
	for _, sn := range sns {
		if hasTag(sn, jobTag(run.JobID)) {
			mine = append(mine, sn)
		}
	}
	_, remove := repo.ApplyPolicy(mine, *run.Retention, time.Local)
	if len(remove) == 0 {
		return nil, ""
	}

	lock, err := r.Lock(ctx, true, lockWait)
	if err != nil {
		return nil, "retention skipped: " + err.Error()
	}
	defer lock.Unlock()
	var forgotten []string
	for _, sn := range remove {
		if err := r.RemoveSnapshot(ctx, sn.ID); err != nil {
			return forgotten, "retention stopped: " + err.Error()
		}
		forgotten = append(forgotten, sn.ID.String())
	}
	st, err := r.Prune(ctx, repo.PruneOptions{Progress: func(m string) { a.log.Info("prune", "run", run.ID, "msg", m) }})
	if err != nil {
		return forgotten, fmt.Sprintf("removed %d old backups; freeing space failed: %v", len(forgotten), err)
	}
	return forgotten, fmt.Sprintf("removed %d old backups, freed %s", len(forgotten), humanSize(st.BytesFreed))
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
	if err := a.keepImmutable(ctx, r, res.SnapshotID); err != nil {
		a.log.Warn("extend immutability", "run", run.ID, "err", err)
		res.Status = api.StatusWarning
		res.Message = strings.TrimSpace(res.Message + " Could not extend immutability of reused data: " + err.Error())
	}
	forgotten, msg := a.applyRetention(ctx, r, run)
	res.Forgotten = forgotten
	if msg != "" {
		if res.Message != "" {
			res.Message += ". "
		}
		res.Message += msg
	}
	return res
}

// keepImmutable extends the object lock of all data the new snapshot
// reuses (no-op on storage without object lock).
func (a *Agent) keepImmutable(ctx context.Context, r *repo.Repository, snapshotID string) error {
	if snapshotID == "" {
		return nil
	}
	lock, err := r.Lock(ctx, false, lockWait)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	sn, err := r.LoadSnapshot(ctx, snapshotID)
	if err != nil {
		return err
	}
	n, err := r.KeepImmutable(ctx, sn)
	if n > 0 {
		a.log.Info("extended immutability", "snapshot", sn.ID.Short(), "objects", n)
	}
	return err
}
