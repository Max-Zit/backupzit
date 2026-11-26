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
// points that hold only one VM; a point shared with other VMs stays.
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
}
