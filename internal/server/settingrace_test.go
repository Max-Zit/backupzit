package server_test

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/server"
)

// LOW: missed schedules are recorded in one transaction with the other
// writers. Writers at the same time keep each other's entries, a miss noted
// while the notifier sends survives its pass, and a failed read changes
// nothing.
func TestScheduleMissesNoLostEntries(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	if _, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "0.62.0"); err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	tid, _ := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "local", URL: filepath.Join(t.TempDir(), "nas")})
	var jobs []int64
	for i := 0; i < 12; i++ {
		id, err := e.store.CreateJob(ctx, server.Job{AgentID: agents[0].ID, TargetID: tid, Name: fmt.Sprintf("job%d", i), Paths: []string{"/srv"}, Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, id)
	}
	misses := func() map[string]map[string]any {
		m := map[string]map[string]any{}
		if err := e.store.GetSetting(ctx, "schedule_misses", &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	var wg sync.WaitGroup
	for _, id := range jobs[:10] {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			if err := e.store.NoteScheduleMiss(ctx, id, time.Now()); err != nil {
				t.Error(err)
			}
		}(id)
	}
	wg.Wait()
	if m := misses(); len(m) != 10 {
		t.Fatalf("%d of 10 misses noted at the same time kept", len(m))
	}

	// A miss of another job noted while the notifier sends its notices.
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	var once sync.Once
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() {
			if err := e.store.NoteScheduleMiss(ctx, jobs[10], time.Now()); err != nil {
				t.Error(err)
			}
		})
	}))
	defer hook.Close()
	if _, loc, _ := admin.do("POST", "/settings/chat", url.Values{"action": {"add"}, "name": {"tickets"}, "kind": {"webhook"}, "url": {hook.URL + "/hook"},
		"on_failure": {"on"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("add webhook: %s", loc)
	}
	n := server.NewNotifier(e.store, slog.New(slog.NewTextHandler(io.Discard, nil)), func() string { return "https://backup.example" })
	n.Pass(ctx, time.Now())
	m := misses()
	if _, ok := m[fmt.Sprint(jobs[10])]; !ok || len(m) != 11 {
		t.Fatalf("the miss noted during the notices was lost: %d entries", len(m))
	}
	if m[fmt.Sprint(jobs[0])]["notified"] != true || m[fmt.Sprint(jobs[10])]["notified"] == true {
		t.Errorf("notified flags: %v / %v", m[fmt.Sprint(jobs[0])], m[fmt.Sprint(jobs[10])])
	}

	// A stored value that cannot be read is not overwritten.
	if _, err := e.pool.Exec(ctx, `UPDATE settings SET value='"not a map"' WHERE key='schedule_misses'`); err != nil {
		t.Fatal(err)
	}
	if err := e.store.NoteScheduleMiss(ctx, jobs[11], time.Now()); err == nil {
		t.Error("a miss was written over a value that could not be read")
	}
	var raw string
	e.pool.QueryRow(ctx, `SELECT value::text FROM settings WHERE key='schedule_misses'`).Scan(&raw)
	if raw != `"not a map"` {
		t.Errorf("the stored value was replaced: %s", raw)
	}
}
