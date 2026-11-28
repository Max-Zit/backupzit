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

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/pve"
	"github.com/max-zit/backupzit/internal/server"
)

// V-3 (#51): a VMware host added through its proxy agent. The proxy checks
// the connection (vmware-probe runs); the console pins the certificate the
// proxy saw, stores the VM list, refuses a changed certificate until it is
// accepted, and keeps the checks out of Activity.
func TestVMwareThroughProxy(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	admin := newClient(t, e)
	if !admin.login("admin", "admin-pass-123") {
		t.Fatal("login")
	}
	enroll := func(version string) (*agent.Config, int64) {
		token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
		cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, version)
		if err != nil {
			t.Fatal(err)
		}
		agents, _ := e.store.ListAgents(ctx)
		for _, a := range agents {
			if a.UUID == cfg.AgentUUID {
				return cfg, a.ID
			}
		}
		t.Fatal("agent not found")
		return nil, 0
	}
	call := func(cfg *agent.Config, path string, body any, out any) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", e.ts.URL+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+cfg.AgentUUID+":"+cfg.Secret)
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		defer resp.Body.Close()
		if out != nil {
			_ = json.NewDecoder(resp.Body).Decode(out)
		}
	}
	cfg, agentID := enroll("0.56.0")
	poll := func(busy bool) *api.Run {
		var pr api.PollResponse
		call(cfg, api.PathPoll, api.PollRequest{Hostname: "proxy1", OS: "Linux", Arch: "amd64", Version: "0.56.0", Busy: busy, Parallel: true, Running: map[bool]int{true: 1}[busy]}, &pr)
		return pr.Run
	}
	poll(false)
	const fp1 = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	const fp2 = "SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	inv, _ := json.Marshal(pve.Inventory{Platform: "vmware", Node: "esx-site", Version: "ESXi 8.0.3", Guests: []pve.Guest{
		{VMID: 10, ID: "50211111-2222-3333-4444-555555555555", Name: "web", Node: "esx-site"}, {VMID: 11, ID: "50212222-2222-3333-4444-555555555555", Name: "db", Node: "esx-site"}}})
	// The proxy: answers the next check with the given certificate (or an
	// error), even while it is busy with a backup.
	answer := func(busy bool, thumbprint, fail string) *api.Run {
		t.Helper()
		var run *api.Run
		for i := 0; i < 50 && run == nil; i++ {
			if run = poll(busy); run == nil {
				time.Sleep(100 * time.Millisecond)
			}
		}
		if run == nil || run.Kind != api.KindVMwareProbe || run.VMware == nil || run.VMware.Address != "10.9.9.9" {
			t.Fatalf("check run %+v", run)
		}
		res := api.RunResult{Status: api.StatusSuccess, Message: "Connected"}
		if fail != "" {
			res = api.RunResult{Status: api.StatusFailed, Message: fail}
		} else {
			res.Details, _ = json.Marshal(api.VMwareProbe{Inventory: inv, Version: "8.0.3", APIWrites: true, Thumbprint: thumbprint})
		}
		call(cfg, fmt.Sprintf("%s%d/finish", api.PathRunsPrefix, run.ID), res, nil)
		return run
	}
	done := make(chan *api.Run, 1)
	go func() { done <- answerAsync(e, cfg, inv, fp1) }()
	form := url.Values{"name": {"esx-site"}, "address": {"10.9.9.9"}, "username": {"root"}, "password": {"secret-esx"},
		"proxy_agent_id": {fmt.Sprint(agentID)}, "via_proxy": {"on"}}
	_, loc, _ := admin.do("POST", "/vmware", form)
	if run := <-done; run == nil || run.VMware.Thumbprint != "" {
		t.Fatalf("the first check must not pin anything yet: %+v", run)
	}
	if !strings.Contains(loc, "added+through+the+proxy+agent") {
		t.Fatalf("add through the proxy: %s", loc)
	}
	hosts, _ := e.store.ListVMwareHosts(ctx)
	if len(hosts) != 1 {
		t.Fatalf("%d hosts", len(hosts))
	}
	h := hosts[0]
	if !h.ViaProxy || h.Thumbprint != fp1 || h.Inv() == nil || len(h.Inv().Guests) != 2 || h.InventoryAt == nil || h.LastError != "" {
		t.Fatalf("host after the check: via %v, thumbprint %q, inventory %s, error %q", h.ViaProxy, h.Thumbprint, h.Inventory, h.LastError)
	}

	// Refresh while the proxy runs a backup: the check is not held up, and
	// it carries the pinned certificate.
	go func() { _, _, _ = admin.do("POST", fmt.Sprintf("/vmware/%d/refresh", h.ID), url.Values{}) }()
	if run := answer(true, fp2, ""); run.VMware.Thumbprint != fp1 {
		t.Fatalf("the check does not pin the certificate: %q", run.VMware.Thumbprint)
	}
	time.Sleep(200 * time.Millisecond)
	h, _ = e.store.GetVMwareHost(ctx, h.ID)
	if h.Thumbprint != fp1 || h.NewCertificate() != fp2 {
		t.Fatalf("a changed certificate was not refused: pinned %q, offered %q (%s)", h.Thumbprint, h.NewCertificate(), h.LastError)
	}
	// Accepting it: the next check uses the new one.
	go func() {
		_, _, _ = admin.do("POST", fmt.Sprintf("/vmware/%d/certificate", h.ID), url.Values{"thumbprint": {fp2}})
	}()
	if run := answer(false, fp2, ""); run.VMware.Thumbprint != fp2 {
		t.Fatalf("after accepting: %q", run.VMware.Thumbprint)
	}
	time.Sleep(200 * time.Millisecond)
	if h, _ = e.store.GetVMwareHost(ctx, h.ID); h.Thumbprint != fp2 || h.LastError != "" {
		t.Fatalf("accepted certificate: %q, error %q", h.Thumbprint, h.LastError)
	}
	// A failed check shows as the host's error; the VM list stays.
	go func() { _, _, _ = admin.do("POST", fmt.Sprintf("/vmware/%d/refresh", h.ID), url.Values{}) }()
	answer(false, "", "sign in to 10.9.9.9: incorrect user name or password")
	time.Sleep(200 * time.Millisecond)
	if h, _ = e.store.GetVMwareHost(ctx, h.ID); !strings.Contains(h.LastError, "incorrect user name") || h.Inv() == nil {
		t.Fatalf("failed check: %q", h.LastError)
	}
	// The checks stay out of Activity.
	if runs, _ := e.store.ListRuns(ctx, server.RunFilter{AgentID: agentID}); len(runs) != 0 {
		t.Errorf("%d checks in Activity", len(runs))
	}

	// A proxy older than 0.56 cannot do it.
	_, oldID := enroll("0.55.0")
	form.Set("name", "esx-old")
	form.Set("proxy_agent_id", fmt.Sprint(oldID))
	if _, loc, _ := admin.do("POST", "/vmware", form); !strings.Contains(loc, "0.56.0+or+newer") {
		t.Fatalf("old proxy: %s", loc)
	}
}

// answerAsync is the proxy answering the first check (no t.Fatal in a
// goroutine): it returns the check run, or nil.
func answerAsync(e *env, cfg *agent.Config, inv json.RawMessage, thumbprint string) *api.Run {
	post := func(path string, body any, out any) bool {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", e.ts.URL+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+cfg.AgentUUID+":"+cfg.Secret)
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		if out != nil {
			return json.NewDecoder(resp.Body).Decode(out) == nil
		}
		return true
	}
	for i := 0; i < 100; i++ {
		var pr api.PollResponse
		post(api.PathPoll, api.PollRequest{Hostname: "proxy1", OS: "Linux", Arch: "amd64", Version: "0.56.0"}, &pr)
		if pr.Run != nil {
			details, _ := json.Marshal(api.VMwareProbe{Inventory: inv, Version: "8.0.3", APIWrites: true, Thumbprint: thumbprint})
			post(fmt.Sprintf("%s%d/finish", api.PathRunsPrefix, pr.Run.ID), api.RunResult{Status: api.StatusSuccess, Details: details}, nil)
			return pr.Run
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// B3: through a proxy agent of 0.62 or newer, the agent first reads the
// host's fingerprints without the password; the host is stored and the
// password sent only after the user confirmed them, and the check then
// carries the confirmed certificate.
func TestVMwareThroughProxyConfirm(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "0.62.0")
	if err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	agentID := agents[0].ID
	call := func(path string, body any, out any) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", e.ts.URL+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+cfg.AgentUUID+":"+cfg.Secret)
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		defer resp.Body.Close()
		if out != nil {
			_ = json.NewDecoder(resp.Body).Decode(out)
		}
	}
	poll := func() *api.Run {
		var pr api.PollResponse
		call(api.PathPoll, api.PollRequest{Hostname: "proxy1", OS: "Linux", Arch: "amd64", Version: "0.62.0", Parallel: true}, &pr)
		return pr.Run
	}
	poll()
	const fp = "SHA256:CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
	const sfp = "SHA256:DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"
	inv, _ := json.Marshal(pve.Inventory{Platform: "vmware", Node: "esx-site", Version: "ESXi 8.0.3", Guests: []pve.Guest{{VMID: 10, Name: "web", Node: "esx-site"}}})
	next := func() *api.Run {
		for i := 0; i < 80; i++ {
			if run := poll(); run != nil {
				return run
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("no run")
		return nil
	}
	first := make(chan *api.Run, 1)
	go func() {
		run := next()
		res, _ := json.Marshal(api.VMwareProbe{Thumbprint: fp, SSHHostKey: sfp})
		call(fmt.Sprintf("%s%d/finish", api.PathRunsPrefix, run.ID), api.RunResult{Status: api.StatusSuccess, Details: res}, nil)
		first <- run
	}()
	_, loc, body := admin.do("POST", "/vmware", url.Values{"name": {"esx-site"}, "address": {"10.9.9.9"}, "username": {"root"}, "password": {"secret-esx"},
		"proxy_agent_id": {fmt.Sprint(agentID)}, "via_proxy": {"on"}})
	run := <-first
	if run.VMware == nil || !run.VMware.FingerprintsOnly || run.VMware.Password != "" || run.VMware.User != "" {
		t.Fatalf("the first check carries credentials: %+v", run.VMware)
	}
	if p := pendingPrints(body); len(p) != 2 || p[0] != fp || p[1] != sfp {
		t.Fatalf("fingerprints shown: %v (redirect %q)", p, loc)
	}
	if hosts, _ := e.store.ListVMwareHosts(ctx); len(hosts) != 0 {
		t.Fatal("host stored before the confirmation")
	}
	// The probe is in the audit log, also before the host is added; no
	// password goes into it.
	probed := false
	entries, _ := e.store.ListAudit(ctx, 20)
	for _, a := range entries {
		if strings.Contains(a.Detail, "secret-esx") {
			t.Errorf("password in the audit log: %+v", a)
		}
		probed = probed || (a.Action == "vmware.probe" && strings.Contains(a.Detail, "10.9.9.9 through proxy1") && strings.Contains(a.Detail, fp))
	}
	if !probed {
		t.Errorf("the fingerprint probe is not in the audit log: %+v", entries)
	}
	second := make(chan *api.Run, 1)
	go func() {
		run := next()
		res, _ := json.Marshal(api.VMwareProbe{Inventory: inv, Version: "8.0.3", Thumbprint: fp, SSHHostKey: sfp})
		call(fmt.Sprintf("%s%d/finish", api.PathRunsPrefix, run.ID), api.RunResult{Status: api.StatusSuccess, Details: res}, nil)
		second <- run
	}()
	loc = confirmIdentity(t, admin, "", body)
	run = <-second
	if run.VMware.Password != "secret-esx" || run.VMware.Thumbprint != fp || run.VMware.SSHHostKey != sfp {
		t.Fatalf("the check after the confirmation: %+v", run.VMware)
	}
	if !strings.Contains(loc, "added+through+the+proxy+agent") {
		t.Fatalf("add: %s", loc)
	}
	if hosts, _ := e.store.ListVMwareHosts(ctx); len(hosts) != 1 || hosts[0].Thumbprint != fp || hosts[0].SSHHostKey != sfp {
		t.Fatalf("hosts: %+v", hosts)
	}
}
