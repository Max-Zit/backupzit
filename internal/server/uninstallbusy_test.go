package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
)

// LOW: an agent that runs several jobs at once gets its uninstall only when
// it is idle, as with updates: the uninstaller stops the service and would
// cut off a backup or an in-place restore.
func TestUninstallWaitsForIdleAgent(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "0.62.0")
	if err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	aid := agents[0].ID
	if err := e.store.SetAgentMaxRuns(ctx, aid, 3); err != nil {
		t.Fatal(err)
	}
	var rid int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO runs(agent_id, kind, trigger, repo_url, notified) VALUES ($1,'agent-uninstall','manual','',true) RETURNING id`, aid).Scan(&rid); err != nil {
		t.Fatal(err)
	}
	poll := func(busy bool) *api.Run {
		running := 0
		if busy {
			running = 1
		}
		b, _ := json.Marshal(api.PollRequest{Hostname: "web1", OS: "Debian", Arch: "amd64", Version: "0.62.0", Busy: busy, Parallel: true, Running: running})
		req, _ := http.NewRequest("POST", e.ts.URL+api.PathPoll, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+cfg.AgentUUID+":"+cfg.Secret)
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		var pr api.PollResponse
		json.Unmarshal(out, &pr)
		return pr.Run
	}
	if run := poll(true); run != nil {
		t.Fatalf("a busy agent got run %d (%s)", run.ID, run.Kind)
	}
	if run := poll(false); run == nil || run.ID != rid || run.Kind != api.KindAgentUninstall {
		t.Fatalf("the idle agent did not get its uninstall: %+v", run)
	}
}
