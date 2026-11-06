package server_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/server"
)

// A Hyper-V job requiring application-consistent checkpoints: the option is
// shown and kept, older agents (which would quietly fall back to
// crash-consistent checkpoints) get a clear failure, and a failed VM backup
// that still holds VMs is offered for restore.
func TestRequireAppConsistent(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	target, _ := e.store.CreateTarget(ctx, server.Target{Name: "local", Kind: "local", URL: t.TempDir()})
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	inv := `{"platform":"hyperv","node":"HV1","guests":[{"vmid":1000000001,"id":"guid-dc","name":"dc","type":"vm","node":"HV1","status":"running"}],"storage":["D:/VMs"]}`
	if err := e.store.TouchAgent(ctx, agents[0].ID, api.PollRequest{Hostname: "HV1", OS: "Windows Server 2022", Hypervisor: json.RawMessage(inv)}, ""); err != nil {
		t.Fatal(err)
	}
	jobID, err := e.store.CreateJob(ctx, server.Job{Kind: server.JobVM, AgentID: agents[0].ID, TargetID: target, Name: "dc and sql",
		Paths: []string{"1000000001"}, Enabled: true, Options: server.JobOptions{AppConsistent: true}})
	if err != nil {
		t.Fatal(err)
	}

	c := newClient(t, e)
	c.login("admin", "admin-pass-123")
	_, _, page := c.do("GET", fmt.Sprintf("/jobs/%d/edit", jobID), nil)
	if !strings.Contains(page, `name="app_consistent" checked`) {
		t.Error("edit form does not show the option as on")
	}

	// An agent older than 0.48.0 is refused with a reason.
	old := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "0.47.3")
	rid, _ := e.store.QueueBackup(ctx, jobID, "manual")
	runAgent(t, old)
	if r, _ := e.store.GetRun(ctx, rid); r.Status != api.StatusFailed || !strings.Contains(r.Message, "cannot require application-consistent checkpoints yet") {
		t.Errorf("old agent: %s %q", r.Status, r.Message)
	}

	// A failed VM backup that kept the other VMs is a restore point.
	rid2, _ := e.store.QueueBackup(ctx, jobID, "manual")
	e.store.ClaimRun(ctx, agents[0].ID)
	e.store.FinishRun(ctx, agents[0].ID, rid2, api.RunResult{Status: api.StatusFailed, SnapshotID: "abcdef0123456789",
		Message: "no application-consistent checkpoint for 1 of 2 VMs; they were not backed up; the other 1 VMs were backed up"})
	points, _ := e.store.ListRestorePoints(ctx)
	found := false
	for _, p := range points {
		found = found || p.ID == rid2
	}
	if !found {
		t.Error("failed VM backup with VMs is not a restore point")
	}
}
