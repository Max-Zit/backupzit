package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/hyperv"
	"github.com/max-zit/backupzit/internal/pve"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/vmware"
)

// hypervisorInventory returns the guest list when the agent runs on a
// Proxmox VE node or a Hyper-V host, nil otherwise.
func (a *Agent) hypervisorInventory(ctx context.Context) json.RawMessage {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var inv *pve.Inventory
	var err error
	switch {
	case pve.Available():
		inv, err = pve.GetInventory(cctx)
	case hyperv.Available():
		inv, err = hyperv.GetInventory(cctx)
	default:
		return nil
	}
	if err != nil {
		a.log.Warn("hypervisor inventory", "err", err)
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
	Platform    string `json:"platform,omitempty"`
	Disks       int    `json:"disks"`
	Size        uint64 `json:"size"`
	Stored      uint64 `json:"stored"` // bytes with data
}

func (a *Agent) vmBackup(ctx context.Context, run api.Run) api.RunResult {
	isPVE, isHV := pve.Available(), hyperv.Available()
	if run.VMware == nil && !isPVE && !isHV {
		return failed(fmt.Errorf("this machine is neither a Proxmox VE node nor a Hyper-V host"))
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
	tags := []string{fmt.Sprintf("run:%d", run.ID), jobTag(run.JobID)}
	prog := func(done, total uint64) {
		a.progress(done, total, 0)
		if time.Since(lastLog) > time.Minute {
			lastLog = time.Now()
			a.log.Info("vm backup", "run", run.ID, "done", done, "total", total)
		}
	}
	logf := func(msg string, kv ...any) { a.log.Info(msg, append([]any{"run", run.ID}, kv...)...) }
	var sn *repo.Snapshot
	if run.VMware != nil {
		c, err := vmwareConnect(ctx, run.VMware)
		if err != nil {
			return failed(err)
		}
		defer c.Logout()
		sn, err = c.Backup(ctx, r, vmware.BackupOptions{VMIDs: sel, Exclude: excl, Hostname: run.VMware.Name, Version: a.version, Tags: tags, Progress: prog, Log: logf})
	} else if isPVE {
		sn, err = pve.Backup(ctx, r, pve.BackupOptions{VMIDs: sel, Exclude: excl, Version: a.version, Tags: tags, Progress: prog, Log: logf})
	} else {
		sn, err = hyperv.Backup(ctx, r, hyperv.BackupOptions{VMIDs: sel, Exclude: excl, Version: a.version, Tags: tags, Progress: prog, Log: logf})
	}
	if err != nil {
		return failed(err)
	}
	stats, _ := json.Marshal(sn.Stats)
	res := api.RunResult{Status: api.StatusSuccess, SnapshotID: sn.ID.String(), Stats: stats, Details: vmGuestDetails(sn), Errors: sn.Stats.Errors,
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
	isPVE, isHV := pve.Available(), hyperv.Available()
	if run.VMware == nil && !isPVE && !isHV {
		return failed(fmt.Errorf("this machine is neither a Proxmox VE node nor a Hyper-V host"))
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
	if run.VMware != nil {
		return a.vmwareRestore(ctx, run, r, sn)
	}
	if isHV {
		return a.hypervRestore(ctx, run, r, sn)
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

// hypervRestore restores a Hyper-V VM: NewVMID -1 creates a second VM next
// to the original, 0 restores in place (with Overwrite) or recreates a
// deleted VM. Storage is the folder for the virtual disks.
func (a *Agent) hypervRestore(ctx context.Context, run api.Run, r *repo.Repository, sn *repo.Snapshot) api.RunResult {
	o := run.VMRestore
	res, err := hyperv.Restore(ctx, r, sn, hyperv.RestoreOptions{
		VMID: o.VMID, AsNew: o.NewVMID != 0, Name: o.Name, Folder: o.Storage, Overwrite: o.Overwrite, Start: o.Start,
		Log: func(msg string, kv ...any) { a.log.Info(msg, append([]any{"run", run.ID}, kv...)...) },
	})
	if err != nil {
		return failed(err)
	}
	msg := fmt.Sprintf("VM %q restored on %s", res.Name, res.Host)
	if res.Started {
		msg += " and started"
	}
	if res.NewMACs {
		msg += "; network adapters got new MAC addresses because the original still exists"
	}
	if len(res.Notes) > 0 {
		msg += ". " + strings.Join(res.Notes, "; ")
	}
	details, _ := json.Marshal(res)
	stats, _ := json.Marshal(map[string]uint64{"bytes": res.Bytes})
	return api.RunResult{Status: api.StatusSuccess, Message: msg, Details: details, Stats: stats}
}

func vmwareConnect(ctx context.Context, h *api.VMwareHost) (*vmware.Client, error) {
	return vmware.Connect(ctx, vmware.Conn{Host: h.Address, User: h.User, Password: h.Password, Thumbprint: h.Thumbprint, SSHHostKey: h.SSHHostKey})
}

// vmwareRestore restores a VM onto an ESXi host: NewVMID -1 creates a copy
// next to the original, 0 restores in place (with Overwrite) or recreates a
// deleted VM. Storage is the datastore.
func (a *Agent) vmwareRestore(ctx context.Context, run api.Run, r *repo.Repository, sn *repo.Snapshot) api.RunResult {
	o := run.VMRestore
	c, err := vmwareConnect(ctx, run.VMware)
	if err != nil {
		return failed(err)
	}
	defer c.Logout()
	var written uint64
	res, err := c.Restore(ctx, r, sn, vmware.RestoreOptions{
		VMID: o.VMID, AsNew: o.NewVMID != 0, Name: o.Name, Datastore: o.Storage, Overwrite: o.Overwrite, Start: o.Start,
		Progress: func(done, total uint64) { written = done; a.progress(done, total, 0) },
		Log:      func(msg string, kv ...any) { a.log.Info(msg, append([]any{"run", run.ID}, kv...)...) },
	})
	if err != nil {
		return failed(err)
	}
	msg := fmt.Sprintf("VM %q restored on %s", res.Name, run.VMware.Name)
	if o.Start {
		msg += " and started"
	}
	if len(res.Notes) > 0 {
		msg += ". " + strings.Join(res.Notes, "; ")
	}
	details, _ := json.Marshal(res)
	stats, _ := json.Marshal(map[string]uint64{"bytes": written})
	return api.RunResult{Status: api.StatusSuccess, Message: msg, Details: details, Stats: stats}
}

// vmGuestDetails are the run details listing the guests in a VM backup,
// for the console's restore forms.
func vmGuestDetails(sn *repo.Snapshot) json.RawMessage {
	var sum []vmGuestSummary
	for _, g := range sn.Guests {
		s := vmGuestSummary{VMID: g.VMID, Type: g.Type, Name: g.Name, Consistency: g.Consistency, Disks: len(g.Disks), Platform: g.Platform}
		for _, d := range g.Disks {
			s.Size += d.Size
			if d.Image < len(sn.Images) && len(sn.Images[d.Image].Partitions) == 1 {
				s.Stored += sn.Images[d.Image].Partitions[0].StoredBytes
			}
		}
		sum = append(sum, s)
	}
	details, _ := json.Marshal(map[string]any{"guests": sum})
	return details
}
