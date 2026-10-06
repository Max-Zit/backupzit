package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/vmfs"
)

// vmFileRestore copies files out of a VM disk in a Proxmox backup into a
// folder on this machine.
func (a *Agent) vmFileRestore(ctx context.Context, run api.Run) api.RunResult {
	o := run.VMFiles
	if o == nil || o.Target == "" || len(o.Paths) == 0 {
		return failed(fmt.Errorf("restore options missing"))
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
	rd, size, _, err := vmfs.GuestDisk(ctx, r, sn, o.VMID, o.Disk)
	if err != nil {
		return failed(err)
	}
	vols, err := vmfs.Volumes(rd, size)
	if err != nil {
		return failed(err)
	}
	v, err := vmfs.FindVolume(vols, o.Volume)
	if err != nil {
		return failed(err)
	}
	fsys, err := vmfs.Open(v)
	if err != nil {
		return failed(err)
	}
	st, err := vmfs.Extract(ctx, fsys, o.Paths, o.Target)
	if err != nil {
		return failed(err)
	}
	stats, _ := json.Marshal(map[string]any{"files": st.Files, "dirs": st.Dirs, "bytes": st.Bytes})
	res := api.RunResult{Status: api.StatusSuccess, Stats: stats, Errors: st.Errors,
		Message: fmt.Sprintf("Restored %s and %s (%s) from VM %d, %s into %s", count(int(st.Files), "file", "files"), count(int(st.Dirs), "folder", "folders"), humanSize(st.Bytes), o.VMID, v.Name, o.Target)}
	if len(st.Errors) > 0 {
		res.Status = api.StatusWarning
	}
	return res
}
