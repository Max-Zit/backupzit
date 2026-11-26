package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/server"
)

// #77: a job with a yearly rule sends no retention to an agent before
// 0.62 (it would apply the other rules alone and remove the yearly
// backups), and the job page says so; a 0.62 agent gets the full policy.
func TestYearlyRetentionNeedsNewAgent(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "0.61.1")
	if err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	call := func(path string, body, out any) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", e.ts.URL+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+cfg.AgentUUID+":"+cfg.Secret)
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if out != nil {
			_ = json.NewDecoder(resp.Body).Decode(out)
		}
	}
	poll := func(version string) *api.Run {
		var pr api.PollResponse
		call(api.PathPoll, api.PollRequest{Hostname: "pc1", OS: "Windows", Arch: "amd64", Version: version}, &pr)
		return pr.Run
	}
	tid, _ := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "local", URL: filepath.Join(t.TempDir(), "nas")})
	jobID, _ := e.store.CreateJob(ctx, server.Job{AgentID: agents[0].ID, TargetID: tid, Name: "docs", Paths: []string{t.TempDir()}, Enabled: true,
		Retention: repo.RetentionPolicy{KeepLast: 1, KeepDaily: 7, KeepYearly: 10}})
	poll("0.61.1")
	if _, err := e.store.QueueBackup(ctx, jobID, "manual"); err != nil {
		t.Fatal(err)
	}
	run := poll("0.61.1")
	if run == nil || run.Retention != nil {
		t.Fatalf("old agent got a policy: %+v", run)
	}
	call(fmt.Sprintf("%s%d/finish", api.PathRunsPrefix, run.ID), api.RunResult{Status: api.StatusFailed, Message: "test"}, nil)
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	if _, _, page := admin.do("GET", fmt.Sprintf("/jobs/%d", jobID), nil); !strings.Contains(page, "Retention waits for an agent update") {
		t.Error("job page lacks the agent update notice")
	}
	poll("0.62.0")
	if _, err := e.store.QueueBackup(ctx, jobID, "manual"); err != nil {
		t.Fatal(err)
	}
	if run = poll("0.62.0"); run == nil || run.Retention == nil || run.Retention.KeepYearly != 10 {
		t.Fatalf("new agent: %+v", run)
	}
	if _, _, page := admin.do("GET", fmt.Sprintf("/jobs/%d", jobID), nil); strings.Contains(page, "Retention waits for an agent update") {
		t.Error("notice stays after the update")
	}
	// The form keeps the yearly count.
	if _, _, page := admin.do("GET", fmt.Sprintf("/jobs/%d/edit", jobID), nil); !strings.Contains(page, `name="keep_yearly" min="0" max="999" value="10"`) {
		t.Error("edit form lacks the yearly value")
	}
}
