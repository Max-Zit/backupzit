package server_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/server"
)

func TestChatNotifications(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")

	var mu sync.Mutex
	got := map[string][]http.Header{}
	bodies := map[string][]string{}
	hook := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got[r.URL.Path] = append(got[r.URL.Path], r.Header.Clone())
		bodies[r.URL.Path] = append(bodies[r.URL.Path], string(b))
		mu.Unlock()
		if r.URL.Path == "/broken" {
			http.Error(w, "nope", http.StatusForbidden)
		}
	}))
	defer hook.Close()
	old := http.DefaultTransport
	http.DefaultTransport = hook.Client().Transport // trust the test certificate
	defer func() { http.DefaultTransport = old }()

	add := func(v url.Values) string {
		v.Set("action", "add")
		_, loc, _ := admin.do("POST", "/settings/chat", v)
		return loc
	}
	if loc := add(url.Values{"name": {"x"}, "kind": {"slack"}, "url": {"http://insecure.example/hook"}}); !strings.Contains(loc, "err=") {
		t.Error("plain http Slack URL accepted")
	}
	add(url.Values{"name": {"#backups"}, "kind": {"slack"}, "url": {hook.URL + "/slack"}, "on_failure": {"on"}})
	add(url.Values{"name": {"IT team"}, "kind": {"teams"}, "url": {hook.URL + "/teams"}, "on_failure": {"on"}, "on_success": {"on"}})
	add(url.Values{"name": {"tickets"}, "kind": {"webhook"}, "url": {hook.URL + "/hook"}, "secret": {"s3cret"}, "on_failure": {"on"}, "on_warning": {"on"}})
	add(url.Values{"name": {"broken"}, "kind": {"webhook"}, "url": {hook.URL + "/broken"}, "on_failure": {"on"}})
	if _, _, body := admin.do("GET", "/settings/chat", nil); !strings.Contains(body, "#backups") || !strings.Contains(body, "Microsoft Teams") || !strings.Contains(body, "(signed)") || strings.Contains(body, hook.URL) {
		t.Error("settings page lacks the channels or shows the secret URLs")
	}

	// A failed backup goes to every channel that wants failures.
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	_ = cfg
	agents, _ := e.store.ListAgents(ctx)
	target, _ := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "local", URL: t.TempDir()})
	jobID, _ := e.store.CreateJob(ctx, server.Job{AgentID: agents[0].ID, TargetID: target, Name: "docs", Paths: []string{t.TempDir()}})
	runID, _ := e.store.QueueBackup(ctx, jobID, "manual")
	e.store.ClaimRun(ctx, agents[0].ID)
	e.store.FinishRun(ctx, agents[0].ID, runID, api.RunResult{Status: api.StatusFailed, Message: "disk <full>"})
	n := server.NewNotifier(e.store, slog.New(slog.NewTextHandler(io.Discard, nil)), func() string { return "https://backup.example" })
	n.Pass(ctx, time.Date(2026, 10, 2, 10, 0, 0, 0, time.Local))
	n.Pass(ctx, time.Date(2026, 10, 2, 10, 1, 0, 0, time.Local)) // not sent twice

	mu.Lock()
	defer mu.Unlock()
	var slack struct{ Text string }
	if len(bodies["/slack"]) == 1 {
		json.Unmarshal([]byte(bodies["/slack"][0]), &slack)
	}
	if !strings.Contains(slack.Text, "FAILED") || !strings.Contains(slack.Text, "disk &lt;full&gt;") || !strings.Contains(slack.Text, "<https://backup.example/runs/") {
		t.Errorf("slack: %v", bodies["/slack"])
	}
	if len(bodies["/teams"]) != 1 || !strings.Contains(bodies["/teams"][0], "AdaptiveCard") || !strings.Contains(bodies["/teams"][0], "Open in BackupZit") {
		t.Errorf("teams: %v", bodies["/teams"])
	}
	if len(bodies["/hook"]) != 1 {
		t.Fatalf("webhook: %v", bodies["/hook"])
	}
	var ev map[string]any
	json.Unmarshal([]byte(bodies["/hook"][0]), &ev)
	if ev["event"] != "run.finished" || ev["status"] != "failed" || ev["job"] != "docs" || ev["kind"] != "backup" {
		t.Errorf("webhook event: %v", ev)
	}
	m := hmac.New(sha256.New, []byte("s3cret"))
	m.Write([]byte(bodies["/hook"][0]))
	if sig := got["/hook"][0].Get("X-BackupZit-Signature"); sig != "sha256="+hex.EncodeToString(m.Sum(nil)) {
		t.Errorf("signature %q", sig)
	}
	if len(bodies["/broken"]) != 1 {
		t.Errorf("broken channel called %d times", len(bodies["/broken"]))
	}
}
