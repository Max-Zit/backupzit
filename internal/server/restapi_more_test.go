package server_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/server"
)

func TestAPIMore(t *testing.T) {
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
	ag.LogPath = filepath.Join(t.TempDir(), "agent.log")
	os.WriteFile(ag.LogPath, []byte("time=x level=INFO msg=\"hello from the agent\"\n"), 0o600)
	agents, _ := e.store.ListAgents(ctx)
	tid, _ := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "local", URL: filepath.Join(t.TempDir(), "nas")})
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("a"), 0o644)
	jobID, _ := e.store.CreateJob(ctx, server.Job{AgentID: agents[0].ID, TargetID: tid, Name: "docs \"main\"", Paths: []string{src}, Enabled: true})
	e.store.QueueBackup(ctx, jobID, "manual")
	runAgent(t, ag)

	view := apiToken(t, admin, "viewer")
	op := apiToken(t, admin, "operator")
	if code, body := apiCall(t, e, view, "GET", fmt.Sprintf("/api/v1/agents/%d", agents[0].ID)); code != 200 || !strings.Contains(body, `"online":true`) {
		t.Errorf("agent: %d %s", code, body)
	}
	if code, _ := apiCall(t, e, view, "POST", fmt.Sprintf("/api/v1/jobs/%d/disable", jobID)); code != 403 {
		t.Errorf("viewer disabled a job: %d", code)
	}
	if code, body := apiCall(t, e, op, "POST", fmt.Sprintf("/api/v1/jobs/%d/disable", jobID)); code != 202 || !strings.Contains(body, `"enabled":false`) {
		t.Errorf("disable: %d %s", code, body)
	}
	if j, _ := e.store.GetJob(ctx, jobID); j.Enabled {
		t.Error("job still enabled")
	}
	apiCall(t, e, op, "POST", fmt.Sprintf("/api/v1/jobs/%d/enable", jobID))
	if j, _ := e.store.GetJob(ctx, jobID); !j.Enabled {
		t.Error("job not enabled again")
	}
	if code, body := apiCall(t, e, view, "GET", "/api/v1/storage"); code != 200 || !strings.Contains(body, `"name":"nas"`) {
		t.Errorf("storage: %d %s", code, body)
	}

	// Metrics in the Prometheus text format.
	code, body := apiCall(t, e, view, "GET", "/api/v1/metrics")
	if code != 200 || !strings.Contains(body, `backupzit_agents{state="online"} 1`) ||
		!strings.Contains(body, fmt.Sprintf(`backupzit_job_last_status{job_id="%d",job="docs \"main\"",kind="files",agent=`, jobID)) ||
		!strings.Contains(body, "backupzit_job_last_success_timestamp_seconds{") || strings.Contains(body, "null") {
		t.Errorf("metrics: %d\n%s", code, body)
	}
	if code, _ := apiCall(t, e, "bzt_wrong", "GET", "/api/v1/metrics"); code != 401 {
		t.Errorf("metrics without a valid token: %d", code)
	}

	// The agent's log over the API.
	code, body = apiCall(t, e, op, "POST", fmt.Sprintf("/api/v1/agents/%d/log", agents[0].ID))
	var res struct {
		RunID int64 `json:"run_id"`
	}
	json.Unmarshal([]byte(body), &res)
	if code != 202 || res.RunID == 0 {
		t.Fatalf("request log: %d %s", code, body)
	}
	if code, _ := apiCall(t, e, op, "GET", fmt.Sprintf("/api/v1/runs/%d/log", res.RunID)); code != 404 {
		t.Errorf("log before the agent sent it: %d", code)
	}
	runAgent(t, ag)
	if code, body := apiCall(t, e, op, "GET", fmt.Sprintf("/api/v1/runs/%d/log", res.RunID)); code != 200 || !strings.Contains(body, "hello from the agent") {
		t.Errorf("log: %d %.300s", code, body)
	}
}
