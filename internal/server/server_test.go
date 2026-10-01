package server_test

import (
	"context"
	"crypto/tls"
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
	"github.com/backupzit/backupzit/internal/server"
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

	tenantID, err := e.store.CreateTenant(ctx, "Acme d.o.o.")
	if err != nil {
		t.Fatal(err)
	}
	repoBase := t.TempDir()
	targetID, err := e.store.CreateTarget(ctx, server.Target{TenantID: tenantID, Name: "local", Kind: "local", URL: repoBase})
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := e.store.CreateEnrollmentToken(ctx, tenantID, time.Hour)
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
	if !strings.Contains(run.RepoURL, "/acme-d-o-o/"+cfg.AgentUUID) {
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

	// --- tenant isolation: agent of another tenant cannot restore
	other, _ := e.store.CreateTenant(ctx, "Other")
	tok2, _, _ := e.store.CreateEnrollmentToken(ctx, other, time.Hour)
	cfg2, err := agent.Enroll(ctx, e.ts.URL, tok2, e.fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	all, _ := e.store.ListAgents(ctx)
	var otherAgent int64
	for _, a := range all {
		if a.UUID == cfg2.AgentUUID {
			otherAgent = a.ID
		}
	}
	if _, err := e.store.QueueRestore(ctx, runID, otherAgent, restoreDir, nil, false); err == nil {
		t.Error("cross-tenant restore was allowed")
	}
	if _, err := e.store.CreateJob(ctx, server.Job{AgentID: otherAgent, TargetID: targetID, Name: "x", Paths: []string{src}}); err == nil {
		t.Error("job with target of another tenant was allowed")
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
	if r := post("/tenants", url.Values{"name": {"Evil"}}, false); r.StatusCode != http.StatusForbidden {
		t.Fatalf("post without origin: %d", r.StatusCode)
	}
	if r := post("/tenants", url.Values{"name": {"Acme"}}, true); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("create tenant: %d", r.StatusCode)
	}
	ts, _ := e.store.ListTenants(e.ctx)
	if len(ts) != 1 || ts[0].Name != "Acme" {
		t.Fatalf("tenants: %+v", ts)
	}
	if r := post("/targets", url.Values{"tenant_id": {fmt.Sprint(ts[0].ID)}, "name": {"nas"}, "kind": {"sftp"},
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
	for _, p := range []string{"/", "/jobs", "/runs", "/agents", "/targets", "/tenants"} {
		get(p)
	}
	// Enrollment token page shows the fingerprint.
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/agents/token", strings.NewReader(url.Values{"tenant_id": {fmt.Sprint(ts[0].ID)}}.Encode()))
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
