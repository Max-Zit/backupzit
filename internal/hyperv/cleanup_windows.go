package hyperv

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// staleCheckpointAge is when a BackupZit checkpoint without a completion
// marker counts as left behind by a backup that crashed or lost power.
// Backups on this host are excluded by the VM lock; the age only has to
// cover a backup still reading on another node after a live migration.
// Without it, crash leftovers would stay forever and their differencing
// disks would keep growing.
const staleCheckpointAge = 48 * time.Hour

// doneMarker is the file that tells the node a VM moved to that its backup
// checkpoint name is no longer read: next to the VM's first disk, which on
// a cluster lies on a Cluster Shared Volume every node sees. "" when the VM
// has no disk path.
func doneMarker(vm vmInfo, name string) string {
	if len(vm.Disks) == 0 || vm.Disks[0].Path == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(vm.Disks[0].Path), ".backupzit-"+name+".done")
}

// CleanupCheckpoints removes BackupZit checkpoints of VMs on this host that
// no backup reads any more: those with a completion marker from their
// backup on another node, and those older than staleCheckpointAge. VMs a
// backup on this host is using (VM lock held) are skipped. It returns the
// removed checkpoints as "VM/checkpoint".
func CleanupCheckpoints(ctx context.Context) ([]string, error) {
	out, err := ps(ctx, `@(Get-VM | ForEach-Object { $vm = $_; Get-VMSnapshot -VM $vm -ErrorAction SilentlyContinue | Where-Object Name -like '`+snapPrefix+`*' | ForEach-Object {
 [pscustomobject]@{ id = $vm.Id.Guid; vm = $vm.Name; name = $_.Name; age = [int]((Get-Date) - $_.CreationTime).TotalSeconds;
  disk = "$(@(Get-VMHardDiskDrive -VM $vm)[0].Path)" } } }) | ConvertTo-Json -Compress`)
	if err != nil {
		return nil, err
	}
	var cps []struct {
		ID, VM, Name, Disk string
		Age                int
	}
	if len(out) > 0 && out[0] == '{' {
		out = append(append([]byte{'['}, out...), ']')
	}
	if err := json.Unmarshal(out, &cps); err != nil {
		return nil, err
	}
	var removed []string
	var errs []error
	for _, c := range cps {
		unlock, err := lockVM(ctx, c.ID, false)
		if errors.Is(err, errVMBusy) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		marker := ""
		if c.Disk != "" {
			marker = filepath.Join(filepath.Dir(c.Disk), ".backupzit-"+c.Name+".done")
		}
		_, statErr := os.Stat(marker)
		if (marker == "" || statErr != nil) && time.Duration(c.Age)*time.Second < staleCheckpointAge {
			unlock()
			continue // maybe still read by a backup on another node
		}
		if _, err := ps(ctx, `Get-VMSnapshot -VM (Get-VM -Id `+psq(c.ID)+`) -Name `+psq(c.Name)+` -ErrorAction Stop | Remove-VMSnapshot -ErrorAction Stop`); err != nil {
			errs = append(errs, err)
			unlock()
			continue
		}
		if marker != "" {
			_ = os.Remove(marker)
		}
		removed = append(removed, c.VM+"/"+c.Name)
		unlock()
	}
	return removed, errors.Join(errs...)
}

// vmGone reports whether PowerShell failed because the VM is no longer on
// this host (moved to another node of the cluster).
func vmGone(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unable to find a virtual machine with id")
}
