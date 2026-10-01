package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/backupzit/backupzit/internal/api"
	"github.com/backupzit/backupzit/internal/repo"
)

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
	st, copied, err := repo.Copy(ctx, src, dst, sns, []string{fmt.Sprintf("run:%d", run.ID), jobTag(run.JobID)}, nil)
	if err != nil {
		return failed(fmt.Errorf("copy: %w", err))
	}
	stats, _ := json.Marshal(st)
	res := api.RunResult{Status: api.StatusSuccess, Stats: stats}
	if len(copied) > 0 {
		res.SnapshotID = copied[len(copied)-1].ID.String()
	}
	res.Message = fmt.Sprintf("Copied %d backups (%d already copied); %s new data uploaded", st.Snapshots, st.Skipped, humanSize(st.BytesStored))
	if len(sns) == 0 {
		res.Message = "The source job has no backups yet"
	}
	srcLock.Unlock()
	dstLock.Unlock()
	for _, sn := range copied {
		if err := a.keepImmutable(ctx, dst, sn.ID.String()); err != nil {
			res.Status = api.StatusWarning
			res.Message += ". Could not extend immutability: " + err.Error()
			break
		}
	}
	return a.withRetention(ctx, dst, run, res)
}

