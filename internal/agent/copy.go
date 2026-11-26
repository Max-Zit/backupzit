package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

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
		if hasTag(sn, run.SourceTag) && inPart(sn, run) {
			sns = append(sns, sn)
		}
	}
	sort.Slice(sns, func(i, j int) bool { return sns[i].Time.Before(sns[j].Time) })
	source := len(sns)
	sns, older := copySelection(sns, run.Retention)
	// Each copy keeps the part tag of its source (split VMware backups), so
	// the copy job's retention also keeps every part on its own.
	base := []string{fmt.Sprintf("run:%d", run.ID), jobTag(run.JobID)}
	byPart := map[string][]*repo.Snapshot{}
	var parts []string
	for _, sn := range sns {
		k := partOf(sn)
		if _, seen := byPart[k]; !seen {
			parts = append(parts, k)
		}
		byPart[k] = append(byPart[k], sn)
	}
	st := &repo.CopyStats{}
	var copied []*repo.Snapshot
	// Progress for the console and lines in the agent log (every few
	// minutes and after each backup copied, #75). The parts of a split
	// VMware job are copied one after another; their bytes add up.
	var doneBase, totalBase uint64
	lastLog, lastSnap := time.Now(), 0
	progress := func(p repo.CopyProgress) {
		stage := "copy-plan"
		if p.Phase == repo.CopyTransfer {
			stage = "copy"
		}
		a.progressCopy(ctx, stage, uint64(p.Snapshot), uint64(p.Snapshots), p.Time, p.First) // #nosec G115 -- counts, not negative
		a.progress(ctx, doneBase+p.Done, totalBase+p.Total, 0)
		if p.Phase != repo.CopyTransfer {
			return
		}
		if p.Snapshot != lastSnap && lastSnap > 0 || time.Since(lastLog) >= 5*time.Minute {
			a.log.Info("copy progress", "run", run.ID, "backup", p.Snapshot, "of", p.Snapshots,
				"copied", humanSize(doneBase+p.Done), "total", humanSize(totalBase+p.Total))
			lastLog = time.Now()
		}
		lastSnap = p.Snapshot
	}
	for _, k := range parts {
		tags := base
		if k != "" {
			tags = append(append([]string{}, base...), k)
		}
		s, c, err := repo.Copy(ctx, src, dst, byPart[k], tags, &repo.CopyOptions{JobTag: jobTag(run.JobID), Progress: progress})
		if s != nil {
			st.Add(s)
			doneBase, totalBase = doneBase+s.Packed, totalBase+s.Packed
		}
		copied = append(copied, c...)
		if err != nil {
			return failed(fmt.Errorf("copy: %w", err))
		}
	}
	a.progressStage(ctx, "", 0, 0)
	a.log.Info("copy finished", "run", run.ID, "backups", st.Snapshots, "copied", humanSize(st.BytesRead),
		"uploaded", humanSize(st.BytesStored), "read_mb_s", mbPerSecond(st.Packed, st.ReadSeconds),
		"write_mb_s", mbPerSecond(st.BytesStored, st.WriteSeconds))
	sort.Slice(copied, func(i, j int) bool { return copied[i].Time.Before(copied[j].Time) })
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

// mbPerSecond is a rate for the log, 0 when the time is not known.
func mbPerSecond(bytes uint64, seconds float64) float64 {
	if seconds <= 0 {
		return 0
	}
	return math.Round(float64(bytes)/seconds/1e5) / 10
}
