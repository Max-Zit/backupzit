package server_test

import (
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/server"
	"github.com/max-zit/backupzit/internal/uicheck"
)

// TestUIWalk reads every page of the console like a browser (see package
// uicheck): submit buttons inside their forms, unique ids, no template
// leftovers, and every edit page saved unchanged keeps its values.
func TestUIWalk(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	target, _ := e.store.CreateTarget(ctx, server.Target{Name: "local", Kind: "local", URL: t.TempDir()})
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	enroll := func(host, os, inv string) server.Agent {
		cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
		if err != nil {
			t.Fatal(err)
		}
		agents, _ := e.store.ListAgents(ctx)
		for _, a := range agents {
			if a.UUID == cfg.AgentUUID {
				req := api.PollRequest{Hostname: host, OS: os}
				if inv != "" {
					req.Hypervisor = json.RawMessage(inv)
				}
				if err := e.store.TouchAgent(ctx, a.ID, req, ""); err != nil {
					t.Fatal(err)
				}
				a, _ = e.store.GetAgent(ctx, a.ID)
				return a
			}
		}
		t.Fatal("agent missing")
		return server.Agent{}
	}
	win := enroll("hv1", "Windows Server 2022",
		`{"platform":"hyperv","node":"HV1","guests":[{"vmid":1934519093,"id":"cca3c658-bf70-4f80-859c-dd61d115b66f","name":"File Server","type":"vm","node":"HV1","status":"running"}],"storage":["D:\\VMs"]}`)
	pve := enroll("pve1", "Debian GNU/Linux 12", `{"node":"pve1","guests":[{"vmid":100,"name":"web","type":"vm","node":"pve1","status":"running"}],"storage":["local-lvm"]}`)
	lin := enroll("web1", "Debian GNU/Linux 12", "")

	files := []server.Job{
		{Kind: server.JobFiles, AgentID: win.ID, Name: "docs", Paths: []string{`C:\Data`, `{users}\Documents`}, Excludes: []string{"*.tmp"}, Schedule: "0 22 * * 1-5"},
		{Kind: server.JobSystem, AgentID: lin.ID, Name: "web1 system", Excludes: []string{"/var/cache"}, Schedule: "30 1 * * *"},
		{Kind: server.JobVM, AgentID: win.ID, Name: "hyper-v", Paths: []string{"1934519093"}, Schedule: "0 */4 * * *"},
		{Kind: server.JobVM, AgentID: pve.ID, Name: "proxmox all", Paths: []string{"*"}, Schedule: "0 23 * * *"},
		{Kind: server.JobSQL, AgentID: win.ID, Name: "sql", Paths: []string{"shop", "hr"}, Schedule: "0 21 * * *"},
		{Kind: server.JobNAS, AgentID: lin.ID, Name: "nas", Paths: []string{"projects"}, Schedule: "0 20 * * *",
			Options: server.JobOptions{NASURL: "smb://nas01/data", NASUser: "backup", NASPassword: "nas-secret-1"}},
	}
	var first int64
	for i, j := range files {
		j.TargetID, j.Enabled = target, true
		j.Retention.KeepLast, j.Retention.KeepDaily = 3, 7
		j.Options.PreCommand = "echo before"
		id, err := e.store.CreateJob(ctx, j)
		if err != nil {
			t.Fatalf("job %s: %v", j.Name, err)
		}
		if i == 0 {
			first = id
		}
	}
	offsite, _ := e.store.CreateTarget(ctx, server.Target{Name: "offsite", Kind: "local", URL: t.TempDir()})
	if _, err := e.store.CreateJob(ctx, server.Job{Kind: server.JobCopy, SourceJobID: &first, TargetID: offsite, Name: "docs offsite", Enabled: true}); err != nil {
		t.Fatalf("copy job: %v", err)
	}

	c := newClient(t, e)
	if !c.login("admin", "admin-pass-123") {
		t.Fatal("login")
	}
	r := uicheck.Walk(uiClient{c}, uicheck.Options{RoundTrip: true, Skip: []string{"m365_tenant"}})
	for _, p := range r.Problems {
		t.Error(p)
	}
	if r.Pages < 30 || len(r.Saved) < len(files) {
		t.Fatalf("walked only %d pages and saved %d edit pages", r.Pages, len(r.Saved))
	}
	t.Logf("%d pages, %d edit pages sent back unchanged", r.Pages, len(r.Saved))
}

type uiClient struct{ c *client }

func (u uiClient) Do(method, p string, form url.Values) (int, string, string) {
	return u.c.do(method, p, form)
}
