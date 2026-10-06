package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/server"
)

// TestDuplicateAgentHold: a running copy of an enrolled machine (here an
// older agent without instance IDs at another address) makes the console
// hold the agent's runs until only one machine is left.
func TestDuplicateAgentHold(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	server.SetClock(e.srv, func() time.Time { return now })
	targetID, err := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "sftp", URL: "sftp://u@nas:22/b", SFTPHostKey: "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", SFTPPassword: "x"})
	if err != nil {
		t.Fatal(err)
	}
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	jobID, err := e.store.CreateJob(ctx, server.Job{AgentID: agents[0].ID, TargetID: targetID, Name: "Docs", Paths: []string{"/docs"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	poll := func(at time.Duration, ip string, busy bool) *api.Run {
		now = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC).Add(at)
		b, _ := json.Marshal(api.PollRequest{Hostname: "web1", OS: "Debian", Arch: "amd64", Version: "0.33.3", Busy: busy, IPs: []string{ip}})
		req, _ := http.NewRequest("POST", e.ts.URL+api.PathPoll, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+cfg.AgentUUID+":"+cfg.Secret)
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var pr api.PollResponse
		json.Unmarshal(out, &pr)
		return pr.Run
	}
	poll(0, "10.0.0.5", true)
	poll(30*time.Second, "10.0.0.5", true)
	poll(40*time.Second, "10.0.0.9", true) // the copy starts
	poll(70*time.Second, "10.0.0.5", true) // the original keeps polling
	if _, err := e.store.QueueBackup(ctx, jobID, "manual"); err != nil {
		t.Fatal(err)
	}
	if run := poll(80*time.Second, "10.0.0.9", false); run != nil {
		t.Fatalf("run %d handed out while two machines use the agent", run.ID)
	}
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	if _, _, body := admin.do("GET", "/", nil); !strings.Contains(body, "two machines use this agent") {
		t.Error("dashboard does not show the duplicate")
	}
	if _, _, body := admin.do("GET", "/agents", nil); !strings.Contains(body, "10.0.0.9") {
		t.Error("agents page does not name the machines")
	}
	// The copy is stopped; after a few minutes the original gets its run.
	poll(150*time.Second, "10.0.0.5", true)
	poll(240*time.Second, "10.0.0.5", true)
	if run := poll(300*time.Second, "10.0.0.5", false); run == nil {
		t.Fatal("run still held after the copy stopped")
	}
}
