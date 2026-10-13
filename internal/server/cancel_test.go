package server_test

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/server"
)

func TestCancelRuns(t *testing.T) {
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
	target, _ := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "local", URL: filepath.Join(t.TempDir(), "nas")})
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("a"), 0o644)
	postFile := filepath.Join(t.TempDir(), "post.txt")

	// The command before the backup waits long enough to be cancelled.
	pre, post := "sleep 60", "echo $BACKUPZIT_STATUS > \""+postFile+"\""
	if runtime.GOOS == "windows" {
		pre, post = "ping -n 60 127.0.0.1 >NUL", "echo %BACKUPZIT_STATUS%> \""+postFile+"\""
	}
	_, loc, _ := admin.do("POST", "/jobs", url.Values{"name": {"slow"}, "kind": {"files"}, "agent_id": {fmt.Sprint(agents[0].ID)},
		"target_id": {fmt.Sprint(target)}, "paths": {src}, "sched_kind": {"manual"}, "keep_last": {"5"},
		"pre_command": {pre}, "post_command": {post}, "max_hours": {"2"}})
	if !strings.Contains(loc, "msg=") {
		t.Fatalf("create job: %s", loc)
	}
	jobs, _ := e.store.ListJobs(ctx)
	j := jobs[0]
	if j.Options.MaxHours != 2 {
		t.Fatalf("time limit not stored: %+v", j.Options)
	}

	// A queued run is cancelled at once, without a failure alert.
	runID, _ := e.store.QueueBackup(ctx, j.ID, "manual")
	if _, _, body := admin.do("GET", fmt.Sprintf("/jobs/%d", j.ID), nil); !strings.Contains(body, fmt.Sprintf(`action="/runs/%d/cancel"`, runID)) {
		t.Error("job page lacks the cancel button")
	}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/runs/%d/cancel", runID), nil); !strings.Contains(loc, "msg=") {
		t.Fatalf("cancel queued: %s", loc)
	}
	r, _ := e.store.GetRun(ctx, runID)
	if r.Status != "cancelled" || r.Message != "Cancelled by admin" {
		t.Fatalf("queued run after cancel: %s %q", r.Status, r.Message)
	}
	if _, _, body := admin.do("GET", "/runs", nil); !strings.Contains(body, `class="badge cancelled"`) || strings.Contains(body, `queued</span> <span class="muted"`) {
		t.Error("activity list does not show the run as cancelled")
	}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/runs/%d/cancel", runID), nil); !strings.Contains(loc, "err=") {
		t.Error("finished run cancelled again")
	}

	// A running run stops on the agent's next poll; the command after the
	// backup still runs.
	runID, _ = e.store.QueueBackup(ctx, j.ID, "manual")
	if _, err := ag.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if r, _ := e.store.GetRun(ctx, runID); r.Status != "running" {
		t.Fatalf("run not started: %s", r.Status)
	}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/runs/%d/cancel", runID), nil); !strings.Contains(loc, "msg=") {
		t.Fatalf("cancel running: %s", loc)
	}
	if _, _, body := admin.do("GET", fmt.Sprintf("/runs/%d", runID), nil); !strings.Contains(body, "Stopping:") {
		t.Error("run page does not show the pending stop")
	}
	start := time.Now()
	if _, err := ag.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	ag.Wait()
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("stopping took %v", d)
	}
	r, _ = e.store.GetRun(ctx, runID)
	if r.Status != "cancelled" || r.Message != "Cancelled by admin" {
		t.Errorf("running run after cancel: %s %q", r.Status, r.Message)
	}
	if b, err := os.ReadFile(postFile); err != nil || strings.TrimSpace(string(b)) != "failed" {
		t.Errorf("command after the backup: %q %v", b, err)
	}

	// The time limit stops a run that runs too long.
	runID, _ = e.store.QueueBackup(ctx, j.ID, "manual")
	if _, err := ag.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	sched := server.NewScheduler(e.store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sched.Tick(ctx, time.Now().Add(time.Hour)) // within the limit
	if r, _ := e.store.GetRun(ctx, runID); r.CancelReason != nil {
		t.Fatalf("stopped within the limit: %q", *r.CancelReason)
	}
	sched.Tick(ctx, time.Now().Add(3*time.Hour))
	if _, err := ag.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	ag.Wait()
	r, _ = e.store.GetRun(ctx, runID)
	if r.Status != "failed" || !strings.Contains(r.Message, "time limit of 2 h") {
		t.Errorf("run over the time limit: %s %q", r.Status, r.Message)
	}

	// The REST API cancels too.
	runID, _ = e.store.QueueBackup(ctx, j.ID, "manual")
	tok := apiToken(t, admin, "operator")
	if code, body := apiCall(t, e, tok, "POST", fmt.Sprintf("/api/v1/runs/%d/cancel", runID)); code != 202 || !strings.Contains(body, `"cancelled"`) {
		t.Errorf("API cancel: %d %s", code, body)
	}
	if code, _ := apiCall(t, e, tok, "POST", fmt.Sprintf("/api/v1/runs/%d/cancel", runID)); code != 409 {
		t.Errorf("API cancel of a finished run: %d", code)
	}

	// "Cancel all" on the Activity page cancels the runs listed there — one
	// running, one queued — but not a run queued after the page was shown.
	newJob := func(name string) int64 {
		admin.do("POST", "/jobs", url.Values{"name": {name}, "kind": {"files"}, "agent_id": {fmt.Sprint(agents[0].ID)},
			"target_id": {fmt.Sprint(target)}, "paths": {src}, "sched_kind": {"manual"}, "keep_last": {"5"}})
		jobs, _ := e.store.ListJobs(ctx)
		for _, x := range jobs {
			if x.Name == name {
				return x.ID
			}
		}
		t.Fatalf("job %s not created", name)
		return 0
	}
	running, _ := e.store.QueueBackup(ctx, j.ID, "manual")
	if _, err := ag.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	queued, _ := e.store.QueueBackup(ctx, newJob("second"), "manual")
	_, _, body := admin.do("GET", "/runs", nil)
	ids := regexp.MustCompile(`<input type="hidden" name="id" value="(\d+)">`).FindAllStringSubmatch(body, -1)
	if !strings.Contains(body, `action="/runs/cancel-all"`) || len(ids) != 2 || !strings.Contains(body, "runs scheduled later today") {
		t.Fatalf("activity page: cancel all form with %d runs", len(ids))
	}
	later, _ := e.store.QueueBackup(ctx, newJob("later"), "manual")
	form := url.Values{}
	for _, m := range ids {
		form.Add("id", m[1])
	}
	if _, loc, _ := admin.do("POST", "/runs/cancel-all", form); !strings.Contains(loc, "msg=") || !strings.Contains(loc, "1") {
		t.Fatalf("cancel all: %s", loc)
	}
	if r, _ := e.store.GetRun(ctx, queued); r.Status != "cancelled" {
		t.Errorf("queued run after cancel all: %s", r.Status)
	}
	if r, _ := e.store.GetRun(ctx, later); r.Status != "queued" {
		t.Errorf("run queued after the page was shown: %s", r.Status)
	}
	if _, err := ag.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	ag.Wait()
	if r, _ := e.store.GetRun(ctx, running); r.Status != "cancelled" || r.Message != "Cancelled by admin" {
		t.Errorf("running run after cancel all: %s %q", r.Status, r.Message)
	}
	if _, loc, _ := admin.do("POST", "/runs/cancel-all", nil); !strings.Contains(loc, "err=") {
		t.Errorf("cancel all without runs: %s", loc)
	}
	viewer := newClient(t, e)
	admin.do("POST", "/users", url.Values{"username": {"vera"}, "password": {"viewer-pass-123"}, "password2": {"viewer-pass-123"}, "role": {"viewer"}})
	viewer.login("vera", "viewer-pass-123")
	if code, _, body := viewer.do("POST", "/runs/cancel-all", url.Values{"id": {fmt.Sprint(later)}}); code != 403 {
		t.Errorf("viewer cancel all: %d", code)
	} else if _, _, body = viewer.do("GET", "/runs", nil); strings.Contains(body, "cancel-all") {
		t.Error("viewer sees the cancel all button")
	}
}

// apiToken creates an API token with a role for the signed-in client.
func apiToken(t *testing.T, c *client, role string) string {
	t.Helper()
	code, _, body := c.do("POST", "/account/tokens", url.Values{"name": {"tool " + role}, "role": {role}, "days": {"30"}})
	m := regexp.MustCompile(`<pre id="newtok">(bzt_[A-Za-z0-9_-]+)</pre>`).FindStringSubmatch(body)
	if code != 200 || m == nil {
		t.Fatalf("create %s token: %d", role, code)
	}
	return m[1]
}

// apiCall calls the REST API and returns the status and body.
func apiCall(t *testing.T, e *env, tok, method, p string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, e.ts.URL+p, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}
