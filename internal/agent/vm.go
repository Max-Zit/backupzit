package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/hyperv"
	"github.com/max-zit/backupzit/internal/pve"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/runinfo"
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
	if strings.HasPrefix(run.Part, partPrefix) {
		tags = append(tags, run.Part) // retention per proxy (split VMware backup)
	}
	prog := func(done, total uint64) {
		a.progress(ctx, done, total, 0)
		if time.Since(lastLog) > time.Minute {
			lastLog = time.Now()
			a.log.Info("vm backup", "run", run.ID, "done", done, "total", total)
		}
	}
	logf := func(msg string, kv ...any) { a.log.Info(msg, append([]any{"run", run.ID}, kv...)...) }
	vmStarted := func(name string, total int) { a.vmStarted(ctx, name, total) }
	vmFinished := func(name string, failed bool) { a.vmFinished(ctx, name, failed) }
	var sn *repo.Snapshot
	if run.VMware != nil {
		c, cerr := vmwareConnect(ctx, run.VMware)
		if cerr != nil {
			return failed(cerr)
		}
		defer c.Logout()
		sn, err = c.Backup(ctx, r, vmware.BackupOptions{VMIDs: sel, Exclude: excl, Hostname: run.VMware.Name, Version: a.version, Tags: tags, Progress: prog, Log: logf,
			VMStarted: vmStarted, VMFinished: vmFinished})
	} else if isPVE {
		sn, err = pve.Backup(ctx, r, pve.BackupOptions{VMIDs: sel, Exclude: excl, Version: a.version, Tags: tags, Progress: prog, Log: logf,
			VMStarted: vmStarted, VMFinished: vmFinished})
	} else {
		sn, err = hyperv.Backup(ctx, r, hyperv.BackupOptions{VMIDs: sel, Exclude: excl, Cluster: run.HVCluster != "", RequireAppConsistent: run.AppConsistent, ExcludeDisks: run.VMExcludeDisks, CrashConsistentOK: run.VMCrashConsistentOK,
			Version: a.version, Tags: tags, Progress: prog, Log: logf, VMStarted: vmStarted, VMFinished: vmFinished})
	}
	if run.Part != "" && errors.Is(err, vmware.ErrNoVMs) {
		// The connection's proxy in a split backup: all VMs of the job are
		// on the hosts of the other proxies right now.
		return api.RunResult{Status: api.StatusSuccess, Message: "None of the job's VMs is left for this proxy now; the proxies on their ESXi hosts back them up"}
	}
	if errors.Is(err, hyperv.ErrNoneHere) {
		return api.RunResult{Status: api.StatusSuccess, Message: "None of the job's VMs runs on this node now; the other nodes of cluster " + run.HVCluster + " back them up"}
	}
	res, keep := vmBackupResult(sn, err)
	if !keep {
		return res
	}
	_ = lock.Unlock() // retention needs an exclusive lock
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
		VMID: o.VMID, AsNew: o.NewVMID != 0, Name: o.Name, Folder: o.Storage, Overwrite: o.Overwrite, Start: o.Start, Cluster: o.HVCluster,
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
	return vmware.Connect(ctx, vmware.Conn{Host: h.Address, User: h.User, Password: h.Password, Thumbprint: h.Thumbprint, SSHHostKey: h.SSHHostKey, Method: h.Method, HotAdd: h.HotAdd})
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
		VMID: o.VMID, AsNew: o.NewVMID != 0, Name: o.Name, Datastore: o.Storage, Node: o.Node, Overwrite: o.Overwrite, Start: o.Start,
		Progress: func(done, total uint64) { written = done; a.progress(ctx, done, total, 0) },
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

// checkpointCleanupEvery is how often an idle agent on a Hyper-V host looks
// for backup checkpoints left on VMs that moved here during a backup.
const checkpointCleanupEvery = 2 * time.Minute

// cleanupCheckpoints removes such checkpoints (see hyperv.CleanupCheckpoints);
// PollOnce calls it only while no run is active.
func (a *Agent) cleanupCheckpoints(ctx context.Context) {
	a.mu.Lock()
	due := time.Since(a.checkpointsAt) > checkpointCleanupEvery
	if due {
		a.checkpointsAt = time.Now()
	}
	a.mu.Unlock()
	if !due || !hyperv.Available() {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if done, err := hyperv.CleanupCheckpoints(cctx); err != nil {
		a.log.Warn("remove leftover checkpoints", "err", err)
	} else if len(done) > 0 {
		a.log.Info("removed leftover backup checkpoints", "checkpoints", done)
	}
}

// vmBackupResult turns the snapshot of a VM backup into the run result.
// retention reports whether the job's retention may run: not after a
// failed run, so older restore points of a missing guest stay.
func vmBackupResult(sn *repo.Snapshot, err error) (res api.RunResult, retention bool) {
	if errors.Is(err, hyperv.ErrNotAppConsistent) && sn != nil && len(sn.Stats.Failed) == 0 {
		// The job requires application-consistent checkpoints: the run
		// fails, but the VMs that had one are kept and can be restored.
		// No retention after a failed run.
		stats, _ := json.Marshal(sn.Stats)
		return api.RunResult{Status: api.StatusFailed, SnapshotID: sn.ID.String(), Stats: stats, Details: runinfo.Guests(sn), Errors: sn.Stats.Errors,
			Message: fmt.Sprintf("%v; the other %d VMs were backed up", err, len(sn.Guests))}, false
	}
	if sn != nil && len(sn.Stats.Failed) > 0 {
		// A guest without a restore point in this run must not hide among
		// warnings (issue #38): the run fails and names it. The other guests
		// are kept and can be restored. No retention after a failed run, so
		// the older restore points of that guest stay.
		stats, _ := json.Marshal(sn.Stats)
		msg := fmt.Sprintf("No backup of %s in this run; the other %d guests were backed up", strings.Join(sn.Stats.Failed, ", "), len(sn.Guests))
		if errors.Is(err, hyperv.ErrNotAppConsistent) {
			msg = fmt.Sprintf("%v. %s", err, msg)
		}
		return api.RunResult{Status: api.StatusFailed, SnapshotID: sn.ID.String(), Stats: stats, Details: runinfo.Guests(sn), Errors: sn.Stats.Errors, Message: msg}, false
	}
	if err != nil {
		return failed(err), false
	}
	stats, _ := json.Marshal(sn.Stats)
	res = api.RunResult{Status: api.StatusSuccess, SnapshotID: sn.ID.String(), Stats: stats, Details: runinfo.Guests(sn), Errors: sn.Stats.Errors,
		Message: fmt.Sprintf("Backed up %d guests", len(sn.Guests))}
	if len(sn.Guests) == 1 {
		res.Message = "Backed up 1 guest"
	}
	if len(sn.Stats.Errors) > 0 {
		res.Status = api.StatusWarning
	}
	return res, true
}

// vmwareProbe connects to the VMware host of the run (the console may have
// no route to it) and returns its VM list, version, license and the
// certificate and SSH key it presented (KindVMwareProbe, V-3). A host whose
// certificate differs from the pinned one is refused, as in every run; the
// error names the new fingerprint for the console to offer.
func (a *Agent) vmwareProbe(ctx context.Context, run api.Run) api.RunResult {
	if run.VMware == nil {
		return failed(errors.New("the run names no VMware host"))
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	c, err := vmwareConnect(ctx, run.VMware)
	if err != nil {
		return failed(err)
	}
	defer c.Logout()
	inv, err := c.Inventory(ctx, run.VMware.Name)
	if err != nil {
		return failed(fmt.Errorf("read the VM list: %w", err))
	}
	p := api.VMwareProbe{Version: c.Version, VCenter: c.VCenter, APIWrites: c.APIWrites, Thumbprint: c.Thumbprint, UsesSSH: c.Method() == vmware.MethodSSH}
	p.Inventory, _ = json.Marshal(inv)
	if p.UsesSSH { // the API needs no SSH
		if err := c.CheckSSH(ctx); err != nil {
			p.SSHError = err.Error()
		}
	}
	p.SSHHostKey = c.SSHHostKey
	details, _ := json.Marshal(p)
	return api.RunResult{Status: api.StatusSuccess, Details: details, Message: fmt.Sprintf("Connected to %s (%s): %d VMs", run.VMware.Name, inv.Version, len(inv.Guests))}
}
