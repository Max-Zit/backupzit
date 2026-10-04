package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/backupzit/backupzit/internal/api"
	"github.com/backupzit/backupzit/internal/pve"
	"github.com/backupzit/backupzit/internal/repo"
)

// replicaEntry is the outcome for one guest, reported in the run details.
type replicaEntry struct {
	pve.ReplicaResult
	SnapshotID string `json:"snapshot,omitempty"` // the backup the replica now holds
	Error      string `json:"error,omitempty"`
}

// vmReplica updates the replicas of the guests in a VM backup, or starts one
// (failover).
func (a *Agent) vmReplica(ctx context.Context, run api.Run) api.RunResult {
	if !pve.Available() {
		return failed(fmt.Errorf("replication needs the agent on a Proxmox VE node"))
	}
	if run.Replica == nil {
		return failed(fmt.Errorf("options missing"))
	}
	if run.Kind == api.KindVMReplicaStart {
		if len(run.Replica.Guests) != 1 {
			return failed(fmt.Errorf("which replica to start is missing"))
		}
		g := run.Replica.Guests[0]
		if err := pve.StartReplica(ctx, g.ReplicaVMID, g.VMID); err != nil {
			return failed(err)
		}
		return api.RunResult{Status: api.StatusSuccess, Message: fmt.Sprintf("Replica %d of guest %d started", g.ReplicaVMID, g.VMID)}
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
	known := map[int]api.ReplicaState{}
	for _, g := range run.Replica.Guests {
		known[g.VMID] = g
	}
	prevs := map[string]*repo.Snapshot{}
	var entries []replicaEntry
	var failures []string
	var written uint64
	for _, g := range sn.Guests {
		if g.Platform != "" && g.Platform != "proxmox" {
			continue
		}
		st := known[g.VMID]
		var prev *repo.Snapshot
		if st.SnapshotID != "" {
			if p, ok := prevs[st.SnapshotID]; ok {
				prev = p
			} else if p, err := r.LoadSnapshot(ctx, st.SnapshotID); err == nil {
				prevs[st.SnapshotID], prev = p, p
			}
		}
		a.log.Info("replicating", "run", run.ID, "vmid", g.VMID, "replica", st.ReplicaVMID)
		res, err := pve.Replicate(ctx, r, sn, pve.ReplicaOptions{VMID: g.VMID, ReplicaVMID: st.ReplicaVMID, Storage: run.Replica.Storage, Prev: prev,
			Log:      func(msg string, kv ...any) { a.log.Info(msg, append([]any{"run", run.ID}, kv...)...) },
			Progress: func(done, total uint64) { a.progress(done, total, 0) }})
		e := replicaEntry{}
		if res != nil {
			e.ReplicaResult = *res
		} else {
			e.VMID, e.ReplicaVMID, e.Name = g.VMID, st.ReplicaVMID, g.Name
		}
		if err != nil {
			e.Error = err.Error()
			failures = append(failures, fmt.Sprintf("guest %d (%s): %v", g.VMID, g.Name, err))
		} else {
			e.SnapshotID = sn.ID.String()
			written += e.Written
		}
		entries = append(entries, e)
	}
	details, _ := json.Marshal(map[string]any{"replicas": entries})
	res := api.RunResult{Status: api.StatusSuccess, Details: details, Errors: failures,
		Message: fmt.Sprintf("%d replicas updated from the backup of %s; %s written", len(entries)-len(failures), sn.Time.Local().Format("2006-01-02 15:04"), humanSize(written))}
	if len(entries) == 0 {
		res.Message = "The backup has no Proxmox guests"
	}
	if len(failures) > 0 {
		res.Status = api.StatusWarning
		if len(failures) == len(entries) {
			res.Status = api.StatusFailed
		}
		res.Message += ". Not updated: " + strings.Join(failures, "; ")
	}
	return res
}
