package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/pve"
	"github.com/max-zit/backupzit/internal/server"
)

// #59 (0.58): a VMware VM job can take its snapshots without VMware Tools
// quiescing. The form shows and keeps the option, the proxy agent gets it,
// and an agent older than 0.58.0 (which would still quiesce) is refused with
// a reason.
func TestVMwareNoQuiesce(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	admin := newClient(t, e)
	if !admin.login("admin", "admin-pass-123") {
		t.Fatal("login")
	}
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "0.57.0")
	if err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	agentID := agents[0].ID
	poll := func(version string) *api.Run {
		t.Helper()
		b, _ := json.Marshal(api.PollRequest{Hostname: "proxy1", OS: "Linux", Arch: "amd64", Version: version})
		hr, _ := http.NewRequest("POST", e.ts.URL+api.PathPoll, bytes.NewReader(b))
		hr.Header.Set("Authorization", "Bearer "+cfg.AgentUUID+":"+cfg.Secret)
		resp, err := e.ts.Client().Do(hr)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var pr api.PollResponse
		_ = json.NewDecoder(resp.Body).Decode(&pr)
		return pr.Run
	}
	poll("0.57.0")
	inv, _ := json.Marshal(pve.Inventory{Platform: "vmware", Node: "esx1", Version: "ESXi 6.7.0", Guests: []pve.Guest{
		{VMID: 10, ID: "50211111-2222-3333-4444-555555555555", Name: "SRV-DC1", Node: "esx1"},
	}})
	hostID, err := e.store.CreateVMwareHost(ctx, server.VMwareHost{Name: "esx1", Address: "esx1.example", Username: "root", Password: "x",
		ProxyAgentID: &agentID, RepoDir: "esxi-esx1_0001", Inventory: inv, Version: "6.7.0", APIWrites: true})
	if err != nil {
		t.Fatal(err)
	}
	target, _ := e.store.CreateTarget(ctx, server.Target{Name: "local", Kind: "local", URL: t.TempDir()})
	jobID, err := e.store.CreateJob(ctx, server.Job{Kind: server.JobVM, AgentID: agentID, TargetID: target, Name: "dc",
		Paths: []string{"10"}, Enabled: true, VMwareHostID: &hostID, Options: server.JobOptions{VMwareNoQuiesce: true}})
	if err != nil {
		t.Fatal(err)
	}

	_, _, page := admin.do("GET", fmt.Sprintf("/jobs/%d/edit", jobID), nil)
	if !strings.Contains(page, `name="vmware_no_quiesce" checked`) {
		t.Error("edit form does not show the option as on")
	}
	if strings.Contains(page, `name="app_consistent"`) {
		t.Error("the Hyper-V checkpoint option is shown for a VMware job")
	}

	// An agent older than 0.58.0 is refused with a reason.
	rid, _ := e.store.QueueBackup(ctx, jobID, "manual")
	if run := poll("0.57.0"); run != nil {
		t.Fatalf("old agent got the run: %+v", run)
	}
	if r, _ := e.store.GetRun(ctx, rid); r.Status != api.StatusFailed || !strings.Contains(r.Message, "cannot take snapshots without quiescing yet") {
		t.Errorf("old agent: %s %q", r.Status, r.Message)
	}

	// 0.58.0 gets the option with the run.
	poll("0.58.0")
	if _, err := e.store.QueueBackup(ctx, jobID, "manual"); err != nil {
		t.Fatal(err)
	}
	run := poll("0.58.0")
	if run == nil || run.Kind != api.KindVMBackup || !run.VMwareNoQuiesce {
		t.Fatalf("run: %+v", run)
	}

	// Off again: the box is shown unticked.
	j, _ := e.store.GetJob(ctx, jobID)
	j.Options.VMwareNoQuiesce = false
	if err := e.store.UpdateJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	_, _, page = admin.do("GET", fmt.Sprintf("/jobs/%d/edit", jobID), nil)
	if strings.Contains(page, `name="vmware_no_quiesce" checked`) || !strings.Contains(page, `name="vmware_no_quiesce"`) {
		t.Error("edit form after switching the option off")
	}
}
