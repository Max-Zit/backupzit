package server_test

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
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
	// damaged counts the damaged packs a failed check listed.
	damaged := func(r server.Run) int {
		var st struct{ Damaged []json.RawMessage }
		_ = json.Unmarshal(r.Stats, &st)
		return len(st.Damaged)
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

	// Repair (0.54.2): offered on the failed check, takes the damaged pack
	// out of the index; the next backup stores the data again and a new
	// check passes.
	if _, _, body := admin.do("GET", fmt.Sprintf("/runs/%d", r.ID), nil); !strings.Contains(body, fmt.Sprintf(`action="/runs/%d/repair"`, r.ID)) {
		t.Error("the failed check does not offer Repair")
	}
	_, loc, _ := admin.do("POST", fmt.Sprintf("/runs/%d/repair", r.ID), nil)
	if !strings.Contains(loc, "/runs/") || !strings.Contains(loc, "msg=") {
		t.Fatalf("queue repair: %s", loc)
	}
	runAgent(t, ag)
	runs, _ := e.store.ListRuns(ctx, server.RunFilter{JobID: jobID, Limit: 1})
	rep := runs[0]
	if rep.Kind != "repo-repair" || rep.Status != "warning" || !strings.Contains(rep.Message, "Took 1 damaged data packs") || len(rep.Errors) == 0 {
		t.Fatalf("repair: %s %s %q %v", rep.Kind, rep.Status, rep.Message, rep.Errors)
	}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/runs/%d/repair", rep.ID), nil); !strings.Contains(loc, "err=") {
		t.Error("a repair run could be repaired again")
	}
	// The next backup (even with the lost parent tree) stores the data again.
	// The source did not change, so the content-addressed data and trees are
	// the same: the old backup is complete again and the check passes.
	e.store.QueueBackup(ctx, jobID, "manual")
	runAgent(t, ag)
	if r = check(); r.Status != "success" {
		t.Fatalf("check after repair and a new backup: %s %q %v", r.Status, r.Message, r.Errors)
	}

	// The source changes, a new backup stores only new data, then the packs
	// of the earlier backups are damaged. Repair with "also remove the
	// backups that need the lost data" removes those (their runs expire) and
	// keeps the newest one: the check passes again without a new backup.
	dataPacks := func() map[string]bool {
		m := map[string]bool{}
		filepath.WalkDir(store, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.Contains(filepath.ToSlash(p), "/data/") {
				m[p] = true
			}
			return nil
		})
		return m
	}
	old := dataPacks()
	os.WriteFile(filepath.Join(src, "a.txt"), []byte(strings.Repeat("changed content ", 7000)), 0o644)
	e.store.QueueBackup(ctx, jobID, "manual")
	runAgent(t, ag)
	latest, _ := e.store.ListRuns(ctx, server.RunFilter{JobID: jobID, Limit: 1})
	if latest[0].Kind != "backup" || latest[0].Status != "success" || latest[0].SnapshotID == "" {
		t.Fatalf("backup of the changed source: %s %s %q", latest[0].Kind, latest[0].Status, latest[0].Message)
	}
	for p := range old {
		b, _ := os.ReadFile(p)
		b[len(b)/2] ^= 0xff
		os.Chmod(p, 0o644)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if r = check(); damaged(r) == 0 {
		t.Fatalf("second damage not found: %s %q", r.Status, r.Message)
	}
	// Removing backups is protected like other deletions: it needs the
	// password again, is refused while retention is paused after a
	// suspicious backup, and waits for a second user with four-eyes on.
	repairURL := fmt.Sprintf("/runs/%d/repair", r.ID)
	if _, loc, _ := admin.do("POST", repairURL, url.Values{"forget": {"on"}}); !strings.Contains(loc, "err=") {
		t.Fatalf("repair removing backups without the password: %s", loc)
	}
	e.pool.Exec(ctx, `UPDATE jobs SET retention_hold=true WHERE id=$1`, jobID)
	if _, loc, _ := admin.do("POST", repairURL, url.Values{"forget": {"on"}, "reauth_password": {"admin-pass-123"}}); !strings.Contains(loc, "paused") {
		t.Fatalf("repair removing backups during a retention hold: %s", loc)
	}
	e.pool.Exec(ctx, `UPDATE jobs SET retention_hold=false WHERE id=$1`, jobID)
	if _, loc, _ := admin.do("POST", "/users", url.Values{"reauth_password": {"admin-pass-123"}, "username": {"admin2"}, "role": {"admin"},
		"password": {"correct-horse-9"}, "password2": {"correct-horse-9"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("create admin2: %s", loc)
	}
	if _, loc, _ := admin.do("POST", "/settings/four-eyes", url.Values{"four_eyes": {"on"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("four-eyes on: %s", loc)
	}
	if _, loc, _ := admin.do("POST", repairURL, url.Values{"forget": {"on"}, "reauth_password": {"admin-pass-123"}}); !strings.Contains(loc, "msg=Waiting") {
		t.Fatalf("repair removing backups not held for approval: %s", loc)
	}
	if runs, _ := e.store.ListRuns(ctx, server.RunFilter{JobID: jobID, Limit: 1}); runs[0].Kind == "repo-repair" {
		t.Fatal("repair queued before the approval")
	}
	var appr int64
	e.pool.QueryRow(ctx, `SELECT id FROM approvals WHERE status='pending' AND action='repair.forget'`).Scan(&appr)
	admin2 := newClient(t, e)
	admin2.login("admin2", "correct-horse-9")
	if _, loc, _ := admin2.do("POST", fmt.Sprintf("/approvals/%d/approve", appr), url.Values{}); !strings.Contains(loc, "err=") {
		t.Fatalf("approval removing backups without the approver's password: %s", loc)
	}
	if _, loc, _ := admin2.do("POST", fmt.Sprintf("/approvals/%d/approve", appr), url.Values{"reauth_password": {"correct-horse-9"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("approve the repair: %s", loc)
	}
	runAgent(t, ag)
	runs, _ = e.store.ListRuns(ctx, server.RunFilter{JobID: jobID, Limit: 1})
	rep = runs[0]
	if rep.Kind != "repo-repair" || rep.Status != "warning" || !strings.Contains(rep.Message, "Removed 2 of the 2 backups") {
		t.Fatalf("repair with forget: %s %s %q %v", rep.Kind, rep.Status, rep.Message, rep.Errors)
	}
	runs, _ = e.store.ListRuns(ctx, server.RunFilter{JobID: jobID, Limit: 50})
	expired := 0
	for _, x := range runs {
		if x.Kind == "backup" && x.Expired {
			expired++
			if x.SnapshotID == latest[0].SnapshotID {
				t.Error("the intact newest backup was removed")
			}
		}
	}
	if expired != 2 {
		t.Errorf("backup runs marked expired after repair with forget: %d, want 2", expired)
	}
	if r = check(); r.Status != "success" {
		t.Fatalf("check after repair with forget: %s %q %v", r.Status, r.Message, r.Errors)
	}

	// Scheduled: once per repository and interval.
	sched := server.NewScheduler(e.store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sched.Tick(ctx, time.Now().Add(31*24*time.Hour))
	runs, _ = e.store.ListRuns(ctx, server.RunFilter{JobID: jobID, Limit: 10})
	n := 0
	for _, r := range runs {
		if r.Kind == "repo-check" && r.Status == "queued" && r.Trigger == "schedule" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("scheduled checks queued: %d", n)
	}

	// M-22 (0.55): the job also has older backups in another repository (a
	// rotating disk, a moved folder): both are checked once, no loop.
	if _, err := e.pool.Exec(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, status, repo_url, target_id, queued_at, started_at, finished_at)
		SELECT agent_id, job_id, 'backup', 'manual', 'success', repo_url || '-old', target_id, queued_at, started_at, finished_at
		FROM runs WHERE job_id=$1 AND kind='backup' ORDER BY id LIMIT 1`, jobID); err != nil {
		t.Fatal(err)
	}
	countChecks := func() map[string]int {
		m := map[string]int{}
		runs, _ := e.store.ListRuns(ctx, server.RunFilter{JobID: jobID, Limit: 100})
		for _, r := range runs {
			if r.Kind == "repo-check" && r.Status == "queued" && r.Trigger == "schedule" {
				m[r.RepoURL]++
			}
		}
		return m
	}
	sched.Tick(ctx, time.Now().Add(31*24*time.Hour))
	sched.Tick(ctx, time.Now().Add(31*24*time.Hour))
	if m := countChecks(); len(m) != 2 {
		t.Errorf("scheduled checks per repository: %v", m)
	} else {
		for u, c := range m {
			if c != 1 {
				t.Errorf("repository %s checked %d times", u, c)
			}
		}
	}
}
