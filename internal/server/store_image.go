package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/backupzit/backupzit/internal/api"
	"github.com/backupzit/backupzit/internal/imaging"
)

// Job kinds.
const (
	JobFiles = "files"
	JobImage = "image"
)

// checkImageSelection validates a disk/partition choice against the
// agent's reported inventory. Without an inventory it cannot check and
// accepts the choice; the agent validates again when the job runs.
func checkImageSelection(a Agent, disk int, parts []int) error {
	disks := a.Disks()
	if len(disks) == 0 {
		return nil
	}
	for _, d := range disks {
		if d.Number != disk {
			continue
		}
		for _, n := range parts {
			found := false
			for _, p := range d.Partitions {
				found = found || p.Number == n
			}
			if !found {
				return fmt.Errorf("disk %d has no partition %d", disk, n)
			}
		}
		return nil
	}
	return fmt.Errorf("agent %s has no disk %d", a.Hostname, disk)
}

// DescribeImageSelection renders "Disk 0 (whole disk)" or "Disk 0: partitions 1, 3".
func DescribeImageSelection(disk *int, parts []int) string {
	if disk == nil {
		return ""
	}
	if len(parts) == 0 {
		return fmt.Sprintf("Disk %d (whole disk)", *disk)
	}
	s := make([]string, len(parts))
	for i, p := range parts {
		s[i] = fmt.Sprint(p)
	}
	return fmt.Sprintf("Disk %d: partitions %s", *disk, strings.Join(s, ", "))
}

// QueueImageRestore creates a run that writes the image of a backup run
// onto a disk of the given agent. The disk is erased.
func (s *Store) QueueImageRestore(ctx context.Context, backupRunID, agentID int64, targetDisk int, keepOffline bool) (int64, error) {
	b, err := s.GetRun(ctx, backupRunID)
	if err != nil {
		return 0, err
	}
	if b.Kind != api.KindImageBackup || b.SnapshotID == "" {
		return 0, errors.New("run has no disk image to restore")
	}
	a, err := s.GetAgent(ctx, agentID)
	if err != nil {
		return 0, errors.New("unknown agent")
	}
	for _, d := range a.Disks() {
		if d.Number == targetDisk && d.System {
			return 0, fmt.Errorf("disk %d on %s holds the running Windows and cannot be overwritten", targetDisk, a.Hostname)
		}
	}
	var id int64
	err = s.db.QueryRow(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, target_id, snapshot_id, target_disk, keep_offline)
		VALUES($1,$2,'image-restore','manual',$3,$4,$5,$6,$7) RETURNING id`,
		agentID, b.JobID, b.RepoURL, b.TargetID, b.SnapshotID, targetDisk, keepOffline).Scan(&id)
	return id, err
}

// systemDiskOf returns the number of the agent's system disk, or -1.
func systemDiskOf(disks []imaging.Disk) int {
	for _, d := range disks {
		if d.System {
			return d.Number
		}
	}
	return -1
}
