package server_test

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/server"
)

// Enabling and disabling a job is audited; unknown jobs are not found.
func TestJobEnableAudited(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	if _, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test"); err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	target, _ := e.store.CreateTarget(ctx, server.Target{Name: "store", Kind: "local", URL: t.TempDir()})
	jid, err := e.store.CreateJob(ctx, server.Job{Kind: server.JobFiles, AgentID: agents[0].ID, TargetID: target, Name: "docs", Paths: []string{t.TempDir()}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/jobs/%d/disable", jid), url.Values{}); !strings.Contains(loc, "msg=") {
		t.Fatalf("disable: %s", loc)
	}
	if j, _ := e.store.GetJob(ctx, jid); j.Enabled {
		t.Fatal("job still enabled")
	}
	admin.do("POST", fmt.Sprintf("/jobs/%d/enable", jid), url.Values{})
	log, _ := e.store.ListAudit(ctx, 20)
	var acts []string
	for _, a := range log {
		acts = append(acts, a.Action)
	}
	if s := strings.Join(acts, " "); !strings.Contains(s, "job.disable") || !strings.Contains(s, "job.enable") {
		t.Errorf("audit log: %s", s)
	}
	if code, _, _ := admin.do("POST", "/jobs/99999/disable", url.Values{}); code != 404 {
		t.Errorf("unknown job: %d", code)
	}
}
