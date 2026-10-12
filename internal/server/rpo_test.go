package server_test

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/server"
	"github.com/max-zit/backupzit/internal/testutil"
)

func TestRPO(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	smtpSrv, err := testutil.StartSMTPServer()
	if err != nil {
		t.Fatal(err)
	}
	defer smtpSrv.Close()
	host, port, _ := net.SplitHostPort(smtpSrv.Addr)
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	if _, loc, _ := admin.do("POST", "/settings/email", url.Values{"action": {"save"}, "enabled": {"on"}, "host": {host}, "port": {port}, "security": {"none"},
		"from": {"backup@example.com"}, "to": {"it@example.com"}, "on_failure": {"on"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("email settings: %s", loc)
	}
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	ag.VSS = false
	agents, _ := e.store.ListAgents(ctx)
	target, _ := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "local", URL: filepath.Join(t.TempDir(), "nas")})
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("a"), 0o644)
	_, loc, _ := admin.do("POST", "/jobs", url.Values{"name": {"docs"}, "kind": {"files"}, "agent_id": {fmt.Sprint(agents[0].ID)},
		"target_id": {fmt.Sprint(target)}, "paths": {src}, "sched_kind": {"manual"}, "keep_last": {"5"}, "rpo_hours": {"24"}})
	if !strings.Contains(loc, "msg=") {
		t.Fatalf("create job: %s", loc)
	}
	jobs, _ := e.store.ListJobs(ctx)
	j := jobs[0]
	if j.Options.RPOHours != 24 {
		t.Fatalf("RPO not stored: %+v", j.Options)
	}
	e.store.QueueBackup(ctx, j.ID, "manual")
	runAgent(t, ag)

	// Within the RPO now.
	if _, _, body := admin.do("GET", "/", nil); !strings.Contains(body, "1 / 1") || !strings.Contains(body, "jobs within RPO") {
		t.Error("dashboard lacks the RPO count")
	}
	if _, _, body := admin.do("GET", fmt.Sprintf("/jobs/%d", j.ID), nil); !strings.Contains(body, "badge success") || !strings.Contains(body, "24 h") {
		t.Error("job page lacks the met RPO")
	}
	tok := apiToken(t, admin, "viewer")
	if _, body := apiCall(t, e, tok, "GET", fmt.Sprintf("/api/v1/jobs/%d", j.ID)); !strings.Contains(body, `"rpo_hours":24`) || !strings.Contains(body, `"rpo_met":true`) || !strings.Contains(body, `"last_good_backup"`) {
		t.Errorf("API job: %s", body)
	}
	if _, body := apiCall(t, e, tok, "GET", "/api/v1/metrics"); !strings.Contains(body, `backupzit_job_rpo_met{job_id="`) || !strings.Contains(body, `rpo_hours="24"} 1`) {
		t.Errorf("metrics lack the RPO")
	}

	// Two days later the RPO is missed: one notification, not repeated.
	n := server.NewNotifier(e.store, slog.New(slog.NewTextHandler(io.Discard, nil)), func() string { return "https://backup.example" })
	before := len(smtpSrv.Messages())
	later := time.Now().Add(48 * time.Hour)
	n.Pass(ctx, later)
	n.Pass(ctx, later.Add(time.Hour))
	msgs := smtpSrv.Messages()[before:]
	if len(msgs) != 1 || !strings.Contains(msgs[0].Data, "RPO missed: docs") || !strings.Contains(msgs[0].Data, "https://backup.example/jobs/") {
		t.Fatalf("RPO notifications: %d %v", len(msgs), msgs)
	}
	// Met again (a new backup), then missed again: a new notification.
	n.Pass(ctx, time.Now())
	n.Pass(ctx, later.Add(2*time.Hour))
	if got := len(smtpSrv.Messages()) - before; got != 2 {
		t.Errorf("notifications after meeting the RPO again: %d", got)
	}
}
