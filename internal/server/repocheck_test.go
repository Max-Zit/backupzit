package server_test

import (
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/server"
)

func TestRepoCheck(t *testing.T) {
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
	store := filepath.Join(t.TempDir(), "nas")
	target, _ := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "local", URL: store})
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "a.txt"), []byte(strings.Repeat("backup data ", 5000)), 0o644)
	jobID, _ := e.store.CreateJob(ctx, server.Job{AgentID: agents[0].ID, TargetID: target, Name: "docs", Paths: []string{src}, Enabled: true})

	if _, loc, _ := admin.do("POST", fmt.Sprintf("/jobs/%d/check", jobID), nil); !strings.Contains(loc, "err=") {
		t.Error("check without a backup accepted")
	}
	e.store.QueueBackup(ctx, jobID, "manual")
	runAgent(t, ag)
	if _, _, body := admin.do("GET", fmt.Sprintf("/jobs/%d", jobID), nil); !strings.Contains(body, fmt.Sprintf(`action="/jobs/%d/check"`, jobID)) {
		t.Error("job page lacks Check data")
	}
	check := func() server.Run {
		t.Helper()
		_, loc, _ := admin.do("POST", fmt.Sprintf("/jobs/%d/check", jobID), nil)
		if !strings.Contains(loc, "/runs/") {
			t.Fatalf("queue check: %s", loc)
		}
		runAgent(t, ag)
		runs, _ := e.store.ListRuns(ctx, server.RunFilter{JobID: jobID, Limit: 1})
		return runs[0]
	}
	r := check()
	if r.Kind != "repo-check" || r.Status != "success" || !strings.Contains(r.Message, "Checked 1 backups and read all") {
		t.Fatalf("check: %s %s %q", r.Kind, r.Status, r.Message)
	}

	// Damage a data pack on the storage: the check finds it.
	var pack string
	filepath.WalkDir(store, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.Contains(filepath.ToSlash(p), "/data/") {
			pack = p
		}
		return nil
	})
	if pack == "" {
		t.Fatal("no data pack found")
	}
	b, _ := os.ReadFile(pack)
	b[len(b)/2] ^= 0xff
	os.Chmod(pack, 0o644)
	if err := os.WriteFile(pack, b, 0o644); err != nil {
		t.Fatal(err)
	}
	r = check()
	if r.Status != "failed" || !strings.Contains(r.Message, "problems found") || len(r.Errors) == 0 {
		t.Errorf("check of damaged data: %s %q %v", r.Status, r.Message, r.Errors)
	}

	// Scheduled: once per repository and interval.
	sched := server.NewScheduler(e.store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sched.Tick(ctx, time.Now().Add(31*24*time.Hour))
	runs, _ := e.store.ListRuns(ctx, server.RunFilter{JobID: jobID, Limit: 10})
	n := 0
	for _, r := range runs {
		if r.Kind == "repo-check" && r.Status == "queued" && r.Trigger == "schedule" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("scheduled checks queued: %d", n)
	}
}
