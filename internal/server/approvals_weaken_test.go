package server_test

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/server"
)

// B1: with four-eyes approval, an edit that weakens a job (a schedule that
// runs less, a source removed, an exclude added, shorter retention) saves
// the rest at once and waits with the weakened settings; the approval
// carries them out, unless the job was changed again meanwhile.
func TestFourEyesWeakenedJob(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	if _, loc, _ := admin.do("POST", "/users", url.Values{"reauth_password": {"admin-pass-123"}, "username": {"admin2"}, "role": {"admin"},
		"password": {"correct-horse-9"}, "password2": {"correct-horse-9"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("create admin2: %s", loc)
	}
	time.Sleep(10 * time.Millisecond) // admin2 exists before the requests
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	if _, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test"); err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	tid, _ := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "local", URL: filepath.Join(t.TempDir(), "nas")})
	a, b := t.TempDir(), t.TempDir()
	jobID, err := e.store.CreateJob(ctx, server.Job{AgentID: agents[0].ID, TargetID: tid, Name: "docs", Paths: []string{a, b}, Enabled: true,
		Schedule: `{"kind":"daily","times":["21:00"]}`, Retention: repo.RetentionPolicy{KeepLast: 5}})
	if err != nil {
		t.Fatal(err)
	}
	if _, loc, _ := admin.do("POST", "/settings/four-eyes", url.Values{"four_eyes": {"on"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("four-eyes on: %s", loc)
	}
	edit := func(c *client, v url.Values) string {
		_, loc, _ := c.do("POST", fmt.Sprintf("/jobs/%d/edit", jobID), v)
		return loc
	}

	// A harmless edit (another time, a new folder) is saved at once.
	if loc := edit(admin, url.Values{"name": {"docs"}, "paths": {a + "\n" + b + "\n" + filepath.Join(a, "x")}, "keep_last": {"5"},
		"sched_kind": {"daily"}, "sched_days": {"1", "2", "3", "4", "5"}, "sched_time": {"22:00"}}); !strings.Contains(loc, "msg=") || strings.Contains(loc, "Waiting") {
		t.Fatalf("harmless edit: %s", loc)
	}
	if j, _ := e.store.GetJob(ctx, jobID); len(j.Paths) != 3 || !strings.Contains(j.Schedule, "22:00") {
		t.Fatalf("harmless edit not saved: %+v %s", j.Paths, j.Schedule)
	}

	// Weakening: by hand only, b removed, an exclude, shorter retention;
	// the new name is saved now.
	if loc := edit(admin, url.Values{"name": {"documents"}, "paths": {a + "\n" + filepath.Join(a, "x")}, "excludes": {"*.tmp"}, "keep_last": {"2"},
		"sched_kind": {"manual"}}); !strings.Contains(loc, "msg=Waiting") {
		t.Fatalf("weakening edit not held: %s", loc)
	}
	j, _ := e.store.GetJob(ctx, jobID)
	if j.Name != "documents" || len(j.Paths) != 3 || len(j.Excludes) != 0 || j.Retention.KeepLast != 5 || !strings.Contains(j.Schedule, "22:00") {
		t.Fatalf("held settings changed before approval: %+v", j)
	}
	admin2 := newClient(t, e)
	admin2.login("admin2", "correct-horse-9")
	_, _, page := admin2.do("GET", "/approvals", nil)
	for _, want := range []string{"Change backup job so that it protects less", "Runs only when started by hand from now on", "No longer backed up: " + b, "Excluded from now on: *.tmp", "Shorter retention:"} {
		if !strings.Contains(page, want) {
			t.Errorf("approvals page lacks %q", want)
		}
	}
	var id int64
	e.pool.QueryRow(ctx, `SELECT id FROM approvals WHERE status='pending' AND action='job.update'`).Scan(&id)
	if _, loc, _ := admin2.do("POST", fmt.Sprintf("/approvals/%d/approve", id), url.Values{}); !strings.Contains(loc, "msg=") {
		t.Fatalf("approve: %s", loc)
	}
	j, _ = e.store.GetJob(ctx, jobID)
	if len(j.Paths) != 2 || len(j.Excludes) != 1 || j.Retention.KeepLast != 2 || strings.Contains(j.Schedule, "daily") {
		t.Fatalf("approved change not carried out: %+v", j)
	}

	// A request whose settings were changed again meanwhile is not carried
	// out over the newer edit.
	if loc := edit(admin, url.Values{"name": {"documents"}, "paths": {a + "\n" + filepath.Join(a, "x")}, "excludes": {"*.tmp"}, "keep_last": {"2"},
		"sched_kind": {"daily"}, "sched_days": {"1", "2", "3", "4", "5"}, "sched_time": {"23:00"}}); strings.Contains(loc, "Waiting") {
		t.Fatalf("stronger schedule held: %s", loc)
	}
	if loc := edit(admin, url.Values{"name": {"documents"}, "paths": {a + "\n" + filepath.Join(a, "x")}, "excludes": {"*.tmp"}, "keep_last": {"1"},
		"sched_kind": {"daily"}, "sched_days": {"1", "2", "3", "4", "5"}, "sched_time": {"23:00"}}); !strings.Contains(loc, "msg=Waiting") {
		t.Fatalf("shorter retention not held: %s", loc)
	}
	e.pool.QueryRow(ctx, `SELECT id FROM approvals WHERE status='pending' AND action='job.update'`).Scan(&id)
	newer, _ := e.store.GetJob(ctx, jobID)
	newer.Retention = repo.RetentionPolicy{KeepLast: 9}
	if err := e.store.UpdateJob(ctx, newer); err != nil {
		t.Fatal(err)
	}
	if _, loc, _ := admin2.do("POST", fmt.Sprintf("/approvals/%d/approve", id), url.Values{}); !strings.Contains(loc, "err=") {
		t.Errorf("stale request carried out: %s", loc)
	}
	if j, _ := e.store.GetJob(ctx, jobID); j.Retention.KeepLast != 9 {
		t.Errorf("newer retention overwritten: %+v", j.Retention)
	}
}

// B1: the same for a job for several agents: removing a folder waits as a
// whole, the approval carries it out, a stale request is refused.
func TestFourEyesWeakenedGroup(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	if _, loc, _ := admin.do("POST", "/users", url.Values{"reauth_password": {"admin-pass-123"}, "username": {"admin2"}, "role": {"admin"},
		"password": {"correct-horse-9"}, "password2": {"correct-horse-9"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("create admin2: %s", loc)
	}
	time.Sleep(10 * time.Millisecond)
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	if _, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test"); err != nil {
		t.Fatal(err)
	}
	target, _ := e.store.CreateTarget(ctx, server.Target{Name: "store", Kind: "local", URL: t.TempDir()})
	if _, loc, _ := admin.do("POST", "/jobs", url.Values{"name": {"Docs"}, "agent_id": {"group:all"}, "target_id": {fmt.Sprint(target)},
		"kind": {"files"}, "paths": {"{users}/Documents\n{users}/Desktop"}, "keep_last": {"7"}, "sched_kind": {"manual"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("create group: %s", loc)
	}
	groups, _ := e.store.ListJobGroups(ctx)
	gid := groups[0].ID
	if _, loc, _ := admin.do("POST", "/settings/four-eyes", url.Values{"four_eyes": {"on"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("four-eyes on: %s", loc)
	}
	editGroup := func(paths string) string {
		_, loc, _ := admin.do("POST", fmt.Sprintf("/job-groups/%d/edit", gid), url.Values{"name": {"Docs"}, "scope": {"all"},
			"paths": {paths}, "keep_last": {"7"}, "sched_kind": {"manual"}})
		return loc
	}
	if loc := editGroup("{users}/Documents"); !strings.Contains(loc, "msg=Waiting") {
		t.Fatalf("folder removal not held: %s", loc)
	}
	admin2 := newClient(t, e)
	admin2.login("admin2", "correct-horse-9")
	if _, _, page := admin2.do("GET", "/approvals", nil); !strings.Contains(page, "No longer backed up: {users}/Desktop") {
		t.Error("approvals page lacks the removed folder")
	}
	var id int64
	e.pool.QueryRow(ctx, `SELECT id FROM approvals WHERE status='pending' AND action='group.update'`).Scan(&id)
	if _, loc, _ := admin2.do("POST", fmt.Sprintf("/approvals/%d/approve", id), url.Values{}); !strings.Contains(loc, "msg=") {
		t.Fatalf("approve: %s", loc)
	}
	if g, _ := e.store.GetJobGroup(ctx, gid); strings.Join(g.Paths, ",") != "{users}/Documents" {
		t.Fatalf("group after approval: %v", g.Paths)
	}
	// Stale: the group changes after the request.
	if loc := editGroup("{users}/Pictures"); !strings.Contains(loc, "msg=Waiting") {
		t.Fatalf("second removal not held: %s", loc)
	}
	e.pool.QueryRow(ctx, `SELECT id FROM approvals WHERE status='pending' AND action='group.update'`).Scan(&id)
	if _, err := e.pool.Exec(ctx, `UPDATE job_groups SET name='Docs (renamed)' WHERE id=$1`, gid); err != nil {
		t.Fatal(err)
	}
	if _, loc, _ := admin2.do("POST", fmt.Sprintf("/approvals/%d/approve", id), url.Values{}); !strings.Contains(loc, "err=") {
		t.Errorf("stale group request carried out: %s", loc)
	}
}
