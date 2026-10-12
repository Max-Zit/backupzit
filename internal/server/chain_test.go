package server_test

import (
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/server"
)

func TestJobChainsAndWindow(t *testing.T) {
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
	aid := fmt.Sprint(agents[0].ID)
	target, _ := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "local", URL: filepath.Join(t.TempDir(), "nas")})
	newJob := func(name, path string, extra url.Values) int64 {
		t.Helper()
		v := url.Values{"name": {name}, "kind": {"files"}, "agent_id": {aid}, "target_id": {fmt.Sprint(target)}, "paths": {path}, "keep_last": {"5"}, "sched_kind": {"manual"}}
		for k, x := range extra {
			v[k] = x
		}
		_, loc, _ := admin.do("POST", "/jobs", v)
		if !strings.Contains(loc, "msg=") {
			t.Fatalf("create %s: %s", name, loc)
		}
		jobs, _ := e.store.ListJobs(ctx)
		for _, j := range jobs {
			if j.Name == name {
				return j.ID
			}
		}
		return 0
	}
	good := t.TempDir()
	os.WriteFile(filepath.Join(good, "a.txt"), []byte("a"), 0o644)
	dbJob := newJob("database", good, nil)
	filesJob := newJob("files", good, url.Values{"sched_kind": {"chain"}, "sched_after_job": {fmt.Sprint(dbJob)}})
	latest := func(job int64) server.Run {
		runs, _ := e.store.ListRuns(ctx, server.RunFilter{JobID: job, Limit: 1})
		if len(runs) == 0 {
			return server.Run{}
		}
		return runs[0]
	}
	if _, _, body := admin.do("GET", fmt.Sprintf("/jobs/%d", filesJob), nil); !strings.Contains(body, "Runs after") || !strings.Contains(body, "After another job succeeded") {
		t.Error("job page lacks the chain")
	}

	// The database job succeeds: the files job follows.
	e.store.QueueBackup(ctx, dbJob, "manual")
	runAgent(t, ag)
	if r := latest(filesJob); r.Status != "queued" || r.Trigger != "after-job" {
		t.Fatalf("chained job after success: %+v", r)
	}
	runAgent(t, ag)

	// A loop is refused.
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/jobs/%d/edit", dbJob), url.Values{"name": {"database"}, "paths": {good}, "keep_last": {"5"},
		"sched_kind": {"chain"}, "sched_after_job": {fmt.Sprint(filesJob)}}); !strings.Contains(loc, "circle") {
		t.Errorf("loop accepted: %s", loc)
	}

	// The database job fails: the files job does not follow, unless it runs
	// after any result.
	admin.do("POST", fmt.Sprintf("/jobs/%d/edit", dbJob), url.Values{"name": {"database"}, "paths": {filepath.Join(good, "missing")}, "keep_last": {"5"}, "sched_kind": {"manual"}})
	before := latest(filesJob).ID
	e.store.QueueBackup(ctx, dbJob, "manual")
	runAgent(t, ag)
	if r := latest(dbJob); r.Status != "failed" {
		t.Fatalf("database job should fail: %s", r.Status)
	}
	if latest(filesJob).ID != before {
		t.Error("chained job started after a failure")
	}
	admin.do("POST", fmt.Sprintf("/jobs/%d/edit", filesJob), url.Values{"name": {"files"}, "paths": {good}, "keep_last": {"5"},
		"sched_kind": {"chain"}, "sched_after_job": {fmt.Sprint(dbJob)}, "sched_after_any": {"on"}})
	e.store.QueueBackup(ctx, dbJob, "manual")
	runAgent(t, ag)
	if r := latest(filesJob); r.Status != "queued" || r.Trigger != "after-job" {
		t.Errorf("chained job with 'also when it failed': %+v", r)
	}
	runAgent(t, ag)

	// Backup window: a scheduled run outside it waits and is cancelled when
	// the window closes; a run started by hand ignores it.
	h := time.Now().Hour()
	from, to := (h+2)%24, (h+4)%24
	if _, loc, _ := admin.do("POST", "/jobs", url.Values{"name": {"bad window"}, "kind": {"files"}, "agent_id": {aid}, "target_id": {fmt.Sprint(target)}, "paths": {good}, "keep_last": {"5"},
		"sched_kind": {"daily"}, "sched_time": {fmt.Sprintf("%02d:30", h)}, "sched_days": {"0", "1", "2", "3", "4", "5", "6"},
		"window": {"on"}, "window_from": {fmt.Sprint(from)}, "window_to": {fmt.Sprint(to)}}); !strings.Contains(loc, "outside+the+backup+window") {
		t.Errorf("schedule outside the window accepted: %s", loc)
	}
	night := newJob("night", good, url.Values{"window": {"on"}, "window_from": {fmt.Sprint(from)}, "window_to": {fmt.Sprint(to)}})
	if _, _, body := admin.do("GET", fmt.Sprintf("/jobs/%d", night), nil); !strings.Contains(body, fmt.Sprintf("%02d:00–%02d:00", from, to)) {
		t.Error("job page lacks the window")
	}
	rid, _ := e.store.QueueBackup(ctx, night, "schedule")
	runAgent(t, ag)
	if r, _ := e.store.GetRun(ctx, rid); r.Status != "queued" {
		t.Fatalf("scheduled run outside the window started: %s", r.Status)
	}
	sched := server.NewScheduler(e.store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sched.Tick(ctx, time.Now().Add(2*time.Minute))
	if r, _ := e.store.GetRun(ctx, rid); r.Status != "cancelled" || !strings.Contains(r.Message, "backup window") {
		t.Errorf("missed window: %s %q", r.Status, r.Message)
	}
	rid, _ = e.store.QueueBackup(ctx, night, "manual")
	runAgent(t, ag)
	if r, _ := e.store.GetRun(ctx, rid); r.Status != "success" {
		t.Errorf("manual run outside the window: %s %q", r.Status, r.Message)
	}
	// Inside the window a scheduled run starts.
	admin.do("POST", fmt.Sprintf("/jobs/%d/edit", night), url.Values{"name": {"night"}, "paths": {good}, "keep_last": {"5"}, "sched_kind": {"manual"},
		"window": {"on"}, "window_from": {fmt.Sprint(h)}, "window_to": {fmt.Sprint((h + 1) % 24)}})
	rid, _ = e.store.QueueBackup(ctx, night, "schedule")
	runAgent(t, ag)
	if r, _ := e.store.GetRun(ctx, rid); r.Status != "success" {
		t.Errorf("scheduled run inside the window: %s", r.Status)
	}
}
