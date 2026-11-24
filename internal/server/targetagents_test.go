package server_test

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/archiver"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/server"
	"github.com/max-zit/backupzit/internal/testutil"
)

// logBuffer collects the console log for tests that read it while the
// server writes.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// unflash is the message a redirect carries (msg= or err=).
func unflash(loc string) string {
	u, err := url.Parse(loc)
	if err != nil {
		return loc
	}
	q := u.Query()
	return q.Get("msg") + q.Get("err")
}

// #63: storage on a customer's network that only the site's agent reaches.
// The console tests it from an agent instead of itself, never connects to
// it when it is marked reachable by agents only, searches it through an
// agent, keeps it out of the console backup, refuses to browse its files
// at once, and logs every failed connection test.
func TestTargetReachableByAgentsOnly(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	logs := &logBuffer{}
	e.srv.SetLog(slog.New(slog.NewTextHandler(logs, nil)))
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	page := func(p string) string {
		t.Helper()
		code, _, body := admin.do("GET", p, nil)
		if code != 200 {
			t.Fatalf("%s: %d", p, code)
		}
		return body
	}

	enroll := func(version string) (*agent.Agent, server.Agent) {
		t.Helper()
		token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
		cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, version)
		if err != nil {
			t.Fatal(err)
		}
		ag := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), version)
		ag.VSS = false
		if _, err := ag.PollOnce(ctx); err != nil {
			t.Fatal(err)
		}
		agents, _ := e.store.ListAgents(ctx)
		for _, a := range agents {
			if a.UUID == cfg.AgentUUID {
				return ag, a
			}
		}
		t.Fatal("agent missing")
		return nil, server.Agent{}
	}
	site, siteAgent := enroll("0.60.0")
	_, oldAgent := enroll("0.59.1")
	_, media := enroll("0.60.0")
	if _, err := e.pool.Exec(ctx, `UPDATE agents SET recovery=true WHERE id=$1`, media.ID); err != nil {
		t.Fatal(err)
	}

	s3, err := testutil.StartS3Server("backups")
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	s3.TruncateNextPuts("backupzit-test", 0) // counts the test files
	form := func(name, bucket string, extra url.Values) url.Values {
		v := url.Values{"name": {name}, "kind": {"s3"}, "s3_endpoint": {s3.Host}, "s3_bucket": {bucket}, "s3_prefix": {"site"},
			"s3_http": {"on"}, "s3_access_key": {"AK"}, "s3_secret_key": {"SK"}, "encrypted": {"on"}}
		for k, x := range extra {
			v[k] = x
		}
		return v
	}

	// Added with the option and no agent chosen: saved, not tested by the
	// console, and the message says how it is tested.
	_, loc, _ := admin.do("POST", "/targets", form("site-nas", "backups", url.Values{"agents_only": {"on"}, "test_from": {"console"}}))
	if m := unflash(loc); !strings.HasPrefix(m, "Storage target added. It is reachable by agents only") || !strings.Contains(m, "Test from an agent tests it") {
		t.Fatalf("create: %s", loc)
	}
	if n := s3.Puts("backupzit-test"); n != 0 {
		t.Fatalf("the console wrote %d test files to storage reachable by agents only", n)
	}
	targets, _ := e.store.ListTargets(ctx)
	if len(targets) != 1 || !targets[0].AgentsOnly {
		t.Fatalf("targets: %+v", targets)
	}
	tg := targets[0]
	if b := page("/targets"); !strings.Contains(b, ">agents only</span>") || !strings.Contains(b, "Storage marked agents only is reachable by the agents but not by the console") ||
		!strings.Contains(b, fmt.Sprintf(`href="/targets/%d/test">Test from an agent</a>`, tg.ID)) {
		t.Fatal("targets page shows no hint for the option")
	}

	// The console refuses to test it itself.
	testURL := fmt.Sprintf("/targets/%d/test", tg.ID)
	if _, loc, _ := admin.do("POST", testURL, url.Values{}); !strings.Contains(unflash(loc), "reachable by agents only, so the console does not connect to it") {
		t.Fatalf("console test: %s", loc)
	}
	if n := s3.Puts("backupzit-test"); n != 0 {
		t.Fatal("the console tested storage reachable by agents only")
	}
	b := page(testURL)
	if strings.Contains(b, `value="console"`) || !strings.Contains(b, fmt.Sprintf(`value="%d"`, siteAgent.ID)) || strings.Contains(b, fmt.Sprintf(`value="%d"`, media.ID)) {
		t.Fatalf("test page offers the console or recovery media:\n%s", b)
	}

	// Too old agents and recovery media are refused with a clear message.
	if _, loc, _ := admin.do("POST", testURL, url.Values{"from": {fmt.Sprint(oldAgent.ID)}}); !strings.Contains(unflash(loc), "(0.59.1) cannot test storage yet; update it to 0.60.0 or newer") {
		t.Fatalf("old agent: %s", loc)
	}
	if _, loc, _ := admin.do("POST", testURL, url.Values{"from": {fmt.Sprint(media.ID)}}); unflash(loc) != "choose an agent" {
		t.Fatalf("recovery media: %s", loc)
	}

	// The site's agent tests it; the page waits, then the Storage page
	// shows the result.
	_, loc, _ = admin.do("POST", testURL, url.Values{"from": {fmt.Sprint(siteAgent.ID)}})
	if !strings.HasPrefix(loc, testURL+"?run=") {
		t.Fatalf("agent test not started: %s", loc)
	}
	if b := page(loc); !strings.Contains(b, "is testing the connection") || !strings.Contains(b, "data-autorefresh") {
		t.Fatal("no waiting notice")
	}
	runAgent(t, site)
	_, res, _ := admin.do("GET", loc, nil)
	want := fmt.Sprintf("site-nas: connection test on the agent %s passed — connected, wrote, read and removed a test file", siteAgent.Hostname)
	if !strings.HasPrefix(res, "/targets?msg=") || unflash(res) != want {
		t.Fatalf("result: %s, want %q", res, want)
	}
	if n := s3.Puts("backupzit-test"); n != 1 {
		t.Fatalf("%d test files written by the agent", n)
	}
	rid := strings.TrimPrefix(loc, testURL+"?run=")
	var kind string
	var notified bool
	e.pool.QueryRow(ctx, `SELECT kind, notified FROM runs WHERE id=$1`, rid).Scan(&kind, &notified)
	if kind != api.KindTargetTest || !notified {
		t.Errorf("run %s: kind %q, notified %v", rid, kind, notified)
	}
	if b := page("/runs/" + rid); !strings.Contains(b, "Connection test") {
		t.Error("run page does not name the connection test")
	}

	// A test that fails on the agent is shown and goes to the console log.
	_, loc, _ = admin.do("POST", "/targets", form("site-missing", "missing-bucket", url.Values{"agents_only": {"on"}, "test_from": {fmt.Sprint(siteAgent.ID)}}))
	if !strings.Contains(loc, "/test?run=") || unflash(loc) != "Storage target added." {
		t.Fatalf("create with an agent test: %s", loc)
	}
	runAgent(t, site)
	_, res, _ = admin.do("GET", loc, nil)
	if m := unflash(res); !strings.Contains(res, "err=") || !strings.HasPrefix(m, "site-missing: connection test on the agent "+siteAgent.Hostname+" failed: ") {
		t.Fatalf("failed agent test: %s", res)
	}
	if l := logs.String(); !strings.Contains(l, `msg="storage connection test failed" target=site-missing kind=s3 from="agent `+siteAgent.Hostname+`"`) {
		t.Fatalf("failed agent test not logged:\n%s", l)
	}

	// A failed test on the console is logged too, and it points to the
	// option.
	_, loc, _ = admin.do("POST", "/targets", url.Values{"name": {"far-nas"}, "kind": {"s3"}, "s3_endpoint": {"127.0.0.1:1"}, "s3_bucket": {"b"},
		"s3_http": {"on"}, "s3_access_key": {"AK"}, "s3_secret_key": {"SK"}, "test_from": {"console"}})
	if m := unflash(loc); !strings.HasPrefix(m, "Storage target added. far-nas: connection test failed: ") {
		t.Fatalf("console test: %s", loc)
	}
	if l := logs.String(); !regexp.MustCompile(`msg="storage connection test failed" target=far-nas kind=s3 from=console err=`).MatchString(l) {
		t.Fatalf("failed console test not logged:\n%s", l)
	}

	// The default agent of a test is the one that used the storage last.
	jobID, err := e.store.CreateJob(ctx, server.Job{AgentID: siteAgent.ID, TargetID: tg.ID, Name: "site files", Paths: []string{t.TempDir()}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	backupID, err := e.store.QueueBackup(ctx, jobID, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if b := page(testURL); !strings.Contains(b, fmt.Sprintf(`value="%d" selected>`, siteAgent.ID)) || !strings.Contains(b, "used this storage last") {
		t.Fatalf("last agent not chosen:\n%s", b)
	}

	// Browsing and downloading files say why at once, without connecting.
	if run, err := e.store.ClaimRun(ctx, siteAgent.ID); err != nil || run == nil || run.ID != backupID {
		t.Fatalf("claim: %+v %v", run, err)
	}
	if err := e.store.FinishRun(ctx, siteAgent.ID, backupID, api.RunResult{Status: api.StatusSuccess, SnapshotID: strings.Repeat("ab", 32)}); err != nil {
		t.Fatal(err)
	}
	const refused = "this backup is on storage reachable by agents only; the console does not connect to it — restore with the paths instead"
	if b := page(fmt.Sprintf("/runs/%d/files", backupID)); !strings.Contains(b, refused) {
		t.Fatal("file browser does not say why")
	}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/runs/%d/files-download", backupID), url.Values{"sel": {"C/x.txt"}}); unflash(loc) != refused {
		t.Fatalf("download: %s", loc)
	}

	// Find backups goes through an agent that signs in with the target's
	// credentials.
	be, err := backend.Open(ctx, "s3://"+s3.Host+"/backups/site/old-pc_1a2b?tls=false", backend.Options{S3AccessKey: "AK", S3SecretKey: "SK"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be, repo.Password(tg.RecoveryKey))
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("old"), 0o644)
	if _, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}, Hostname: "OLD-PC", Tags: []string{"job:7"}}); err != nil {
		t.Fatal(err)
	}
	r.Close()
	scan := fmt.Sprintf("/targets/%d/scan", tg.ID)
	if b := page(scan); !strings.Contains(b, `name="agent_id"`) || !strings.Contains(b, "This storage is reachable by agents only. Choose an agent that reaches it") {
		t.Fatalf("no agent choice for the search:\n%s", b)
	}
	if _, loc, _ := admin.do("POST", scan, url.Values{"agent_id": {fmt.Sprint(oldAgent.ID)}}); !strings.Contains(unflash(loc), "(0.59.1) cannot search network storage yet; update it to 0.60.0 or newer") {
		t.Fatalf("old agent search: %s", loc)
	}
	_, loc, _ = admin.do("POST", scan, url.Values{"agent_id": {fmt.Sprint(siteAgent.ID)}})
	if !strings.HasPrefix(loc, scan+"?run=") {
		t.Fatalf("search not started: %s", loc)
	}
	if b := page(loc); !strings.Contains(b, "is searching the storage") {
		t.Fatal("no waiting notice for the search")
	}
	runAgent(t, site)
	if b := page(loc); !strings.Contains(b, `value="old-pc_1a2b"`) || !strings.Contains(b, "OLD-PC") {
		t.Fatalf("found repository not listed:\n%s", b)
	}
	run := strings.TrimPrefix(loc, scan+"?run=")
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/targets/%d/import", tg.ID), url.Values{"dir": {"old-pc_1a2b"}, "run": {run}}); !strings.Contains(loc, "Backups+added%3A+1%2C+of+computers%3A+1") {
		t.Fatalf("import: %s", loc)
	}

	// The console backup does not offer it and refuses it.
	if b := page("/settings/console"); strings.Contains(b, ">site-nas</option>") || !strings.Contains(b, ">far-nas</option>") {
		t.Fatal("console backup offers storage reachable by agents only")
	}
	if _, loc, _ := admin.do("POST", "/settings/console", url.Values{"enabled": {"on"}, "target_id": {fmt.Sprint(tg.ID)}, "hour": {"2"}, "keep": {"14"}}); !strings.Contains(unflash(loc), "this storage target is reachable by agents only") {
		t.Fatalf("console backup: %s", loc)
	}

	// The REST API shows the option.
	code, body := apiCallAs(t, e, admin, "GET", "/api/v1/targets")
	if code != 200 || !strings.Contains(body, `"agents_only":true`) || !strings.Contains(body, `"agents_only":false`) {
		t.Fatalf("API: %d %s", code, body)
	}

	// The option is turned off and on again for an existing target, but not
	// for the target of the console backup or for a local disk.
	toggle := func(id int64, on bool) string {
		v := url.Values{}
		if on {
			v.Set("on", "on")
		}
		_, loc, _ := admin.do("POST", fmt.Sprintf("/targets/%d/agents-only", id), v)
		return loc
	}
	if loc := toggle(tg.ID, false); !strings.Contains(unflash(loc), "The console connects to the storage target again") {
		t.Fatalf("off: %s", loc)
	}
	if got, _ := e.store.GetTarget(ctx, tg.ID); got.AgentsOnly {
		t.Fatal("still reachable by agents only")
	}
	if loc := toggle(tg.ID, true); !strings.Contains(unflash(loc), "marked reachable by agents only") {
		t.Fatalf("on: %s", loc)
	}
	far := targets[0].ID
	for _, x := range func() []server.Target { l, _ := e.store.ListTargets(ctx); return l }() {
		if x.Name == "far-nas" {
			far = x.ID
		}
	}
	if _, loc, _ := admin.do("POST", "/settings/console", url.Values{"enabled": {"on"}, "target_id": {fmt.Sprint(far)}, "hour": {"2"}, "keep": {"14"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("console backup to far-nas: %s", loc)
	}
	if loc := toggle(far, true); !strings.Contains(unflash(loc), "the console backup goes to this storage target") {
		t.Fatalf("console backup target marked: %s", loc)
	}
	local, _ := e.store.CreateTarget(ctx, server.Target{Name: "disk", Kind: "local", URL: t.TempDir(), AgentsOnly: true})
	if got, _ := e.store.GetTarget(ctx, local); got.AgentsOnly {
		t.Error("a local disk got the option")
	}
	if loc := toggle(local, true); !strings.Contains(loc, "err=") {
		t.Fatalf("local disk marked: %s", loc)
	}
}

// apiCallAs calls the REST API with a new token of the signed-in user c.
func apiCallAs(t *testing.T, e *env, c *client, method, p string) (int, string) {
	t.Helper()
	_, _, body := c.do("POST", "/account/tokens", url.Values{"name": {"test"}, "role": {"viewer"}, "days": {"1"}})
	m := regexp.MustCompile(`<pre id="newtok">(bzt_[A-Za-z0-9_-]+)</pre>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no API token")
	}
	return apiCall(t, e, m[1], method, p)
}
