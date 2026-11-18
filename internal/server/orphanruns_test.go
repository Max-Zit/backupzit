package server_test

import (
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/server"
)

// An agent restarted in the middle of a run does not know the run any more.
// Agents 0.52.0 and newer list the runs they work on with each poll; the
// console fails the agent's running runs that are missing (after a grace
// for runs just handed out). Older agents, which send no list, keep the old
// behaviour.
func TestOrphanedRunsFailed(t *testing.T) {
	x := newRemoveEnv(t, "0.52.0")
	ctx := x.e.ctx
	jobs, err := x.e.store.ListJobs(ctx)
	if err != nil || len(jobs) == 0 {
		t.Fatal("no job", err)
	}
	poll := func(req api.PollRequest) api.PollResponse {
		t.Helper()
		req.Hostname, req.OS, req.Arch, req.Version = "PC-17", "Windows 11", "amd64", "0.52.0"
		var resp api.PollResponse
		if code := x.call("POST", api.PathPoll, req, &resp); code != 200 {
			t.Fatalf("poll: %d", code)
		}
		return resp
	}
	status := func(id int64) (string, string) {
		t.Helper()
		r, err := x.e.store.GetRun(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return r.Status, r.Message
	}
	claim := func() int64 {
		t.Helper()
		id, err := x.e.store.QueueBackup(ctx, jobs[0].ID, "manual")
		if err != nil {
			t.Fatal(err)
		}
		resp := poll(api.PollRequest{ReportsRuns: true})
		if resp.Run == nil || resp.Run.ID != id {
			t.Fatalf("run %d not handed out: %+v", id, resp.Run)
		}
		return id
	}

	defer server.SetOrphanGrace(server.SetOrphanGrace(0))
	id := claim()
	poll(api.PollRequest{Busy: true, Running: 1, ReportsRuns: true, Runs: []int64{id}})
	poll(api.PollRequest{Busy: false}) // an older agent sends no list
	if st, _ := status(id); st != "running" {
		t.Fatalf("listed run changed to %s", st)
	}
	poll(api.PollRequest{ReportsRuns: true}) // restarted: knows no runs
	st, msg := status(id)
	if st != "failed" || !strings.Contains(msg, "no longer running") {
		t.Fatalf("orphaned run: %s %q", st, msg)
	}
	if code := x.call("POST", api.PathRunsPrefix+itoa(id)+"/finish", api.RunResult{Status: api.StatusSuccess}, nil); code != 404 {
		t.Errorf("late result for a failed run: %d", code)
	}

	// A run just handed out is spared during the grace.
	server.SetOrphanGrace(time.Hour)
	id = claim()
	poll(api.PollRequest{ReportsRuns: true})
	if st, _ := status(id); st != "running" {
		t.Fatalf("run failed within the grace: %s", st)
	}
	server.SetOrphanGrace(0)
	poll(api.PollRequest{ReportsRuns: true})
	if st, _ := status(id); st != "failed" {
		t.Fatalf("run not failed after the grace: %s", st)
	}
}
