package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/pve"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/restorer"
	"github.com/max-zit/backupzit/internal/sysbackup"
)

// systemSummary is the run detail of a system backup shown in the console.
type systemSummary struct {
	OS          string          `json:"os"`
	UEFI        bool            `json:"uefi"`
	Disks       []systemDiskSum `json:"disks"`
	FileSystems []repo.SystemFS `json:"file_systems"`
	VGs         []string        `json:"vgs,omitempty"`
}

type systemDiskSum struct {
	Name  string `json:"name"`
	Size  uint64 `json:"size"`
	Table string `json:"table"`
	Parts int    `json:"partitions"`
}

func (a *Agent) systemBackup(ctx context.Context, run api.Run) api.RunResult {
	if runtime.GOOS != "linux" {
		return failed(fmt.Errorf("system backups are for Linux; use a disk image job on Windows"))
	}
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
	sn, err := sysbackup.Backup(ctx, r, sysbackup.BackupOptions{
		Excludes: run.Excludes, Version: a.version,
		Tags:     []string{fmt.Sprintf("run:%d", run.ID), jobTag(run.JobID)},
		Progress: func(_ string, s *repo.SnapshotStats) { a.progress(s.BytesRead, 0, s.Files) },
	})
	if err != nil {
		return failed(err)
	}
	lay := sn.System
	sum := systemSummary{OS: lay.OS, UEFI: lay.UEFI, FileSystems: lay.FileSystems}
	for _, d := range lay.Disks {
		sum.Disks = append(sum.Disks, systemDiskSum{Name: d.Name, Size: d.Size, Table: d.Table, Parts: len(d.Partitions)})
	}
	for _, v := range lay.VGs {
		sum.VGs = append(sum.VGs, v.Name)
	}
	stats, _ := json.Marshal(sn.Stats)
	details, _ := json.Marshal(map[string]any{"system": sum})
	res := api.RunResult{Status: api.StatusSuccess, SnapshotID: sn.ID.String(), Stats: stats, Details: details, Errors: sn.Stats.Errors,
		Message: fmt.Sprintf("%s: %d file systems, %d files", lay.OS, len(lay.FileSystems), sn.Stats.Files)}
	if len(sn.Stats.Errors) > 0 {
		res.Status = api.StatusWarning
	}
	lock.Unlock()
	return a.withRetention(ctx, r, run, res)
}

func (a *Agent) systemRestore(ctx context.Context, run api.Run) api.RunResult {
	o := run.SystemRestore
	if o == nil {
		return failed(fmt.Errorf("restore options missing"))
	}
	if runtime.GOOS != "linux" {
		return failed(fmt.Errorf("a system restore runs on a Linux agent (or the agent on a Proxmox host)"))
	}
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
	if sn.System == nil {
		return failed(fmt.Errorf("backup %s is not a system backup", sn.ID.Short()))
	}
	target, newHW, vmid := o.Device, o.NewHardware, 0
	if o.Mode == "pve-vm" {
		if !pve.Available() {
			return failed(fmt.Errorf("this agent is not on a Proxmox VE node"))
		}
		var size uint64
		for _, d := range sn.System.Disks {
			size = max(size, d.Size)
		}
		name := o.Name
		if name == "" {
			name = sn.System.Hostname
		}
		vmid, target, err = pve.CreateSystemVM(ctx, pve.SystemVMOptions{VMID: o.VMID, Name: name, Storage: o.Storage, DiskSize: size,
			UEFI: sn.System.UEFI, Memory: o.Memory, Cores: o.Cores, Bridge: o.Bridge})
		if err != nil {
			if vmid > 0 {
				pve.DestroyVM(context.Background(), vmid)
			}
			return failed(fmt.Errorf("create VM: %w", err))
		}
		newHW = true
	}
	var lastLog time.Time
	res, err := sysbackup.Restore(ctx, r, sn, sysbackup.RestoreOptions{Target: target, NewHardware: newHW, DisableAgent: vmid > 0,
		Log: func(s string) { a.log.Info("system restore", "run", run.ID, "step", s) },
		Progress: func(_ string, s *restorer.Stats) {
			a.progress(s.Bytes, 0, s.Files)
			if time.Since(lastLog) > time.Minute {
				lastLog = time.Now()
				a.log.Info("system restore", "run", run.ID, "files", s.Files)
			}
		}})
	if err != nil {
		if vmid > 0 {
			pve.DestroyVM(context.Background(), vmid)
		}
		return failed(err)
	}
	msg := fmt.Sprintf("%s restored onto %s: %d files; %s", sn.System.OS, target, res.Files, res.Boot)
	if vmid > 0 {
		msg = fmt.Sprintf("%s restored as Proxmox VM %d: %d files; %s", sn.System.OS, vmid, res.Files, res.Boot)
		if o.Start {
			if err := pve.StartVM(ctx, vmid); err != nil {
				res.Notes = append(res.Notes, "VM not started: "+err.Error())
			} else {
				msg += "; VM started"
			}
		}
	}
	if len(res.Notes) > 0 {
		msg += ". " + strings.Join(res.Notes, "; ")
	}
	details, _ := json.Marshal(map[string]any{"system_restore": res, "vmid": vmid})
	stats, _ := json.Marshal(map[string]uint64{"files": res.Files, "bytes": res.Bytes})
	out := api.RunResult{Status: api.StatusSuccess, Message: msg, Details: details, Stats: stats, Errors: res.Errors}
	if len(res.Errors) > 0 {
		out.Status = api.StatusWarning
	}
	return out
}
