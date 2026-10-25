package server_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/api"
)

// The agent reports the progress of a running backup with its polls and
// Activity, the job page and the run page show it.
func TestRunProgress(t *testing.T) {
	x := newRemoveEnv(t, "0.42.8")
	ctx := x.e.ctx
	jobs, err := x.e.store.ListJobs(ctx)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs: %v %v", jobs, err)
	}
	runID, _ := x.e.store.QueueBackup(ctx, jobs[0].ID, "manual")
	if run, err := x.e.store.ClaimRun(ctx, x.agentID); err != nil || run == nil || run.ID != runID {
		t.Fatalf("claim: %+v %v", run, err)
	}
	poll := func(p api.RunProgress) {
		p.RunID = runID
		x.call("POST", api.PathPoll, api.PollRequest{Hostname: "PC-17", OS: "Windows 11", Arch: "amd64", Version: "0.42.8", Running: 1, Progress: []api.RunProgress{p}}, nil)
	}
	page := func(p string) string {
		code, _, body := x.admin.do("GET", p, nil)
		if code != 200 {
			t.Fatalf("%s: %d", p, code)
		}
		return body
	}
	if strings.Contains(page("/runs"), "<progress") {
		t.Fatal("progress shown before the agent reported any")
	}

	// Size known (disk image, VM): exact percentage.
	poll(api.RunProgress{Done: 400 << 20, Total: 1000 << 20})
	for _, p := range []string{"/runs", fmt.Sprint("/runs/", runID), fmt.Sprint("/jobs/", jobs[0].ID), "/"} {
		if b := page(p); !strings.Contains(b, `<progress max="100" value="40">`) || !strings.Contains(b, "<b>40%</b>") {
			t.Fatalf("%s: no 40%% progress", p)
		}
	}

	// File backup: estimated from the 10 files of the previous backup.
	poll(api.RunProgress{Done: 1 << 20, Files: 4})
	if b := page("/runs"); !strings.Contains(b, `value="40"`) || !strings.Contains(b, "<b>≈40%</b>") || !strings.Contains(b, "4 files") {
		t.Fatal("no estimated progress")
	}
	// More files than last time: no percentage, only the counts.
	poll(api.RunProgress{Done: 2 << 20, Files: 25})
	if b := page("/runs"); strings.Contains(b, "<progress") || !strings.Contains(b, "25 files") {
		t.Fatal("estimate shown past the previous file count")
	}

	// Finished runs show no progress.
	if err := x.e.store.FinishRun(ctx, x.agentID, runID, api.RunResult{Status: api.StatusSuccess}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page("/runs"), "<progress") {
		t.Fatal("progress shown for a finished run")
	}
}
