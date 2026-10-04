package server_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vmware/govmomi/simulator"

	"github.com/backupzit/backupzit/internal/agent"
	"github.com/backupzit/backupzit/internal/api"
	"github.com/backupzit/backupzit/internal/backend"
	"github.com/backupzit/backupzit/internal/repo"
	"github.com/backupzit/backupzit/internal/server"
	"github.com/backupzit/backupzit/internal/testutil"
	"github.com/backupzit/backupzit/internal/update"
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

func TestConsoleUpdate(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	pub, key, _ := ed25519.GenerateKey(nil)
	saved := update.TrustedKeys
	update.TrustedKeys = []string{base64.StdEncoding.EncodeToString(pub)}
	defer func() { update.TrustedKeys = saved }()

	src := t.TempDir()
	pkg := []byte("new console package")
	sum := sha256.Sum256(pkg)
	m := update.Manifest{Product: "backupzit", Version: "9.9.9", Notes: "Better everything."}
	for _, n := range []string{"backupzit-server_9.9.9_amd64.deb", "backupzit-server-9.9.9-1.x86_64.rpm"} {
		os.WriteFile(filepath.Join(src, n), pkg, 0o644)
		format := "deb"
		if strings.HasSuffix(n, ".rpm") {
			format = "rpm"
		}
		m.Files = append(m.Files, update.File{Name: n, Kind: "server", Format: format, Arch: "amd64", Size: int64(len(pkg)), SHA256: hex.EncodeToString(sum[:])})
	}
	raw, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(src, "manifest.json"), raw, 0o644)
	os.WriteFile(filepath.Join(src, "manifest.json.sig"), []byte(update.Sign(raw, key)), 0o644)

	data := t.TempDir()
	e.srv.Version = "0.27.0"
	e.srv.StartUpdates(ctx, data)
	e.srv.EnableUpdateHelper()
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	if _, loc, _ := admin.do("POST", "/settings/updates", url.Values{"source": {src}, "notify": {"on"}, "auto_agents": {"on"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("save source: %s", loc)
	}
	if _, _, body := admin.do("GET", "/settings/updates", nil); !strings.Contains(body, "BackupZit 9.9.9 is available") || !strings.Contains(body, "Better everything.") {
		t.Fatal("new version not shown")
	}
	if _, loc, _ := admin.do("POST", "/settings/updates/install", url.Values{}); !strings.Contains(loc, "msg=") {
		t.Fatalf("install: %s", loc)
	}
	var req update.Request
	if err := update.ReadJSON(filepath.Join(data, "update", "request.json"), &req); err != nil || !strings.Contains(req.File, "9.9.9") || req.User != "admin" {
		t.Fatalf("request for the helper: %+v %v", req, err)
	}
	if _, err := update.Verify(req.Manifest, req.Signature, nil); err != nil {
		t.Errorf("request manifest: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(data, "update", req.File)); string(b) != string(pkg) {
		t.Error("package not staged")
	}
	if _, loc, _ := admin.do("POST", "/settings/updates/install", url.Values{}); !strings.Contains(loc, "already+being+installed") {
		t.Errorf("second install: %s", loc)
	}
	// A tampered source is refused.
	os.WriteFile(filepath.Join(src, "manifest.json"), append(raw, ' '), 0o644)
	if _, loc, _ := admin.do("POST", "/settings/updates/check", url.Values{}); !strings.Contains(loc, "err=") {
		t.Errorf("tampered manifest accepted: %s", loc)
	}
	// The helper's result is recorded once after the restart.
	os.Remove(filepath.Join(data, "update", "request.json"))
	update.WriteJSON(filepath.Join(data, "update", "result.json"), update.Result{From: "0.27.0", Version: "9.9.9", Status: "rolled-back", Message: "did not start", Finished: time.Now().UTC()}, 0o644)
	e.srv.StartUpdates(ctx, data)
	e.srv.EnableUpdateHelper()
	if _, _, body := admin.do("GET", "/audit", nil); !strings.Contains(body, "update.result") {
		t.Error("update result not in the audit log")
	}
	if _, _, body := admin.do("GET", "/settings/updates", nil); !strings.Contains(body, "rolled-back") {
		t.Error("last update not shown")
	}
	// Operating system: status shown, tasks handed to the helper.
	update.WriteJSON(filepath.Join(data, "update", "os-status.json"), update.OSStatus{OS: "Debian GNU/Linux 12", Pending: 3, Security: 2, RebootRequired: true, Checked: time.Now()}, 0o644)
	if _, _, body := admin.do("GET", "/settings/updates", nil); !strings.Contains(body, "2 security") || !strings.Contains(body, "Switch on automatic security updates") || !strings.Contains(body, "required") {
		t.Error("OS status not shown")
	}
	if _, loc, _ := admin.do("POST", "/settings/os", url.Values{"action": {"auto-on"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("auto-on: %s", loc)
	}
	var osReq update.Request
	if update.ReadJSON(filepath.Join(data, "update", "request.json"), &osReq); osReq.Action != update.ActionOSAuto || !osReq.Enable {
		t.Errorf("OS request %+v", osReq)
	}
	if _, loc, _ := admin.do("POST", "/settings/os", url.Values{"action": {"reboot"}}); !strings.Contains(loc, "another+update+task") {
		t.Errorf("second task: %s", loc)
	}
}

func TestConsoleBackupRestore(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	s3, err := testutil.StartS3Server("company-backups")
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	targetID, err := e.store.CreateTarget(ctx, server.Target{Name: "s3", Kind: "s3", URL: "s3://" + s3.Host + "/company-backups?tls=false",
		S3AccessKey: "k", S3SecretKey: "s", Encrypted: true})
	if err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	os.WriteFile(filepath.Join(data, server.SecretKeyFile), []byte("secret key bytes"), 0o600)
	os.WriteFile(filepath.Join(data, "cert.pem"), []byte("pinned certificate"), 0o644)
	os.WriteFile(filepath.Join(data, "key.pem"), []byte("certificate key"), 0o600)
	e.srv.StartConsoleBackup(ctx, data)
	if _, err := e.store.CreateUser(ctx, server.User{Username: "ana", DisplayName: "Ana", Role: "operator"}, "ana-password-1"); err != nil {
		t.Fatal(err)
	}
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	if _, loc, _ := admin.do("POST", "/settings/console", url.Values{"enabled": {"on"}, "target_id": {fmt.Sprint(targetID)}, "hour": {"3"}, "keep": {"2"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("settings: %s", loc)
	}
	for i := 0; i < 3; i++ { // three backups, two are kept
		if _, loc, _ := admin.do("POST", "/settings/console/run", url.Values{}); !strings.Contains(loc, "msg=") {
			t.Fatalf("backup: %s", loc)
		}
	}
	if _, _, body := admin.do("GET", "/settings/console", nil); !strings.Contains(body, "badge success") || !strings.Contains(body, "backupzit-console") {
		t.Error("console backup page")
	}
	var cfg server.ConsoleBackupSettings
	e.store.GetSetting(ctx, "console_backup", &cfg)
	tgt, _ := e.store.GetTarget(ctx, targetID)
	be, err := backend.Open(ctx, cfg.LastRepo, backend.Options{S3AccessKey: "k", S3SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Open(ctx, be, repo.Password(tgt.RecoveryKey))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if sns, _ := r.ListSnapshots(ctx); len(sns) != 2 {
		t.Errorf("%d console backups kept, want 2", len(sns))
	}

	// Disaster: users gone, a new data directory without keys.
	e.pool.Exec(ctx, `DELETE FROM sessions`)
	e.pool.Exec(ctx, `DELETE FROM users WHERE username='ana'`)
	fresh := t.TempDir()
	if _, err := server.RestoreConsole(ctx, e.pool.Config().ConnString(), fresh, r, "latest", t.Logf); err != nil {
		t.Fatal(err)
	}
	var n int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE username IN ('admin','ana')`).Scan(&n)
	if n != 2 {
		t.Errorf("%d users after restore, want 2", n)
	}
	var tn string
	e.pool.QueryRow(ctx, `SELECT name FROM storage_targets WHERE id=$1`, targetID).Scan(&tn)
	if tn != "s3" {
		t.Error("storage target not restored")
	}
	for name, want := range map[string]string{server.SecretKeyFile: "secret key bytes", "cert.pem": "pinned certificate", "key.pem": "certificate key"} {
		if b, _ := os.ReadFile(filepath.Join(fresh, name)); string(b) != want {
			t.Errorf("%s not restored: %q", name, b)
		}
	}
	// New rows get new IDs (sequences continue after the restored ones).
	if _, err := e.store.CreateUser(ctx, server.User{Username: "marko", DisplayName: "Marko", Role: "viewer"}, "marko-password-1"); err != nil {
		t.Errorf("insert after restore: %v", err)
	}
	if _, loc, _ := newClientLogin(t, e, "ana", "ana-password-1"); loc != "/" {
		t.Errorf("restored user cannot sign in: %s", loc)
	}
}

func newClientLogin(t *testing.T, e *env, user, pw string) (*client, string, string) {
	c := newClient(t, e)
	code, loc, body := c.do("POST", "/login", url.Values{"username": {user}, "password": {pw}})
	_ = code
	return c, loc, body
}

func TestInstantRecovery(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	targetID, err := e.store.CreateTarget(ctx, server.Target{Name: "local", Kind: "local", URL: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	var a server.Agent
	for _, x := range agents {
		if x.UUID == cfg.AgentUUID {
			a = x
		}
	}
	inv := `{"node":"pve","guests":[{"vmid":100,"name":"web","type":"qemu","node":"pve","status":"running"},{"vmid":200,"name":"db","type":"lxc","node":"pve","status":"running"}],"storage":["local-lvm","backupzit-instant"]}`
	e.store.TouchAgent(ctx, a.ID, api.PollRequest{Hypervisor: json.RawMessage(inv)}, "")
	poll := func() *api.Run {
		b, _ := json.Marshal(api.PollRequest{Hostname: "pve", Version: "test", Hypervisor: json.RawMessage(inv)})
		req, _ := http.NewRequest(http.MethodPost, e.ts.URL+api.PathPoll, bytes.NewReader(b))
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
	jobID, err := e.store.CreateJob(ctx, server.Job{Kind: server.JobVM, AgentID: a.ID, TargetID: targetID, Name: "vms", Paths: []string{"100", "200"},
		Retention: repo.RetentionPolicy{KeepLast: 1}})
	if err != nil {
		t.Fatal(err)
	}
	backupRun, _ := e.store.QueueBackup(ctx, jobID, "manual")
	if r := poll(); r == nil || r.ID != backupRun || len(r.KeepSnapshots) != 0 {
		t.Fatalf("backup run: %+v", r)
	}
	snap := strings.Repeat("ab", 32)
	e.store.FinishRun(ctx, a.ID, backupRun, api.RunResult{Status: api.StatusSuccess, SnapshotID: snap,
		Details: json.RawMessage(`{"guests":[{"vmid":100,"type":"qemu","name":"web","disks":1},{"vmid":200,"type":"lxc","name":"db","disks":1}]}`)})

	if _, err := e.store.QueueVMInstant(ctx, backupRun, a.ID, api.VMRestore{VMID: 200, NewVMID: -1}); err == nil {
		t.Error("instant recovery of a container accepted")
	}
	if _, err := e.store.QueueVMInstant(ctx, backupRun, a.ID, api.VMRestore{VMID: 100, NewVMID: 200}); err == nil {
		t.Error("instant recovery onto an existing guest ID accepted")
	}
	ir, err := e.store.QueueVMInstant(ctx, backupRun, a.ID, api.VMRestore{VMID: 100, NewVMID: -1, Name: "web-instant"})
	if err != nil {
		t.Fatal(err)
	}
	r := poll()
	if r == nil || r.ID != ir || r.Kind != api.KindVMInstant || r.SnapshotID != snap || r.VMRestore == nil || r.VMRestore.VMID != 100 || r.Repository.URL == "" {
		t.Fatalf("instant run: %+v", r)
	}
	if _, err := e.store.QueueInstantEnd(ctx, ir, false, ""); err == nil {
		t.Error("discard queued before the VM started")
	}
	e.store.FinishRun(ctx, a.ID, ir, api.RunResult{Status: api.StatusSuccess, Message: "VM 101 runs from the backup",
		Details: json.RawMessage(`{"instant_vmid":101,"node":"pve","name":"web-instant","state":"running","disks":["scsi0 → backupzit-instant:101/vm-101-disk-0.qcow2"]}`)})
	if vms, err := e.store.RunningInstantVMs(ctx); err != nil || len(vms) != 1 || vms[0].VMID != 101 || vms[0].RunID != ir {
		t.Fatalf("running instant VMs: %+v %v", vms, err)
	}

	// While VM 101 runs from the backup, retention keeps it.
	b2, _ := e.store.QueueBackup(ctx, jobID, "manual")
	if r := poll(); r == nil || r.ID != b2 || r.Retention == nil || strings.Join(r.KeepSnapshots, ",") != snap {
		t.Fatalf("backup run during instant recovery: %+v", r)
	}
	e.store.FinishRun(ctx, a.ID, b2, api.RunResult{Status: api.StatusSuccess, SnapshotID: strings.Repeat("cd", 32)})

	jar, _ := cookiejar.New(nil)
	hc := e.ts.Client()
	hc.Jar = jar
	post := func(p string, form url.Values) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, e.ts.URL+p, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", e.ts.URL)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	get := func(p string) string {
		resp, err := hc.Get(e.ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return html.UnescapeString(string(b))
	}
	post("/login", url.Values{"username": {"admin"}, "password": {"admin-pass-123"}})
	if p := get("/"); !strings.Contains(p, "1 VM still runs from a backup") || !strings.Contains(p, "VM 101 (web-instant)") {
		t.Error("dashboard does not show the instant VM")
	}
	if p := get(fmt.Sprintf("/runs/%d", backupRun)); !strings.Contains(p, "Instantly, running from the backup") {
		t.Error("restore form lacks instant recovery")
	}
	p := get(fmt.Sprintf("/runs/%d", ir))
	if !strings.Contains(p, "VM 101 runs from the backup") || !strings.Contains(p, "<option>local-lvm</option>") || strings.Contains(p, "<option>backupzit-instant</option>") {
		t.Error("instant run page lacks finish/discard or offers the overlay storage")
	}
	post(fmt.Sprintf("/runs/%d/instant", ir), url.Values{"action": {"finish"}, "storage": {"backupzit-instant"}})
	post(fmt.Sprintf("/runs/%d/instant", ir), url.Values{"action": {"finish"}, "storage": {"local-lvm"}})
	if _, err := e.store.QueueInstantEnd(ctx, ir, false, ""); err == nil {
		t.Error("discard queued while finish is pending")
	}
	r = poll()
	if r == nil || r.Kind != api.KindVMInstantFinish || r.VMRestore == nil || r.VMRestore.InstantVMID != 101 || r.VMRestore.Storage != "local-lvm" || r.VMRestore.InstantRun != ir {
		t.Fatalf("finish run: %+v", r)
	}
	e.store.FinishRun(ctx, a.ID, r.ID, api.RunResult{Status: api.StatusSuccess})
	if vms, _ := e.store.RunningInstantVMs(ctx); len(vms) != 0 {
		t.Errorf("VM still listed as running from the backup: %+v", vms)
	}
	if p := get(fmt.Sprintf("/runs/%d", ir)); !strings.Contains(p, "101 on node pve — finished") {
		t.Error("instant run page does not show the finished state")
	}
	if _, err := e.store.QueueInstantEnd(ctx, ir, false, ""); err == nil {
		t.Error("discard of a finished VM accepted")
	}
	b3, _ := e.store.QueueBackup(ctx, jobID, "manual")
	if r := poll(); r == nil || r.ID != b3 || len(r.KeepSnapshots) != 0 {
		t.Fatalf("backup after finish still keeps snapshots: %+v", r)
	}
	e.store.FinishRun(ctx, a.ID, b3, api.RunResult{Status: api.StatusFailed})

	// Copies of VM backups are restored like the originals.
	offsite, err := e.store.CreateTarget(ctx, server.Target{Name: "offsite", Kind: "local", URL: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	copyJob, err := e.store.CreateJob(ctx, server.Job{Kind: server.JobCopy, SourceJobID: &jobID, TargetID: offsite, Name: "vms offsite"})
	if err != nil {
		t.Fatal(err)
	}
	cr, err := e.store.QueueBackup(ctx, copyJob, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if r := poll(); r == nil || r.ID != cr || r.Kind != api.KindCopy {
		t.Fatalf("copy run: %+v", r)
	}
	copySnap := strings.Repeat("ef", 32)
	if err := e.store.FinishRun(ctx, a.ID, cr, api.RunResult{Status: api.StatusSuccess, SnapshotID: copySnap,
		Details: json.RawMessage(`{"guests":[{"vmid":100,"type":"qemu","name":"web","disks":1,"platform":"proxmox"}]}`)}); err != nil {
		t.Fatal(err)
	}
	if p := get(fmt.Sprintf("/runs/%d", cr)); !strings.Contains(p, "Restore a VM or container") || !strings.Contains(p, "Browse files") {
		t.Error("copy run page lacks the VM restore")
	}
	if p := get("/restore?type=vm&src=vm%3aproxmox%3a100"); !strings.Contains(p, fmt.Sprintf("run=%d", cr)) || !strings.Contains(p, "copy") {
		t.Error("restore wizard does not offer the copy of the VM backup")
	}
	rr, err := e.store.QueueVMRestore(ctx, cr, a.ID, api.VMRestore{VMID: 100, NewVMID: -1})
	if err != nil {
		t.Fatal(err)
	}
	if r := poll(); r == nil || r.ID != rr || r.Kind != api.KindVMRestore || r.SnapshotID != copySnap || r.Repository.URL == "" {
		t.Fatalf("restore from copy: %+v", r)
	}
	e.store.FinishRun(ctx, a.ID, rr, api.RunResult{Status: api.StatusSuccess})

	// Restore tests with boot test.
	if resp := post("/settings/tests", url.Values{"enabled": {"on"}, "every": {"weekly"}, "files": {"10"}, "max_mb": {"100"}, "blocks": {"100"},
		"boot_vms": {"on"}, "boot_minutes": {"0"}}); !strings.Contains(resp.Header.Get("Location"), "err=") && resp.Request.URL.Query().Get("err") == "" {
		t.Error("boot test of 0 minutes accepted")
	}
	post("/settings/tests", url.Values{"enabled": {"on"}, "every": {"weekly"}, "files": {"10"}, "max_mb": {"100"}, "blocks": {"100"},
		"boot_vms": {"on"}, "boot_minutes": {"3"}})
	vr, err := e.store.QueueRestoreTest(ctx, jobID, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if r := poll(); r == nil || r.ID != vr || r.Kind != api.KindVerify || r.VerifyBootSeconds != 180 {
		t.Fatalf("restore test run: %+v", r)
	}
	e.store.FinishRun(ctx, a.ID, vr, api.RunResult{Status: api.StatusFailed, Message: "BOOT TEST FAILED",
		Details: json.RawMessage(`{"boot":[{"vmid":100,"name":"web","booted":true,"seconds":42},{"vmid":101,"name":"db","error":"the VM stopped by itself"}]}`)})
	if p := get(fmt.Sprintf("/runs/%d", vr)); !strings.Contains(p, "Boot test") || !strings.Contains(p, "QEMU guest agent answered") || !strings.Contains(p, "the VM stopped by itself") {
		t.Error("restore test page lacks the boot results")
	}
	if p := get("/settings/tests"); !strings.Contains(p, `name="boot_vms" checked`) || !strings.Contains(p, `value="3"`) {
		t.Error("boot test settings not shown")
	}
}
