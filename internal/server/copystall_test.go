package server_test

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/server"
)

// #75: a copy run that moves no data for half an hour while its agent still
// polls shows a warning and sends one notice; it keeps running.
func TestCopyStallNotice(t *testing.T) {
	x := newRemoveEnv(t, "0.62.0")
	ctx := x.e.ctx
	var mu sync.Mutex
	var events []string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		events = append(events, string(b))
		mu.Unlock()
	}))
	defer hook.Close()
	if _, loc, _ := x.admin.do("POST", "/settings/chat", url.Values{"action": {"add"}, "name": {"tickets"}, "kind": {"webhook"}, "url": {hook.URL + "/hook"},
		"on_failure": {"on"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("add webhook: %s", loc)
	}
	jobs, _ := x.e.store.ListJobs(ctx)
	runID, _ := x.e.store.QueueBackup(ctx, jobs[0].ID, "manual")
	if run, err := x.e.store.ClaimRun(ctx, x.agentID); err != nil || run == nil || run.ID != runID {
		t.Fatalf("claim: %+v %v", run, err)
	}
	if _, err := x.e.pool.Exec(ctx, `UPDATE runs SET kind='copy' WHERE id=$1`, runID); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	server.SetClock(x.e.srv, func() time.Time { return now })
	poll := func(done uint64) {
		p := api.RunProgress{RunID: runID, Done: done, Total: 10 << 30, Stage: "copy", StageDone: 2, StageTotal: 5}
		x.call("POST", api.PathPoll, api.PollRequest{Hostname: "PC-17", OS: "Linux", Arch: "amd64", Version: "0.62.0", Running: 1, Progress: []api.RunProgress{p}}, nil)
	}
	n := server.NewNotifier(x.e.store, slog.New(slog.NewTextHandler(io.Discard, nil)), func() string { return "https://backup.example" })
	stalls := func() int {
		mu.Lock()
		defer mu.Unlock()
		c := 0
		for _, ev := range events {
			if strings.Contains(ev, `"event":"copy.stalled"`) {
				c++
			}
		}
		return c
	}
	page := func() string {
		_, _, b := x.admin.do("GET", fmt.Sprint("/runs/", runID), nil)
		return b
	}
	poll(1 << 30)
	now = now.Add(10 * time.Minute)
	poll(2 << 30)
	n.Pass(ctx, now)
	if c := stalls(); c != 0 || strings.Contains(page(), "no data moved") {
		t.Fatalf("a moving copy: %d notices", c)
	}
	for i := 0; i < 4; i++ { // 40 minutes of polls without data
		now = now.Add(10 * time.Minute)
		poll(2 << 30)
		n.Pass(ctx, now)
	}
	if c := stalls(); c != 1 {
		t.Fatalf("%d notices for a stalled copy, want 1 (%v)", c, events)
	}
	if b := page(); !strings.Contains(b, "no data moved for 40 min") || !strings.Contains(b, "Copying backup 2 of 5") {
		t.Error("the run page shows no stall warning")
	}
	if run, _ := x.e.store.GetRun(ctx, runID); run.Status != "running" {
		t.Errorf("a stalled copy was ended: %s", run.Status)
	}
	// Once the run ended, its entry is forgotten.
	if _, err := x.e.pool.Exec(ctx, `UPDATE runs SET status='success', finished_at=now() WHERE id=$1`, runID); err != nil {
		t.Fatal(err)
	}
	n.Pass(ctx, now)
	var m map[string]any
	if err := x.e.store.GetSetting(ctx, "copy_stalls", &m); err != nil || len(m) != 0 {
		t.Errorf("copy_stalls after the run ended: %v %v", m, err)
	}
}
