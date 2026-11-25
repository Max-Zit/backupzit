package server_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
)

// A Mac agent reports its Full Disk Access with its polls (0.61, #62): the
// Agents page marks the Mac while it is missing, and the change from granted
// to denied is written to the audit log once, not with every poll.
func TestMacFullDiskAccess(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "0.61.0")
	if err != nil {
		t.Fatal(err)
	}
	poll := func(fda string) {
		b, _ := json.Marshal(api.PollRequest{Hostname: "studio-mac", OS: "macOS 26.1", Arch: "arm64", Version: "0.61.0", FullDiskAccess: fda})
		req, _ := http.NewRequest("POST", e.ts.URL+api.PathPoll, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+cfg.AgentUUID+":"+cfg.Secret)
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("poll: %d", resp.StatusCode)
		}
	}
	notices := func() int {
		list, err := e.store.ListAudit(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, a := range list {
			if a.Action == "agent.full_disk_access" {
				n++
			}
		}
		return n
	}
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	marked := func() bool {
		code, _, body := admin.do("GET", "/agents", nil)
		if code != 200 {
			t.Fatalf("agents page: %d", code)
		}
		return strings.Contains(body, "Full Disk Access missing")
	}

	poll("") // an older agent or another system: nothing is stored
	poll("unknown")
	if marked() || notices() != 0 {
		t.Fatal("marked without a report of denied")
	}
	poll("denied") // first report: marked, but no change from granted
	if !marked() || notices() != 0 {
		t.Fatalf("first denied: marked %v, notices %d", marked(), notices())
	}
	poll("granted")
	if marked() {
		t.Fatal("still marked after it was granted")
	}
	poll("denied")
	poll("denied")
	poll("bogus") // ignored, the state stays
	if !marked() || notices() != 1 {
		t.Fatalf("lost after granted: marked %v, notices %d", marked(), notices())
	}
	agents, _ := e.store.ListAgents(ctx)
	if len(agents) != 1 || agents[0].FullDiskAccess != "denied" {
		t.Fatalf("stored: %+v", agents)
	}
	if code, _, body := admin.do("GET", "/agents/"+itoa(agents[0].ID), nil); code != 200 || !strings.Contains(body, "has no Full Disk Access") {
		t.Errorf("agent page: %d", code)
	}
}
