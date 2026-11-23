package server_test

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/server"
)

// Agents with the same host name (servers of different customers) get a
// display name and tags in the console (issue #34). Only console users set
// them; the reported host name stays visible and the repository directory
// never changes.
func TestAgentDisplayNameAndTags(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	for i := 0; i < 2; i++ {
		token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
		if _, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test"); err != nil {
			t.Fatal(err)
		}
	}
	agents, _ := e.store.ListAgents(ctx)
	if len(agents) != 2 || agents[0].Hostname != agents[1].Hostname {
		t.Fatalf("agents %+v", agents)
	}
	a := agents[0]
	target, _ := e.store.CreateTarget(ctx, server.Target{Name: "local", Kind: "local", URL: t.TempDir()})
	job, _ := e.store.CreateJob(ctx, server.Job{AgentID: a.ID, TargetID: target, Name: "files", Paths: []string{t.TempDir()}, Enabled: true})

	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	_, _, page := admin.do("GET", fmt.Sprintf("/agents/%d", a.ID), nil)
	if !strings.Contains(page, "Other agents report the same host name") {
		t.Error("agent page does not point out the duplicate host name")
	}
	_, loc, _ := admin.do("POST", fmt.Sprintf("/agents/%d/labels", a.ID), url.Values{"display_name": {"  Customer A file server "}, "tags": {"Customer A, Vienna, customer a,"}})
	if !strings.Contains(loc, "msg=") {
		msg, _ := url.QueryUnescape(loc)
		t.Fatalf("save: %s", msg)
	}
	got, _ := e.store.GetAgent(ctx, a.ID)
	if got.DisplayName != "Customer A file server" || strings.Join(got.Tags, "|") != "Customer A|Vienna" {
		t.Fatalf("stored %q %q", got.DisplayName, got.Tags)
	}
	if got.RepoDir != a.RepoDir || got.Hostname != a.Hostname {
		t.Fatalf("rename changed repo dir or host name: %q→%q %q→%q", a.RepoDir, got.RepoDir, a.Hostname, got.Hostname)
	}

	// The display name is shown, with the reported host name next to it.
	_, _, page = admin.do("GET", "/agents", nil)
	if !strings.Contains(page, "Customer A file server") || !strings.Contains(page, a.Hostname) || !strings.Contains(page, `href="/agents?tag=Vienna"`) {
		t.Error("agents page does not show name, host name and tags")
	}
	_, _, page = admin.do("GET", "/agents?tag=vienna", nil)
	if !strings.Contains(page, "Customer A file server") || strings.Contains(page, fmt.Sprintf(`href="/agents/%d"`, agents[1].ID)) {
		t.Error("tag filter does not narrow the list")
	}
	_, _, page = admin.do("GET", "/jobs", nil)
	if !strings.Contains(page, "Customer A file server ("+a.Hostname+")") {
		t.Error("job form does not offer the agent by its name")
	}
	if j, _ := e.store.GetJob(ctx, job); j.Hostname != "Customer A file server" {
		t.Errorf("job shows %q", j.Hostname)
	}
	rid, _ := e.store.QueueBackup(ctx, job, "manual")
	if r, _ := e.store.GetRun(ctx, rid); r.Hostname != "Customer A file server" || r.ReportedHost != a.Hostname {
		t.Errorf("run shows %q, reported %q", r.Hostname, r.ReportedHost)
	}

	// Every change is audited.
	entries, _ := e.store.ListAudit(ctx, 20)
	var rename, tags bool
	for _, x := range entries {
		rename = rename || (x.Action == "agent.rename" && strings.Contains(x.Detail, "Customer A file server"))
		tags = tags || (x.Action == "agent.tags" && strings.Contains(x.Detail, "Vienna"))
	}
	if !rename || !tags {
		t.Errorf("audit: rename %v tags %v", rename, tags)
	}

	// Invalid values are refused; the host name itself clears the name.
	for _, v := range []url.Values{
		{"display_name": {"bad\nname"}},
		{"display_name": {strings.Repeat("x", 65)}},
		{"tags": {"a,b,c,d,e,f,g,h,i,j,k"}},
		{"tags": {strings.Repeat("t", 33)}},
		{"display_name": {"rtl" + string(rune(0x202e)) + "evil"}},
	} {
		if _, loc, _ := admin.do("POST", fmt.Sprintf("/agents/%d/labels", a.ID), v); !strings.Contains(loc, "err=") {
			t.Errorf("accepted %v", v)
		}
	}
	admin.do("POST", fmt.Sprintf("/agents/%d/labels", a.ID), url.Values{"display_name": {strings.ToUpper(a.Hostname)}})
	if got, _ := e.store.GetAgent(ctx, a.ID); got.DisplayName != "" || len(got.Tags) != 0 {
		t.Errorf("after reset %q %q", got.DisplayName, got.Tags)
	}

	// A viewer sees the agent but cannot rename it.
	if _, err := e.store.CreateUser(ctx, server.User{Username: "vera", DisplayName: "Vera", Role: "viewer"}, "vera-password-1"); err != nil {
		t.Fatal(err)
	}
	viewer := newClient(t, e)
	viewer.login("vera", "vera-password-1")
	if code, _, page := viewer.do("GET", fmt.Sprintf("/agents/%d", a.ID), nil); code != http.StatusOK || strings.Contains(page, `action="/agents/`) {
		t.Errorf("viewer page: %d", code)
	}
	if code, _, _ := viewer.do("POST", fmt.Sprintf("/agents/%d/labels", a.ID), url.Values{"display_name": {"x"}}); code != http.StatusForbidden {
		t.Errorf("viewer renamed an agent: %d", code)
	}
}
