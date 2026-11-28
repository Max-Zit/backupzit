package server_test

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/server"
)

// MED 9: the backup window holds and cancels a job's scheduled backups and
// copies only. SQL log backups, restore tests, data checks and replicas are
// not cancelled by it; tests and checks are queued while the window is open,
// and one the console cancelled itself does not count as done.
func TestBackupWindowOnlyBackups(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	if _, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "0.62.0"); err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	aid := agents[0].ID
	target, _ := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "local", URL: filepath.Join(t.TempDir(), "nas")})
	h := time.Now().Hour()
	closed := server.JobOptions{Window: true, WindowFrom: (h + 2) % 24, WindowTo: (h + 4) % 24}
	job, err := e.store.CreateJob(ctx, server.Job{AgentID: aid, TargetID: target, Name: "night", Paths: []string{"/srv"}, Enabled: true, Options: closed})
	if err != nil {
		t.Fatal(err)
	}
	// A backup from long ago: a restore test and a data check are due.
	if _, err := e.pool.Exec(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, target_id, status, snapshot_id, queued_at, started_at, finished_at)
		VALUES ($1,$2,'backup','manual','file:///nas/x',$3,'success','abc',now()-interval '40 days',now()-interval '40 days',now()-interval '40 days')`, aid, job, target); err != nil {
		t.Fatal(err)
	}
	queue := func(kind, trigger string) int64 {
		var id int64
		if err := e.pool.QueryRow(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, target_id, queued_at)
			VALUES ($1,$2,$3,$4,'file:///nas/x',$5,now()-interval '5 minutes') RETURNING id`, aid, job, kind, trigger, target).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	status := func(id int64) string {
		r, _ := e.store.GetRun(ctx, id)
		return r.Status
	}
	backup := queue("backup", "schedule")
	logs := queue("sql-log", "schedule")
	replica := queue("vm-replica", "after-backup")
	test := queue("verify", "schedule")
	check := queue("repo-check", "schedule")
	sched := server.NewScheduler(e.store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sched.Tick(ctx, time.Now().Add(2*time.Minute))
	if s := status(backup); s != "cancelled" {
		t.Errorf("scheduled backup outside the window: %s", s)
	}
	for name, id := range map[string]int64{"SQL log backup": logs, "replica": replica, "restore test": test, "data check": check} {
		if s := status(id); s != "queued" {
			t.Errorf("%s cancelled by the backup window: %s", name, s)
		}
	}
	count := func(kind string) int {
		var n int
		e.pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE job_id=$1 AND kind=$2`, job, kind).Scan(&n)
		return n
	}

	// Tests and checks the console cancelled itself (the window did so
	// before 0.63) do not count as done; with the window closed none is
	// queued, once it is open one is.
	e.pool.Exec(ctx, `UPDATE runs SET status='cancelled', finished_at=now(), queued_at=now() WHERE id = ANY($1)`, []int64{test, check})
	sched.Tick(ctx, time.Now())
	if count("verify") != 1 || count("repo-check") != 1 {
		t.Fatalf("queued outside the window: %d tests, %d checks", count("verify"), count("repo-check"))
	}
	open := server.JobOptions{Window: true, WindowFrom: h, WindowTo: (h + 2) % 24}
	j, _ := e.store.GetJob(ctx, job)
	j.Options = open
	if err := e.store.UpdateJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	sched.Tick(ctx, time.Now())
	if count("verify") != 2 || count("repo-check") != 2 {
		t.Fatalf("not queued in the open window after a cancel by the console: %d tests, %d checks", count("verify"), count("repo-check"))
	}
	// A test a user cancelled counts: it is not queued again at once.
	var newTest int64
	e.pool.QueryRow(ctx, `SELECT id FROM runs WHERE job_id=$1 AND kind='verify' AND status='queued'`, job).Scan(&newTest)
	if _, err := e.store.CancelRun(ctx, newTest, "admin", "Cancelled by admin"); err != nil {
		t.Fatal(err)
	}
	sched.Tick(ctx, time.Now())
	if count("verify") != 2 {
		t.Errorf("a test the user cancelled was queued again: %d tests", count("verify"))
	}
}
