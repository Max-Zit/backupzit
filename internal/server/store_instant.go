package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/api"
)

// Instant recovery: a vm-instant run starts a VM on a Proxmox node that
// reads its disks from a backup. Its details record the state of that VM
// ("running", "finished" or "discarded"); while it is running, retention
// never removes the backup.

// instantDetails are the details of a vm-instant run.
type instantDetails struct {
	InstantVMID int      `json:"instant_vmid"`
	Node        string   `json:"node"`
	Name        string   `json:"name"`
	State       string   `json:"state"`
	Disks       []string `json:"disks,omitempty"`
}

func runInstant(run Run) *instantDetails {
	if run.Kind != api.KindVMInstant || len(run.Details) == 0 {
		return nil
	}
	var d instantDetails
	if json.Unmarshal(run.Details, &d) != nil || d.InstantVMID == 0 {
		return nil
	}
	return &d
}

// InstantVM is a VM running from a backup.
type InstantVM struct {
	RunID     int64
	AgentID   int64
	Hostname  string
	VMID      int
	Name      string
	StartedAt time.Time
}

// RunningInstantVMs lists the VMs that still run from a backup.
func (s *Store) RunningInstantVMs(ctx context.Context) ([]InstantVM, error) {
	rows, err := s.db.Query(ctx, `SELECT r.id, r.agent_id, a.hostname, (r.details->>'instant_vmid')::int, coalesce(r.details->>'name',''), coalesce(r.finished_at, r.queued_at)
		FROM runs r JOIN agents a ON a.id=r.agent_id
		WHERE r.kind='vm-instant' AND r.status='success' AND r.details->>'state'='running' ORDER BY r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InstantVM
	for rows.Next() {
		var v InstantVM
		if err := rows.Scan(&v.RunID, &v.AgentID, &v.Hostname, &v.VMID, &v.Name, &v.StartedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// instantSnapshots are the backups VMs run from (or are about to): retention
// must keep them.
func (s *Store) instantSnapshots(ctx context.Context) ([]string, error) {
	rows, err := s.db.Query(ctx, `SELECT DISTINCT snapshot_id FROM runs WHERE kind='vm-instant' AND snapshot_id <> ''
		AND (status IN ('queued','running') OR (status='success' AND details->>'state'='running'))`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// QueueVMInstant starts a VM on a Proxmox node directly from a VM backup.
func (s *Store) QueueVMInstant(ctx context.Context, backupRunID, agentID int64, o api.VMRestore) (int64, error) {
	b, err := s.GetRun(ctx, backupRunID)
	if err != nil {
		return 0, err
	}
	if vmDetails(b) == nil || b.SnapshotID == "" {
		return 0, errors.New("run has no VM backup")
	}
	if b.Expired {
		return 0, errors.New("this backup was removed by the retention policy")
	}
	d := vmDetails(b)
	if d == nil || d.Platform() != "proxmox" {
		return 0, errors.New("instant recovery is available for Proxmox VE backups")
	}
	gtype := ""
	for _, g := range d.Guests {
		if g.VMID == o.VMID {
			gtype = g.Type
		}
	}
	if gtype == "" {
		return 0, fmt.Errorf("guest %d is not in this backup", o.VMID)
	}
	if gtype == "lxc" {
		return 0, errors.New("instant recovery is available for VMs, not for containers")
	}
	a, err := s.GetAgent(ctx, agentID)
	if err != nil {
		return 0, errors.New("unknown agent")
	}
	if a.PVE() == nil || a.Platform() != "proxmox" {
		return 0, fmt.Errorf("%s is not a Proxmox VE node", a.Hostname)
	}
	if o.NewVMID != -1 && (o.NewVMID < 100 || o.NewVMID > 999999999) {
		return 0, errors.New("guest IDs are between 100 and 999999999")
	}
	for _, g := range a.LocalGuests() {
		if g.VMID == o.NewVMID {
			return 0, fmt.Errorf("guest %d exists on %s; instant recovery creates a new VM", o.NewVMID, a.Hostname)
		}
	}
	if len(o.Name) > 63 || strings.ContainsAny(o.Name, " \t\r\n:,=") {
		return 0, errors.New("the name may not contain spaces or : , = and has at most 63 characters")
	}
	o = api.VMRestore{VMID: o.VMID, NewVMID: o.NewVMID, Name: o.Name, Start: true}
	opts, _ := json.Marshal(o)
	var id int64
	err = s.db.QueryRow(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, target_id, snapshot_id, vm_restore)
		VALUES($1,$2,'vm-instant','manual',$3,$4,$5,$6) RETURNING id`,
		agentID, b.JobID, b.RepoURL, b.TargetID, b.SnapshotID, opts).Scan(&id)
	return id, err
}

// QueueInstantEnd finishes (moves the disks to storage) or discards (deletes)
// the VM a vm-instant run started.
func (s *Store) QueueInstantEnd(ctx context.Context, instantRunID int64, finish bool, storage string) (int64, error) {
	ir, err := s.GetRun(ctx, instantRunID)
	if err != nil {
		return 0, err
	}
	d := runInstant(ir)
	if d == nil || ir.Status != api.StatusSuccess {
		return 0, errors.New("this run did not start a VM from a backup")
	}
	if d.State != "running" {
		return 0, fmt.Errorf("VM %d was already %s", d.InstantVMID, d.State)
	}
	var busy bool
	s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runs WHERE kind IN ('vm-instant-finish','vm-instant-discard')
		AND status IN ('queued','running') AND (vm_restore->>'instant_run')::bigint=$1)`, instantRunID).Scan(&busy)
	if busy {
		return 0, errors.New("finishing or discarding this VM is already queued")
	}
	kind := api.KindVMInstantDiscard
	o := api.VMRestore{VMID: d.InstantVMID, InstantVMID: d.InstantVMID, InstantRun: instantRunID}
	if finish {
		kind = api.KindVMInstantFinish
		a, err := s.GetAgent(ctx, ir.AgentID)
		if err != nil {
			return 0, err
		}
		ok := false
		if inv := a.PVE(); inv != nil {
			for _, st := range inv.Storage {
				ok = ok || (st == storage && st != "backupzit-instant")
			}
		}
		if !ok {
			return 0, fmt.Errorf("choose a storage of %s for the disks", a.Hostname)
		}
		o.Storage = storage
	}
	opts, _ := json.Marshal(o)
	var id int64
	err = s.db.QueryRow(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, target_id, snapshot_id, vm_restore)
		VALUES($1,$2,$3,'manual',$4,$5,$6,$7) RETURNING id`,
		ir.AgentID, ir.JobID, kind, ir.RepoURL, ir.TargetID, ir.SnapshotID, opts).Scan(&id)
	return id, err
}

// endInstant records that the VM of a vm-instant run was finished or
// discarded, after the agent reported success.
func (s *Store) endInstant(ctx context.Context, runID int64) error {
	var kind string
	var opts []byte
	if err := s.db.QueryRow(ctx, `SELECT kind, coalesce(vm_restore, '{}'::jsonb) FROM runs WHERE id=$1`, runID).Scan(&kind, &opts); err != nil {
		return err
	}
	state := map[string]string{api.KindVMInstantFinish: "finished", api.KindVMInstantDiscard: "discarded"}[kind]
	if state == "" {
		return nil
	}
	var o api.VMRestore
	if json.Unmarshal(opts, &o) != nil || o.InstantRun == 0 {
		return nil
	}
	_, err := s.db.Exec(ctx, `UPDATE runs SET details=jsonb_set(details, '{state}', to_jsonb($2::text)) WHERE id=$1 AND kind='vm-instant'`, o.InstantRun, state)
	return err
}
