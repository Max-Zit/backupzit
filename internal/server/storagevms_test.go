package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/runinfo"
	"github.com/max-zit/backupzit/internal/server"
	"github.com/max-zit/backupzit/internal/testutil"
)

// #73: the storage page lists every VM with restore points, marks VMs no
// job backs up any more (also on the dashboard), and deletes restore
// points that hold only one VM; a point shared with other VMs stays while
// its job exists (#79: with the job gone it goes, with all its VMs).
func TestStorageVMs(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	s3, err := testutil.StartS3Server("backups")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s3.Close)
	targetID, err := e.store.CreateTarget(ctx, server.Target{Name: "offsite", Kind: "s3", URL: "s3://" + s3.Host + "/backups/vms?tls=false", S3AccessKey: "AK", S3SecretKey: "SK"})
	if err != nil {
		t.Fatal(err)
	}
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	if _, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "0.62.0"); err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	a := agents[0]
	inv := `{"platform":"proxmox","node":"pve1","guests":[{"vmid":101,"name":"web","type":"qemu","node":"pve1"},{"vmid":102,"name":"db","type":"qemu","node":"pve1"},{"vmid":103,"name":"old-erp","type":"qemu","node":"pve1"}],"storage":["local"]}`
	if err := e.store.TouchAgent(ctx, a.ID, api.PollRequest{Hostname: "pve1", OS: "Proxmox VE 8", Hypervisor: json.RawMessage(inv)}, ""); err != nil {
		t.Fatal(err)
	}
	job1, err := e.store.CreateJob(ctx, server.Job{AgentID: a.ID, TargetID: targetID, Name: "web vms", Kind: server.JobVM, Paths: []string{"101", "102"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	job2, err := e.store.CreateJob(ctx, server.Job{AgentID: a.ID, TargetID: targetID, Name: "old vm", Kind: server.JobVM, Paths: []string{"103"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	var r *repo.Repository
	var repoURL string
	runOf := map[string]int64{} // snapshot name -> run
	backup := func(name string, jobID int64, guests ...repo.Guest) {
		t.Helper()
		runID, _ := e.store.QueueBackup(ctx, jobID, "manual")
		run, err := e.store.ClaimRun(ctx, a.ID)
		if err != nil || run == nil || run.ID != runID {
			t.Fatalf("claim: %+v %v", run, err)
		}
		if r == nil {
			repoURL = run.RepoURL
			be, err := backend.Open(ctx, run.RepoURL, backend.Options{S3AccessKey: "AK", S3SecretKey: "SK"})
			if err != nil {
				t.Fatal(err)
			}
			if r, err = repo.Init(ctx, be, repo.Password("")); err != nil {
				t.Fatal(err)
			}
		}
		data, _, err := r.SaveBlob(ctx, repo.DataBlob, []byte(strings.Repeat("disk of "+name+" ", 4000)))
		if err != nil {
			t.Fatal(err)
		}
		tree, err := r.SaveTree(ctx, &repo.Tree{Nodes: []repo.Node{{Name: "disk", Type: repo.NodeFile, Size: 1, Content: []repo.ID{data}}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		sn := &repo.Snapshot{Time: time.Now(), Hostname: "pve1", Tree: tree, Guests: guests, Tags: []string{fmt.Sprintf("job:%d", jobID)}}
		if _, err := r.SaveSnapshot(ctx, sn); err != nil {
			t.Fatal(err)
		}
		if err := e.store.FinishRun(ctx, a.ID, runID, api.RunResult{Status: api.StatusSuccess, SnapshotID: sn.ID.String(), Details: runinfo.Guests(sn)}); err != nil {
			t.Fatal(err)
		}
		runOf[name] = runID
		time.Sleep(10 * time.Millisecond)
	}
	web := repo.Guest{Platform: "proxmox", Type: "qemu", VMID: 101, Name: "web"}
	db := repo.Guest{Platform: "proxmox", Type: "qemu", VMID: 102, Name: "db"}
	old := repo.Guest{Platform: "proxmox", Type: "qemu", VMID: 103, Name: "old-erp"}
	backup("web1", job1, web)
	backup("both", job1, web, db)
	backup("old1", job2, old)
	backup("old2", job2, old)
	_ = r.Close()
	if err := e.store.DeleteJob(ctx, job2); err != nil { // its backups stay on the storage
		t.Fatal(err)
	}
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	page := func() string {
		code, _, b := admin.do("GET", fmt.Sprintf("/targets/%d/vms", targetID), nil)
		if code != 200 {
			t.Fatalf("page: %d", code)
		}
		return b
	}
	b := page()
	for _, want := range []string{"web", "db", "old-erp", "1 VMs without a job", "also holds 1 other VMs"} {
		if !strings.Contains(b, want) && !strings.Contains(b, strings.Replace(want, "VMs", "VM", 1)) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Count(b, ">no job<") != 1 || strings.Index(b, "old-erp") > strings.Index(b, ">web<") {
		t.Error("the VM without a job is not marked, or not listed first")
	}
	for _, name := range []string{"web1", "old1", "old2"} {
		if !strings.Contains(b, fmt.Sprintf(`name="run" value="%d"`, runOf[name])) {
			t.Errorf("no checkbox for %s, which holds one VM", name)
		}
	}
	if strings.Contains(b, fmt.Sprintf(`name="run" value="%d"`, runOf["both"])) {
		t.Error("a restore point of two VMs can be ticked")
	}
	_, _, dash := admin.do("GET", "/", nil)
	if !strings.Contains(dash, "belong to no job") || !strings.Contains(dash, "old-erp") {
		t.Error("the dashboard does not list the VM without a job")
	}

	del := func(runs ...int64) string {
		form := url.Values{"vm": {"old-erp"}, "reauth_password": {"admin-pass-123"}}
		for _, id := range runs {
			form.Add("run", fmt.Sprint(id))
		}
		_, loc, _ := admin.do("POST", fmt.Sprintf("/targets/%d/vms/delete", targetID), form)
		return loc
	}
	if loc := del(runOf["both"]); !strings.Contains(loc, "err=") {
		t.Errorf("deleting a shared restore point: %s", loc)
	}
	form := url.Values{"vm": {"old-erp"}, "run": {fmt.Sprint(runOf["old1"])}, "reauth_password": {"wrong"}}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/targets/%d/vms/delete", targetID), form); !strings.Contains(loc, "err=") {
		t.Errorf("deleting with a wrong password: %s", loc)
	}
	if loc := del(runOf["old1"], runOf["old2"]); !strings.Contains(loc, "msg=") {
		t.Fatalf("delete: %s", loc)
	}
	deadline := time.Now().Add(30 * time.Second)
	var result string
	for result == "" && time.Now().Before(deadline) {
		entries, _ := e.store.ListAudit(ctx, 50)
		for _, x := range entries {
			if x.Action == "storage.vm_backups" {
				result = x.Detail
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(result, "deleted 2 restore points, freed") {
		t.Fatalf("result %q", result)
	}
	be, _ := backend.Open(ctx, repoURL, backend.Options{S3AccessKey: "AK", S3SecretKey: "SK"})
	r2, err := repo.Open(context.Background(), be, repo.Password(""))
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	sns, _ := r2.ListSnapshots(ctx)
	if len(sns) != 2 {
		t.Fatalf("%d snapshots left, want 2 (web1, both)", len(sns))
	}
	for _, sn := range sns {
		for _, g := range sn.Guests {
			if g.Name == "old-erp" {
				t.Error("a deleted restore point is still there")
			}
		}
	}
	if b := page(); strings.Contains(b, "old-erp") {
		t.Error("the deleted VM is still listed")
	}
	if run, _ := e.store.GetRun(ctx, runOf["old1"]); !run.Expired {
		t.Error("the run of a deleted restore point is not marked expired")
	}
	// The job's selection no longer has db: db has no job now, but its only
	// restore point also holds web, so it cannot be deleted from here.
	if _, err := e.pool.Exec(ctx, `UPDATE jobs SET paths='{101}' WHERE id=$1`, job1); err != nil {
		t.Fatal(err)
	}
	if b := page(); strings.Count(b, ">no job<") != 1 || strings.Contains(b, fmt.Sprintf(`name="run" value="%d"`, runOf["both"])) {
		t.Error("db without a job: not marked, or its shared point can be ticked")
	}
	// A viewer sees the list but no delete form.
	if _, err := e.store.CreateUser(ctx, server.User{Username: "viewer1", DisplayName: "Viewer", Role: "viewer"}, "viewer-pass-123"); err != nil {
		t.Fatal(err)
	}
	viewer := newClient(t, e)
	viewer.login("viewer1", "viewer-pass-123")
	if code, _, b := viewer.do("GET", fmt.Sprintf("/targets/%d/vms", targetID), nil); code != 200 || strings.Contains(b, `name="run"`) {
		t.Errorf("viewer: %d, form shown %v", code, strings.Contains(b, `name="run"`))
	}
	if code, _, _ := viewer.do("POST", fmt.Sprintf("/targets/%d/vms/delete", targetID), url.Values{"run": {fmt.Sprint(runOf["web1"])}}); code == 303 || code == 200 {
		if run, _ := e.store.GetRun(ctx, runOf["web1"]); run.Expired {
			t.Error("a viewer deleted a restore point")
		}
	}

	// #79: a restore point of several VMs whose job was deleted. No retention
	// removes it any more, so it can be deleted here, with the backups of all
	// its VMs, which the page names.
	job3, err := e.store.CreateJob(ctx, server.Job{AgentID: a.ID, TargetID: targetID, Name: "pair", Kind: server.JobVM, Paths: []string{"104", "105"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	be3, _ := backend.Open(ctx, repoURL, backend.Options{S3AccessKey: "AK", S3SecretKey: "SK"})
	if r, err = repo.Open(ctx, be3, repo.Password("")); err != nil {
		t.Fatal(err)
	}
	app1 := repo.Guest{Platform: "proxmox", Type: "qemu", VMID: 104, Name: "app1"}
	app2 := repo.Guest{Platform: "proxmox", Type: "qemu", VMID: 105, Name: "app2"}
	backup("pair", job3, app1, app2)
	_ = r.Close()
	box := func(name string) string { return fmt.Sprintf(`name="run" value="%d"`, runOf[name]) }
	if b := page(); strings.Contains(b, box("pair")) {
		t.Error("a shared restore point of an existing job can be ticked")
	}
	if err := e.store.DeleteJob(ctx, job3); err != nil {
		t.Fatal(err)
	}
	b = page()
	if strings.Count(b, box("pair")) != 2 || !strings.Contains(b, "deleting it also deletes the backups of app2") || !strings.Contains(b, "deleting it also deletes the backups of app1") {
		t.Errorf("the shared point of a deleted job: %d checkboxes, VMs named %v", strings.Count(b, box("pair")), strings.Contains(b, "deletes the backups of app2"))
	}
	if strings.Contains(b, box("both")) {
		t.Error("a shared restore point of an existing job can be ticked")
	}
	form = url.Values{"run": {fmt.Sprint(runOf["pair"])}, "reauth_password": {"admin-pass-123"}}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/targets/%d/vms/delete", targetID), form); !strings.Contains(loc, "msg=") {
		t.Fatalf("delete the shared point of a deleted job: %s", loc)
	}
	result, deadline = "", time.Now().Add(30*time.Second)
	for result == "" && time.Now().Before(deadline) {
		entries, _ := e.store.ListAudit(ctx, 50)
		for _, x := range entries {
			if x.Action == "storage.vm_backups" && strings.HasPrefix(x.Detail, "app1, app2,") {
				result = x.Detail
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(result, "deleted 1 restore points, freed") {
		t.Fatalf("result %q", result)
	}
	if b := page(); strings.Contains(b, "app1") || strings.Contains(b, "app2") {
		t.Error("the VMs of the deleted point are still listed")
	}
	if run, _ := e.store.GetRun(ctx, runOf["pair"]); !run.Expired {
		t.Error("the run of the deleted point is not marked expired")
	}
	if run, _ := e.store.GetRun(ctx, runOf["both"]); run.Expired {
		t.Error("the shared point of the existing job went too")
	}
}

// vmPointsEnv is a Proxmox VM job with three single-VM restore points on
// S3 storage.
type vmPointsEnv struct {
	e        *env
	s3Host   string
	agentID  int64
	targetID int64
	jobID    int64
	repoURL  string
	runs     []string
	snaps    []string
	admin    *client
}

func newVMPointsEnv(t *testing.T) *vmPointsEnv {
	e := setup(t)
	ctx := e.ctx
	s3, err := testutil.StartS3Server("backups")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s3.Close)
	x := &vmPointsEnv{e: e, s3Host: s3.Host}
	x.targetID, err = e.store.CreateTarget(ctx, server.Target{Name: "offsite", Kind: "s3", URL: "s3://" + s3.Host + "/backups/vms?tls=false", S3AccessKey: "AK", S3SecretKey: "SK"})
	if err != nil {
		t.Fatal(err)
	}
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	if _, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "0.62.0"); err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	x.agentID = agents[0].ID
	inv := `{"platform":"proxmox","node":"pve1","guests":[{"vmid":101,"name":"web","type":"qemu","node":"pve1"}],"storage":["local"]}`
	if err := e.store.TouchAgent(ctx, x.agentID, api.PollRequest{Hostname: "pve1", OS: "Proxmox VE 8", Hypervisor: json.RawMessage(inv)}, ""); err != nil {
		t.Fatal(err)
	}
	x.jobID, err = e.store.CreateJob(ctx, server.Job{AgentID: x.agentID, TargetID: x.targetID, Name: "web vm", Kind: server.JobVM, Paths: []string{"101"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	var r *repo.Repository
	web := repo.Guest{Platform: "proxmox", Type: "qemu", VMID: 101, Name: "web"}
	for i := 0; i < 3; i++ {
		runID, _ := e.store.QueueBackup(ctx, x.jobID, "manual")
		run, err := e.store.ClaimRun(ctx, x.agentID)
		if err != nil || run == nil || run.ID != runID {
			t.Fatalf("claim: %+v %v", run, err)
		}
		if r == nil {
			x.repoURL = run.RepoURL
			be, err := backend.Open(ctx, run.RepoURL, backend.Options{S3AccessKey: "AK", S3SecretKey: "SK"})
			if err != nil {
				t.Fatal(err)
			}
			if r, err = repo.Init(ctx, be, repo.Password("")); err != nil {
				t.Fatal(err)
			}
		}
		data, _, err := r.SaveBlob(ctx, repo.DataBlob, []byte(strings.Repeat(fmt.Sprintf("disk %d ", i), 4000)))
		if err != nil {
			t.Fatal(err)
		}
		tree, err := r.SaveTree(ctx, &repo.Tree{Nodes: []repo.Node{{Name: "disk", Type: repo.NodeFile, Size: 1, Content: []repo.ID{data}}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		sn := &repo.Snapshot{Time: time.Now(), Hostname: "pve1", Tree: tree, Guests: []repo.Guest{web}, Tags: []string{fmt.Sprintf("job:%d", x.jobID)}}
		if _, err := r.SaveSnapshot(ctx, sn); err != nil {
			t.Fatal(err)
		}
		if err := e.store.FinishRun(ctx, x.agentID, runID, api.RunResult{Status: api.StatusSuccess, SnapshotID: sn.ID.String(), Details: runinfo.Guests(sn)}); err != nil {
			t.Fatal(err)
		}
		x.runs, x.snaps = append(x.runs, fmt.Sprint(runID)), append(x.snaps, sn.ID.String())
		time.Sleep(10 * time.Millisecond)
	}
	_ = r.Close()
	x.admin = newClient(t, e)
	x.admin.login("admin", "admin-pass-123")
	return x
}

// del asks to delete one restore point on a target and returns the kind
// ("err" or "msg") and the text of the answer.
func (x *vmPointsEnv) del(t *testing.T, target int64, run string) (kind, text string) {
	t.Helper()
	_, loc, _ := x.admin.do("POST", fmt.Sprintf("/targets/%d/vms/delete", target), url.Values{"run": {run}, "reauth_password": {"admin-pass-123"}})
	u, _ := url.Parse(loc)
	if m := u.Query().Get("err"); m != "" {
		return "err", m
	}
	return "msg", u.Query().Get("msg")
}

func (x *vmPointsEnv) expired(run string) bool {
	var v bool
	x.e.pool.QueryRow(x.e.ctx, `SELECT expired FROM runs WHERE id=$1::bigint`, run).Scan(&v)
	return v
}

// vmBackupsResult waits for the audit entry with the result of a deletion
// in the repository.
func (x *vmPointsEnv) vmBackupsResult(t *testing.T, after int) string {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		entries, _ := x.e.store.ListAudit(x.e.ctx, 100)
		n := 0
		for i := len(entries) - 1; i >= 0; i-- {
			if entries[i].Action == "storage.vm_backups" {
				if n++; n > after {
					return entries[i].Detail
				}
			}
		}
	}
	t.Fatal("no result of the deletion in the audit log")
	return ""
}

// U4, M-N13(a): restore points are not deleted from the storage page while
// retention is paused after a suspicious backup (of the job, or of the
// source job of a copy) or while a VM runs from a backup in the same
// repository (instant recovery), also not when a four-eyes approval is
// carried out later.
func TestStorageVMsDeleteRefusals(t *testing.T) {
	x := newVMPointsEnv(t)
	e, ctx := x.e, x.e.ctx

	// Retention paused after a suspicious backup.
	e.pool.Exec(ctx, `UPDATE jobs SET retention_hold=true WHERE id=$1`, x.jobID)
	if kind, text := x.del(t, x.targetID, x.runs[0]); kind != "err" || !strings.Contains(text, "Retention is paused") {
		t.Errorf("deleted while retention is paused: %s %q", kind, text)
	}
	e.pool.Exec(ctx, `UPDATE jobs SET retention_hold=false WHERE id=$1`, x.jobID)

	// A VM runs from the newest point: no point of the repository goes,
	// since the prune after it could take data the VM still reads.
	var instantID int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, target_id, snapshot_id, status, details, finished_at)
		VALUES($1,$2,'vm-instant','manual',$3,$4,$5,'success','{"state":"running"}',now()) RETURNING id`, x.agentID, x.jobID, x.repoURL, x.targetID, x.snaps[2]).Scan(&instantID); err != nil {
		t.Fatal(err)
	}
	for _, run := range []string{x.runs[2], x.runs[0]} {
		if kind, text := x.del(t, x.targetID, run); kind != "err" || !strings.Contains(text, "instant recovery") {
			t.Errorf("deleted while a VM runs from the repository: %s %q", kind, text)
		}
	}
	e.pool.Exec(ctx, `UPDATE runs SET details='{"state":"finished"}' WHERE id=$1`, instantID)

	// With four-eyes approval on, the check runs again when the request is
	// approved: retention was paused in between.
	const pw = "correct-horse-9"
	x.admin.do("POST", "/users", url.Values{"reauth_password": {"admin-pass-123"}, "username": {"admin2"}, "role": {"admin"}, "password": {pw}, "password2": {pw}})
	if _, loc, _ := x.admin.do("POST", "/settings/four-eyes", url.Values{"four_eyes": {"on"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("four-eyes: %s", loc)
	}
	if kind, text := x.del(t, x.targetID, x.runs[0]); kind != "msg" || !strings.Contains(text, "Waiting") {
		t.Fatalf("not held for approval: %s %q", kind, text)
	}
	e.pool.Exec(ctx, `UPDATE jobs SET retention_hold=true WHERE id=$1`, x.jobID)
	var apID int64
	e.pool.QueryRow(ctx, `SELECT id FROM approvals WHERE status='pending' ORDER BY id DESC LIMIT 1`).Scan(&apID)
	admin2 := newClient(t, e)
	admin2.login("admin2", pw)
	if _, loc, _ := admin2.do("POST", fmt.Sprintf("/approvals/%d/approve", apID), url.Values{"reauth_password": {pw}}); !strings.Contains(loc, "err=") {
		t.Errorf("approved deletion carried out while retention is paused: %s", loc)
	}
	var status, note string
	e.pool.QueryRow(ctx, `SELECT status, note FROM approvals WHERE id=$1`, apID).Scan(&status, &note)
	if status != "failed" || !strings.Contains(note, "Retention is paused") {
		t.Errorf("approval %s %q", status, note)
	}
	time.Sleep(300 * time.Millisecond)
	if x.expired(x.runs[0]) {
		t.Fatal("a held restore point was deleted")
	}

	// A copy of a VM backup on another storage: its source job's retention
	// is paused, so the copy stays too.
	copyTarget, err := e.store.CreateTarget(ctx, server.Target{Name: "copies", Kind: "s3", URL: "s3://" + x.s3Host + "/backups/copies?tls=false", S3AccessKey: "AK", S3SecretKey: "SK"})
	if err != nil {
		t.Fatal(err)
	}
	if _, loc, _ := x.admin.do("POST", "/jobs", url.Values{"name": {"vm copy"}, "kind": {"copy"}, "source_job": {fmt.Sprint(x.jobID)},
		"target_id": {fmt.Sprint(copyTarget)}, "sched_kind": {"manual"}, "keep_last": {"2"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("copy job: %s", loc)
	}
	var copyRun string
	if err := e.pool.QueryRow(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, target_id, snapshot_id, status, details, finished_at)
		SELECT agent_id, (SELECT id FROM jobs WHERE name='vm copy'), 'copy', 'manual', 'copy-repo', $1, $2, 'success', details, now() FROM runs WHERE id=$3::bigint
		RETURNING id::text`, copyTarget, strings.Repeat("c", 64), x.runs[1]).Scan(&copyRun); err != nil {
		t.Fatal(err)
	}
	if kind, text := x.del(t, copyTarget, copyRun); kind != "err" || !strings.Contains(text, "Retention is paused") {
		t.Errorf("a copy deleted while its source job's retention is paused: %s %q", kind, text)
	}

	// Resumed: the request goes through as before.
	e.pool.Exec(ctx, `UPDATE jobs SET retention_hold=false WHERE id=$1`, x.jobID)
	if kind, text := x.del(t, x.targetID, x.runs[0]); kind != "msg" || !strings.Contains(text, "Waiting") {
		t.Fatalf("not held for approval: %s %q", kind, text)
	}
	e.pool.QueryRow(ctx, `SELECT id FROM approvals WHERE status='pending' ORDER BY id DESC LIMIT 1`).Scan(&apID)
	if _, loc, _ := admin2.do("POST", fmt.Sprintf("/approvals/%d/approve", apID), url.Values{"reauth_password": {pw}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("approve: %s", loc)
	}
	if res := x.vmBackupsResult(t, 0); !strings.Contains(res, "deleted 1 restore points") || !x.expired(x.runs[0]) {
		t.Errorf("the approved deletion: %q", res)
	}
}
