package server_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/server"
)

func TestJobEditAndCommands(t *testing.T) {
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
	agents, _ := e.store.ListAgents(ctx)
	target, _ := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "local", URL: filepath.Join(t.TempDir(), "nas")})
	src := filepath.Join(t.TempDir(), "data")
	os.MkdirAll(src, 0o755)
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("a"), 0o644)
	statusFile := filepath.Join(t.TempDir(), "status.txt")

	pre := "echo dump > \"" + filepath.Join(src, "dump.sql") + "\""
	post := "echo $BACKUPZIT_STATUS > \"" + statusFile + "\""
	if runtime.GOOS == "windows" {
		post = "echo %BACKUPZIT_STATUS%> \"" + statusFile + "\""
	}
	_, loc, _ := admin.do("POST", "/jobs", url.Values{"name": {"docs"}, "kind": {"files"}, "agent_id": {fmt.Sprint(agents[0].ID)},
		"target_id": {fmt.Sprint(target)}, "paths": {src}, "sched_kind": {"manual"}, "keep_last": {"5"},
		"limit_mbps": {"50"}, "limit_window": {"on"}, "limit_from": {"8"}, "limit_to": {"18"},
		"pre_command": {pre}, "post_command": {post}, "command_minutes": {"5"}})
	if !strings.Contains(loc, "msg=") {
		t.Fatalf("create job: %s", loc)
	}
	jobs, _ := e.store.ListJobs(ctx)
	j := jobs[0]
	if o := j.Options; o.LimitMBps != 50 || o.LimitFrom != 8 || o.LimitTo != 18 || o.PreCommand != pre || o.PostCommand != post || o.CommandMinutes != 5 {
		t.Fatalf("options not stored: %+v", o)
	}
	if _, _, body := admin.do("GET", fmt.Sprintf("/jobs/%d", j.ID), nil); !strings.Contains(body, "50 MB/s from 08:00 to 18:00") || !strings.Contains(body, "Edit job") || !strings.Contains(body, "dump.sql") {
		t.Error("job page lacks options or the edit form")
	}

	// The commands run around the backup; the file the command before
	// writes is in the backup.
	runID, _ := e.store.QueueBackup(ctx, j.ID, "manual")
	runAgent(t, ag)
	r, _ := e.store.GetRun(ctx, runID)
	if r.Status != api.StatusSuccess || !strings.Contains(string(r.Stats), `"files": 2`) {
		t.Fatalf("backup with commands: %s %s %s %v", r.Status, r.Message, r.Stats, r.Errors)
	}
	if b, _ := os.ReadFile(statusFile); strings.TrimSpace(string(b)) != "success" {
		t.Errorf("command after the backup wrote %q", b)
	}

	// Compression: the run shows the ratio; with "off" new data is stored
	// as it is, with "max" it is compressed (checked with compressible text).
	if _, _, body := admin.do("GET", fmt.Sprintf("/runs/%d", runID), nil); !strings.Contains(body, "Compression") {
		t.Error("run page lacks the compression ratio")
	}
	for i, level := range []string{"off", "max"} {
		text := strings.Repeat(fmt.Sprintf("level %s: the same words again and again %d\n", level, i), 6000)
		os.WriteFile(filepath.Join(src, "log-"+level+".txt"), []byte(text), 0o644)
		v := url.Values{"name": {"docs"}, "paths": {src}, "sched_kind": {"manual"}, "keep_last": {"5"}, "compression": {level}, "pre_command": {pre}, "post_command": {post}}
		if _, loc, _ := admin.do("POST", fmt.Sprintf("/jobs/%d/edit", j.ID), v); !strings.Contains(loc, "msg=") {
			t.Fatalf("edit compression: %s", loc)
		}
		if j2, _ := e.store.GetJob(ctx, j.ID); j2.Options.Compression != level {
			t.Fatalf("compression not stored: %q", j2.Options.Compression)
		}
		id, _ := e.store.QueueBackup(ctx, j.ID, "manual")
		runAgent(t, ag)
		r, _ := e.store.GetRun(ctx, id)
		var st struct {
			Added  uint64 `json:"bytes_added"`
			Stored uint64 `json:"bytes_stored"`
		}
		json.Unmarshal(r.Stats, &st)
		if r.Status != api.StatusSuccess || st.Added < uint64(len(text)) {
			t.Fatalf("%s: %s %s", level, r.Status, r.Stats)
		}
		if level == "off" && st.Stored < st.Added {
			t.Errorf("off: %d stored for %d new bytes", st.Stored, st.Added)
		}
		if level == "max" && st.Stored*10 > st.Added {
			t.Errorf("max: %d stored for %d new bytes", st.Stored, st.Added)
		}
	}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/jobs/%d/edit", j.ID), url.Values{"name": {"docs"}, "paths": {src}, "sched_kind": {"manual"}, "keep_last": {"5"}, "compression": {"best"}}); !strings.Contains(loc, "err=") {
		t.Errorf("unknown compression level accepted: %s", loc)
	}

	// A failing command before the backup stops it.
	failing := url.Values{"name": {"docs"}, "paths": {src}, "sched_kind": {"manual"}, "keep_last": {"5"}, "pre_command": {"exit 4"}, "post_command": {post}}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/jobs/%d/edit", j.ID), failing); !strings.Contains(loc, "msg=") {
		t.Fatalf("edit: %s", loc)
	}
	runID, _ = e.store.QueueBackup(ctx, j.ID, "manual")
	runAgent(t, ag)
	if r, _ := e.store.GetRun(ctx, runID); r.Status != api.StatusFailed || !strings.Contains(r.Message, "exit code 4") {
		t.Errorf("failing command before: %s %s", r.Status, r.Message)
	}
	if b, _ := os.ReadFile(statusFile); strings.TrimSpace(string(b)) != "failed" {
		t.Errorf("command after a failed backup wrote %q", b)
	}

	// Editing: name, paths, schedule, retention; the speed limit is gone.
	other := filepath.Join(t.TempDir(), "other")
	edit := url.Values{"name": {"docs nightly"}, "paths": {src + "\n" + other}, "excludes": {"*.tmp"}, "keep_daily": {"14"},
		"sched_kind": {"daily"}, "sched_time": {"21:30", "", ""}, "sched_days": {"1", "2", "3", "4", "5"}}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/jobs/%d/edit", j.ID), edit); !strings.Contains(loc, "msg=") {
		t.Fatalf("edit: %s", loc)
	}
	j, _ = e.store.GetJob(ctx, j.ID)
	if j.Name != "docs nightly" || len(j.Paths) != 2 || j.Excludes[0] != "*.tmp" || j.Retention.KeepDaily != 14 || j.Options.LimitMBps != 0 ||
		j.Options.HasCommands() || !strings.Contains(j.Schedule, "21:30") {
		t.Fatalf("job after edit: %+v", j)
	}
	if _, _, body := admin.do("GET", fmt.Sprintf("/jobs/%d", j.ID), nil); !strings.Contains(body, `value="21:30"`) || !strings.Contains(body, `value="14"`) {
		t.Error("edit form not filled with the job's settings")
	}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/jobs/%d/edit", j.ID), url.Values{"name": {"x"}, "paths": {""}, "sched_kind": {"manual"}}); !strings.Contains(loc, "err=") {
		t.Error("job without paths accepted")
	}

	// Operators manage jobs but cannot set commands that run as root/SYSTEM.
	admin.do("POST", fmt.Sprintf("/jobs/%d/edit", j.ID), url.Values{"name": {"docs nightly"}, "paths": {src}, "sched_kind": {"manual"}, "pre_command": {"echo admin"}})
	if _, err := e.store.CreateUser(ctx, server.User{Username: "ana", DisplayName: "Ana", Role: "operator"}, "ana-password-1"); err != nil {
		t.Fatal(err)
	}
	op := newClient(t, e)
	op.login("ana", "ana-password-1")
	if _, _, body := op.do("GET", fmt.Sprintf("/jobs/%d", j.ID), nil); strings.Contains(body, "echo admin") || !strings.Contains(body, "Only administrators can see and change them") {
		t.Error("operator sees the commands")
	}
	op.do("POST", fmt.Sprintf("/jobs/%d/edit", j.ID), url.Values{"name": {"renamed"}, "paths": {src}, "sched_kind": {"manual"}, "pre_command": {"rm -rf /"}})
	j, _ = e.store.GetJob(ctx, j.ID)
	if j.Name != "renamed" || j.Options.PreCommand != "echo admin" {
		t.Errorf("operator edit: name %q, command %q", j.Name, j.Options.PreCommand)
	}
}
