package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/backupzit/backupzit/internal/api"
	"github.com/backupzit/backupzit/internal/imaging"
)

// inventoryInterval is how often the disk inventory is reported.
const inventoryInterval = 15 * time.Minute

// diskInventory returns the disk list as JSON, or nil if unavailable
// (non-Windows platforms, missing privileges).
func (a *Agent) diskInventory() json.RawMessage {
	disks, err := imaging.ListDisks()
	if err != nil {
		return nil
	}
	b, err := json.Marshal(disks)
	if err != nil {
		return nil
	}
	return b
}

func (a *Agent) imageBackup(ctx context.Context, run api.Run) api.RunResult {
	r, closeRepo, err := a.openRepo(ctx, run.Repository, true)
	if err != nil {
		return failed(fmt.Errorf("open repository: %w", err))
	}
	defer closeRepo()
	var lastLog time.Time
	sn, err := imaging.Backup(ctx, r, imaging.BackupOptions{
		Disk:       run.ImageDisk,
		Partitions: run.ImagePartitions,
		VSS:        a.VSS,
		Version:    a.version,
		Tags:       []string{fmt.Sprintf("run:%d", run.ID)},
		Progress: func(done, total uint64) {
			if time.Since(lastLog) > time.Minute {
				lastLog = time.Now()
				a.log.Info("imaging", "run", run.ID, "done", done, "total", total)
			}
		},
	})
	if err != nil {
		return failed(err)
	}
	stats, _ := json.Marshal(sn.Stats)
	details, _ := json.Marshal(sn.Images[0])
	res := api.RunResult{Status: api.StatusSuccess, SnapshotID: sn.ID.String(), Stats: stats, Details: details, Errors: sn.Stats.Errors}
	if len(sn.VSSVolumes) > 0 {
		res.Message = "Read from VSS snapshot of " + strings.Join(sn.VSSVolumes, ", ")
	}
	if len(sn.Stats.Errors) > 0 {
		res.Status = api.StatusWarning
	}
	return res
}

func (a *Agent) imageRestore(ctx context.Context, run api.Run) api.RunResult {
	r, closeRepo, err := a.openRepo(ctx, run.Repository, false)
	if err != nil {
		return failed(fmt.Errorf("open repository: %w", err))
	}
	defer closeRepo()
	sn, err := r.LoadSnapshot(ctx, run.SnapshotID)
	if err != nil {
		return failed(err)
	}
	st, err := imaging.Restore(ctx, r, sn, imaging.RestoreOptions{TargetDisk: run.TargetDisk, KeepOffline: run.KeepOffline})
	if err != nil {
		return failed(err)
	}
	stats, _ := json.Marshal(st)
	msg := fmt.Sprintf("Wrote %d partitions to disk %d", st.Partitions, run.TargetDisk)
	if run.KeepOffline {
		msg += " (left offline)"
	}
	return api.RunResult{Status: api.StatusSuccess, Stats: stats, Message: msg}
}
