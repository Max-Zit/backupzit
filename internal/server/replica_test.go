package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/backupzit/backupzit/internal/agent"
	"github.com/backupzit/backupzit/internal/api"
	"github.com/backupzit/backupzit/internal/server"
)

func TestReplication(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	target, _ := e.store.CreateTarget(ctx, server.Target{Name: "local", Kind: "local", URL: t.TempDir()})
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	inv := `{"node":"pve","guests":[{"vmid":100,"name":"web","type":"qemu","node":"pve","status":"running"},{"vmid":1100,"name":"web-replica","type":"qemu","node":"pve","status":"stopped"}],"storage":["local-lvm","ceph"]}`
	call := func(method, p string, body any, out any) int {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, e.ts.URL+p, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+cfg.AgentUUID+":"+cfg.Secret)
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if out != nil {
			json.NewDecoder(resp.Body).Decode(out)
		}
		return resp.StatusCode
	}
	poll := func() *api.Run {
		var pr api.PollResponse
		call("POST", api.PathPoll, api.PollRequest{Hostname: "pve", Version: "test", Hypervisor: json.RawMessage(inv)}, &pr)
		return pr.Run
	}
	finish := func(id int64, res api.RunResult) {
		if c := call("POST", fmt.Sprintf("%s%d/finish", api.PathRunsPrefix, id), res, nil); c != 200 {
			t.Fatalf("finish run %d: %d", id, c)
		}
	}
	poll()
	agents, _ := e.store.ListAgents(ctx)
	a := agents[0]

	form := url.Values{"name": {"vms"}, "kind": {"vm"}, "agent_id": {fmt.Sprint(a.ID)}, "target_id": {fmt.Sprint(target)}, "vm_mode": {"selected"},
		"vms": {"100"}, "sched_kind": {"manual"}, "replicate": {"on"}, "replica_agent": {fmt.Sprint(a.ID)}, "replica_storage": {"nfs"}}
	if _, loc, _ := admin.do("POST", "/jobs", form); !strings.Contains(loc, "err=") {
		t.Error("replica storage that the node lacks accepted")
	}
	form.Set("replica_storage", "ceph")
	if _, loc, _ := admin.do("POST", "/jobs", form); !strings.Contains(loc, "msg=") {
		t.Fatalf("create job: %s", loc)
	}
	jobs, _ := e.store.ListJobs(ctx)
	j := jobs[0]
	if j.Options.ReplicaAgentID != a.ID || j.Options.ReplicaStorage != "ceph" {
		t.Fatalf("replica options: %+v", j.Options)
	}

	backup := func(snap string) int64 {
		id, err := e.store.QueueBackup(ctx, j.ID, "manual")
		if err != nil {
			t.Fatal(err)
		}
		if r := poll(); r == nil || r.ID != id {
			t.Fatalf("backup run: %+v", r)
		}
		finish(id, api.RunResult{Status: api.StatusSuccess, SnapshotID: snap,
			Details: json.RawMessage(`{"guests":[{"vmid":100,"type":"qemu","name":"web","disks":1,"platform":"proxmox"}]}`)})
		return id
	}
	snap1, snap2 := strings.Repeat("a1", 32), strings.Repeat("b2", 32)
	backup(snap1)
	r := poll()
	if r == nil || r.Kind != api.KindVMReplica || r.SnapshotID != snap1 || r.Replica == nil || r.Replica.Storage != "ceph" || len(r.Replica.Guests) != 0 {
		t.Fatalf("first replication run: %+v", r)
	}
	finish(r.ID, api.RunResult{Status: api.StatusSuccess,
		Details: json.RawMessage(`{"replicas":[{"vmid":100,"replica_vmid":1100,"name":"web-replica","created":true,"snapshot":"` + snap1 + `"}]}`)})

	backup(snap2)
	r = poll()
	if r == nil || r.Kind != api.KindVMReplica || r.SnapshotID != snap2 || len(r.Replica.Guests) != 1 ||
		r.Replica.Guests[0] != (api.ReplicaState{VMID: 100, ReplicaVMID: 1100, SnapshotID: snap1}) {
		t.Fatalf("second replication run: %+v", r)
	}
	// A failed update keeps the replica but forgets which backup it holds.
	finish(r.ID, api.RunResult{Status: api.StatusFailed, Message: "x",
		Details: json.RawMessage(`{"replicas":[{"vmid":100,"replica_vmid":1100,"name":"web-replica","error":"write failed"}]}`)})

	// Overview: the job list names the protected VMs, the job page lists them.
	if _, _, list := admin.do("GET", "/jobs", nil); !strings.Contains(list, "1 VM: web") || !strings.Contains(list, ">Virtual machines <small>1</small>") {
		t.Error("job list lacks the protected VMs or the kind filter")
	}
	if _, _, dash := admin.do("GET", "/", nil); !strings.Contains(dash, "VMs protected") || !strings.Contains(dash, "Needs attention") || !strings.Contains(dash, "Replication") {
		t.Error("dashboard lacks the protection counts, attention panel or readable run kinds")
	}
	_, _, body := admin.do("GET", fmt.Sprintf("/jobs/%d", j.ID), nil)
	if !strings.Contains(body, "Protected VMs") || !strings.Contains(body, "Restore points") {
		t.Error("job page lacks the protected VMs")
	}
	if !strings.Contains(body, "Replicas") || !strings.Contains(body, "web-replica") || !strings.Contains(body, "write failed") || !strings.Contains(body, "Start replica") {
		t.Error("job page lacks the replicas")
	}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/jobs/%d/replica-start", j.ID), url.Values{"vmid": {"100"}}); !strings.Contains(loc, "/runs/") {
		t.Fatalf("start replica: %s", loc)
	}
	r = poll()
	if r == nil || r.Kind != api.KindVMReplicaStart || r.Replica == nil || len(r.Replica.Guests) != 1 || r.Replica.Guests[0].ReplicaVMID != 1100 {
		t.Fatalf("replica start run: %+v", r)
	}
	finish(r.ID, api.RunResult{Status: api.StatusSuccess})
	backup(strings.Repeat("c3", 32))
	if r = poll(); r == nil || r.Kind != api.KindVMReplica || r.Replica.Guests[0].SnapshotID != "" {
		t.Fatalf("replication after a failed update: %+v", r)
	}
}
