package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
	"github.com/jackc/pgx/v5/pgxpool"
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
	pool  *pgxpool.Pool
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
	if err := store.UseSecretKey(bytes.Repeat([]byte{7}, 32)); err != nil {
		t.Fatal(err)
	}
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
	return &env{store: store, pool: pool, srv: srv, ts: ts, fp: srv.CertFingerprint, ctx: ctx}
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
	if err := e.store.TouchAgent(ctx, a.ID, api.PollRequest{Disks: json.RawMessage(inv)}, ""); err != nil {
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
	// Immutability needs a bucket with Object Lock; the form stores the period.
	r = post("/targets", url.Values{"name": {"locked"}, "kind": {"s3"}, "s3_endpoint": {s3.Host},
		"s3_bucket": {"company-backups"}, "s3_http": {"on"}, "s3_immutable": {"on"}, "s3_lock_days": {"30"},
		"s3_access_key": {"AK"}, "s3_secret_key": {"SK"}})
	if loc := r.Header.Get("Location"); strings.Contains(loc, "err=") {
		t.Fatalf("create locked target: %s", loc)
	}
	r = post("/targets", url.Values{"name": {"bad"}, "kind": {"s3"}, "s3_endpoint": {s3.Host},
		"s3_bucket": {"b"}, "s3_immutable": {"on"}, "s3_lock_days": {"0"}, "s3_access_key": {"AK"}, "s3_secret_key": {"SK"}})
	if loc := r.Header.Get("Location"); !strings.Contains(loc, "err=") {
		t.Error("immutability of 0 days accepted")
	}
	if all, _ := e.store.ListTargets(ctx); len(all) != 2 || all[0].Name != "locked" || all[0].S3LockDays != 30 || all[1].S3LockDays != 0 {
		t.Fatalf("lock days: %+v", all)
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

func TestEncryptedTarget(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	jar, _ := cookiejar.New(nil)
	c := e.ts.Client()
	c.Jar = jar
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
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
	repoBase := t.TempDir()
	post("/targets", url.Values{"name": {"enc"}, "kind": {"local"}, "url": {repoBase}, "encrypted": {"on"}})
	targets, _ := e.store.ListTargets(ctx)
	if len(targets) != 1 || !targets[0].Encrypted || len(targets[0].RecoveryKey) != 39 {
		t.Fatalf("encrypted target: %+v", targets)
	}
	key := targets[0].RecoveryKey
	if _, page := post(fmt.Sprintf("/targets/%d/recovery-key", targets[0].ID), nil); !strings.Contains(page, key) {
		t.Error("recovery key page does not show the key")
	}
	if resp, sheet := post(fmt.Sprintf("/targets/%d/recovery-sheet", targets[0].ID), nil); !strings.Contains(sheet, "RECOVERY KEY:   "+key) ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), "recovery-sheet") {
		t.Error("recovery sheet")
	}

	// Backups through the agent are encrypted and readable with the key.
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, _ := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	ag := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	ag.VSS = false
	agents, _ := e.store.ListAgents(ctx)
	src := filepath.Join(t.TempDir(), "data")
	writeTree(t, src)
	jobID, _ := e.store.CreateJob(ctx, server.Job{AgentID: agents[0].ID, TargetID: targets[0].ID, Name: "enc", Paths: []string{src}, Enabled: true})
	runID, _ := e.store.QueueBackup(ctx, jobID, "manual")
	runAgent(t, ag)
	if r, _ := e.store.GetRun(ctx, runID); r.Status != api.StatusSuccess {
		t.Fatalf("encrypted backup: %s %q", r.Status, r.Message)
	}
	be, _ := backend.OpenLocal(filepath.Join(repoBase, agents[0].RepoDir))
	if _, err := repo.Open(ctx, be); !errors.Is(err, repo.ErrPasswordRequired) {
		t.Fatalf("repository is not encrypted: %v", err)
	}
	rp, err := repo.Open(ctx, be, repo.Password(key))
	if err != nil {
		t.Fatal(err)
	}
	if res, err := checker.Run(ctx, rp, checker.Options{ReadData: true}); err != nil || !res.OK() || res.Snapshots != 1 {
		t.Fatalf("check with recovery key: %v %+v", err, res)
	}
}

func TestHardenedTarget(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	hr, err := testutil.StartHardened(filepath.Join(t.TempDir(), "repo"), 7)
	if err != nil {
		t.Fatal(err)
	}
	defer hr.Close()

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
	r := post("/targets", url.Values{"name": {"vault"}, "kind": {"hardened"}, "hardened_host": {hr.Host},
		"hardened_path": {"/office/"}, "hardened_key": {hr.Key}, "hardened_fingerprint": {hr.Fingerprint}, "encrypted": {"on"}})
	if loc := r.Header.Get("Location"); strings.Contains(loc, "err=") {
		t.Fatalf("create hardened target: %s", loc)
	}
	r = post("/targets", url.Values{"name": {"bad"}, "kind": {"hardened"}, "hardened_host": {"10.0.0.1"}, "hardened_key": {"k"}})
	if loc := r.Header.Get("Location"); !strings.Contains(loc, "err=") {
		t.Error("hardened target without fingerprint accepted")
	}
	targets, _ := e.store.ListTargets(ctx)
	if len(targets) != 1 || targets[0].URL != "hardened://"+hr.Host+"/office" || targets[0].HardenedKey != hr.Key {
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
	jobID, _ := e.store.CreateJob(ctx, server.Job{AgentID: agents[0].ID, TargetID: targets[0].ID, Name: "to vault", Paths: []string{src}, Enabled: true,
		Retention: repo.RetentionPolicy{KeepLast: 1}})
	var runID int64
	for i := 0; i < 2; i++ {
		runID, _ = e.store.QueueBackup(ctx, jobID, "manual")
		runAgent(t, ag)
		run, _ := e.store.GetRun(ctx, runID)
		if run.Status != api.StatusSuccess {
			t.Fatalf("backup to hardened repository: %s %q", run.Status, run.Message)
		}
	}
	// Retention removed the first snapshot: on the server it is only hidden.
	if st, _ := hr.Store.Stats(); st.Hidden == 0 {
		t.Errorf("expected hidden files after retention: %+v", st)
	}
	dst := filepath.Join(t.TempDir(), "out")
	rr, _ := e.store.QueueRestore(ctx, runID, agents[0].ID, dst, nil, true)
	runAgent(t, ag)
	if r, _ := e.store.GetRun(ctx, rr); r.Status != api.StatusSuccess {
		t.Fatalf("restore from hardened repository: %s %q %v", r.Status, r.Message, r.Errors)
	}
	comps := strings.Split(strings.ReplaceAll(strings.Replace(src, ":", "", 1), `\`, "/"), "/")
	for p, want := range files {
		got, err := os.ReadFile(filepath.Join(append(append([]string{dst}, comps...), filepath.FromSlash(p))...))
		if err != nil || string(got) != want {
			t.Errorf("restored %s: %v", p, err)
		}
	}
}

func TestReportsCalendarDocs(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	jar, _ := cookiejar.New(nil)
	c := e.ts.Client()
	c.Jar = jar
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	do := func(method, p string, form url.Values) (*http.Response, string) {
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req, _ := http.NewRequest(method, e.ts.URL+p, body)
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		req.Header.Set("Origin", e.ts.URL)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}
	do("POST", "/login", url.Values{"username": {"admin"}, "password": {"admin-pass-123"}})

	// Some history: an agent, a job with a schedule and finished runs.
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	ag.VSS = false
	agents, _ := e.store.ListAgents(ctx)
	tid, _ := e.store.CreateTarget(ctx, server.Target{Name: "local", Kind: "local", URL: filepath.Join(t.TempDir(), "repo")})
	src := filepath.Join(t.TempDir(), "data")
	writeTree(t, src)
	jobID, _ := e.store.CreateJob(ctx, server.Job{AgentID: agents[0].ID, TargetID: tid, Name: "=Nightly docs", Paths: []string{src},
		Enabled: true, Schedule: `{"kind":"daily","times":["22:00"]}`})
	for i := 0; i < 2; i++ {
		e.store.QueueBackup(ctx, jobID, "manual")
		runAgent(t, ag)
	}

	resp, body := do("GET", "/reports?period=last7", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "=Nightly docs") || !strings.Contains(body, "100.0 %") {
		t.Fatalf("report page: %d %s", resp.StatusCode, body[:min(len(body), 400)])
	}
	resp, body = do("GET", "/reports/csv?period=last30", nil)
	if resp.StatusCode != 200 || strings.Count(body, "\n") != 3 || !strings.Contains(body, "'=Nightly docs") {
		t.Fatalf("csv: %d %q", resp.StatusCode, body)
	}
	resp, body = do("GET", "/calendar", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "=Nightly docs") || !strings.Contains(body, "22:00") {
		t.Fatalf("calendar: %d", resp.StatusCode)
	}
	for _, p := range []string{"", "/start", "/install", "/agents", "/storage", "/jobs", "/restore", "/images", "/ransomware",
		"/monitoring", "/notifications", "/users", "/cli", "/security", "/troubleshooting"} {
		if resp, _ := do("GET", "/docs"+p, nil); resp.StatusCode != 200 {
			t.Errorf("docs%s: %d", p, resp.StatusCode)
		}
	}
	if resp, _ := do("GET", "/docs/../settings", nil); resp.StatusCode == 200 {
		t.Error("unknown docs page served")
	}

	// Scheduled report
	if resp, _ := do("POST", "/reports/schedules", url.Values{"name": {"Weekly"}, "frequency": {"weekly"}, "hour": {"7"}, "recipients": {"a@example.com"}}); !strings.Contains(resp.Header.Get("Location"), "msg=") {
		t.Fatalf("create schedule: %s", resp.Header.Get("Location"))
	}
	if list, _ := e.store.ListReportSchedules(ctx); len(list) != 1 || list[0].Recipients[0] != "a@example.com" {
		t.Fatalf("schedules: %+v", list)
	}

	// Session settings
	if resp, _ := do("POST", "/settings/sessions", url.Values{"lifetime_hours": {"0"}, "idle_minutes": {"5"}}); !strings.Contains(resp.Header.Get("Location"), "err=") {
		t.Error("lifetime 0 accepted")
	}
	if resp, _ := do("POST", "/settings/sessions", url.Values{"lifetime_hours": {"2"}, "idle_minutes": {"15"}}); !strings.Contains(resp.Header.Get("Location"), "msg=") {
		t.Fatalf("save sessions: %s", resp.Header.Get("Location"))
	}
	if _, body := do("GET", "/settings/security", nil); !strings.Contains(body, `name="lifetime_hours" min="1" max="2160" value="2"`) {
		t.Error("session settings not shown")
	}
}

// client is a browser session for UI tests.
type client struct {
	t   *testing.T
	c   *http.Client
	url string
}

func newClient(t *testing.T, e *env) *client {
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Transport: e.ts.Client().Transport, Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &client{t: t, c: c, url: e.ts.URL}
}

func (c *client) do(method, p string, form url.Values) (int, string, string) {
	c.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, c.url+p, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Origin", c.url)
	resp, err := c.c.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Location"), string(b)
}

func (c *client) login(user, pw string) bool {
	_, loc, _ := c.do("POST", "/login", url.Values{"username": {user}, "password": {pw}})
	return loc == "/"
}

func TestUsersAndRoles(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	admin := newClient(t, e)
	if !admin.login("admin", "admin-pass-123") {
		t.Fatal("admin login")
	}
	// Create one user per role.
	for _, role := range []string{"operator", "restore", "viewer"} {
		_, loc, _ := admin.do("POST", "/users", url.Values{"username": {role + "1"}, "role": {role}, "display_name": {"User " + role},
			"password": {"correct-horse-9"}, "password2": {"correct-horse-9"}})
		if !strings.Contains(loc, "msg=") {
			t.Fatalf("create %s: %s", role, loc)
		}
	}
	if _, loc, _ := admin.do("POST", "/users", url.Values{"username": {"short"}, "role": {"viewer"}, "password": {"abc"}, "password2": {"abc"}}); !strings.Contains(loc, "err=") {
		t.Error("short password accepted")
	}
	if _, loc, _ := admin.do("POST", "/users", url.Values{"username": {"Operator1"}, "role": {"viewer"}, "password": {"correct-horse-9"}, "password2": {"correct-horse-9"}}); !strings.Contains(loc, "err=") {
		t.Error("duplicate (case-insensitive) username accepted")
	}

	viewer := newClient(t, e)
	if !viewer.login("VIEWER1", "correct-horse-9") {
		t.Fatal("viewer login (case-insensitive username)")
	}
	for _, p := range []string{"/", "/jobs", "/runs", "/reports", "/calendar", "/docs", "/account"} {
		if code, _, _ := viewer.do("GET", p, nil); code != 200 {
			t.Errorf("viewer GET %s: %d", p, code)
		}
	}
	for _, p := range []string{"/settings", "/users", "/audit"} {
		if code, _, _ := viewer.do("GET", p, nil); code != http.StatusForbidden {
			t.Errorf("viewer GET %s: %d, want 403", p, code)
		}
	}
	if code, _, _ := viewer.do("POST", "/agents/token", url.Values{}); code != http.StatusForbidden {
		t.Errorf("viewer created an enrollment token: %d", code)
	}
	if _, _, body := viewer.do("GET", "/", nil); strings.Contains(body, `href="/settings/email"`) || strings.Contains(body, `href="/users"`) {
		t.Error("viewer sees admin navigation")
	}

	op := newClient(t, e)
	op.login("operator1", "correct-horse-9")
	if code, _, _ := op.do("POST", "/agents/token", url.Values{}); code != 200 {
		t.Errorf("operator enrollment token: %d", code)
	}
	if code, _, _ := op.do("POST", "/targets", url.Values{"name": {"x"}, "kind": {"local"}, "url": {"/tmp/x"}}); code != http.StatusForbidden {
		t.Errorf("operator created storage: %d", code)
	}
	tid, _ := e.store.CreateTarget(ctx, server.Target{Name: "enc", Kind: "local", URL: filepath.Join(t.TempDir(), "r"), Encrypted: true})
	if code, _, _ := op.do("POST", fmt.Sprintf("/targets/%d/recovery-key", tid), url.Values{}); code != http.StatusForbidden {
		t.Errorf("operator saw a recovery key: %d", code)
	}
	if code, _, _ := admin.do("POST", fmt.Sprintf("/targets/%d/recovery-key", tid), url.Values{}); code != 200 {
		t.Errorf("admin recovery key: %d", code)
	}

	// The last local administrator cannot be removed or demoted.
	users, _ := e.store.ListUsers(ctx)
	var adminID, viewerID int64
	for _, u := range users {
		switch u.Username {
		case "admin":
			adminID = u.ID
		case "viewer1":
			viewerID = u.ID
		}
	}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/users/%d", adminID), url.Values{"role": {"viewer"}}); !strings.Contains(loc, "err=") {
		t.Error("admin demoted itself")
	}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/users/%d/delete", adminID), url.Values{}); !strings.Contains(loc, "err=") {
		t.Error("admin deleted itself")
	}

	// Disabling a user ends its session immediately.
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/users/%d", viewerID), url.Values{"role": {"viewer"}, "disabled": {"on"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("disable: %s", loc)
	}
	if code, loc, _ := viewer.do("GET", "/", nil); code != http.StatusSeeOther || loc != "/login" {
		t.Errorf("disabled user still signed in: %d %s", code, loc)
	}
	if viewer.login("viewer1", "correct-horse-9") {
		t.Error("disabled user signed in")
	}

	// Own password change signs out; the new password works.
	r := newClient(t, e)
	r.login("restore1", "correct-horse-9")
	if _, loc, _ := r.do("POST", "/account/password", url.Values{"current": {"wrong"}, "password": {"new-password-123"}, "password2": {"new-password-123"}}); !strings.Contains(loc, "err=") {
		t.Error("password changed with a wrong current password")
	}
	r.do("POST", "/account/password", url.Values{"current": {"correct-horse-9"}, "password": {"new-password-123"}, "password2": {"new-password-123"}})
	if code, _, _ := r.do("GET", "/", nil); code != http.StatusSeeOther {
		t.Error("session survived password change")
	}
	if !r.login("restore1", "new-password-123") {
		t.Error("new password does not work")
	}

	// Audit log records it all.
	_, _, body := admin.do("GET", "/audit", nil)
	for _, want := range []string{"user.create", "login.failed", "user.update", "storage.recovery_key", "account.password"} {
		if !strings.Contains(body, want) {
			t.Errorf("audit log misses %s", want)
		}
	}
}

func TestSecretsEncryptedAtRest(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	id, err := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "smb", URL: "smb://bk@nas/share", SMBPassword: "smb-Secret-1", Encrypted: true})
	if err != nil {
		t.Fatal(err)
	}
	e.store.SetSetting(ctx, "email", server.EmailSettings{Host: "smtp.example.com", Password: "smtp-Secret-2"})
	// Raw database contents contain no secrets.
	var raw string
	e.pool.QueryRow(ctx, `SELECT smb_password || repo_password FROM storage_targets WHERE id=$1`, id).Scan(&raw)
	var settings string
	e.pool.QueryRow(ctx, `SELECT value::text FROM settings WHERE key='email'`).Scan(&settings)
	tg, _ := e.store.GetTarget(ctx, id)
	if strings.Contains(raw, "smb-Secret-1") || strings.Contains(raw, tg.RecoveryKey) || strings.Contains(settings, "smtp-Secret-2") {
		t.Fatalf("secret in plaintext: %q %q", raw, settings)
	}
	if tg.SMBPassword != "smb-Secret-1" || !strings.HasPrefix(raw, "enc:v1:") {
		t.Fatalf("round trip: %q", tg.SMBPassword)
	}
	var es server.EmailSettings
	if e.store.GetSetting(ctx, "email", &es); es.Password != "smtp-Secret-2" {
		t.Fatal("email password round trip")
	}
	// Plaintext from older versions is encrypted at start; swapping
	// ciphertexts between fields is detected.
	e.pool.Exec(ctx, `UPDATE storage_targets SET smb_password='legacy-pw' WHERE id=$1`, id)
	if n, err := e.store.EncryptExistingSecrets(ctx); err != nil || n != 1 {
		t.Fatalf("migrate: %d %v", n, err)
	}
	if tg, _ := e.store.GetTarget(ctx, id); tg.SMBPassword != "legacy-pw" {
		t.Fatal("legacy secret lost")
	}
	e.pool.Exec(ctx, `UPDATE storage_targets SET smb_password=repo_password WHERE id=$1`, id)
	if _, err := e.store.GetTarget(ctx, id); err == nil {
		t.Error("swapped ciphertext accepted")
	}
	// A wrong key cannot read the secrets.
	other := server.NewStore(e.pool)
	other.UseSecretKey(bytes.Repeat([]byte{8}, 32))
	if _, err := other.GetTarget(ctx, id); err == nil {
		t.Error("wrong key decrypted secrets")
	}
}

func TestCopyJob(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	ag.VSS = false
	agents, _ := e.store.ListAgents(ctx)
	nas, _ := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "local", URL: filepath.Join(t.TempDir(), "nas"), Encrypted: true})
	cloud, _ := e.store.CreateTarget(ctx, server.Target{Name: "cloud", Kind: "local", URL: filepath.Join(t.TempDir(), "cloud"), Encrypted: true})
	src := filepath.Join(t.TempDir(), "data")
	files := writeTree(t, src)
	jobID, _ := e.store.CreateJob(ctx, server.Job{AgentID: agents[0].ID, TargetID: nas, Name: "docs", Paths: []string{src}, Enabled: true})

	// Copy job through the form, running after each backup.
	if _, loc, _ := admin.do("POST", "/jobs", url.Values{"name": {"docs offsite"}, "kind": {"copy"}, "source_job": {fmt.Sprint(jobID)},
		"target_id": {fmt.Sprint(nas)}, "sched_kind": {"after"}}); !strings.Contains(loc, "err=") {
		t.Error("copy to the same target accepted")
	}
	_, loc, _ := admin.do("POST", "/jobs", url.Values{"name": {"docs offsite"}, "kind": {"copy"}, "source_job": {fmt.Sprint(jobID)},
		"target_id": {fmt.Sprint(cloud)}, "sched_kind": {"after"}, "keep_monthly": {"12"}})
	if !strings.Contains(loc, "msg=") {
		t.Fatalf("create copy job: %s", loc)
	}
	if _, loc, _ := admin.do("POST", "/jobs", url.Values{"name": {"x"}, "kind": {"files"}, "agent_id": {fmt.Sprint(agents[0].ID)},
		"target_id": {fmt.Sprint(cloud)}, "paths": {src}, "sched_kind": {"after"}}); !strings.Contains(loc, "err=") {
		t.Error("after-schedule accepted for a normal job")
	}

	runID, _ := e.store.QueueBackup(ctx, jobID, "manual")
	runAgent(t, ag) // backup; finishing it queues the copy
	if r, _ := e.store.GetRun(ctx, runID); r.Status != api.StatusSuccess {
		t.Fatalf("backup: %s %s", r.Status, r.Message)
	}
	runs, _ := e.store.ListRuns(ctx, server.RunFilter{})
	if len(runs) < 2 || runs[0].Kind != api.KindCopy || runs[0].Trigger != "after-backup" {
		t.Fatalf("copy run not queued after backup: %+v", runs[0])
	}
	copyRunID := runs[0].ID
	runAgent(t, ag)
	cr, _ := e.store.GetRun(ctx, copyRunID)
	if cr.Status != api.StatusSuccess || cr.SnapshotID == "" || !strings.Contains(cr.Message, "Copied 1 backups") {
		t.Fatalf("copy run: %s %q", cr.Status, cr.Message)
	}
	if _, _, body := admin.do("GET", fmt.Sprintf("/runs/%d", copyRunID), nil); !strings.Contains(body, "backups copied") || !strings.Contains(body, "Restore from this backup") {
		t.Error("copy run page lacks stats or restore")
	}

	// Restore from the copy.
	dst := filepath.Join(t.TempDir(), "out")
	rr, err := e.store.QueueRestore(ctx, copyRunID, agents[0].ID, dst, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	runAgent(t, ag)
	if r, _ := e.store.GetRun(ctx, rr); r.Status != api.StatusSuccess {
		t.Fatalf("restore from copy: %s %q %v", r.Status, r.Message, r.Errors)
	}
	comps := strings.Split(strings.ReplaceAll(strings.Replace(src, ":", "", 1), `\`, "/"), "/")
	for p, want := range files {
		got, err := os.ReadFile(filepath.Join(append(append([]string{dst}, comps...), filepath.FromSlash(p))...))
		if err != nil || string(got) != want {
			t.Errorf("restored %s: %v", p, err)
		}
	}
}

func TestTwoFactor(t *testing.T) {
	e := setup(t)
	now := time.Unix(1_800_000_000, 0)
	server.SetClock(e.srv, func() time.Time { return now })
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	admin.do("POST", "/users", url.Values{"username": {"ana"}, "role": {"operator"}, "password": {"correct-horse-9"}, "password2": {"correct-horse-9"}})

	ana := newClient(t, e)
	if !ana.login("ana", "correct-horse-9") {
		t.Fatal("login without 2FA")
	}
	if _, _, acc := ana.do("GET", "/account", nil); strings.Count(acc, "Two-factor</dt>") != 1 {
		t.Error("account page shows the two-factor row more than once")
	}
	_, _, page := ana.do("GET", "/account/2fa", nil)
	m := regexp.MustCompile(`<code class="secret">([A-Z2-7 ]+)</code>`).FindStringSubmatch(page)
	if m == nil || !strings.Contains(page, `<svg class="qr"`) {
		t.Fatal("setup page lacks secret or QR code")
	}
	secret := strings.ReplaceAll(m[1], " ", "")
	if _, loc, _ := ana.do("POST", "/account/2fa/enable", url.Values{"code": {"000000"}}); !strings.Contains(loc, "err=") {
		t.Error("wrong code enabled 2FA")
	}
	code, _, body := ana.do("POST", "/account/2fa/enable", url.Values{"code": {server.TOTPCodeForTest(secret, now)}})
	recovery := regexp.MustCompile(`<li class="mono">([A-Z2-7]{4}-[A-Z2-7]{4})</li>`).FindAllStringSubmatch(body, -1)
	if code != 200 || len(recovery) != 10 {
		t.Fatalf("enable: %d, %d recovery codes", code, len(recovery))
	}

	// Password alone is no longer enough.
	c2 := newClient(t, e)
	_, loc, _ := c2.do("POST", "/login", url.Values{"username": {"ana"}, "password": {"correct-horse-9"}})
	if loc != "/login/2fa" {
		t.Fatalf("expected second step, got %q", loc)
	}
	if code, loc, _ := c2.do("GET", "/", nil); code != http.StatusSeeOther || loc != "/login" {
		t.Error("signed in before the second factor")
	}
	// The code used to enable 2FA cannot be replayed.
	if code, loc, body := c2.do("POST", "/login/2fa", url.Values{"code": {server.TOTPCodeForTest(secret, now)}}); loc == "/" || code != 200 || !strings.Contains(body, "Invalid code") {
		t.Errorf("replayed code: %d %q", code, loc)
	}
	now = now.Add(30 * time.Second)
	if _, loc, _ := c2.do("POST", "/login/2fa", url.Values{"code": {server.TOTPCodeForTest(secret, now)}}); loc != "/" {
		t.Fatalf("valid code rejected: %q", loc)
	}
	// A recovery code works once.
	rc := recovery[0][1]
	for i, want := range []string{"/", ""} {
		c := newClient(t, e)
		c.do("POST", "/login", url.Values{"username": {"ana"}, "password": {"correct-horse-9"}})
		if _, loc, _ := c.do("POST", "/login/2fa", url.Values{"code": {strings.ToLower(rc)}}); loc != want {
			t.Errorf("recovery code use %d: %q", i+1, loc)
		}
	}
	// Without the password step there is no way to the second step.
	c3 := newClient(t, e)
	if _, loc, _ := c3.do("POST", "/login/2fa", url.Values{"code": {server.TOTPCodeForTest(secret, now)}}); loc != "/login" {
		t.Error("second step without password")
	}

	// Policy: required for administrators.
	admin.do("POST", "/settings/sessions", url.Values{"lifetime_hours": {"12"}, "idle_minutes": {"0"}, "require_2fa": {"admins"}})
	if code, loc, _ := admin.do("GET", "/jobs", nil); code != http.StatusSeeOther || !strings.HasPrefix(loc, "/account/2fa") {
		t.Errorf("admin without 2FA not sent to setup: %d %s", code, loc)
	}
	if code, _, _ := admin.do("GET", "/account/2fa", nil); code != 200 {
		t.Error("setup page blocked")
	}
	// Admin reset of a user's 2FA.
	users, _ := e.store.ListUsers(e.ctx)
	for _, u := range users {
		if u.Username == "ana" {
			e.store.DisableTwoFactor(e.ctx, u.ID)
		}
	}
	c4 := newClient(t, e)
	if !c4.login("ana", "correct-horse-9") {
		t.Error("login after 2FA reset")
	}
}

func TestRestoreTests(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	ag.VSS = false
	agents, _ := e.store.ListAgents(ctx)
	tid, _ := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "local", URL: filepath.Join(t.TempDir(), "nas"), Encrypted: true})
	src := filepath.Join(t.TempDir(), "data")
	writeTree(t, src)
	jobID, _ := e.store.CreateJob(ctx, server.Job{AgentID: agents[0].ID, TargetID: tid, Name: "docs", Paths: []string{src}, Enabled: true})

	sch := server.NewScheduler(e.store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sch.Tick(ctx, time.Now())
	if runs, _ := e.store.ListRuns(ctx, server.RunFilter{}); len(runs) != 0 {
		t.Fatal("restore test queued without any backup")
	}
	e.store.QueueBackup(ctx, jobID, "manual")
	runAgent(t, ag)
	sch.Tick(ctx, time.Now()) // due: never tested
	runs, _ := e.store.ListRuns(ctx, server.RunFilter{})
	if runs[0].Kind != api.KindVerify {
		t.Fatalf("restore test not queued: %s", runs[0].Kind)
	}
	runAgent(t, ag)
	v, _ := e.store.GetRun(ctx, runs[0].ID)
	if v.Status != api.StatusSuccess || !strings.Contains(v.Message, "Restored and verified") {
		t.Fatalf("restore test: %s %q %v", v.Status, v.Message, v.Errors)
	}
	sch.Tick(ctx, time.Now()) // tested recently: nothing new
	if runs, _ := e.store.ListRuns(ctx, server.RunFilter{}); runs[0].ID != v.ID {
		t.Error("restore test queued again within the interval")
	}
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	if _, _, body := admin.do("GET", "/reports?period=last7", nil); !strings.Contains(body, "restore tests") || !strings.Contains(body, `badge success`) {
		t.Error("report lacks restore tests")
	}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/jobs/%d/test", jobID), url.Values{}); !strings.Contains(loc, "msg=") {
		t.Errorf("manual restore test: %s", loc)
	}
}

func TestRansomwareDetection(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	ag.VSS = false
	agents, _ := e.store.ListAgents(ctx)
	tid, _ := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "local", URL: filepath.Join(t.TempDir(), "nas")})
	src := filepath.Join(t.TempDir(), "docs")
	os.MkdirAll(src, 0o755)
	for i := 0; i < 120; i++ {
		os.WriteFile(filepath.Join(src, fmt.Sprintf("doc%03d.txt", i)), []byte(strings.Repeat(fmt.Sprintf("invoice %d line\n", i), 400)), 0o644)
	}
	jobID, _ := e.store.CreateJob(ctx, server.Job{AgentID: agents[0].ID, TargetID: tid, Name: "docs", Paths: []string{src}, Enabled: true,
		Retention: repo.RetentionPolicy{KeepLast: 2}})
	backup := func() server.Run {
		id, _ := e.store.QueueBackup(ctx, jobID, "manual")
		runAgent(t, ag)
		r, _ := e.store.GetRun(ctx, id)
		if r.Status != api.StatusSuccess {
			t.Fatalf("backup: %s %s", r.Status, r.Message)
		}
		return r
	}
	for i := 0; i < 3; i++ {
		os.WriteFile(filepath.Join(src, fmt.Sprintf("doc%03d.txt", i)), []byte(fmt.Sprintf("edited %d\n", i)), 0o644)
		if r := backup(); r.Anomaly != "" {
			t.Fatalf("normal backup flagged: %s", r.Anomaly)
		}
	}
	// "Ransomware" encrypts every document.
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 120; i++ {
		b := make([]byte, 6000)
		rng.Read(b)
		os.WriteFile(filepath.Join(src, fmt.Sprintf("doc%03d.txt", i)), b, 0o644)
	}
	r := backup()
	if r.Anomaly == "" {
		t.Fatal("encrypted files not detected")
	}
	t.Log(r.Anomaly)
	if j, _ := e.store.GetJob(ctx, jobID); !j.RetentionHold {
		t.Fatal("retention not paused")
	}
	// While paused, further backups do not expire the clean ones.
	backup()
	runs, _ := e.store.ListRuns(ctx, server.RunFilter{JobID: jobID})
	clean := 0
	for _, x := range runs {
		if x.Anomaly == "" && !x.Expired && x.Kind == api.KindBackup {
			clean++
		}
	}
	if clean < 2 {
		t.Errorf("clean backups expired while retention was paused (%d left)", clean)
	}
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	if _, _, body := admin.do("GET", "/", nil); !strings.Contains(body, "suspicious backups to review") {
		t.Error("dashboard lacks suspicious count")
	}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/jobs/%d/release-hold", jobID), url.Values{}); !strings.Contains(loc, "msg=") {
		t.Fatalf("release: %s", loc)
	}
	if j, _ := e.store.GetJob(ctx, jobID); j.RetentionHold {
		t.Error("retention still paused")
	}
}

func TestRESTAPI(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	admin.do("POST", "/users", url.Values{"username": {"mon"}, "role": {"viewer"}, "password": {"correct-horse-9"}, "password2": {"correct-horse-9"}})
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	ag.VSS = false
	agents, _ := e.store.ListAgents(ctx)
	tid, _ := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "local", URL: filepath.Join(t.TempDir(), "nas")})
	src := filepath.Join(t.TempDir(), "data")
	writeTree(t, src)
	jobID, _ := e.store.CreateJob(ctx, server.Job{AgentID: agents[0].ID, TargetID: tid, Name: "docs", Paths: []string{src}, Enabled: true})

	newToken := func(c *client, role string) string {
		code, _, body := c.do("POST", "/account/tokens", url.Values{"name": {"tool " + role}, "role": {role}, "days": {"30"}})
		m := regexp.MustCompile(`<pre id="newtok">(bzt_[A-Za-z0-9_-]+)</pre>`).FindStringSubmatch(body)
		if code != 200 || m == nil {
			t.Fatalf("create %s token: %d", role, code)
		}
		return m[1]
	}
	call := func(tok, method, p string) (int, map[string]any, []any) {
		req, _ := http.NewRequest(method, e.ts.URL+p, nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var obj map[string]any
		var arr []any
		if json.Unmarshal(b, &obj) != nil {
			json.Unmarshal(b, &arr)
		}
		return resp.StatusCode, obj, arr
	}

	opTok := newToken(admin, "restore")
	mon := newClient(t, e)
	mon.login("mon", "correct-horse-9")
	viewTok := newToken(mon, "viewer")
	if code, _, _ := mon.do("POST", "/account/tokens", url.Values{"name": {"x"}, "role": {"admin"}}); code == 200 {
		t.Error("viewer created an admin token")
	}

	if code, _, _ := call("", "GET", "/api/v1/status"); code != 401 {
		t.Errorf("no token: %d", code)
	}
	if code, _, _ := call("bzt_invalid", "GET", "/api/v1/status"); code != 401 {
		t.Errorf("bad token: %d", code)
	}
	code, st, _ := call(viewTok, "GET", "/api/v1/status")
	if code != 200 || st["agents"] != float64(1) || st["jobs"] != float64(1) {
		t.Fatalf("status: %d %v", code, st)
	}
	if code, _, list := call(viewTok, "GET", "/api/v1/jobs"); code != 200 || len(list) != 1 {
		t.Fatalf("jobs: %d %v", code, list)
	}
	if code, _, _ := call(viewTok, "POST", fmt.Sprintf("/api/v1/jobs/%d/run", jobID)); code != 403 {
		t.Errorf("viewer token started a backup: %d", code)
	}
	code, res, _ := call(opTok, "POST", fmt.Sprintf("/api/v1/jobs/%d/run", jobID))
	if code != 202 || res["run_id"] == nil {
		t.Fatalf("run: %d %v", code, res)
	}
	if code, _, _ := call(opTok, "POST", fmt.Sprintf("/api/v1/jobs/%d/run", jobID)); code != 409 {
		t.Errorf("second run while queued: %d", code)
	}
	runAgent(t, ag)
	code, run, _ := call(viewTok, "GET", fmt.Sprintf("/api/v1/runs/%v", res["run_id"]))
	if code != 200 || run["status"] != "success" || run["stats"] == nil {
		t.Fatalf("run status: %d %v", code, run)
	}
	if code, _, list := call(viewTok, "GET", "/api/v1/agents"); code != 200 || len(list) != 1 || list[0].(map[string]any)["seen_from"] != "127.0.0.1" {
		t.Errorf("agent addresses: %d %v", code, list)
	}
	if code, rep, _ := call(viewTok, "GET", "/api/v1/report?period=last7"); code != 200 || rep["backups"] != float64(1) || rep["success_rate"] != float64(100) {
		t.Errorf("report: %d %v", code, rep)
	}
	if code, _, list := call(viewTok, "GET", "/api/v1/targets"); code != 200 || len(list) != 1 || strings.Contains(fmt.Sprint(list), "password") {
		t.Errorf("targets: %d %v", code, list)
	}
	if code, _, _ := call(viewTok, "GET", "/api/v1/runs/999999"); code != 404 {
		t.Errorf("missing run: %d", code)
	}

	// Revocation and disabling the owner stop the token.
	toks, _ := e.store.ListAPITokens(ctx, 0)
	for _, tk := range toks {
		if tk.Username == "admin" {
			admin.do("POST", fmt.Sprintf("/account/tokens/%d/delete", tk.ID), url.Values{})
		}
	}
	if code, _, _ := call(opTok, "GET", "/api/v1/status"); code != 401 {
		t.Errorf("revoked token works: %d", code)
	}
	users, _ := e.store.ListUsers(ctx)
	for _, u := range users {
		if u.Username == "mon" {
			admin.do("POST", fmt.Sprintf("/users/%d", u.ID), url.Values{"role": {"viewer"}, "disabled": {"on"}})
		}
	}
	if code, _, _ := call(viewTok, "GET", "/api/v1/status"); code != 401 {
		t.Errorf("token of disabled user works: %d", code)
	}
	if _, _, body := admin.do("GET", "/audit", nil); !strings.Contains(body, "api.job_run") || !strings.Contains(body, "api_token.create") {
		t.Error("API actions missing from audit log")
	}
}

func TestAzureTarget(t *testing.T) {
	e := setup(t)
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	if _, loc, _ := admin.do("POST", "/targets", url.Values{"name": {"bad"}, "kind": {"azure"}, "azure_account": {"Bad_Name"}, "azure_container": {"backups"}, "azure_secret": {"k"}}); !strings.Contains(loc, "err=") {
		t.Error("invalid account accepted")
	}
	_, loc, _ := admin.do("POST", "/targets", url.Values{"name": {"azure"}, "kind": {"azure"}, "azure_account": {"contosobackups"},
		"azure_container": {"backups"}, "azure_path": {"office"}, "azure_auth": {"sas"}, "azure_secret": {"?sv=2024&sig=SECRET"},
		"azure_endpoint": {"http://10.0.0.5:10000/devstoreaccount1"}, "encrypted": {"on"}})
	if !strings.Contains(loc, "msg=") {
		t.Fatalf("create: %s", loc)
	}
	ts, _ := e.store.ListTargets(e.ctx)
	if len(ts) != 1 || ts[0].URL != "azure://contosobackups/backups/office?endpoint=http%3A%2F%2F10.0.0.5%3A10000%2Fdevstoreaccount1" || ts[0].AzureSAS != "sv=2024&sig=SECRET" {
		t.Fatalf("target: %+v", ts)
	}
	var raw string
	e.pool.QueryRow(e.ctx, `SELECT azure_sas FROM storage_targets`).Scan(&raw)
	if strings.Contains(raw, "SECRET") {
		t.Error("SAS token stored in plaintext")
	}
}

func TestProxmoxJobs(t *testing.T) {
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
	if _, err := e.store.CreateJob(ctx, server.Job{Kind: server.JobVM, AgentID: a.ID, TargetID: targetID, Name: "vms", Paths: []string{"*"}}); err == nil {
		t.Error("VM job accepted for an agent that is not on a Proxmox node")
	}
	inv := `{"node":"pve","guests":[{"vmid":100,"name":"web","type":"qemu","node":"pve","status":"running","maxdisk":8589934592},
		{"vmid":200,"name":"db","type":"lxc","node":"pve","status":"stopped","maxdisk":4294967296},
		{"vmid":300,"name":"elsewhere","type":"qemu","node":"pve2","status":"running"}],"storage":["local-lvm","ceph"]}`
	if err := e.store.TouchAgent(ctx, a.ID, api.PollRequest{Hypervisor: json.RawMessage(inv)}, ""); err != nil {
		t.Fatal(err)
	}
	a, _ = e.store.GetAgent(ctx, a.ID)
	if a.PVE() == nil || len(a.LocalGuests()) != 2 {
		t.Fatalf("inventory not stored: %+v", a.PVE())
	}
	if _, err := e.store.CreateJob(ctx, server.Job{Kind: server.JobVM, AgentID: a.ID, TargetID: targetID, Name: "vms", Paths: []string{}}); err == nil {
		t.Error("VM job without guests accepted")
	}
	if _, err := e.store.CreateJob(ctx, server.Job{Kind: server.JobVM, AgentID: a.ID, TargetID: targetID, Name: "vms", Paths: []string{"abc"}}); err == nil {
		t.Error("VM job with an invalid ID accepted")
	}
	jobID, err := e.store.CreateJob(ctx, server.Job{Kind: server.JobVM, AgentID: a.ID, TargetID: targetID, Name: "Proxmox", Paths: []string{"100", "200"}})
	if err != nil {
		t.Fatal(err)
	}
	runID, err := e.store.QueueBackup(ctx, jobID, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.QueueBackup(ctx, jobID, "manual"); err == nil {
		t.Error("second VM backup queued while one is pending")
	}
	claimed, err := e.store.ClaimRun(ctx, a.ID)
	if err != nil || claimed == nil || claimed.ID != runID || claimed.Kind != api.KindVMBackup || strings.Join(claimed.Paths, ",") != "100,200" {
		t.Fatalf("claimed run: %+v %v", claimed, err)
	}
	details := `{"guests":[{"vmid":100,"type":"qemu","name":"web","consistency":"snapshot, file systems frozen by the QEMU guest agent","disks":1,"size":8589934592,"stored":1000000000},
		{"vmid":200,"type":"lxc","name":"db","consistency":"snapshot of the stopped guest","disks":1,"size":4294967296,"stored":500000000}]}`
	if err := e.store.FinishRun(ctx, a.ID, runID, api.RunResult{Status: api.StatusSuccess, SnapshotID: strings.Repeat("ef", 32),
		Stats: json.RawMessage(`{"files":2,"bytes_read":12884901888}`), Details: json.RawMessage(details)}); err != nil {
		t.Fatal(err)
	}
	if r, _ := e.store.GetRun(ctx, runID); r.SnapshotID == "" {
		t.Fatal("snapshot ID of the VM backup not stored")
	}

	if _, err := e.store.QueueVMRestore(ctx, runID, a.ID, api.VMRestore{VMID: 999}); err == nil {
		t.Error("restore of a guest that is not in the backup accepted")
	}
	if _, err := e.store.QueueVMRestore(ctx, runID, a.ID, api.VMRestore{VMID: 100, Storage: "nfs-elsewhere"}); err == nil {
		t.Error("restore to unknown storage accepted")
	}
	rr, err := e.store.QueueVMRestore(ctx, runID, a.ID, api.VMRestore{VMID: 100, NewVMID: -1, Name: "web-copy", Storage: "ceph", Start: true})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err = e.store.ClaimRun(ctx, a.ID)
	if err != nil || claimed == nil || claimed.ID != rr || claimed.Kind != api.KindVMRestore {
		t.Fatalf("claimed restore: %+v %v", claimed, err)
	}

	if claimed.VMRestore == nil {
		t.Fatal("restore options not stored")
	}
	jar, _ := cookiejar.New(nil)
	hc := e.ts.Client()
	hc.Jar = jar
	form := url.Values{"username": {"admin"}, "password": {"admin-pass-123"}}
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.ts.URL)
	if resp, err := hc.Do(req); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}
	body := func(p string) string {
		resp, err := hc.Get(e.ts.URL + p)
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
	if p := body("/jobs"); !strings.Contains(p, "Proxmox VMs and containers") || !strings.Contains(p, "Guests 100, 200") || !strings.Contains(p, `"web"`) || strings.Contains(p, `"elsewhere"`) {
		t.Error("jobs page lacks the Proxmox option, selection or inventory")
	}
	if p := body(fmt.Sprintf("/runs/%d", runID)); !strings.Contains(p, "VM backup #") || !strings.Contains(p, "frozen by the QEMU guest agent") ||
		!strings.Contains(p, "Restore a VM or container") || !strings.Contains(p, `<option value="ceph">`) {
		t.Error("VM run page lacks guests or restore form")
	}
	if p := body(fmt.Sprintf("/runs/%d", rr)); !strings.Contains(p, "next free ID, name web-copy") {
		t.Error("restore run page lacks the options")
	}
	if p := body("/agents"); !strings.Contains(p, "Proxmox VE · 3 guests") {
		t.Error("agents page lacks the Proxmox badge")
	}
}

func TestAgentUpdate(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	dist := t.TempDir()
	e.srv.DistDir = dist
	msi := []byte("fake msi 0.20.0")
	for name, data := range map[string][]byte{
		"backupzit-agent-0.19.0-x64.msi":        []byte("old"),
		"backupzit-agent-0.20.0-x64.msi":        msi,
		"backupzit-agent-0.21.0-x64-legacy.msi": []byte("legacy"),
		"backupzit-agent_0.20.0_amd64.deb":      []byte("deb"),
		"backupzit-server_0.20.0_amd64.deb":     []byte("server"),
	} {
		os.WriteFile(filepath.Join(dist, name), data, 0o644)
	}
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "0.19.0")
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
	agentReq := func(method, p string, body any) *http.Response {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, e.ts.URL+p, rd)
		req.Header.Set("Authorization", "Bearer "+cfg.AgentUUID+":"+cfg.Secret)
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := agentReq("POST", api.PathPoll, api.PollRequest{Hostname: "win", OS: "Windows 11 Pro (build 26100)", Arch: "amd64", Version: "0.19.0"})
	resp.Body.Close()

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
	post("/login", url.Values{"username": {"admin"}, "password": {"admin-pass-123"}})
	page, _ := hc.Get(e.ts.URL + "/agents")
	b, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if !strings.Contains(string(b), "Update to 0.20.0") || !strings.Contains(string(b), "Update all agents (1)") {
		t.Fatal("agents page does not offer the update")
	}
	post(fmt.Sprintf("/agents/%d/update", a.ID), nil)
	post(fmt.Sprintf("/agents/%d/update", a.ID), nil) // no duplicate

	// An agent that is up to date is not offered an update.
	resp = agentReq("POST", api.PathPoll, api.PollRequest{Hostname: "win", OS: "Windows 11 Pro (build 26100)", Arch: "amd64", Version: "0.20.0", Busy: true})
	resp.Body.Close()
	if p, err := hc.Get(e.ts.URL + "/agents"); err == nil {
		b, _ := io.ReadAll(p.Body)
		p.Body.Close()
		if strings.Contains(string(b), "Update to") {
			t.Error("up-to-date agent offered an update")
		}
	}
	resp = agentReq("POST", api.PathPoll, api.PollRequest{Hostname: "win", OS: "Windows 11 Pro (build 26100)", Arch: "amd64", Version: "0.19.0", Busy: true})
	resp.Body.Close()
	if rr, _ := e.store.ListRuns(ctx, server.RunFilter{AgentID: a.ID}); len(rr) != 1 {
		resp := post(fmt.Sprintf("/agents/%d/update", a.ID), nil)
		t.Fatalf("update not queued: %d runs; %s", len(rr), resp.Request.URL)
	}

	resp = agentReq("POST", api.PathPoll, api.PollRequest{Hostname: "win", OS: "Windows 11 Pro (build 26100)", Arch: "amd64", Version: "0.19.0"})
	var pr api.PollResponse
	json.NewDecoder(resp.Body).Decode(&pr)
	resp.Body.Close()
	sum := sha256.Sum256(msi)
	if pr.Run == nil || pr.Run.Kind != api.KindAgentUpdate || pr.Run.UpdateFile != "backupzit-agent-0.20.0-x64.msi" ||
		pr.Run.UpdateVersion != "0.20.0" || pr.Run.UpdateSHA256 != hex.EncodeToString(sum[:]) {
		rr, _ := e.store.ListRuns(ctx, server.RunFilter{AgentID: a.ID})
		t.Fatalf("update run: %+v; runs %+v", pr.Run, rr)
	}
	resp = agentReq("GET", api.PathDownloadPrefix+"backupzit-agent-0.20.0-x64.msi", nil)
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(got, msi) {
		t.Fatalf("download: %d %q", resp.StatusCode, got)
	}
	for _, bad := range []string{"backupzit-server_0.20.0_amd64.deb", "..%2Fsecrets.key"} {
		if resp := agentReq("GET", api.PathDownloadPrefix+bad, nil); resp.StatusCode != 404 {
			t.Errorf("download of %s: %d", bad, resp.StatusCode)
		}
	}
	req, _ := http.NewRequest("GET", e.ts.URL+api.PathDownloadPrefix+"backupzit-agent-0.20.0-x64.msi", nil)
	if resp, _ := e.ts.Client().Do(req); resp.StatusCode != 401 {
		t.Errorf("download without agent credentials: %d", resp.StatusCode)
	}
	runs, _ := e.store.ListRuns(ctx, server.RunFilter{AgentID: a.ID})
	if len(runs) != 1 {
		t.Errorf("%d update runs, want 1", len(runs))
	}
}

func TestTrayStatus(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	targetID, _ := e.store.CreateTarget(ctx, server.Target{Name: "local", Kind: "local", URL: t.TempDir()})
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	agents, _ := e.store.ListAgents(ctx)
	ids := map[string]int64{}
	for _, a := range agents {
		ids[a.UUID] = a.ID
	}
	jobID, err := e.store.CreateJob(ctx, server.Job{AgentID: ids[cfg.AgentUUID], TargetID: targetID, Name: "Documents", Paths: []string{"/docs"}, Schedule: "0 22 * * *", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	otherJob, _ := e.store.CreateJob(ctx, server.Job{AgentID: ids[other.AgentUUID], TargetID: targetID, Name: "Other", Paths: []string{"/x"}, Enabled: true})

	call := func(c *agent.Config, method, p string, body any) (*http.Response, []byte) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, e.ts.URL+p, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+c.AgentUUID+":"+c.Secret)
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, out
	}
	// The status comes only when asked for, with this agent's jobs only.
	_, out := call(cfg, "POST", api.PathPoll, api.PollRequest{Hostname: "pc", Busy: true})
	var pr api.PollResponse
	json.Unmarshal(out, &pr)
	if pr.Status != nil {
		t.Error("status sent without WantStatus")
	}
	_, out = call(cfg, "POST", api.PathPoll, api.PollRequest{Hostname: "pc", Busy: true, WantStatus: true})
	pr = api.PollResponse{}
	json.Unmarshal(out, &pr)
	if pr.Status == nil || len(pr.Status.Jobs) != 1 || pr.Status.Jobs[0].Name != "Documents" || pr.Status.Jobs[0].NextRun == nil || pr.Status.Jobs[0].Running {
		t.Fatalf("status: %s", out)
	}
	// "Back up now" works for the agent's own job only, once at a time.
	if resp, _ := call(cfg, "POST", fmt.Sprintf("%s%d/run", api.PathJobRunPrefix, otherJob), struct{}{}); resp.StatusCode != 404 {
		t.Errorf("agent started another agent's job: %d", resp.StatusCode)
	}
	if resp, out := call(cfg, "POST", fmt.Sprintf("%s%d/run", api.PathJobRunPrefix, jobID), struct{}{}); resp.StatusCode != 200 {
		t.Fatalf("run now: %d %s", resp.StatusCode, out)
	}
	if resp, _ := call(cfg, "POST", fmt.Sprintf("%s%d/run", api.PathJobRunPrefix, jobID), struct{}{}); resp.StatusCode != 409 {
		t.Errorf("second run now: %d", resp.StatusCode)
	}
	_, out = call(cfg, "POST", api.PathPoll, api.PollRequest{Hostname: "pc", Busy: true, WantStatus: true})
	pr = api.PollResponse{}
	json.Unmarshal(out, &pr)
	if !pr.Status.Jobs[0].Running {
		t.Errorf("queued job not shown as running: %s", out)
	}
	runs, _ := e.store.ListRuns(ctx, server.RunFilter{JobID: jobID})
	if len(runs) != 1 || runs[0].Trigger != "agent" {
		t.Errorf("runs: %+v", runs)
	}
}
