package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/backupzit/backupzit/internal/api"
	"github.com/backupzit/backupzit/internal/pve"
)

// PVE decodes the Proxmox VE inventory of an agent on a Proxmox node (nil
// for other agents).
func (a Agent) PVE() *pve.Inventory {
	if len(a.Hypervisor) == 0 {
		return nil
	}
	var inv pve.Inventory
	if json.Unmarshal(a.Hypervisor, &inv) != nil {
		return nil
	}
	return &inv
}

// Platform is "proxmox" or "hyperv" for agents on a hypervisor, "" otherwise.
func (a Agent) Platform() string {
	inv := a.PVE()
	switch {
	case inv == nil:
		return ""
	case inv.Platform == "":
		return "proxmox"
	}
	return inv.Platform
}

// HypervisorLabel names the hypervisor of the agent's machine.
func (a Agent) HypervisorLabel() string {
	if a.Platform() == "hyperv" {
		return "Hyper-V"
	}
	return "Proxmox VE"
}

// LocalGuests lists the guests running on the agent's own node.
func (a Agent) LocalGuests() []pve.Guest {
	inv := a.PVE()
	if inv == nil {
		return nil
	}
	var gs []pve.Guest
	for _, g := range inv.Guests {
		if g.Node == inv.Node {
			gs = append(gs, g)
		}
	}
	return gs
}

// checkVMSelection validates the guests of a VM job: "*" (all guests of
// the node) or guest IDs.
func checkVMSelection(a Agent, vms, exclude []string) error {
	if a.PVE() == nil {
		return fmt.Errorf("%s is not a Proxmox VE node or Hyper-V host (install the BackupZit agent on the hypervisor host)", a.Hostname)
	}
	if len(vms) == 0 {
		return errors.New("choose the virtual machines and containers to back up, or all of them")
	}
	if len(vms) == 1 && vms[0] == "*" {
		for _, x := range exclude {
			if _, err := strconv.Atoi(x); err != nil {
				return fmt.Errorf("invalid guest ID %q", x)
			}
		}
		return nil
	}
	for _, v := range vms {
		if n, err := strconv.Atoi(v); err != nil || n < 100 {
			return fmt.Errorf("invalid guest ID %q", v)
		}
	}
	return nil
}

// DescribeVMSelection renders the guests of a VM job.
func DescribeVMSelection(vms []string) string {
	if len(vms) == 1 && vms[0] == "*" {
		return "All VMs (and containers) on the host"
	}
	for _, v := range vms {
		if len(v) >= 10 { // Hyper-V: numbers derived from VM GUIDs
			return fmt.Sprintf("%d selected VMs", len(vms))
		}
	}
	return "Guests " + strings.Join(vms, ", ")
}

// vmBackupDetails is the run detail reported by the agent.
type vmBackupDetails struct {
	Guests []struct {
		VMID        int    `json:"vmid"`
		Type        string `json:"type"`
		Name        string `json:"name"`
		Consistency string `json:"consistency"`
		Platform    string `json:"platform"`
		Disks       int    `json:"disks"`
		Size        uint64 `json:"size"`
		Stored      uint64 `json:"stored"`
	} `json:"guests"`
}

func vmDetails(run Run) *vmBackupDetails {
	if run.Kind != api.KindVMBackup || len(run.Details) == 0 {
		return nil
	}
	var d vmBackupDetails
	if json.Unmarshal(run.Details, &d) != nil || len(d.Guests) == 0 {
		return nil
	}
	return &d
}

func vmRestoreOptions(run Run) *api.VMRestore {
	if len(run.VMRestore) == 0 {
		return nil
	}
	var o api.VMRestore
	if json.Unmarshal(run.VMRestore, &o) != nil {
		return nil
	}
	return &o
}

// QueueVMRestore creates a vm-restore run from a VM backup run.
func (s *Store) QueueVMRestore(ctx context.Context, backupRunID, agentID int64, o api.VMRestore) (int64, error) {
	b, err := s.GetRun(ctx, backupRunID)
	if err != nil {
		return 0, err
	}
	if b.Kind != api.KindVMBackup || b.SnapshotID == "" {
		return 0, errors.New("run has no VM backup to restore")
	}
	if b.Expired {
		return 0, errors.New("this backup was removed by the retention policy")
	}
	d := vmDetails(b)
	found := false
	if d != nil {
		for _, g := range d.Guests {
			found = found || g.VMID == o.VMID
		}
	}
	if !found {
		return 0, fmt.Errorf("guest %d is not in this backup", o.VMID)
	}
	a, err := s.GetAgent(ctx, agentID)
	if err != nil {
		return 0, errors.New("unknown agent")
	}
	inv := a.PVE()
	if inv == nil {
		return 0, fmt.Errorf("%s is not a Proxmox VE node or Hyper-V host", a.Hostname)
	}
	if p := d.Platform(); p != a.Platform() {
		return 0, fmt.Errorf("this is a %s backup; restore it on a %s host", platformLabel(p), platformLabel(p))
	}
	if o.NewVMID != 0 && o.NewVMID != -1 && (o.NewVMID < 100 || o.NewVMID > 999999999) {
		return 0, errors.New("guest IDs are between 100 and 999999999")
	}
	if o.Storage != "" {
		ok := false
		for _, st := range inv.Storage {
			ok = ok || st == o.Storage
		}
		if !ok {
			return 0, fmt.Errorf("storage %q is not available on %s", o.Storage, a.Hostname)
		}
	}
	o.Name = strings.TrimSpace(o.Name)
	if a.Platform() == "hyperv" {
		if o.NewVMID != 0 && o.NewVMID != -1 {
			return 0, errors.New("Hyper-V VMs are restored as a new VM or in place of the original")
		}
		if len(o.Name) > 100 || strings.ContainsAny(o.Name, "\t\r\n\\/:*?\"<>|") {
			return 0, errors.New(`the VM name has at most 100 characters and none of \ / : * ? " < > |`)
		}
	} else if len(o.Name) > 63 || strings.ContainsAny(o.Name, " \t\r\n:,=") {
		return 0, errors.New("the name may not contain spaces or : , = and has at most 63 characters")
	}
	opts, _ := json.Marshal(o)
	var id int64
	err = s.db.QueryRow(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, target_id, snapshot_id, vm_restore)
		VALUES($1,$2,'vm-restore','manual',$3,$4,$5,$6) RETURNING id`,
		agentID, b.JobID, b.RepoURL, b.TargetID, b.SnapshotID, opts).Scan(&id)
	return id, err
}

// Platform of the guests in a VM backup ("proxmox" for older backups).
func (d *vmBackupDetails) Platform() string {
	if d != nil && len(d.Guests) > 0 && d.Guests[0].Platform != "" {
		return d.Guests[0].Platform
	}
	return "proxmox"
}

func platformLabel(p string) string {
	if p == "hyperv" {
		return "Hyper-V"
	}
	return "Proxmox VE"
}
