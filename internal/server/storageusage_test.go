package server_test

import (
	"html"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/server"
)

// The agent reports the repository size and the disk's capacity after a
// backup; the Storage page charts the history and forecasts when the
// storage is full, and the dashboard warns in time.
func TestStorageUsage(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	targetID, _ := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "local", URL: t.TempDir()})
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	ag.VSS = false
	agents, _ := e.store.ListAgents(ctx)
	src := t.TempDir()
	b := make([]byte, 3<<20)
	rand.New(rand.NewSource(1)).Read(b)
	os.WriteFile(filepath.Join(src, "f.bin"), b, 0o644)
	jobID, err := e.store.CreateJob(ctx, server.Job{AgentID: agents[0].ID, TargetID: targetID, Name: "docs", Paths: []string{src}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	runID, _ := e.store.QueueBackup(ctx, jobID, "manual")
	runAgent(t, ag)
	if r, _ := e.store.GetRun(ctx, runID); r.Status != api.StatusSuccess {
		t.Fatalf("backup: %s %s", r.Status, r.Message)
	}
	var repoBytes, total, free int64
	if err := e.pool.QueryRow(ctx, `SELECT repo_bytes, storage_total, storage_free FROM runs WHERE id=$1`, runID).Scan(&repoBytes, &total, &free); err != nil {
		t.Fatal(err)
	}
	if repoBytes < 3<<20 || repoBytes > 4<<20 || total <= 0 || free <= 0 || free > total {
		t.Fatalf("reported repo %d bytes, disk %d free of %d", repoBytes, free, total)
	}

	// Thirty days of another repository growing by 1 GiB a day on a 40 GiB
	// disk: 30 GiB used now, full in about 10 days.
	const gib = int64(1) << 30
	e.pool.Exec(ctx, `UPDATE runs SET finished_at=now()-interval '40 days' WHERE id=$1`, runID)
	for d := 29; d >= 0; d-- {
		used := int64(30-d) * gib
		if _, err := e.pool.Exec(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, target_id, status, started_at, finished_at, repo_bytes, storage_total, storage_free)
			SELECT agent_id, job_id, kind, trigger, 'synthetic', target_id, status, now()-make_interval(days => $2)-interval '1 minute', now()-make_interval(days => $2), $3, $4, $5 FROM runs WHERE id=$1`,
			runID, d, used, 40*gib, 40*gib-used); err != nil {
			t.Fatal(err)
		}
	}
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	_, _, body := admin.do("GET", "/targets", nil)
	body = html.UnescapeString(body)
	for _, want := range []string{"Storage usage", `class="usage-chart"`, "used by 2 repositories", "Storage 75 % full", "+1.0 GiB", "full in about 10 days"} {
		if !strings.Contains(body, want) {
			t.Errorf("storage page lacks %q", want)
		}
	}
	_, _, body = admin.do("GET", "/", nil)
	if !strings.Contains(html.UnescapeString(body), "storage 75 % full, full in about 10 days") {
		t.Error("dashboard does not warn about the filling storage")
	}
}
