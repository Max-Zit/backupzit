package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/pve"
)

// Replication of Proxmox VM jobs: after each backup the agent on the
// replica node (JobOptions.ReplicaAgentID) brings a stopped copy of every
// guest up to date. The replicas and the backup each holds are recorded in
// the details of the vm-replica runs.

// checkReplica validates the replication settings of a job.
func (s *Store) checkReplica(ctx context.Context, j *Job, agent Agent) error {
	o := &j.Options
	if o.ReplicaAgentID == 0 {
		o.ReplicaStorage = ""
		return nil
	}
	if j.Kind != JobVM || j.VMwareHostID != nil || agent.Platform() != "proxmox" {
		return errors.New("replication is available for Proxmox VE VM jobs")
	}
	ra, err := s.GetAgent(ctx, o.ReplicaAgentID)
	if err != nil {
		return errors.New("unknown agent for the replicas")
	}
	inv := ra.PVE()
	if inv == nil || ra.Platform() != "proxmox" {
		return fmt.Errorf("%s is not a Proxmox VE node", ra.Hostname)
	}
	if o.ReplicaStorage != "" {
		ok := false
		for _, st := range inv.Storage {
			ok = ok || (st == o.ReplicaStorage && st != pve.InstantStorage)
		}
		if !ok {
			return fmt.Errorf("storage %q is not available on %s", o.ReplicaStorage, ra.Hostname)
		}
	}
	return nil
}

// ReplicaView is a replica on the job page.
type ReplicaView struct {
	VMID, ReplicaVMID int
	Name              string
	SnapshotID        string
	Updated           time.Time
	Error             string
	Status            string // of the replica guest: stopped, running, missing
}

type replicaDetail struct {
	VMID        int    `json:"vmid"`
	ReplicaVMID int    `json:"replica_vmid"`
	Name        string `json:"name"`
	SnapshotID  string `json:"snapshot"`
	Error       string `json:"error"`
}

// replicas returns the replicas of a job as the vm-replica runs recorded
// them, oldest run first so that later results win.
func (s *Store) replicas(ctx context.Context, jobID int64) ([]ReplicaView, error) {
	rows, err := s.db.Query(ctx, `SELECT details, coalesce(finished_at, queued_at) FROM runs WHERE job_id=$1 AND kind='vm-replica'
		AND status IN ('success','warning','failed') AND details IS NOT NULL ORDER BY id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byVM := map[int]*ReplicaView{}
	for rows.Next() {
		var b []byte
		var at time.Time
		if err := rows.Scan(&b, &at); err != nil {
			return nil, err
		}
		var d struct {
			Replicas []replicaDetail `json:"replicas"`
		}
		if json.Unmarshal(b, &d) != nil {
			continue
		}
		for _, e := range d.Replicas {
			v := byVM[e.VMID]
			if v == nil {
				v = &ReplicaView{VMID: e.VMID}
				byVM[e.VMID] = v
			}
			v.Name, v.Error = e.Name, e.Error
			if e.ReplicaVMID != 0 {
				v.ReplicaVMID = e.ReplicaVMID
			}
			if e.Error == "" {
				v.SnapshotID, v.Updated = e.SnapshotID, at
			} else if e.ReplicaVMID != 0 {
				v.SnapshotID = "" // may be partly written: compare everything next time
			}
		}
	}
	var out []ReplicaView
	for _, v := range byVM {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VMID < out[j].VMID })
	return out, rows.Err()
}

// QueueReplica queues the replication of a successful VM backup run.
func (s *Store) QueueReplica(ctx context.Context, backup Run) (int64, error) {
	if backup.JobID == nil || backup.Kind != api.KindVMBackup || backup.SnapshotID == "" {
		return 0, nil
	}
	j, err := s.GetJob(ctx, *backup.JobID)
	if err != nil || j.Options.ReplicaAgentID == 0 || !j.Enabled {
		return 0, err
	}
	var id int64
	err = s.db.QueryRow(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, target_id, snapshot_id)
		SELECT $1,$2,'vm-replica','after-backup',$3,$4,$5
		WHERE NOT EXISTS (SELECT 1 FROM runs WHERE job_id=$2 AND kind='vm-replica' AND status='queued')
		RETURNING id`, j.Options.ReplicaAgentID, j.ID, backup.RepoURL, backup.TargetID, backup.SnapshotID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// One is already waiting: let it replicate the newest backup.
		_, err = s.db.Exec(ctx, `UPDATE runs SET snapshot_id=$2, repo_url=$3, target_id=$4 WHERE job_id=$1 AND kind='vm-replica' AND status='queued'`,
			j.ID, backup.SnapshotID, backup.RepoURL, backup.TargetID)
		return 0, err
	}
	return id, err
}

// QueueReplicaStart starts the replica of guest vmid (failover).
func (s *Store) QueueReplicaStart(ctx context.Context, jobID int64, vmid int) (int64, error) {
	j, err := s.GetJob(ctx, jobID)
	if err != nil {
		return 0, err
	}
	reps, err := s.replicas(ctx, jobID)
	if err != nil {
		return 0, err
	}
	var rep *ReplicaView
	for i := range reps {
		if reps[i].VMID == vmid && reps[i].ReplicaVMID != 0 {
			rep = &reps[i]
		}
	}
	if rep == nil {
		return 0, fmt.Errorf("guest %d has no replica", vmid)
	}
	agentID := j.Options.ReplicaAgentID
	if agentID == 0 {
		// Replication was switched off later: the replica is on the node of
		// the last replication run.
		if err := s.db.QueryRow(ctx, `SELECT agent_id FROM runs WHERE job_id=$1 AND kind='vm-replica' ORDER BY id DESC LIMIT 1`, jobID).Scan(&agentID); err != nil {
			return 0, err
		}
	}
	opts, _ := json.Marshal(api.ReplicaRun{Guests: []api.ReplicaState{{VMID: vmid, ReplicaVMID: rep.ReplicaVMID}}})
	var id int64
	err = s.db.QueryRow(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, vm_restore) VALUES($1,$2,'vm-replica-start','manual','',$3) RETURNING id`,
		agentID, jobID, opts).Scan(&id)
	return id, err
}

// addReplica fills the replica options of a replication or failover run.
func (s *Server) addReplica(ctx context.Context, run *Run, ar *api.Run) error {
	if run.Kind == api.KindVMReplicaStart {
		var o api.ReplicaRun
		if err := json.Unmarshal(run.VMRestore, &o); err != nil {
			return err
		}
		ar.Replica = &o
		return nil
	}
	ar.SnapshotID = run.SnapshotID
	if run.JobID == nil {
		return errors.New("replication run without job")
	}
	j, err := s.store.GetJob(ctx, *run.JobID)
	if err != nil {
		return err
	}
	ar.JobID = j.ID
	reps, err := s.store.replicas(ctx, j.ID)
	if err != nil {
		return err
	}
	o := &api.ReplicaRun{Storage: j.Options.ReplicaStorage}
	for _, r := range reps {
		if r.ReplicaVMID != 0 {
			o.Guests = append(o.Guests, api.ReplicaState{VMID: r.VMID, ReplicaVMID: r.ReplicaVMID, SnapshotID: r.SnapshotID})
		}
	}
	ar.Replica = o
	return nil
}

// jobReplicas are the replicas of a job with the state of each replica
// guest, for the job page.
func (s *Server) jobReplicas(ctx context.Context, j Job) []ReplicaView {
	reps, err := s.store.replicas(ctx, j.ID)
	if err != nil || len(reps) == 0 {
		return nil
	}
	var guests []pve.Guest
	var agentID int64
	s.store.db.QueryRow(ctx, `SELECT agent_id FROM runs WHERE job_id=$1 AND kind='vm-replica' ORDER BY id DESC LIMIT 1`, j.ID).Scan(&agentID)
	if a, err := s.store.GetAgent(ctx, agentID); err == nil {
		guests = a.LocalGuests()
	}
	var out []ReplicaView
	for _, rep := range reps {
		rep.Status = "missing"
		for _, g := range guests {
			if g.VMID == rep.ReplicaVMID {
				rep.Status = g.Status
			}
		}
		// With replication off, only replicas that still exist matter.
		if j.Options.ReplicaAgentID != 0 || rep.Status != "missing" {
			out = append(out, rep)
		}
	}
	return out
}

// handleReplicaStart serves POST /jobs/{id}/replica-start.
func (s *Server) handleReplicaStart(w http.ResponseWriter, r *http.Request, _ string) {
	id, _ := pathID(r)
	back := fmt.Sprintf("/jobs/%d", id)
	vmid := atoiDefault(r.FormValue("vmid"))
	rid, err := s.store.QueueReplicaStart(r.Context(), id, vmid)
	if err != nil {
		redirectErr(w, r, back, err)
		return
	}
	s.audit(r, "replica.start", "run #%d: replica of guest %d of job %d", rid, vmid, id)
	redirectMsg(w, r, fmt.Sprintf("/runs/%d", rid), "Starting the replica.")
}
