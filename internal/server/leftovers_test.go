package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/leftover"
	"github.com/max-zit/backupzit/internal/pve"
	"github.com/max-zit/backupzit/internal/server"
)

// S-3 (0.56): agents report what their backups left on a hypervisor; the
// VMware inventory shows BackupZit snapshots of its own. The dashboard
// lists both with Remove now, which queues a clean-up run that carries the
// host's credentials and the snapshots found in the inventory.
func TestLeftovers(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	admin := newClient(t, e)
	if !admin.login("admin", "admin-pass-123") {
		t.Fatal("login")
	}
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "0.56.0")
	if err != nil {
		t.Fatal(err)
	}
	var agentID int64
	agents, _ := e.store.ListAgents(ctx)
	for _, a := range agents {
		if a.UUID == cfg.AgentUUID {
			agentID = a.ID
		}
	}
	poll := func(items []leftover.Item) *api.Run {
		t.Helper()
		req := api.PollRequest{Hostname: "proxy1", OS: "Linux", Arch: "amd64", Version: "0.56.0"}
		if items != nil {
			req.Leftovers, _ = json.Marshal(items)
		}
		b, _ := json.Marshal(req)
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
	const vmUUID = "50211111-2222-3333-4444-555555555555"
	old := time.Now().Add(-30 * time.Hour).UTC()
	inv, _ := json.Marshal(pve.Inventory{Platform: "vmware", Node: "esx1", Version: "ESXi 8.0.3", Guests: []pve.Guest{
		{VMID: 10, ID: vmUUID, Name: "web", Node: "esx1"},
		{VMID: 11, ID: "50219999-2222-3333-4444-555555555555", Name: "db", Node: "esx1", BackupSnapshots: []pve.SnapRef{
			{Name: "backupzit_1700000000", Created: old},
			{Name: "backupzit_9999999999", Created: time.Now().UTC()}, // a backup's own, too young
		}},
	}})
	hostID, err := e.store.CreateVMwareHost(ctx, server.VMwareHost{Name: "esx1", Address: "esx1.example", Username: "root", Password: "x",
		ProxyAgentID: &agentID, RepoDir: "esxi-esx1_0001", Inventory: inv, Version: "8.0.3", APIWrites: true})
	if err != nil {
		t.Fatal(err)
	}

	// The agent reports a snapshot it could not remove.
	poll([]leftover.Item{{Platform: leftover.VMware, Kind: leftover.KindSnapshot, Host: "esx1.example", VM: vmUUID, VMName: "web",
		Name: "backupzit_1763600000", Created: old, Tries: 2, Error: "task timed out"}})
	_, _, page := admin.do("GET", "/", nil)
	page = html.UnescapeString(page)
	for _, want := range []string{"backup snapshot backupzit_1763600000 left on esx1", "removing it failed", "task timed out",
		"backup snapshot backupzit_1700000000", "found in the VMware inventory", "Remove now"} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard lacks %q", want)
		}
	}
	if strings.Contains(page, "backupzit_9999999999") {
		t.Error("the snapshot of a running backup is listed as a leftover")
	}

	// Remove now for the agent's item: a clean-up run with the host.
	q := url.Values{"agent": {fmt.Sprint(agentID)}, "host": {fmt.Sprint(hostID)}}
	if code, loc, _ := admin.do("POST", "/leftovers/clean?"+q.Encode(), url.Values{}); code != http.StatusSeeOther || !strings.HasPrefix(loc, "/runs/") {
		t.Fatalf("remove now: %d %s", code, loc)
	}
	if code, loc, _ := admin.do("POST", "/leftovers/clean?"+q.Encode(), url.Values{}); code != http.StatusSeeOther || strings.HasPrefix(loc, "/runs/") {
		t.Fatalf("a second clean-up was queued: %d %s", code, loc)
	}
	run := poll(nil)
	if run == nil || run.Kind != api.KindLeftoverCleanup || run.VMware == nil || run.VMware.Address != "esx1.example" || len(run.Cleanup) != 0 {
		t.Fatalf("clean-up run %+v", run)
	}
	finish := func(id int64) {
		b, _ := json.Marshal(api.RunResult{Status: api.StatusSuccess, Message: "Removed 1 leftover"})
		hr, _ := http.NewRequest("POST", fmt.Sprintf("%s%s%d/finish", e.ts.URL, api.PathRunsPrefix, id), bytes.NewReader(b))
		hr.Header.Set("Authorization", "Bearer "+cfg.AgentUUID+":"+cfg.Secret)
		resp, err := e.ts.Client().Do(hr)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	finish(run.ID)

	// The snapshot found in the inventory: named in the run.
	q.Set("vm", "50219999-2222-3333-4444-555555555555")
	q.Set("name", "backupzit_1700000000")
	if code, loc, _ := admin.do("POST", "/leftovers/clean?"+q.Encode(), url.Values{}); code != http.StatusSeeOther || !strings.HasPrefix(loc, "/runs/") {
		t.Fatalf("remove now (inventory): %d %s", code, loc)
	}
	run = poll([]leftover.Item{})
	if run == nil || len(run.Cleanup) != 1 || run.Cleanup[0].Name != "backupzit_1700000000" || run.Cleanup[0].VM != "50219999-2222-3333-4444-555555555555" {
		t.Fatalf("clean-up run of the inventory snapshot %+v", run)
	}
	finish(run.ID)
	// Only BackupZit snapshots.
	q.Set("name", "before-upgrade")
	if _, loc, _ := admin.do("POST", "/leftovers/clean?"+q.Encode(), url.Values{}); strings.HasPrefix(loc, "/runs/") {
		t.Fatal("a snapshot of the user was queued for removal")
	}

	// The agent's list is empty now: only the inventory one is left.
	_, _, page = admin.do("GET", "/", nil)
	page = html.UnescapeString(page)
	if strings.Contains(page, "backupzit_1763600000") || !strings.Contains(page, "backupzit_1700000000") {
		t.Error("the dashboard did not follow the agent's new list")
	}

	// A viewer sees the leftover but gets no Remove now.
	if _, err := e.store.CreateUser(ctx, server.User{Username: "viewer1", DisplayName: "Viewer", Role: "viewer"}, "viewer-pass-123"); err != nil {
		t.Fatal(err)
	}
	viewer := newClient(t, e)
	if !viewer.login("viewer1", "viewer-pass-123") {
		t.Fatal("viewer login")
	}
	_, _, page = viewer.do("GET", "/", nil)
	if strings.Contains(page, "Remove now") || !strings.Contains(html.UnescapeString(page), "backupzit_1700000000") {
		t.Error("viewer: Remove now shown or leftover missing")
	}
	if code, loc, _ := viewer.do("POST", "/leftovers/clean?"+q.Encode(), url.Values{}); code == http.StatusSeeOther && strings.HasPrefix(loc, "/runs/") {
		t.Error("a viewer queued a clean-up")
	}
}

// U3: any agent can report a leftover with any address. Only a proxy of
// the connection gets a clean-up with the host's credentials: an item of
// another agent is shown without a run, and a clean-up run that reaches an
// agent that is not (or no longer) a proxy fails without the credentials.
func TestLeftoverOfAnotherAgent(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	type ag struct {
		cfg *agent.Config
		id  int64
	}
	enroll := func(name string) ag {
		token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
		cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "0.56.0")
		if err != nil {
			t.Fatal(err)
		}
		agents, _ := e.store.ListAgents(ctx)
		for _, a := range agents {
			if a.UUID == cfg.AgentUUID {
				return ag{cfg, a.ID}
			}
		}
		t.Fatal(name, "not enrolled")
		return ag{}
	}
	poll := func(a ag, items []leftover.Item) *api.Run {
		t.Helper()
		req := api.PollRequest{Hostname: fmt.Sprintf("agent%d", a.id), OS: "Linux", Arch: "amd64", Version: "0.56.0"}
		if items != nil {
			req.Leftovers, _ = json.Marshal(items)
		}
		b, _ := json.Marshal(req)
		hr, _ := http.NewRequest("POST", e.ts.URL+api.PathPoll, bytes.NewReader(b))
		hr.Header.Set("Authorization", "Bearer "+a.cfg.AgentUUID+":"+a.cfg.Secret)
		resp, err := e.ts.Client().Do(hr)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var pr api.PollResponse
		_ = json.NewDecoder(resp.Body).Decode(&pr)
		return pr.Run
	}
	proxy, other := enroll("proxy"), enroll("other")
	hostID, err := e.store.CreateVMwareHost(ctx, server.VMwareHost{Name: "esx1", Address: "esx1.example", Username: "root", Password: "secret-esx-pw",
		ProxyAgentID: &proxy.id, RepoDir: "esxi-esx1_0001", Version: "8.0.3", APIWrites: true})
	if err != nil {
		t.Fatal(err)
	}
	item := leftover.Item{Platform: leftover.VMware, Kind: leftover.KindSnapshot, Host: "ESX1.example", VM: "50211111-2222-3333-4444-555555555555",
		VMName: "web", Name: "backupzit_1763600000", Created: time.Now().Add(-2 * time.Hour)}
	poll(other, []leftover.Item{item})
	poll(proxy, []leftover.Item{})
	cleanups := func(agentID int64) int {
		var n int
		e.pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE agent_id=$1 AND kind='leftover-cleanup'`, agentID).Scan(&n)
		return n
	}
	e.store.AutoLeftoverCleanup(ctx, time.Now())
	if n := cleanups(other.id); n != 0 {
		t.Fatalf("%d clean-up runs queued for an agent that is not a proxy of the host", n)
	}
	admin := newClient(t, e)
	if !admin.login("admin", "admin-pass-123") {
		t.Fatal("login")
	}
	_, _, page := admin.do("GET", "/", nil)
	page = html.UnescapeString(page)
	if !strings.Contains(page, "backupzit_1763600000 left on ESX1.example") || strings.Contains(page, fmt.Sprintf("agent=%d&host=%d", other.id, hostID)) {
		t.Error("the other agent's item: not shown with its address, or offered with the host")
	}
	q := url.Values{"agent": {fmt.Sprint(other.id)}, "host": {fmt.Sprint(hostID)}}
	if _, loc, _ := admin.do("POST", "/leftovers/clean?"+q.Encode(), url.Values{}); strings.HasPrefix(loc, "/runs/") {
		t.Error("Remove now queued a clean-up with the host for an agent that is not its proxy")
	}
	// A run queued while it was a proxy (or by an older console) reaches it
	// after the proxy changed: it fails, without the credentials.
	var runID int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO runs(agent_id, kind, trigger, repo_url, notified, chat_notified, vmware_host_id, paths)
		VALUES($1,'leftover-cleanup','auto','',true,true,$2,'{}') RETURNING id`, other.id, hostID).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if run := poll(other, nil); run != nil {
		t.Fatalf("the other agent got a clean-up run of the host: %+v", run)
	}
	if r, _ := e.store.GetRun(ctx, runID); r.Status != api.StatusFailed || !strings.Contains(r.Message, "not a proxy of esx1") {
		t.Errorf("clean-up run of a non-proxy: %s %q", r.Status, r.Message)
	}
	// The proxy's own item is cleaned up automatically, with the host.
	poll(proxy, []leftover.Item{item})
	e.store.AutoLeftoverCleanup(ctx, time.Now())
	run := poll(proxy, nil)
	if run == nil || run.Kind != api.KindLeftoverCleanup || run.VMware == nil || run.VMware.Password != "secret-esx-pw" {
		t.Fatalf("the proxy's clean-up run: %+v", run)
	}
}

// Each leftover that stays is notified once; one the agent will remove
// right away (no failed try, younger than an hour) is not.
func TestLeftoverAlertsOnce(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "0.56.0")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(api.PollRequest{Hostname: "pve1", OS: "Linux", Arch: "amd64", Version: "0.56.0",
		Leftovers: mustItems(t, []leftover.Item{
			{Platform: leftover.Proxmox, Kind: leftover.KindSnapshot, VM: "101", Name: "backupzit_1", Created: time.Now().Add(-2 * time.Hour), Tries: 1, Error: "storage busy"},
			{Platform: leftover.Proxmox, Kind: leftover.KindLock, VM: "102", Name: "backup", Created: time.Now()},
		})})
	hr, _ := http.NewRequest("POST", e.ts.URL+api.PathPoll, bytes.NewReader(b))
	hr.Header.Set("Authorization", "Bearer "+cfg.AgentUUID+":"+cfg.Secret)
	resp, err := e.ts.Client().Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	n := server.NewNotifier(e.store, slog.New(slog.NewTextHandler(io.Discard, nil)), func() string { return "" })
	for i := 0; i < 2; i++ {
		n.Pass(ctx, time.Now())
	}
	var sent map[string]bool
	if err := e.store.GetSetting(ctx, "leftover_alerts", &sent); err != nil || len(sent) != 1 {
		t.Fatalf("notified %v (%v), want the failed snapshot only", sent, err)
	}
}

func mustItems(t *testing.T, items []leftover.Item) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
