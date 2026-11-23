package server_test

import (
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/server"
)

func TestAgentLog(t *testing.T) {
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
	ag.LogPath = filepath.Join(t.TempDir(), "agent.log")
	var log strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&log, "time=x level=INFO msg=\"line %d\"\n", i)
	}
	os.WriteFile(ag.LogPath, []byte(log.String()), 0o600)
	agents, _ := e.store.ListAgents(ctx)

	if _, _, body := admin.do("GET", "/agents", nil); !strings.Contains(body, fmt.Sprintf(`action="/agents/%d/log"`, agents[0].ID)) {
		t.Error("agents page lacks Get log")
	}
	_, loc, _ := admin.do("POST", fmt.Sprintf("/agents/%d/log", agents[0].ID), nil)
	if !strings.Contains(loc, "/runs/") || !strings.Contains(loc, "msg=") {
		t.Fatalf("request log: %s", loc)
	}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/agents/%d/log", agents[0].ID), nil); !strings.Contains(loc, "err=") {
		t.Error("second request while one is pending accepted")
	}
	runAgent(t, ag)
	runs, _ := e.store.ListRuns(ctx, server.RunFilter{AgentID: agents[0].ID, Limit: 5})
	r := runs[0]
	if r.Kind != "agent-log" || r.Status != "success" {
		t.Fatalf("run %s %s %q", r.Kind, r.Status, r.Message)
	}
	_, _, body := admin.do("GET", fmt.Sprintf("/runs/%d", r.ID), nil)
	if !strings.Contains(body, "Log of the agent") || !strings.Contains(body, "line 299") || strings.Contains(body, `"line 5"`) {
		t.Error("run page lacks the end of the log")
	}
	code, _, file := admin.do("GET", fmt.Sprintf("/runs/%d/log", r.ID), nil)
	if code != 200 || !strings.Contains(file, "BackupZit agent test") || !strings.Contains(file, `"line 0"`) || !strings.Contains(file, `"line 299"`) {
		t.Errorf("download: %d %.200s", code, file)
	}

	// L-1 (0.55): roles that may not download the log do not see its tail.
	if _, loc, _ := admin.do("POST", "/users", url.Values{"reauth_password": {"admin-pass-123"}, "username": {"vera"}, "role": {"viewer"},
		"password": {"correct-horse-9"}, "password2": {"correct-horse-9"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("create viewer: %s", loc)
	}
	viewer := newClient(t, e)
	viewer.login("vera", "correct-horse-9")
	if code, _, body := viewer.do("GET", fmt.Sprintf("/runs/%d", r.ID), nil); code != 200 || strings.Contains(body, "line 299") {
		t.Errorf("viewer sees the agent log on the run page (%d)", code)
	}
}
