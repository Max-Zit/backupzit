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

	// R-2 (0.55): a step after the data shows with its count, not a silent 100 %.
	poll(api.RunProgress{Done: 1000 << 20, Total: 1000 << 20, Stage: "locks", StageDone: 3200, StageTotal: 11000})
	if b := page(fmt.Sprint("/runs/", runID)); !strings.Contains(b, "Extending object locks 3200 / 11000") {
		t.Fatal("the lock extension step is not shown")
	}
	poll(api.RunProgress{Done: 0, Total: 0, Stage: "locks-read", StageDone: 77})
	if b := page(fmt.Sprint("/runs/", runID)); !strings.Contains(b, "Reading the kept backups for the object locks: 77 read") {
		t.Fatal("reading the kept backups is not shown")
	}

	// #44 (0.57): a VM backup shows the VM it is on and the VMs finished.
	poll(api.RunProgress{Done: 300 << 20, Total: 1000 << 20, VM: "web-01", VMsDone: 1, VMsTotal: 4,
		VMsFinished: []api.VMFinished{{Name: "db-<01>", Failed: true}}})
	for _, p := range []string{"/runs", fmt.Sprint("/runs/", runID)} {
		if b := page(p); !strings.Contains(b, "VM 2 of 4: web-01") {
			t.Fatalf("%s: current VM not shown", p)
		}
	}
	if b := page(fmt.Sprint("/runs/", runID)); !strings.Contains(b, "db-&lt;01&gt;") || !strings.Contains(b, `badge failed`) {
		t.Fatal("finished VM not listed (escaped) with its result")
	}

	// Finished runs show no progress.
	if err := x.e.store.FinishRun(ctx, x.agentID, runID, api.RunResult{Status: api.StatusSuccess}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page("/runs"), "<progress") {
		t.Fatal("progress shown for a finished run")
	}
}

// A data check reports how many data packs it read, not bytes: the pages
// show the count of packs, not "5.2 KiB / 7.1 KiB".
func TestRunProgressDataCheckCountsPacks(t *testing.T) {
	x := newRemoveEnv(t, "0.54.2")
	ctx := x.e.ctx
	jobs, err := x.e.store.ListJobs(ctx)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs: %v %v", jobs, err)
	}
	runID, err := x.e.store.QueueRepoCheck(ctx, jobs[0].ID, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if run, err := x.e.store.ClaimRun(ctx, x.agentID); err != nil || run == nil || run.ID != runID || run.Kind != api.KindRepoCheck {
		t.Fatalf("claim: %+v %v", run, err)
	}
	x.call("POST", api.PathPoll, api.PollRequest{Hostname: "PC-17", OS: "Windows 11", Arch: "amd64", Version: "0.54.2", Running: 1,
		Progress: []api.RunProgress{{RunID: runID, Done: 5300, Total: 7300}}}, nil)
	for _, p := range []string{"/runs", fmt.Sprint("/runs/", runID)} {
		code, _, b := x.admin.do("GET", p, nil)
		if code != 200 {
			t.Fatalf("%s: %d", p, code)
		}
		if !strings.Contains(b, "<b>72%</b>") || !strings.Contains(b, "Data packs checked: 5300 / 7300") {
			t.Fatalf("%s: data check progress not shown as packs", p)
		}
		if strings.Contains(b, "KiB / ") {
			t.Fatalf("%s: pack counts shown as bytes", p)
		}
	}
}
