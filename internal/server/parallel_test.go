package server_test

import (
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/server"
)

func TestParallelRuns(t *testing.T) {
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
	aid := agents[0].ID
	target, _ := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "local", URL: filepath.Join(t.TempDir(), "nas")})
	wait := "sleep 3"
	if runtime.GOOS == "windows" {
		wait = "ping -n 4 127.0.0.1 >NUL"
	}
	var jobs []int64
	for i := 0; i < 2; i++ {
		src := t.TempDir()
		os.WriteFile(filepath.Join(src, "a.txt"), []byte("a"), 0o644)
		_, loc, _ := admin.do("POST", "/jobs", url.Values{"name": {fmt.Sprintf("job%d", i)}, "kind": {"files"}, "agent_id": {fmt.Sprint(aid)},
			"target_id": {fmt.Sprint(target)}, "paths": {src}, "sched_kind": {"manual"}, "keep_last": {"5"}, "pre_command": {wait}})
		if !strings.Contains(loc, "msg=") {
			t.Fatalf("create job: %s", loc)
		}
	}
	list, _ := e.store.ListJobs(ctx)
	for _, j := range list {
		jobs = append(jobs, j.ID)
	}
	running := func() int {
		n := 0
		for _, id := range jobs {
			runs, _ := e.store.ListRuns(ctx, server.RunFilter{JobID: id, Limit: 1})
			if len(runs) > 0 && runs[0].Status == "running" {
				n++
			}
		}
		return n
	}
	both := func() int {
		for _, id := range jobs {
			e.store.QueueBackup(ctx, id, "manual")
		}
		ag.PollOnce(ctx)
		ag.PollOnce(ctx) // a second run while the first is busy, if allowed
		n := running()
		ag.Wait()
		return n
	}

	// Default: one run at a time.
	if n := both(); n != 1 {
		t.Errorf("runs at once by default: %d", n)
	}
	ag.Wait()
	for i := 0; i < 3 && running() > 0; i++ { // the second run of the first round
		ag.PollOnce(ctx)
		ag.Wait()
	}

	if _, loc, _ := admin.do("POST", fmt.Sprintf("/agents/%d/max-runs", aid), url.Values{"max_runs": {"2"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("set max runs: %s", loc)
	}
	if _, _, body := admin.do("GET", "/agents", nil); !strings.Contains(body, `<option value="2" selected>`) {
		t.Error("agents page does not show 2 jobs at once")
	}
	if n := both(); n != 2 {
		t.Errorf("runs at once with 2 allowed: %d", n)
	}
	for _, id := range jobs {
		runs, _ := e.store.ListRuns(ctx, server.RunFilter{JobID: id, Limit: 1})
		if runs[0].Status != "success" {
			t.Errorf("job %d: %s %q", id, runs[0].Status, runs[0].Message)
		}
	}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/agents/%d/max-runs", aid), url.Values{"max_runs": {"9"}}); !strings.Contains(loc, "err=") {
		t.Error("9 jobs at once accepted")
	}
}
