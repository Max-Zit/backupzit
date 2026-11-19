package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/runinfo"
)

// copySelection leaves out the source backups that the copy job's own
// retention would remove right after copying them: when the source keeps
// more (e.g. 30 daily backups, the copy 7), they were copied again on
// every run and removed again by the retention (uploads, lock extension
// and pruning for nothing). The policy picks the same backups in the
// source as in the copy: those only in the copy are older than all of the
// source's. It returns the backups to copy and the number left out.
func copySelection(sns []*repo.Snapshot, p *repo.RetentionPolicy) ([]*repo.Snapshot, int) {
	if p == nil || p.Empty() {
		return sns, 0
	}
	drop := map[*repo.Snapshot]bool{}
	for _, sn := range retentionRemove(sns, *p) {
		drop[sn] = true
	}
	var keep []*repo.Snapshot
	for _, sn := range sns {
		if !drop[sn] {
			keep = append(keep, sn)
		}
	}
	return keep, len(sns) - len(keep)
}

// copyRun copies the snapshots of the source job from run.Source into
// run.Repository (backup copy job, e.g. NAS → off-site S3).
func (a *Agent) copyRun(ctx context.Context, run api.Run) api.RunResult {
	if run.Source == nil || run.SourceTag == "" {
		return failed(errors.New("copy run without source"))
	}
	src, closeSrc, err := a.openRepo(ctx, *run.Source, false)
	if err != nil {
		return failed(fmt.Errorf("open source repository: %w", err))
	}
	defer closeSrc()
	dst, closeDst, err := a.openRepo(ctx, run.Repository, true)
	if err != nil {
		return failed(fmt.Errorf("open destination repository: %w", err))
	}
	defer closeDst()
	srcLock, err := src.Lock(ctx, false, lockWait)
	if err != nil {
		return failed(err)
	}
	defer srcLock.Unlock()
	dstLock, err := dst.Lock(ctx, false, lockWait)
	if err != nil {
		return failed(err)
	}
	defer dstLock.Unlock()

	all, err := src.ListSnapshots(ctx)
	if err != nil {
		return failed(err)
	}
	var sns []*repo.Snapshot
	for _, sn := range all {
		if hasTag(sn, run.SourceTag) {
			sns = append(sns, sn)
		}
	}
	sort.Slice(sns, func(i, j int) bool { return sns[i].Time.Before(sns[j].Time) })
	source := len(sns)
	sns, older := copySelection(sns, run.Retention)
	st, copied, err := repo.Copy(ctx, src, dst, sns, []string{fmt.Sprintf("run:%d", run.ID), jobTag(run.JobID)}, nil)
	if err != nil {
		return failed(fmt.Errorf("copy: %w", err))
	}
	stats, _ := json.Marshal(st)
	res := api.RunResult{Status: api.StatusSuccess, Stats: stats}
	if len(copied) > 0 {
		res.SnapshotID = copied[len(copied)-1].ID.String()
		if last := copied[len(copied)-1]; len(last.Guests) > 0 {
			// Copies of VM backups can be restored like the originals.
			res.Details = runinfo.Guests(last)
		}
	}
	res.Message = fmt.Sprintf("Copied %s (%d already copied); %s new data uploaded", count(st.Snapshots, "backup", "backups"), st.Skipped, humanSize(st.BytesStored))
	if older > 0 {
		res.Message += fmt.Sprintf("; %s of the source not copied (older than what this job's retention keeps)", count(older, "backup", "backups"))
	}
	if source == 0 {
		res.Message = "The source job has no backups yet"
	}
	_ = srcLock.Unlock()
	_ = dstLock.Unlock()
	// withRetention extends the locks of every copy the job keeps, the ones
	// just copied included (they carry the job's tag).
	return a.withRetention(ctx, dst, run, res)
}
