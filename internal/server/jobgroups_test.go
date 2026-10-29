package server_test

import (
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/server"
)

// A job for several agents keeps one member job per matching agent, adds
// agents that enroll later, follows changes of its settings and leaves
// agents out on request; member jobs are changed through the group only.
func TestJobGroups(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	enroll := func() {
		t.Helper()
		token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
		if _, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test"); err != nil {
			t.Fatal(err)
		}
	}
	tick := func() { server.NewScheduler(e.store, slog.Default()).Tick(ctx, time.Now()) }
	members := func(gid int64) []server.Job {
		t.Helper()
		jobs, err := e.store.ListJobs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []server.Job
		for _, j := range jobs {
			if j.GroupID != nil && *j.GroupID == gid {
				out = append(out, j)
			}
		}
		return out
	}
	enroll()
	target, _ := e.store.CreateTarget(ctx, server.Target{Name: "store", Kind: "local", URL: t.TempDir()})
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")

	// Only files jobs can be for several agents.
	_, loc, _ := admin.do("POST", "/jobs", url.Values{"name": {"x"}, "agent_id": {"group:all"}, "target_id": {fmt.Sprint(target)},
		"kind": {"image"}, "sched_kind": {"manual"}})
	if !strings.Contains(loc, "err=") {
		t.Fatalf("image job for several agents accepted: %s", loc)
	}
	_, loc, _ = admin.do("POST", "/jobs", url.Values{"name": {"Documents of everyone"}, "agent_id": {"group:all"}, "target_id": {fmt.Sprint(target)},
		"kind": {"files"}, "paths": {"{users}/Documents\n{users}/Desktop"}, "keep_last": {"7"}, "sched_kind": {"manual"}})
	if !strings.Contains(loc, "/job-groups/") || !strings.Contains(loc, "msg=") {
		t.Fatalf("create: %s", loc)
	}
	groups, _ := e.store.ListJobGroups(ctx)
	if len(groups) != 1 {
		t.Fatalf("groups: %d", len(groups))
	}
	g := groups[0]
	if m := members(g.ID); len(m) != 1 || m[0].Name != "Documents of everyone" || m[0].Retention.KeepLast != 7 || len(m[0].Paths) != 2 {
		t.Fatalf("members after create: %+v", m)
	}

	// A new agent joins with the next scheduler pass.
	enroll()
	tick()
	m := members(g.ID)
	if len(m) != 2 {
		t.Fatalf("members after a new agent: %d", len(m))
	}

	// Member jobs are edited and deleted through the group.
	_, loc, _ = admin.do("POST", fmt.Sprintf("/jobs/%d/edit", m[0].ID), url.Values{"name": {"mine"}, "paths": {"/x"}, "sched_kind": {"manual"}})
	if !strings.Contains(loc, "/job-groups/") || !strings.Contains(loc, "err=") {
		t.Fatalf("member edit: %s", loc)
	}
	_, loc, _ = admin.do("POST", fmt.Sprintf("/jobs/%d/delete", m[0].ID), url.Values{})
	if !strings.Contains(loc, "err=") {
		t.Fatalf("member delete: %s", loc)
	}

	// Settings of the group reach all members; a left-out agent loses its
	// member job.
	_, loc, _ = admin.do("POST", fmt.Sprintf("/job-groups/%d/edit", g.ID), url.Values{"name": {"Documents"}, "scope": {"all"},
		"group_exclude": {fmt.Sprint(m[1].AgentID)}, "paths": {"{users}/Documents"}, "keep_last": {"14"}, "sched_kind": {"manual"}})
	if !strings.Contains(loc, "msg=") {
		t.Fatalf("group edit: %s", loc)
	}
	m = members(g.ID)
	if len(m) != 1 || m[0].Name != "Documents" || m[0].Retention.KeepLast != 14 || strings.Join(m[0].Paths, ",") != "{users}/Documents" {
		t.Fatalf("members after edit: %+v", m)
	}

	// Run on all agents, then the page lists them.
	_, loc, _ = admin.do("POST", fmt.Sprintf("/job-groups/%d/run", g.ID), url.Values{})
	if !strings.Contains(loc, "msg=Backups+queued+for+1+agents") {
		t.Fatalf("run all: %s", loc)
	}
	if code, _, body := admin.do("GET", fmt.Sprintf("/job-groups/%d", g.ID), nil); code != 200 || !strings.Contains(body, "Run backup now on all agents") {
		t.Fatalf("group page: %d", code)
	}
	if _, _, body := admin.do("GET", "/jobs", nil); !strings.Contains(body, "Jobs for several agents") || !strings.Contains(body, "New backup job") ||
		!strings.Contains(body, fmt.Sprintf(`href="/job-groups/%d"`, g.ID)) {
		t.Fatal("jobs page does not list the group and its member, or stops rendering")
	}
	if code, _, body := admin.do("GET", fmt.Sprintf("/jobs/%d", m[0].ID), nil); code != 200 || !strings.Contains(body, "Part of the job for several agents") || strings.Contains(body, "/edit\"") {
		t.Fatalf("member job page: %d", code)
	}

	// Deleting the group deletes its member jobs, not their history.
	admin.do("POST", fmt.Sprintf("/job-groups/%d/delete", g.ID), url.Values{})
	if gs, _ := e.store.ListJobGroups(ctx); len(gs) != 0 || len(members(g.ID)) != 0 {
		t.Fatalf("after delete: %d groups", len(gs))
	}
	log, _ := e.store.ListAudit(ctx, 50)
	var acts []string
	for _, a := range log {
		acts = append(acts, a.Action)
	}
	for _, want := range []string{"group.create", "group.member", "group.update", "group.delete"} {
		if !strings.Contains(strings.Join(acts, " "), want) {
			t.Errorf("audit log lacks %s: %v", want, acts)
		}
	}
}
