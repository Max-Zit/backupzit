package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/backupzit/backupzit/internal/api"
	"github.com/backupzit/backupzit/internal/pve"
)

// hypervisorInventory returns the Proxmox VE guest list when the agent runs
// on a Proxmox node, nil otherwise.
func (a *Agent) hypervisorInventory(ctx context.Context) json.RawMessage {
	if !pve.Available() {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	inv, err := pve.GetInventory(cctx)
	if err != nil {
		a.log.Warn("Proxmox inventory", "err", err)
		return nil
	}
	b, _ := json.Marshal(inv)
	return b
}

func parseVMIDs(ss []string) ([]int, error) {
	var ids []int
	for _, s := range ss {
		id, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return nil, fmt.Errorf("invalid guest ID %q", s)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// vmGuestSummary is the run detail shown in the console.
type vmGuestSummary struct {
	VMID        int    `json:"vmid"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	Consistency string `json:"consistency"`
	Disks       int    `json:"disks"`
	Size        uint64 `json:"size"`
	Stored      uint64 `json:"stored"` // bytes with data
}

func (a *Agent) vmBackup(ctx context.Context, run api.Run) api.RunResult {
	if !pve.Available() {
		return failed(fmt.Errorf("this machine is not a Proxmox VE node"))
	}
	var sel []int
	if !(len(run.VMs) == 1 && run.VMs[0] == "*") {
		ids, err := parseVMIDs(run.VMs)
		if err != nil {
			return failed(err)
		}
		if len(ids) == 0 {
			return failed(fmt.Errorf("the job selects no virtual machines"))
		}
		sel = ids
	}
	excl, err := parseVMIDs(run.VMExclude)
	if err != nil {
		return failed(err)
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
	var lastLog time.Time
	sn, err := pve.Backup(ctx, r, pve.BackupOptions{
		VMIDs: sel, Exclude: excl, Version: a.version,
		Tags: []string{fmt.Sprintf("run:%d", run.ID), jobTag(run.JobID)},
		Progress: func(done, total uint64) {
			if time.Since(lastLog) > time.Minute {
				lastLog = time.Now()
				a.log.Info("vm backup", "run", run.ID, "done", done, "total", total)
			}
		},
		Log: func(msg string, kv ...any) { a.log.Info(msg, append([]any{"run", run.ID}, kv...)...) },
	})
	if err != nil {
		return failed(err)
	}
	var sum []vmGuestSummary
	for _, g := range sn.Guests {
		s := vmGuestSummary{VMID: g.VMID, Type: g.Type, Name: g.Name, Consistency: g.Consistency, Disks: len(g.Disks)}
		for _, d := range g.Disks {
			s.Size += d.Size
			if d.Image < len(sn.Images) && len(sn.Images[d.Image].Partitions) == 1 {
				s.Stored += sn.Images[d.Image].Partitions[0].StoredBytes
			}
		}
		sum = append(sum, s)
	}
	stats, _ := json.Marshal(sn.Stats)
	details, _ := json.Marshal(map[string]any{"guests": sum})
	res := api.RunResult{Status: api.StatusSuccess, SnapshotID: sn.ID.String(), Stats: stats, Details: details, Errors: sn.Stats.Errors,
		Message: fmt.Sprintf("Backed up %d guests", len(sn.Guests))}
	if len(sn.Guests) == 1 {
		res.Message = "Backed up 1 guest"
	}
	if len(sn.Stats.Errors) > 0 {
		res.Status = api.StatusWarning
	}
	lock.Unlock() // retention needs an exclusive lock
	return a.withRetention(ctx, r, run, res)
}

func (a *Agent) vmRestore(ctx context.Context, run api.Run) api.RunResult {
	if !pve.Available() {
		return failed(fmt.Errorf("this machine is not a Proxmox VE node"))
	}
	if run.VMRestore == nil {
		return failed(fmt.Errorf("restore options missing"))
	}
	o := run.VMRestore
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
	res, err := pve.Restore(ctx, r, sn, pve.RestoreOptions{
		VMID: o.VMID, NewVMID: o.NewVMID, Name: o.Name, Storage: o.Storage, Overwrite: o.Overwrite, Start: o.Start,
		Log: func(msg string, kv ...any) { a.log.Info(msg, append([]any{"run", run.ID}, kv...)...) },
	})
	if err != nil {
		return failed(err)
	}
	kind := "VM"
	if res.Type == "lxc" {
		kind = "Container"
	}
	msg := fmt.Sprintf("%s %d", kind, res.VMID)
	if res.Name != "" {
		msg += " (" + res.Name + ")"
	}
	msg += " restored on node " + res.Node
	if res.Started {
		msg += " and started"
	}
	if res.NewMACs {
		msg += "; network cards got new MAC addresses because the original still exists"
	}
	if len(res.Notes) > 0 {
		msg += ". " + strings.Join(res.Notes, "; ")
	}
	details, _ := json.Marshal(res)
	stats, _ := json.Marshal(map[string]uint64{"bytes": res.Bytes})
	return api.RunResult{Status: api.StatusSuccess, Message: msg, Details: details, Stats: stats}
}
