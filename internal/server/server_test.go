package server_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/backupzit/backupzit/internal/agent"
	"github.com/backupzit/backupzit/internal/api"
	"github.com/backupzit/backupzit/internal/backend"
	"github.com/backupzit/backupzit/internal/checker"
	"github.com/backupzit/backupzit/internal/repo"
	"github.com/backupzit/backupzit/internal/server"
	"github.com/backupzit/backupzit/internal/testutil"
	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

func freePort(t *testing.T) uint32 {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return uint32(l.Addr().(*net.TCPAddr).Port)
}

type env struct {
	store *server.Store
	srv   *server.Server
	ts    *httptest.Server
	fp    string
	ctx   context.Context
}

func setup(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	port := freePort(t)
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().Port(port).
		RuntimePath(filepath.Join(dir, "pg")).Logger(io.Discard))
	if err := pg.Start(); err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { pg.Stop() })

	ctx := context.Background()
	pool, err := server.OpenDB(ctx, fmt.Sprintf("postgres://postgres:postgres@localhost:%d/postgres?sslmode=disable", port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	store := server.NewStore(pool)
	if _, err := store.EnsureAdmin(ctx, "admin", "admin-pass-123"); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := server.New(store, log)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := server.LoadOrCreateCert(filepath.Join(dir, "tls"), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.CertFingerprint = server.CertFingerprint(cert)
	srv.PollInterval = 1
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return &env{store: store, srv: srv, ts: ts, fp: srv.CertFingerprint, ctx: ctx}
}

func writeTree(t *testing.T, root string) map[string]string {
	files := map[string]string{
		"a.txt":             "alpha",
		"sub/b.txt":         strings.Repeat("bravo ", 10000),
		"sub/deeper/c.bin":  string(make([]byte, 300000)),
		"unicode ćčž/d.txt": "delta",
	}
	for p, c := range files {
		fp := filepath.Join(root, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(fp), 0o755)
		if err := os.WriteFile(fp, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return files
}

// runAgent polls once and waits for any assigned run to finish.
func runAgent(t *testing.T, a *agent.Agent) {
	t.Helper()
	if _, err := a.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	a.Wait()
}

func TestAgentLifecycle(t *testing.T) {
	e := setup(t)
	ctx := e.ctx

	repoBase := t.TempDir()
	targetID, err := e.store.CreateTarget(ctx, server.Target{Name: "local", Kind: "local", URL: repoBase})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CreateTarget(ctx, server.Target{Name: "local", Kind: "local", URL: repoBase}); err == nil {
		t.Error("duplicate target name accepted")
	}
	token, _, err := e.store.CreateEnrollmentToken(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// --- enrollment security
	if _, err := agent.Enroll(ctx, e.ts.URL, token, "SHA256:wrongwrongwrong", "test"); err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("expected fingerprint mismatch, got %v", err)
	}
	if _, err := agent.Enroll(ctx, e.ts.URL, "bad-token", e.fp, "test"); err == nil {
		t.Fatal("expected invalid token error")
	}
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "agent.json")
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	cfg, err = agent.LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	ag.VSS = false // tests do not run elevated

	// Wrong secret is rejected.
	bad := *cfg
	bad.Secret = "nope"
	if _, err := agent.New(&bad, slog.New(slog.NewTextHandler(io.Discard, nil)), "test").PollOnce(ctx); err == nil {
		t.Fatal("expected auth failure with wrong secret")
	}

	runAgent(t, ag) // heartbeat, no work
	agents, _ := e.store.ListAgents(ctx)
	if len(agents) != 1 || !agents[0].Online() || agents[0].OS == "" {
		t.Fatalf("agent not registered/online: %+v", agents)
	}
	agentID := agents[0].ID

	// --- backup job, run now
	src := filepath.Join(t.TempDir(), "data")
	files := writeTree(t, src)
	jobID, err := e.store.CreateJob(ctx, server.Job{AgentID: agentID, TargetID: targetID, Name: "docs", Paths: []string{src}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	runID, err := e.store.QueueBackup(ctx, jobID, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.QueueBackup(ctx, jobID, "manual"); err != server.ErrRunActive {
		t.Fatalf("expected ErrRunActive for duplicate queue, got %v", err)
	}
	runAgent(t, ag)
	run, err := e.store.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != api.StatusSuccess || run.SnapshotID == "" {
		t.Fatalf("backup run: status %s message %q errors %v", run.Status, run.Message, run.Errors)
	}
	if !strings.Contains(run.RepoURL, "_"+strings.ReplaceAll(cfg.AgentUUID, "-", "")[:8]) {
		t.Errorf("unexpected repo url %s", run.RepoURL)
	}

	// --- restore from the console into a folder
	restoreDir := filepath.Join(t.TempDir(), "restore")
	rrID, err := e.store.QueueRestore(ctx, runID, agentID, restoreDir, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	runAgent(t, ag)
	rr, _ := e.store.GetRun(ctx, rrID)
	if rr.Status != api.StatusSuccess {
		t.Fatalf("restore run: status %s message %q errors %v", rr.Status, rr.Message, rr.Errors)
	}
	comps := strings.Split(strings.ReplaceAll(strings.Replace(src, ":", "", 1), `\`, "/"), "/")
	restored := filepath.Join(append([]string{restoreDir}, comps...)...)
	for p, want := range files {
		got, err := os.ReadFile(filepath.Join(restored, filepath.FromSlash(p)))
		if err != nil {
			t.Fatalf("restored file %s: %v", p, err)
		}
		if string(got) != want {
			t.Errorf("restored %s differs", p)
		}
	}

	// --- backup with an unreadable path finishes with failed status
	jobBad, _ := e.store.CreateJob(ctx, server.Job{AgentID: agentID, TargetID: targetID, Name: "missing", Paths: []string{filepath.Join(src, "does-not-exist")}, Enabled: true})
	badRun, _ := e.store.QueueBackup(ctx, jobBad, "manual")
	runAgent(t, ag)
	if r, _ := e.store.GetRun(ctx, badRun); r.Status != api.StatusFailed || r.Message == "" {
		t.Errorf("missing source: want failed with message, got %s %q", r.Status, r.Message)
	}

	// --- scheduler queues a due job once
	jobSched, err := e.store.CreateJob(ctx, server.Job{AgentID: agentID, TargetID: targetID, Name: "sched", Paths: []string{src}, Schedule: "*/5 * * * *", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CreateJob(ctx, server.Job{AgentID: agentID, TargetID: targetID, Name: "x", Paths: []string{src}, Schedule: "not a cron"}); err == nil {
		t.Error("invalid schedule accepted")
	}
	sch := server.NewScheduler(e.store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sch.Tick(ctx, time.Now().Add(6*time.Minute))
	sch.Tick(ctx, time.Now().Add(6*time.Minute))
	runs, _ := e.store.ListRuns(ctx, server.RunFilter{JobID: jobSched})
	if len(runs) != 1 || runs[0].Trigger != "schedule" || runs[0].Status != api.StatusQueued {
		t.Fatalf("scheduler: want 1 queued scheduled run, got %+v", runs)
	}
	runAgent(t, ag)
	if r, _ := e.store.GetRun(ctx, runs[0].ID); r.Status != api.StatusSuccess {
		t.Fatalf("scheduled run: %s %q", r.Status, r.Message)
	}

	// --- incremental: second backup of same job reads nothing new
	run2, _ := e.store.QueueBackup(ctx, jobID, "manual")
	runAgent(t, ag)
	r2, _ := e.store.GetRun(ctx, run2)
	if r2.Status != api.StatusSuccess || !strings.Contains(strings.ReplaceAll(string(r2.Stats), " ", ""), `"bytes_read":0,`) {
		t.Errorf("incremental run: %s stats %s", r2.Status, r2.Stats)
	}

	// --- restore onto a different machine
	cfg2, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	ag2 := agent.New(cfg2, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	ag2.VSS = false
	all, _ := e.store.ListAgents(ctx)
	var otherAgent int64
	for _, a := range all {
		if a.UUID == cfg2.AgentUUID {
			otherAgent = a.ID
		}
	}
	otherDir := filepath.Join(t.TempDir(), "other")
	xr, err := e.store.QueueRestore(ctx, runID, otherAgent, otherDir, []string{filepath.Join(src, "a.txt")}, true)
	if err != nil {
		t.Fatal(err)
	}
	runAgent(t, ag) // first agent must not pick up the other agent's run
	if r, _ := e.store.GetRun(ctx, xr); r.Status != api.StatusQueued {
		t.Fatalf("run for agent 2 was taken by agent 1: %s", r.Status)
	}
	runAgent(t, ag2)
	if r, _ := e.store.GetRun(ctx, xr); r.Status != api.StatusSuccess {
		t.Fatalf("cross-machine restore: %s %q %v", r.Status, r.Message, r.Errors)
	}
	if b, err := os.ReadFile(filepath.Join(append([]string{otherDir}, comps...)...) + string(filepath.Separator) + "a.txt"); err != nil || string(b) != "alpha" {
		t.Errorf("cross-machine restore content: %q %v", b, err)
	}

	// --- stale runs of a vanished agent are failed
	stale, _ := e.store.QueueBackup(ctx, jobID, "manual")
	if _, err := e.store.ClaimRun(ctx, agentID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if n, err := e.store.FailStaleRuns(ctx, time.Second); err != nil || n != 1 {
		t.Fatalf("fail stale: n=%d err=%v", n, err)
	}
	if r, _ := e.store.GetRun(ctx, stale); r.Status != api.StatusFailed {
		t.Errorf("stale run status %s", r.Status)
	}
}

func TestWebUI(t *testing.T) {
	e := setup(t)
	jar, _ := cookiejar.New(nil)
	c := e.ts.Client()
	c.Jar = jar
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	// Not logged in: redirect to login.
	resp, err := c.Get(e.ts.URL + "/jobs")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("unauthenticated: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}

	post := func(path string, form url.Values, origin bool) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, e.ts.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin {
			req.Header.Set("Origin", e.ts.URL)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	if r := post("/login", url.Values{"username": {"admin"}, "password": {"wrong"}}, true); r.StatusCode != http.StatusOK {
		t.Fatalf("bad login should re-render form, got %d", r.StatusCode)
	}
	if r := post("/login", url.Values{"username": {"admin"}, "password": {"admin-pass-123"}}, true); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", r.StatusCode)
	}
	// CSRF: POST without Origin is rejected.
	if r := post("/targets", url.Values{"name": {"evil"}, "kind": {"local"}, "url": {"C:/x"}}, false); r.StatusCode != http.StatusForbidden {
		t.Fatalf("post without origin: %d", r.StatusCode)
	}
	if ts, _ := e.store.ListTargets(e.ctx); len(ts) != 0 {
		t.Fatal("cross-site post created a target")
	}
	if r := post("/targets", url.Values{"name": {"nas"}, "kind": {"sftp"},
		"url": {"sftp://u@10.0.0.1/backups"}, "sftp_password": {"x"}, "sftp_host_key": {"SHA256:abc"}}, true); r.StatusCode != http.StatusSeeOther ||
		strings.Contains(r.Header.Get("Location"), "err=") {
		t.Fatalf("create target: %d %s", r.StatusCode, r.Header.Get("Location"))
	}

	get := func(path string) string {
		resp, err := c.Get(e.ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, resp.StatusCode, b)
		}
		return string(b)
	}
	for _, p := range []string{"/", "/jobs", "/runs", "/agents", "/targets"} {
		get(p)
	}
	// Enrollment token page shows the fingerprint.
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/agents/token", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.ts.URL)
	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if page := html.UnescapeString(string(b)); !strings.Contains(page, e.fp) || !strings.Contains(page, "msiexec") {
		t.Fatalf("enrollment page lacks fingerprint/instructions")
	}
	if !strings.Contains(get("/targets"), "sftp://u@10.0.0.1/backups") {
		t.Error("target not listed")
	}
	// Passwords are never rendered.
	if strings.Contains(get("/targets"), `value="x"`) {
		t.Error("password leaked into page")
	}
}

func TestImageJobs(t *testing.T) {
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

	// The agent reports two disks; disk 0 runs Windows.
	inv := `[{"number":0,"model":"Msft Virtual Disk","size":68719476736,"sector_size":512,"style":"gpt","system":true,
	  "partitions":[{"number":1,"offset":1048576,"length":209715200,"gpt_type":"C12A7328-F81F-11D2-BA4B-00A0C93EC93B"},
	                {"number":3,"offset":227540992,"length":67554508800,"mount_points":["C:\\"],"file_system":"NTFS"}]},
	  {"number":1,"size":68719476736,"sector_size":512,"style":"mbr","partitions":[]}]`
	if err := e.store.TouchAgent(ctx, a.ID, api.PollRequest{Disks: json.RawMessage(inv)}); err != nil {
		t.Fatal(err)
	}
	a, _ = e.store.GetAgent(ctx, a.ID)
	if len(a.Disks()) != 2 || !a.Disks()[0].System {
		t.Fatalf("inventory not stored: %+v", a.Disks())
	}

	disk := func(n int) *int { return &n }
	if _, err := e.store.CreateJob(ctx, server.Job{Kind: server.JobImage, AgentID: a.ID, TargetID: targetID, Name: "x", ImageDisk: disk(5)}); err == nil {
		t.Error("image job for a missing disk accepted")
	}
	if _, err := e.store.CreateJob(ctx, server.Job{Kind: server.JobImage, AgentID: a.ID, TargetID: targetID, Name: "x", ImageDisk: disk(0), ImagePartitions: []int{9}}); err == nil {
		t.Error("image job for a missing partition accepted")
	}
	jobID, err := e.store.CreateJob(ctx, server.Job{Kind: server.JobImage, AgentID: a.ID, TargetID: targetID, Name: "System image", ImageDisk: disk(0), ImagePartitions: []int{3}})
	if err != nil {
		t.Fatal(err)
	}
	if j, _ := e.store.GetJob(ctx, jobID); j.Kind != server.JobImage || len(j.Paths) != 0 ||
		server.DescribeImageSelection(j.ImageDisk, j.ImagePartitions) != "Disk 0: partitions 3" {
		t.Fatalf("job: %+v", j)
	}

	runID, err := e.store.QueueBackup(ctx, jobID, "manual")
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := e.store.ClaimRun(ctx, a.ID)
	if err != nil || claimed == nil || claimed.ID != runID || claimed.Kind != api.KindImageBackup ||
		claimed.ImageDisk == nil || *claimed.ImageDisk != 0 || len(claimed.ImagePartitions) != 1 {
		t.Fatalf("claimed run: %+v %v", claimed, err)
	}
	details := `{"number":0,"model":"Msft Virtual Disk","size":68719476736,"sector_size":512,"style":"gpt","head":"` + strings.Repeat("ab", 32) + `",
	  "partitions":[{"number":1,"offset":1048576,"length":209715200,"included":false},
	                {"number":3,"offset":227540992,"length":67554508800,"mount_points":["C:\\"],"file_system":"NTFS","included":true,"method":"used-blocks","source":"vss","stored_bytes":25000000000}]}`
	if err := e.store.FinishRun(ctx, a.ID, runID, api.RunResult{Status: api.StatusSuccess, SnapshotID: strings.Repeat("cd", 32),
		Stats: json.RawMessage(`{"bytes_read":25000000000}`), Details: json.RawMessage(details)}); err != nil {
		t.Fatal(err)
	}

	// Image restore: never onto the system disk, fine onto disk 1.
	if _, err := e.store.QueueImageRestore(ctx, runID, a.ID, 0, false); err == nil {
		t.Error("image restore onto the system disk accepted")
	}
	rr, err := e.store.QueueImageRestore(ctx, runID, a.ID, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := e.store.GetRun(ctx, rr); r.Kind != api.KindImageRestore || r.TargetDisk == nil || *r.TargetDisk != 1 || !r.KeepOffline {
		t.Fatalf("restore run: %+v", r)
	}

	// Pages render the inventory and the disk layout.
	jar, _ := cookiejar.New(nil)
	c := e.ts.Client()
	c.Jar = jar
	form := url.Values{"username": {"admin"}, "password": {"admin-pass-123"}}
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.ts.URL)
	if resp, err := c.Do(req); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}
	body := func(p string) string {
		resp, err := c.Get(e.ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s: %d", p, resp.StatusCode)
		}
		return html.UnescapeString(string(b))
	}
	if p := body("/jobs"); !strings.Contains(p, `"Msft Virtual Disk"`) || !strings.Contains(p, "Disk 0: partitions 3") {
		t.Error("jobs page lacks inventory or image selection")
	}
	if p := body(fmt.Sprintf("/runs/%d", runID)); !strings.Contains(p, "Image backup #") || !strings.Contains(p, "VSS snapshot, used blocks") ||
		!strings.Contains(p, "layout only") || !strings.Contains(p, "Restore this image to a disk") {
		t.Error("image run page lacks layout or restore form")
	}
}

func TestRecovery(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	jar, _ := cookiejar.New(nil)
	c := e.ts.Client()
	c.Jar = jar
	post := func(p string, form url.Values) (*http.Response, string) {
		req, _ := http.NewRequest(http.MethodPost, e.ts.URL+p, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", e.ts.URL)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, html.UnescapeString(string(b))
	}
	post("/login", url.Values{"username": {"admin"}, "password": {"admin-pass-123"}})

	// Create a recovery code and download recovery.json with it.
	_, page := post("/recovery/token", nil)
	start := strings.Index(page, `id="rtok">`)
	if start < 0 {
		t.Fatal("recovery page shows no code")
	}
	tok := page[start+len(`id="rtok">`):]
	tok = tok[:strings.Index(tok, "<")]
	resp, body := post("/recovery/recovery.json", url.Values{"token": {tok}})
	var rj map[string]string
	if err := json.Unmarshal([]byte(body), &rj); err != nil || rj["token"] != tok || rj["fingerprint"] != e.fp ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), "recovery.json") {
		t.Fatalf("recovery.json: %v %q", err, body)
	}

	// Recovery media enrolls with that code and shows up as a recovery agent.
	cfg, err := agent.EnrollRecovery(ctx, rj["server"], rj["token"], rj["fingerprint"], "test")
	if err != nil {
		t.Fatal(err)
	}
	fp, err := agent.FetchFingerprint(ctx, e.ts.URL)
	if err != nil || fp != e.fp {
		t.Errorf("FetchFingerprint: %s %v, want %s", fp, err, e.fp)
	}
	agents, _ := e.store.ListAgents(ctx)
	if len(agents) != 1 || !agents[0].Recovery || agents[0].UUID != cfg.AgentUUID {
		t.Fatalf("recovery agent not registered: %+v", agents)
	}
	resp2, err := c.Get(e.ts.URL + "/recovery")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if !strings.Contains(string(b), agents[0].Hostname) {
		t.Error("recovery page does not list the recovery agent")
	}
}

func TestRetentionJob(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	repoBase := t.TempDir()
	targetID, _ := e.store.CreateTarget(ctx, server.Target{Name: "local", Kind: "local", URL: repoBase})
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	ag.VSS = false
	agents, _ := e.store.ListAgents(ctx)
	src := filepath.Join(t.TempDir(), "data")
	jobID, err := e.store.CreateJob(ctx, server.Job{AgentID: agents[0].ID, TargetID: targetID, Name: "keep one",
		Paths: []string{src}, Enabled: true, Retention: repo.RetentionPolicy{KeepLast: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if j, _ := e.store.GetJob(ctx, jobID); j.Retention.KeepLast != 1 {
		t.Fatalf("retention not stored: %+v", j.Retention)
	}
	var runIDs []int64
	for i := 0; i < 3; i++ {
		// Each backup has unique data that becomes garbage once expired.
		os.MkdirAll(src, 0o755)
		b := make([]byte, 2<<20)
		for k := range b {
			b[k] = byte(i*31 + k*7)
		}
		os.WriteFile(filepath.Join(src, "f.bin"), b, 0o644)
		id, err := e.store.QueueBackup(ctx, jobID, "manual")
		if err != nil {
			t.Fatal(err)
		}
		runAgent(t, ag)
		r, _ := e.store.GetRun(ctx, id)
		if r.Status != api.StatusSuccess {
			t.Fatalf("run %d: %s %q", i, r.Status, r.Message)
		}
		if i > 0 && !strings.Contains(r.Message, "removed 1 old backups") {
			t.Errorf("run %d message %q", i, r.Message)
		}
		runIDs = append(runIDs, id)
	}
	for i, id := range runIDs {
		r, _ := e.store.GetRun(ctx, id)
		if r.Expired != (i < 2) {
			t.Errorf("run %d expired=%v", i, r.Expired)
		}
	}
	// Only one snapshot is left and the repository verifies.
	be, _ := backend.OpenLocal(filepath.Join(repoBase, agents[0].RepoDir))
	rp, err := repo.Open(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	sns, _ := rp.ListSnapshots(ctx)
	res, err := checker.Run(ctx, rp, checker.Options{ReadData: true})
	if len(sns) != 1 || err != nil || !res.OK() {
		t.Fatalf("snapshots %d, check %v %+v", len(sns), err, res)
	}
}

func TestS3Target(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	s3, err := testutil.StartS3Server("company-backups")
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()

	// Create the target through the web form.
	jar, _ := cookiejar.New(nil)
	c := e.ts.Client()
	c.Jar = jar
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	post := func(p string, form url.Values) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, e.ts.URL+p, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", e.ts.URL)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	post("/login", url.Values{"username": {"admin"}, "password": {"admin-pass-123"}})
	r := post("/targets", url.Values{"name": {"minio"}, "kind": {"s3"}, "s3_endpoint": {"http://" + s3.Host + "/"},
		"s3_bucket": {"company-backups"}, "s3_prefix": {"/backupzit/"}, "s3_http": {"on"},
		"s3_access_key": {"AK"}, "s3_secret_key": {"SK"}, "s3_region": {"us-east-1"}})
	if loc := r.Header.Get("Location"); strings.Contains(loc, "err=") {
		t.Fatalf("create s3 target: %s", loc)
	}
	targets, _ := e.store.ListTargets(ctx)
	if len(targets) != 1 || targets[0].URL != "s3://"+s3.Host+"/company-backups/backupzit?tls=false" || targets[0].S3SecretKey != "SK" {
		t.Fatalf("target: %+v", targets)
	}

	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	ag.VSS = false
	agents, _ := e.store.ListAgents(ctx)
	src := filepath.Join(t.TempDir(), "data")
	files := writeTree(t, src)
	jobID, _ := e.store.CreateJob(ctx, server.Job{AgentID: agents[0].ID, TargetID: targets[0].ID, Name: "to s3", Paths: []string{src}, Enabled: true})
	runID, _ := e.store.QueueBackup(ctx, jobID, "manual")
	runAgent(t, ag)
	run, _ := e.store.GetRun(ctx, runID)
	if run.Status != api.StatusSuccess || !strings.HasSuffix(run.RepoURL, "/company-backups/backupzit/"+agents[0].RepoDir+"?tls=false") {
		t.Fatalf("backup to s3: %s %q repo %s", run.Status, run.Message, run.RepoURL)
	}
	dst := filepath.Join(t.TempDir(), "out")
	rr, _ := e.store.QueueRestore(ctx, runID, agents[0].ID, dst, nil, true)
	runAgent(t, ag)
	if r, _ := e.store.GetRun(ctx, rr); r.Status != api.StatusSuccess {
		t.Fatalf("restore from s3: %s %q %v", r.Status, r.Message, r.Errors)
	}
	comps := strings.Split(strings.ReplaceAll(strings.Replace(src, ":", "", 1), `\`, "/"), "/")
	for p, want := range files {
		got, err := os.ReadFile(filepath.Join(append(append([]string{dst}, comps...), filepath.FromSlash(p))...))
		if err != nil || string(got) != want {
			t.Errorf("restored %s: %v", p, err)
		}
	}
}

func TestNotifications(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	smtpSrv, err := testutil.StartSMTPServer()
	if err != nil {
		t.Fatal(err)
	}
	defer smtpSrv.Close()
	host, port, _ := net.SplitHostPort(smtpSrv.Addr)

	// Configure through the settings form, including a test email.
	jar, _ := cookiejar.New(nil)
	c := e.ts.Client()
	c.Jar = jar
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	post := func(p string, form url.Values) string {
		req, _ := http.NewRequest(http.MethodPost, e.ts.URL+p, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", e.ts.URL)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Header.Get("Location")
	}
	post("/login", url.Values{"username": {"admin"}, "password": {"admin-pass-123"}})
	form := url.Values{"enabled": {"on"}, "host": {host}, "port": {port}, "security": {"none"},
		"from": {"backup@example.com"}, "to": {"it@example.com, boss@example.com"},
		"on_failure": {"on"}, "daily_report": {"on"}, "daily_hour": {"8"}}
	form.Set("action", "test")
	if loc := post("/settings/email", form); strings.Contains(loc, "err=") {
		t.Fatalf("test email: %s", loc)
	}
	if m := smtpSrv.Messages(); len(m) != 1 || len(m[0].To) != 2 || !strings.Contains(m[0].Data, "Test message") {
		t.Fatalf("test email not received: %+v", m)
	}
	form.Set("action", "save")
	if loc := post("/settings/email", form); strings.Contains(loc, "err=") {
		t.Fatalf("save settings: %s", loc)
	}

	// A failed and a successful run: only the failure is mailed, once.
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, _ := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	ag := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	ag.VSS = false
	agents, _ := e.store.ListAgents(ctx)
	targetID, _ := e.store.CreateTarget(ctx, server.Target{Name: "local", Kind: "local", URL: t.TempDir()})
	bad, _ := e.store.CreateJob(ctx, server.Job{AgentID: agents[0].ID, TargetID: targetID, Name: "broken", Paths: []string{filepath.Join(t.TempDir(), "missing")}, Enabled: true})
	good, _ := e.store.CreateJob(ctx, server.Job{AgentID: agents[0].ID, TargetID: targetID, Name: "fine", Paths: []string{t.TempDir()}, Enabled: true})
	e.store.QueueBackup(ctx, bad, "manual")
	runAgent(t, ag)
	e.store.QueueBackup(ctx, good, "manual")
	runAgent(t, ag)

	n := server.NewNotifier(e.store, slog.New(slog.NewTextHandler(io.Discard, nil)), func() string { return "https://backup.example" })
	n.Pass(ctx, time.Date(2026, 10, 2, 10, 0, 0, 0, time.Local)) // not report hour
	n.Pass(ctx, time.Date(2026, 10, 2, 10, 0, 0, 0, time.Local)) // nothing new
	msgs := smtpSrv.Messages()
	if len(msgs) != 2 || !strings.Contains(msgs[1].Data, `FAILED Backup "broken"`) || !strings.Contains(msgs[1].Data, "https://backup.example/runs/") {
		t.Fatalf("failure alert: %d messages, last %q", len(msgs), msgs[len(msgs)-1].Data)
	}

	// Daily report: once at 08:xx, not again the same day.
	n.Pass(ctx, time.Date(2026, 10, 2, 8, 0, 0, 0, time.Local))
	n.Pass(ctx, time.Date(2026, 10, 2, 8, 30, 0, 0, time.Local))
	msgs = smtpSrv.Messages()
	if len(msgs) != 3 || !strings.Contains(msgs[2].Data, "Daily report 2026-10-02") || !strings.Contains(msgs[2].Data, "1 failed") {
		t.Fatalf("daily report: %d messages: %q", len(msgs), msgs[len(msgs)-1].Data)
	}
}
