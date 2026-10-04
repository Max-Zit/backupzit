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
	"net/http"
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
