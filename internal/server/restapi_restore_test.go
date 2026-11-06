package server_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/server"
)

// apiPost calls the REST API with a JSON body.
func apiPost(t *testing.T, e *env, tok, p, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", e.ts.URL+p, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestAPIRestore restores files of a backup through the REST API.
func TestAPIRestore(t *testing.T) {
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
	tid, _ := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "local", URL: filepath.Join(t.TempDir(), "nas")})
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("alpha"), 0o644)
	os.WriteFile(filepath.Join(src, "b.txt"), []byte("beta"), 0o644)
	jobID, _ := e.store.CreateJob(ctx, server.Job{AgentID: agents[0].ID, TargetID: tid, Name: "docs", Paths: []string{src}, Enabled: true})
	backup, _ := e.store.QueueBackup(ctx, jobID, "manual")
	runAgent(t, ag)
	if b, _ := e.store.GetRun(ctx, backup); b.SnapshotID == "" {
		t.Fatalf("backup did not finish: %s %s", b.Status, b.Message)
	}

	view := apiToken(t, admin, "viewer")
	rest := apiToken(t, admin, "restore")
	p := fmt.Sprintf("/api/v1/runs/%d/restore", backup)
	dst := t.TempDir()
	if code, _ := apiPost(t, e, view, p, `{"target":"x"}`); code != 403 {
		t.Errorf("viewer restored: %d", code)
	}
	for body, want := range map[string]int{`{}`: 400, `{"target":"x","original":true}`: 400, `{"target":`: 400, ``: 400} {
		if code, out := apiPost(t, e, rest, p, body); code != want {
			t.Errorf("body %q: %d %s", body, code, out)
		}
	}
	if code, _ := apiPost(t, e, rest, "/api/v1/runs/999999/restore", `{"target":"x"}`); code != 404 {
		t.Errorf("unknown run: %d", code)
	}
	if code, _ := apiPost(t, e, rest, fmt.Sprintf("/api/v1/runs/%d/restore", backup), `{"target":"x","agent_id":999999}`); code != 409 {
		t.Errorf("unknown agent: %d", code)
	}

	// Only b.txt, into a folder, verified.
	code, body := apiPost(t, e, rest, p, fmt.Sprintf(`{"target":%q,"paths":[%q],"verify":true}`, dst, filepath.Join(src, "b.txt")))
	var res struct {
		RunID int64 `json:"run_id"`
	}
	json.Unmarshal([]byte(body), &res)
	if code != 202 || res.RunID == 0 {
		t.Fatalf("restore: %d %s", code, body)
	}
	runAgent(t, ag)
	r, _ := e.store.GetRun(ctx, res.RunID)
	if r.Kind != "restore" || r.Status != "success" || r.Trigger != "manual" {
		t.Fatalf("restore run: %s %s %s %s", r.Kind, r.Status, r.Trigger, r.Message)
	}
	var found []string
	filepath.Walk(dst, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			found = append(found, filepath.Base(p))
		}
		return nil
	})
	if strings.Join(found, ",") != "b.txt" {
		t.Errorf("restored files: %v", found)
	}
	if code, body := apiCall(t, e, view, "GET", fmt.Sprintf("/api/v1/runs/%d", res.RunID)); code != 200 || !strings.Contains(body, `"kind":"restore"`) {
		t.Errorf("restore run over the API: %d %s", code, body)
	}
	log, _ := e.store.ListAudit(ctx, 20)
	var acts []string
	for _, a := range log {
		acts = append(acts, a.Action)
	}
	if !strings.Contains(strings.Join(acts, " "), "api.restore") {
		t.Errorf("restore not in the audit log: %v", acts)
	}
}
