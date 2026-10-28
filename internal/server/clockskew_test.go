package server_test

import (
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/api"
)

// An agent whose clock is off is shown on the dashboard and the Agents
// page; a correct clock and older agents (no time sent) are not.
func TestAgentClockSkew(t *testing.T) {
	x := newRemoveEnv(t, "0.44.0")
	poll := func(at time.Time) {
		req := api.PollRequest{Hostname: "PC-17", OS: "Windows 11", Arch: "amd64", Version: "0.44.0"}
		if !at.IsZero() {
			req.Time = at.UnixMilli()
		}
		x.call("POST", api.PathPoll, req, nil)
	}
	page := func(p string) string {
		code, _, body := x.admin.do("GET", p, nil)
		if code != 200 {
			t.Fatalf("%s: %d", p, code)
		}
		return body
	}
	poll(time.Time{})
	poll(time.Now().Add(20 * time.Second))
	if strings.Contains(page("/"), "clock of this computer") || strings.Contains(page("/agents"), "clock off") {
		t.Fatal("warning for a correct clock")
	}
	poll(time.Now().Add(-65 * time.Minute))
	if b := page("/"); !strings.Contains(b, "is 1 h behind the console") {
		t.Fatal("dashboard: no clock warning")
	}
	if b := page("/agents"); !strings.Contains(b, "clock off −1 h</span>") {
		t.Fatal("agents page: no clock badge")
	}
	poll(time.Now().Add(5 * time.Minute))
	if b := page("/"); !strings.Contains(b, "is 4 min ahead of the console") && !strings.Contains(b, "is 5 min ahead of the console") {
		t.Fatal("dashboard: no warning for a clock ahead")
	}
	poll(time.Now())
	if strings.Contains(page("/"), "clock of this computer") {
		t.Fatal("warning stays after the clock was corrected")
	}
}

// Pages are isolated from other sites (no framing, no cross-origin
// resources or windows).
func TestIsolationHeaders(t *testing.T) {
	x := newRemoveEnv(t, "0.44.0")
	resp, err := x.admin.c.Get(x.admin.url + "/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for h, want := range map[string]string{
		"Cross-Origin-Embedder-Policy": "require-corp",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
	} {
		if got := resp.Header.Get(h); got != want {
			t.Errorf("%s: %q", h, got)
		}
	}
}
