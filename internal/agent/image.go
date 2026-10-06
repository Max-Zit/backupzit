package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/imaging"
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
	lock, err := r.Lock(ctx, false, lockWait)
	if err != nil {
		return failed(err)
	}
	defer lock.Unlock()
	var lastLog time.Time
	sn, err := imaging.Backup(ctx, r, imaging.BackupOptions{
		Disk:       run.ImageDisk,
		Partitions: run.ImagePartitions,
		VSS:        a.VSS,
		Version:    a.version,
		Tags:       []string{fmt.Sprintf("run:%d", run.ID), jobTag(run.JobID)},
		Progress: func(done, total uint64) {
			a.progress(done, total, 0)
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
	// One disk: its layout (as before); several: the list.
	var details []byte
	if len(sn.Images) == 1 {
		details, _ = json.Marshal(sn.Images[0])
	} else {
		details, _ = json.Marshal(sn.Images)
	}
	res := api.RunResult{Status: api.StatusSuccess, SnapshotID: sn.ID.String(), Stats: stats, Details: details, Errors: sn.Stats.Errors}
	if len(sn.VSSVolumes) > 0 {
		res.Message = "Read from VSS snapshot of " + strings.Join(sn.VSSVolumes, ", ")
	}
	if len(sn.Stats.Errors) > 0 {
		res.Status = api.StatusWarning
	}
	lock.Unlock() // retention needs an exclusive lock
	return a.withRetention(ctx, r, run, res)
}

func (a *Agent) imageRestore(ctx context.Context, run api.Run) api.RunResult {
	r, closeRepo, err := a.openRepo(ctx, run.Repository, false)
	if err != nil {
		return failed(fmt.Errorf("open repository: %w", err))
	}
	defer closeRepo()
	lock, err := r.Lock(ctx, false, lockWait)
	if err != nil {
		return failed(err)
	}
	defer lock.Unlock()
	sn, err := r.LoadSnapshot(ctx, run.SnapshotID)
	if err != nil {
		return failed(err)
	}
	st, err := imaging.Restore(ctx, r, sn, imaging.RestoreOptions{Image: run.ImageIndex, TargetDisk: run.TargetDisk, KeepOffline: run.KeepOffline})
	if err != nil {
		return failed(err)
	}
	stats, _ := json.Marshal(st)
	msg := fmt.Sprintf("Wrote %d partitions to disk %d", st.Partitions, run.TargetDisk)
	if len(sn.Images) > 1 && run.ImageIndex < len(sn.Images) {
		msg = fmt.Sprintf("Wrote %d partitions of backed up disk %d to disk %d", st.Partitions, sn.Images[run.ImageIndex].Number, run.TargetDisk)
	}
	if run.KeepOffline {
		msg += " (left offline)"
	}
	res := api.RunResult{Status: api.StatusSuccess, Stats: stats, Message: msg}
	if run.NewHardware && !run.KeepOffline {
		dirs := imaging.RecoveryDriverDirs()
		for _, d := range strings.Split(run.DriverPath, ";") {
			if d = strings.TrimSpace(d); d != "" {
				dirs = append(dirs, d)
			}
		}
		hw, err := imaging.PrepareForNewHardware(ctx, run.TargetDisk, dirs, func(s string) { a.log.Info("new hardware", "run", run.ID, "step", s) })
		if err != nil {
			res.Status = api.StatusWarning
			res.Errors = append(res.Errors, "preparing for new hardware failed: "+err.Error())
			return res
		}
		res.Details, _ = json.Marshal(map[string]any{"new_hardware": hw})
		res.Message += fmt.Sprintf(". Prepared for new hardware: %d disk controller drivers enabled, %d drivers added", len(hw.DriversEnabled), hw.DriversAdded)
		if hw.BootRebuilt {
			res.Message += ", boot files rebuilt"
		}
		if len(hw.Warnings) > 0 {
			res.Status = api.StatusWarning
			res.Errors = append(res.Errors, hw.Warnings...)
		}
	}
	return res
}

func (a *Agent) imageFileRestore(ctx context.Context, run api.Run) api.RunResult {
	r, closeRepo, err := a.openRepo(ctx, run.Repository, false)
	if err != nil {
		return failed(fmt.Errorf("open repository: %w", err))
	}
	defer closeRepo()
	lock, err := r.Lock(ctx, false, lockWait)
	if err != nil {
		return failed(err)
	}
	defer lock.Unlock()
	sn, err := r.LoadSnapshot(ctx, run.SnapshotID)
	if err != nil {
		return failed(err)
	}
	p, v, err := imaging.OpenSnapshotVolume(ctx, r, sn, run.ImagePartition)
	if err != nil {
		return failed(err)
	}
	dest, err := imaging.DestFunc(p, run.RestoreTarget)
	if err != nil {
		return failed(err)
	}
	st, err := v.Extract(ctx, run.Includes, dest, nil)
	if err != nil {
		return failed(err)
	}
	stats, _ := json.Marshal(st)
	res := api.RunResult{Status: api.StatusSuccess, Stats: stats, Errors: st.Errors,
		Message: fmt.Sprintf("Restored %s and %s from the image", count(int(st.Files), "file", "files"), count(int(st.Dirs), "folder", "folders"))}
	if len(st.Errors) > 0 {
		res.Status = api.StatusWarning
	}
	return res
}
