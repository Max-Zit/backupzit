package server_test

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vmware/govmomi/simulator"

	"github.com/backupzit/backupzit/internal/agent"
	"github.com/backupzit/backupzit/internal/api"
	"github.com/backupzit/backupzit/internal/server"
)

// TestVMwareHosts drives the console side of VMware support against the
// vSphere simulator: adding a host, a VM job through its proxy agent, the
// connection handed to the agent, and the restore form and wizard.
func TestVMwareHosts(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	model := simulator.ESX()
	defer model.Remove()
	if err := model.Create(); err != nil {
		t.Fatal(err)
	}
	model.Service.TLS = new(tls.Config)
	sim := model.Service.NewServer()
	defer sim.Close()
	simPass, _ := sim.URL.User.Password()

	targetID, _ := e.store.CreateTarget(ctx, server.Target{Name: "local", Kind: "local", URL: t.TempDir()})
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	var proxy server.Agent
	agents, _ := e.store.ListAgents(ctx)
	for _, a := range agents {
		if a.UUID == cfg.AgentUUID {
			proxy = a
		}
	}
	admin := newClient(t, e)
	if !admin.login("admin", "admin-pass-123") {
		t.Fatal("login")
	}

	// An unreachable host is refused.
	_, loc, _ := admin.do("POST", "/vmware", url.Values{"address": {"127.0.0.1:1"}, "username": {"root"}, "password": {"x"}, "proxy_agent_id": {fmt.Sprint(proxy.ID)}})
	if !strings.Contains(loc, "err=") || !strings.HasSuffix(loc, "#vmware") {
		t.Fatalf("unreachable host accepted: %s", loc)
	}
	_, loc, _ = admin.do("POST", "/vmware", url.Values{"name": {"esxi1"}, "address": {sim.URL.Host}, "username": {sim.URL.User.Username()},
		"password": {simPass}, "proxy_agent_id": {fmt.Sprint(proxy.ID)}})
	if !strings.Contains(loc, "msg=") {
		t.Fatalf("add host: %s", loc)
	}
	hosts, err := e.store.ListVMwareHosts(ctx)
	if err != nil || len(hosts) != 1 {
		t.Fatalf("hosts: %v %v", hosts, err)
	}
	h := hosts[0]
	inv := h.Inv()
	if h.Thumbprint == "" || h.Password != simPass || inv == nil || len(inv.Guests) == 0 || h.SSHOK || h.ProxyAgentID == nil || *h.ProxyAgentID != proxy.ID {
		t.Fatalf("host: %+v", h)
	}
	var stored string
	e.pool.QueryRow(ctx, `SELECT password FROM vmware_hosts WHERE id=$1`, h.ID).Scan(&stored)
	if stored == simPass {
		t.Error("ESXi password stored in plaintext")
	}
	if _, _, body := admin.do("GET", "/agents", nil); !strings.Contains(body, "esxi1") || !strings.Contains(body, "SSH off") {
		t.Error("agents page lacks the ESXi host")
	}

	// A VM job on the host runs on the proxy agent.
	g := inv.Guests[0]
	_, loc, _ = admin.do("POST", "/jobs", url.Values{"name": {"esx vms"}, "agent_id": {fmt.Sprintf("vmware:%d", h.ID)}, "target_id": {fmt.Sprint(targetID)},
		"kind": {"vm"}, "vm_mode": {"selected"}, "vms": {fmt.Sprint(g.VMID)}, "keep_last": {"3"}, "sched_kind": {"manual"}})
	m := regexp.MustCompile(`^/jobs/(\d+)`).FindStringSubmatch(loc)
	if m == nil {
		t.Fatalf("create job: %s", loc)
	}
	var jobID int64
	fmt.Sscan(m[1], &jobID)
	j, _ := e.store.GetJob(ctx, jobID)
	if j.AgentID != proxy.ID || j.VMwareHostID == nil || *j.VMwareHostID != h.ID {
		t.Fatalf("job: %+v", j)
	}
	if _, _, body := admin.do("GET", "/jobs", nil); !strings.Contains(body, "esxi1 <span class=\"muted small\">ESXi, via") {
		t.Error("jobs page does not show the ESXi host")
	}
	runID, err := e.store.QueueBackup(ctx, jobID, "manual")
	if err != nil {
		t.Fatal(err)
	}
	poll := func() *api.Run {
		b, _ := json.Marshal(api.PollRequest{Hostname: "proxy", OS: "Linux", Arch: "amd64", Version: "1.0.0"})
		req, _ := http.NewRequest("POST", e.ts.URL+api.PathPoll, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+cfg.AgentUUID+":"+cfg.Secret)
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var pr api.PollResponse
		json.NewDecoder(resp.Body).Decode(&pr)
		return pr.Run
	}
	run := poll()
	if run == nil || run.ID != runID || run.VMware == nil || run.VMware.Password != simPass || run.VMware.Thumbprint != h.Thumbprint ||
		!strings.Contains(run.Repository.URL, "esxi-esxi1_") || len(run.VMs) != 1 {
		t.Fatalf("run for the proxy: %+v", run)
	}
	details, _ := json.Marshal(map[string]any{"guests": []map[string]any{{"vmid": g.VMID, "type": "vm", "name": g.Name, "platform": "vmware",
		"consistency": "snapshot, guest quiesced by VMware Tools (application-consistent)", "disks": 1, "size": 1 << 30, "stored": 1 << 20}}})
	e.store.FinishRun(ctx, proxy.ID, runID, api.RunResult{Status: api.StatusSuccess, SnapshotID: strings.Repeat("cd", 32), Details: details, Stats: json.RawMessage(`{}`)})

	get := func(p string) string {
		_, _, body := admin.do("GET", p, nil)
		return html.UnescapeString(body)
	}
	if p := get(fmt.Sprintf("/runs/%d", runID)); !strings.Contains(p, "VMware virtual machines") || !strings.Contains(p, "Restore a VMware VM") ||
		!strings.Contains(p, `name="vmware_host_id"`) || !strings.Contains(p, "</html>") {
		t.Error("run page lacks the VMware restore form")
	}
	if p := get("/restore?type=vm"); !strings.Contains(p, "VMware ESXi") || !strings.Contains(p, g.Name) {
		t.Error("restore wizard lacks the VMware VM")
	}
	// Restore on the host: in place, and validation.
	_, loc, _ = admin.do("POST", fmt.Sprintf("/runs/%d/restore", runID), url.Values{"kind": {"vm"}, "vmid": {fmt.Sprint(g.VMID)},
		"vmware_host_id": {fmt.Sprint(h.ID)}, "id_mode": {"next"}, "name": {"bad/name"}})
	if !strings.Contains(loc, "err=") {
		t.Error("invalid VM name accepted")
	}
	_, loc, _ = admin.do("POST", fmt.Sprintf("/runs/%d/restore", runID), url.Values{"kind": {"vm"}, "vmid": {fmt.Sprint(g.VMID)},
		"vmware_host_id": {fmt.Sprint(h.ID)}, "id_mode": {"original"}, "overwrite": {"on"}, "start": {"on"}})
	m = regexp.MustCompile(`^/runs/(\d+)`).FindStringSubmatch(loc)
	if m == nil {
		t.Fatalf("queue restore: %s", loc)
	}
	run = poll()
	if run == nil || run.Kind != api.KindVMRestore || run.VMware == nil || run.VMRestore == nil || !run.VMRestore.Overwrite || run.VMRestore.NewVMID != 0 {
		t.Fatalf("restore run: %+v", run)
	}

	// Removing the host removes its jobs.
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/vmware/%d/delete", h.ID), url.Values{}); !strings.Contains(loc, "msg=") {
		t.Fatal(loc)
	}
	if _, err := e.store.GetJob(ctx, jobID); err == nil {
		t.Error("job of the removed host still exists")
	}
}

func TestCertificateSettings(t *testing.T) {
	e := setup(t)
	e.srv.StartWeb(e.ctx, t.TempDir())
	admin := newClient(t, e)
	if !admin.login("admin", "admin-pass-123") {
		t.Fatal("login")
	}
	if _, _, body := admin.do("GET", "/settings/certificate", nil); !strings.Contains(body, "HTTPS certificate for browsers") || !strings.Contains(body, `value="acme"`) {
		t.Fatal("certificate tab missing")
	}
	if _, loc, _ := admin.do("POST", "/settings/certificate", url.Values{"mode": {"upload"}, "hostname": {"backup.example.com"}, "port": {"8444"},
		"cert_pem": {"-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----"}, "key_pem": {"x"}}); !strings.Contains(loc, "err=") {
		t.Errorf("invalid certificate accepted: %s", loc)
	}
	if _, loc, _ := admin.do("POST", "/settings/certificate", url.Values{"mode": {"acme"}, "hostname": {"backup.example.com"}, "port": {"8444"},
		"acme_email": {"a@example.com"}, "challenge": {"cloudflare"}}); !strings.Contains(loc, "err=") || !strings.Contains(loc, "Cloudflare") {
		t.Errorf("Let's Encrypt without token accepted: %s", loc)
	}
	if _, loc, _ := admin.do("POST", "/settings/certificate", url.Values{"mode": {"self"}}); !strings.Contains(loc, "msg=") {
		t.Errorf("self mode: %s", loc)
	}
}
