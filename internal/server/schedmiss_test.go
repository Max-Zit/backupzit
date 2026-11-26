package server_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/server"
)

// B2: a scheduled backup skipped because the previous run is still active
// sends one notice per job, not one per skip, and again only after the job
// backed up in between.
func TestScheduleMissNotice(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	var mu sync.Mutex
	var events []string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		events = append(events, string(b))
		mu.Unlock()
	}))
	defer hook.Close()
	if _, loc, _ := admin.do("POST", "/settings/chat", url.Values{"action": {"add"}, "name": {"tickets"}, "kind": {"webhook"}, "url": {hook.URL + "/hook"},
		"on_failure": {"on"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("add webhook: %s", loc)
	}
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	if _, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test"); err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	tid, _ := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "local", URL: filepath.Join(t.TempDir(), "nas")})
	jobID, _ := e.store.CreateJob(ctx, server.Job{AgentID: agents[0].ID, TargetID: tid, Name: "docs", Paths: []string{t.TempDir()}, Enabled: true,
		Schedule: `{"kind":"interval","every_minutes":15}`})
	if _, err := e.store.QueueBackup(ctx, jobID, "manual"); err != nil { // the run that hangs
		t.Fatal(err)
	}
	sched := server.NewScheduler(e.store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	n := server.NewNotifier(e.store, slog.New(slog.NewTextHandler(io.Discard, nil)), func() string { return "https://backup.example" })
	now := time.Now()
	for i := 0; i < 3; i++ { // three skipped schedules
		now = now.Add(20 * time.Minute)
		sched.Tick(ctx, now)
		n.Pass(ctx, now)
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		c := 0
		for _, ev := range events {
			if strings.Contains(ev, `"event":"schedule.skipped"`) {
				c++
			}
		}
		return c
	}
	if c := count(); c != 1 {
		t.Fatalf("%d notices for three skipped schedules, want 1 (%v)", c, events)
	}
	// A successful backup clears it; the next miss notifies again.
	if _, err := e.pool.Exec(ctx, `UPDATE runs SET status='success', started_at=$2, finished_at=$2 WHERE job_id=$1`, jobID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	n.Pass(ctx, now)
	if _, err := e.store.QueueBackup(ctx, jobID, "manual"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(20 * time.Minute)
	sched.Tick(ctx, now)
	n.Pass(ctx, now)
	if c := count(); c != 2 {
		t.Errorf("%d notices after a new miss, want 2", c)
	}
}
