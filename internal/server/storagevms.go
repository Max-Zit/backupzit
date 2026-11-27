package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// VM backups per storage target (#73). The page lists every virtual
// machine or container with restore points on a target, also those whose
// job is gone or no longer selects them ("no job": forgotten backups that
// keep using storage), with their restore points. A restore point that
// holds only this VM can be deleted from here; one that also holds other
// VMs (a VM job writes one backup per run for all its VMs) stays until
// the job's retention removes it, so the other VMs' history is never
// touched. When that job is gone no retention runs for it any more, so
// such a restore point can be deleted as a whole, with the backups of all
// its VMs, which the page names (#79). Deleting asks for the password or
// two-factor code, waits for a
// second user with four-eyes approval on, and runs on the console like
// the clean-up of a removed agent's backups (audit log entry with the
// result; immutable storage refuses what is still locked).

// storedPoint is one restore point of a VM on a target.
type storedPoint struct {
	RunID      int64
	Time       time.Time
	SnapshotID string
	Repo       string
	Copy       bool
	JobName    string
	Stored     uint64   // bytes of this VM's disks with data
	Others     int      // other VMs in the same backup: not deleted alone
	OtherVMs   []string // their names
	JobGone    bool     // its job was deleted: no retention removes it any more
}

// CanDelete reports whether the point can be deleted from the page: it
// holds only this VM, or its job is gone and it goes with all its VMs.
func (p storedPoint) CanDelete() bool { return p.Others == 0 || p.JobGone }

// storedVM is a VM with restore points in one repository of a target.
type storedVM struct {
	Name     string
	Type     string
	VMID     int
	Hostname string
	Repo     string
	NoJob    bool // no existing job covers these backups any more
	Points   []storedPoint
}

// Deletable is the number of restore points that can be deleted here.
func (v storedVM) Deletable() int {
	n := 0
	for _, p := range v.Points {
		if p.CanDelete() {
			n++
		}
	}
	return n
}

// DeletesOthers reports whether a point that can be deleted here also
// holds other VMs.
func (v storedVM) DeletesOthers() bool {
	for _, p := range v.Points {
		if p.Others > 0 && p.JobGone {
			return true
		}
	}
	return false
}

func (v storedVM) Newest() time.Time { return v.Points[0].Time }
func (v storedVM) Oldest() time.Time { return v.Points[len(v.Points)-1].Time }

// jobSelects reports whether a VM job's selection (paths "*" with
// excludes, or VMIDs) includes vmid.
func jobSelects(paths, excludes []string, vmid int) bool {
	id := strconv.Itoa(vmid)
	if slices.Contains(paths, "*") {
		return !slices.Contains(excludes, id)
	}
	return slices.Contains(paths, id)
}

// storageVMs lists the VMs with restore points on a target, newest
// backups first within each VM, VMs without a job first.
func (s *Store) storageVMs(ctx context.Context, targetID int64) ([]storedVM, error) {
	rows, err := s.db.Query(ctx, `SELECT r.id, r.kind, r.repo_url, r.snapshot_id, COALESCE(r.finished_at, r.started_at, r.queued_at),
			r.details, COALESCE(j.name, NULLIF(r.job_name,''), ''), `+agentNameSQL+`,
			sel.id IS NOT NULL, COALESCE(sel.kind,''), COALESCE(sel.paths,'{}'), COALESCE(sel.excludes,'{}'), j.id IS NULL
		FROM runs r JOIN agents a ON a.id=r.agent_id LEFT JOIN jobs j ON j.id=r.job_id
			LEFT JOIN jobs sel ON sel.id = CASE WHEN r.kind='copy' THEN j.source_job_id ELSE j.id END
		WHERE r.target_id=$1 AND r.status IN ('success','warning') AND r.snapshot_id<>'' AND NOT r.expired
			AND r.kind IN ('vm-backup','copy') AND r.details IS NOT NULL
		ORDER BY 5 DESC, r.id DESC`, targetID)
	if err != nil {
		return nil, err
	}
	type row struct {
		id                  int64
		kind, repoURL, snap string
		at                  time.Time
		details             json.RawMessage
		job, host           string
		hasJob              bool
		selKind             string
		selPaths, selExcl   []string
		jobGone             bool
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.id, &x.kind, &x.repoURL, &x.snap, &x.at, &x.details, &x.job, &x.host, &x.hasJob, &x.selKind, &x.selPaths, &x.selExcl, &x.jobGone)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	byKey := map[string]*storedVM{}
	var order []string
	for _, x := range list {
		var d vmBackupDetails
		if json.Unmarshal(x.details, &d) != nil || len(d.Guests) == 0 {
			continue // not a VM backup (a copy of files or images)
		}
		guests := d.Guests
		for i, g := range guests {
			var others []string
			for k, o := range guests {
				if k != i {
					others = append(others, o.Name)
				}
			}
			key := fmt.Sprintf("%s\x00%s\x00%d\x00%s", x.repoURL, g.Type, g.VMID, g.Name)
			v := byKey[key]
			if v == nil {
				v = &storedVM{Name: g.Name, Type: g.Type, VMID: g.VMID, Hostname: x.host, Repo: x.repoURL, NoJob: true}
				byKey[key] = v
				order = append(order, key)
			}
			if x.hasJob && x.selKind == JobVM && jobSelects(x.selPaths, x.selExcl, g.VMID) {
				v.NoJob = false
			}
			v.Points = append(v.Points, storedPoint{RunID: x.id, Time: x.at, SnapshotID: x.snap, Repo: x.repoURL, Copy: x.kind == "copy",
				JobName: x.job, Stored: g.Stored, Others: len(guests) - 1, OtherVMs: others, JobGone: x.jobGone})
		}
	}
	out := make([]storedVM, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].NoJob != out[j].NoJob {
			return out[i].NoJob
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// noJobCache keeps the VMs without a job per target for the dashboard for
// a few minutes (counting them reads every VM backup run).
type noJobCache struct {
	mu  sync.Mutex
	at  time.Time
	ok  bool
	out []noJobTarget
}

type noJobTarget struct {
	ID     int64
	Name   string
	VMs    []string
	Points int
}

const noJobCacheFor = 5 * time.Minute

// noJobAttention is the dashboard's entry per target with VM backups that
// belong to no job.
func (s *Server) noJobAttention(ctx context.Context, l *language, now time.Time) []Attention {
	c := &s.noJob
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ok || now.Sub(c.at) >= noJobCacheFor || now.Before(c.at) {
		targets, err := s.store.ListTargets(ctx)
		if err != nil {
			return nil
		}
		c.out = nil
		for _, t := range targets {
			vms, err := s.store.storageVMs(ctx, t.ID)
			if err != nil {
				continue
			}
			x := noJobTarget{ID: t.ID, Name: t.Name}
			for _, v := range vms {
				if v.NoJob {
					x.VMs = append(x.VMs, v.Name)
					x.Points += len(v.Points)
				}
			}
			if len(x.VMs) > 0 {
				c.out = append(c.out, x)
			}
		}
		c.at, c.ok = now, true
	}
	var out []Attention
	for _, x := range c.out {
		shown := x.VMs
		if len(shown) > 5 {
			shown = append(shown[:5:5], "…")
		}
		out = append(out, Attention{Severity: "warning", Title: x.Name,
			Detail: l.T("backups of %d VMs belong to no job (restore points: %d): %s", len(x.VMs), x.Points, strings.Join(shown, ", ")),
			Link:   fmt.Sprintf("/targets/%d/vms", x.ID)})
	}
	return out
}

// forgetNoJobCache makes the dashboard count again (after a deletion).
func (s *Server) forgetNoJobCache() {
	s.noJob.mu.Lock()
	s.noJob.ok = false
	s.noJob.mu.Unlock()
}

func (s *Server) handleTargetVMs(w http.ResponseWriter, r *http.Request, user string) {
	id, _ := pathID(r)
	t, err := s.store.GetTarget(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	vms, err := s.store.storageVMs(r.Context(), id)
	if err != nil {
		s.serverError(w, err)
		return
	}
	noJob := 0
	for _, v := range vms {
		if v.NoJob {
			noJob++
		}
	}
	s.render(w, r, "target_vms", pageData{Title: "VM backups", Nav: "targets", User: user, Data: map[string]any{
		"Target": t, "VMs": vms, "NoJob": noJob, "Deletable": vmDeleteRefusal(t) == ""}})
}

// vmDeleteRefusal says why the console cannot delete backups on a target
// itself ("" when it can).
func vmDeleteRefusal(t Target) string {
	switch {
	case t.Kind == "local" || t.Kind == "usb":
		return "the storage is a disk of an agent's computer, which the console cannot reach"
	case t.SFTPKey != "":
		return "the console cannot use SSH key sign-in to the storage"
	case t.AgentsOnly:
		return "the storage is reachable by agents only"
	}
	return ""
}

// vmDelete is the request to delete restore points (also the payload of a
// four-eyes request).
type vmDelete struct {
	Runs []int64 `json:"runs"`
	VM   string  `json:"vm"`
}

func (s *Server) handleTargetVMsDelete(w http.ResponseWriter, r *http.Request, user string) {
	ctx := r.Context()
	id, _ := pathID(r)
	back := fmt.Sprintf("/targets/%d/vms", id)
	l := requestLanguage(r)
	t, err := s.store.GetTarget(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if why := vmDeleteRefusal(t); why != "" {
		redirectErr(w, r, back, errors.New(l.T("The console cannot delete backups here: %s.", l.T(why))))
		return
	}
	_ = r.ParseForm()
	var d vmDelete
	for _, v := range r.Form["run"] {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && !slices.Contains(d.Runs, n) {
			d.Runs = append(d.Runs, n)
		}
	}
	if len(d.Runs) == 0 {
		redirectErr(w, r, back, errors.New(l.T("Tick the restore points to delete.")))
		return
	}
	// The VM names come from the restore points, not from the form.
	_, names, err := s.store.vmDeletePoints(ctx, id, d)
	if err != nil {
		redirectErr(w, r, back, errors.New(l.T(err.Error())))
		return
	}
	d.VM = names
	if err := s.confirmIdentity(r); err != nil {
		redirectErr(w, r, back, errors.New(l.T(err.Error())))
		return
	}
	if s.needsApproval(w, r, back, "storage.vm_delete", id, fmt.Sprintf("%s: %s (%d)", t.Name, d.VM, len(d.Runs)), d) {
		return
	}
	if err := s.startVMDelete(ctx, t, d, user, s.guard.ClientIP(r)); err != nil {
		redirectErr(w, r, back, errors.New(l.T(err.Error())))
		return
	}
	redirectMsg(w, r, back, l.T("The restore points are being deleted on the storage; the audit log shows the result."))
}

// vmDeletePoints checks a deletion: every run is a VM backup (or a copy
// of one) on this target, still kept, holding only the one VM or made by
// a job that is gone. It returns the snapshots to remove per repository
// and the names of all VMs whose backups go.
func (s *Store) vmDeletePoints(ctx context.Context, targetID int64, d vmDelete) (map[string][]string, string, error) {
	vms, err := s.storageVMs(ctx, targetID)
	if err != nil {
		return nil, "", err
	}
	type point struct {
		storedPoint
		vm string
	}
	alone := map[int64]point{}
	for _, v := range vms {
		for _, p := range v.Points {
			if _, seen := alone[p.RunID]; !seen && p.CanDelete() { // a shared point under each of its VMs
				alone[p.RunID] = point{p, v.Name}
			}
		}
	}
	out := map[string][]string{}
	var names []string
	for _, id := range d.Runs {
		p, ok := alone[id]
		if !ok {
			return nil, "", errors.New("a restore point is no longer there, or it also holds other VMs: it is removed with the job's retention")
		}
		out[p.Repo] = append(out[p.Repo], p.SnapshotID)
		for _, n := range append([]string{p.vm}, p.OtherVMs...) {
			if !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
	}
	return out, strings.Join(names, ", "), nil
}

// startVMDelete removes the snapshots in the background and records the
// result in the audit log.
func (s *Server) startVMDelete(ctx context.Context, t Target, d vmDelete, by, remote string) error {
	repos, names, err := s.store.vmDeletePoints(ctx, t.ID, d)
	if err != nil {
		return err
	}
	d.VM = names
	_, _ = s.store.AppendAudit(ctx, by, "storage.vm_delete", fmt.Sprintf("%s on %s: %d restore points", d.VM, t.Name, len(d.Runs)), remote)
	s.cleanups.Add(1)
	go func() { // #nosec G118 -- background work that outlives the request on purpose
		defer s.cleanups.Done()
		cctx, cancel := context.WithTimeout(context.Background(), 12*time.Hour)
		defer cancel()
		for url, ids := range repos {
			detail, removed := s.deleteSnapshots(cctx, t, url, ids)
			if len(removed) > 0 {
				s.store.expireTargetSnapshots(cctx, t.ID, url, removed)
			}
			_, _ = s.store.AppendAudit(cctx, by, "storage.vm_backups", fmt.Sprintf("%s, %s on %s: %s", d.VM, url, t.Name, detail), remote)
			s.log.Info("restore points of a VM deleted", "vm", d.VM, "repo", url, "result", detail)
		}
		s.forgetNoJobCache()
	}()
	return nil
}

// deleteSnapshots removes snapshots from a repository of t and frees the
// space only they used. It returns the result for the audit log and the
// IDs removed.
func (s *Server) deleteSnapshots(ctx context.Context, t Target, url string, ids []string) (string, []string) {
	s.cache.drop(url)
	be, err := backend.Open(ctx, url, backend.Options{
		SFTPPassword: t.SFTPPassword, SFTPHostKey: t.SFTPHostKey,
		S3AccessKey: t.S3AccessKey, S3SecretKey: t.S3SecretKey, S3Region: t.S3Region, S3LockDays: t.S3LockDays,
		SMBPassword: t.SMBPassword, SMBDomain: t.SMBDomain,
		HardenedKey: t.HardenedKey, HardenedFingerprint: t.HardenedFingerprint,
		AzureKey: t.AzureKey, AzureSAS: t.AzureSAS,
	})
	if err != nil {
		return "not deleted: " + err.Error(), nil
	}
	r, err := repo.Open(ctx, be, repo.Password(t.RecoveryKey))
	if err != nil {
		_ = be.Close()
		return "not deleted: " + err.Error(), nil
	}
	defer r.Close() // closes the backend too
	lock, err := r.Lock(ctx, true, 2*time.Minute)
	if err != nil {
		return "not deleted: " + err.Error(), nil
	}
	defer lock.Unlock()
	if bad := r.DamagedIndexFiles(); len(bad) > 0 {
		return fmt.Sprintf("not deleted: the repository has %d damaged index files; run repair first", len(bad)), nil
	}
	var removed []string
	for _, id := range ids {
		sid, err := repo.ParseID(id)
		if err != nil {
			continue
		}
		if err := r.RemoveSnapshot(ctx, sid); err != nil {
			if len(removed) == 0 {
				return "not deleted: " + err.Error(), nil
			}
			return fmt.Sprintf("stopped after %d restore points: %v", len(removed), err), removed
		}
		removed = append(removed, id)
	}
	st, err := r.Prune(ctx, repo.PruneOptions{})
	if err != nil {
		return fmt.Sprintf("deleted %d restore points; freeing the space failed: %v", len(removed), err), removed
	}
	return fmt.Sprintf("deleted %d restore points, freed %s", len(removed), humanBytes(st.BytesFreed)), removed
}

// expireTargetSnapshots marks the runs of removed snapshots of one
// repository of a target.
func (s *Store) expireTargetSnapshots(ctx context.Context, targetID int64, url string, ids []string) {
	_, _ = s.db.Exec(ctx, `UPDATE runs SET expired=true WHERE target_id=$1 AND repo_url=$2 AND snapshot_id = ANY($3)`, targetID, url, ids)
}

// executeVMDelete carries out an approved deletion.
func (s *Server) executeVMDelete(ctx context.Context, ap Approval, approver string) error {
	t, err := s.store.GetTarget(ctx, ap.ObjectID)
	if err != nil {
		return err
	}
	var d vmDelete
	if err := json.Unmarshal(ap.Payload, &d); err != nil {
		return err
	}
	if why := vmDeleteRefusal(t); why != "" {
		return errors.New(why)
	}
	by := ap.RequestedBy
	if approver != "" {
		by += ", approved by " + approver
	}
	return s.startVMDelete(ctx, t, d, by, "")
}
