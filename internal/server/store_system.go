package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/backupzit/backupzit/internal/api"
)

// systemDetails is the run detail of a system backup.
type systemDetails struct {
	OS    string `json:"os"`
	UEFI  bool   `json:"uefi"`
	Disks []struct {
		Name       string `json:"name"`
		Size       uint64 `json:"size"`
		Table      string `json:"table"`
		Partitions int    `json:"partitions"`
	} `json:"disks"`
	FileSystems []struct {
		Device     string `json:"device"`
		Type       string `json:"type"`
		MountPoint string `json:"mount_point"`
		Size       uint64 `json:"size"`
		Used       uint64 `json:"used"`
	} `json:"file_systems"`
	VGs []string `json:"vgs"`
}

func sysDetails(run Run) *systemDetails {
	if run.Kind != api.KindSystemBackup || len(run.Details) == 0 {
		return nil
	}
	var d struct {
		System *systemDetails `json:"system"`
	}
	if json.Unmarshal(run.Details, &d) != nil {
		return nil
	}
	return d.System
}

func systemRestoreOptions(run Run) *api.SystemRestore {
	if len(run.SystemRestore) == 0 {
		return nil
	}
	var o api.SystemRestore
	if json.Unmarshal(run.SystemRestore, &o) != nil {
		return nil
	}
	return &o
}

// isLinuxAgent reports whether a can restore Linux systems.
func isLinuxAgent(a Agent) bool {
	return a.OS != "" && !strings.Contains(strings.ToLower(a.OS), "windows")
}

// QueueSystemRestore creates a system-restore run from a system backup.
func (s *Store) QueueSystemRestore(ctx context.Context, backupRunID, agentID int64, o api.SystemRestore) (int64, error) {
	b, err := s.GetRun(ctx, backupRunID)
	if err != nil {
		return 0, err
	}
	if b.Kind != api.KindSystemBackup || b.SnapshotID == "" {
		return 0, errors.New("run has no system backup to restore")
	}
	if b.Expired {
		return 0, errors.New("this backup was removed by the retention policy")
	}
	a, err := s.GetAgent(ctx, agentID)
	if err != nil {
		return 0, errors.New("unknown agent")
	}
	switch o.Mode {
	case "disk":
		if !isLinuxAgent(a) {
			return 0, fmt.Errorf("%s is not a Linux machine; boot the target from a Linux live system with the agent, or restore into a Proxmox VM", a.Hostname)
		}
		o.Device = strings.TrimSpace(o.Device)
		if !strings.HasPrefix(o.Device, "/dev/") || strings.ContainsAny(o.Device, " \t\n;'\"`$") {
			return 0, errors.New("enter the target disk as a device path, e.g. /dev/sdb")
		}
	case "pve-vm":
		inv := a.PVE()
		if inv == nil {
			return 0, fmt.Errorf("%s is not a Proxmox VE node", a.Hostname)
		}
		ok := false
		for _, st := range inv.Storage {
			ok = ok || st == o.Storage
		}
		if !ok {
			return 0, fmt.Errorf("storage %q is not available on %s", o.Storage, a.Hostname)
		}
		if o.VMID != 0 && o.VMID != -1 && (o.VMID < 100 || o.VMID > 999999999) {
			return 0, errors.New("guest IDs are between 100 and 999999999")
		}
		for _, g := range inv.Guests {
			if o.VMID > 0 && g.VMID == o.VMID {
				return 0, fmt.Errorf("guest %d already exists", o.VMID)
			}
		}
		o.Name = strings.TrimSpace(o.Name)
		if len(o.Name) > 63 || strings.ContainsAny(o.Name, " \t\r\n:,=") {
			return 0, errors.New("the VM name may not contain spaces or : , = and has at most 63 characters")
		}
		if o.Memory < 0 || o.Memory > 1<<20 || o.Cores < 0 || o.Cores > 512 {
			return 0, errors.New("invalid memory or CPU cores")
		}
		if o.Bridge != "" && strings.ContainsAny(o.Bridge, " ,=;") {
			return 0, errors.New("invalid network bridge")
		}
	default:
		return 0, errors.New("choose where to restore the system")
	}
	opts, _ := json.Marshal(o)
	var id int64
	err = s.db.QueryRow(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, target_id, snapshot_id, system_restore)
		VALUES($1,$2,'system-restore','manual',$3,$4,$5,$6) RETURNING id`,
		agentID, b.JobID, b.RepoURL, b.TargetID, b.SnapshotID, opts).Scan(&id)
	return id, err
}
