package server_test

import (
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
	"github.com/max-zit/backupzit/internal/testutil"
)

// TestSearchBackups finds a file by part of its name or a pattern across
// all backups of all jobs, with its versions.
func TestSearchBackups(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	s3, err := testutil.StartS3Server("backups")
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	targetID, err := e.store.CreateTarget(ctx, server.Target{Name: "s3", Kind: "s3", URL: "s3://" + s3.Host + "/backups/files?tls=false", S3AccessKey: "AK", S3SecretKey: "SK"})
	if err != nil {
		t.Fatal(err)
	}
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	ag.VSS = false
	agents, _ := e.store.ListAgents(ctx)
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "Sales", "2025"), 0o755)
	offer := filepath.Join(src, "Sales", "2025", "Ponuda_2025.xlsx")
	os.WriteFile(offer, []byte("v1"), 0o644)
	os.WriteFile(filepath.Join(src, "notes.txt"), []byte("x"), 0o644)
	jobID, _ := e.store.CreateJob(ctx, server.Job{AgentID: agents[0].ID, TargetID: targetID, Name: "Office", Paths: []string{src}, Enabled: true})
	backup := func() {
		e.store.QueueBackup(ctx, jobID, "manual")
		runAgent(t, ag)
	}
	backup()
	backup() // unchanged
	os.WriteFile(offer, []byte("version two"), 0o644)
	os.Chtimes(offer, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	backup()

	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	_, _, body := admin.do("GET", "/search?"+url.Values{"q": {"ponuda_2025"}}.Encode(), nil)
	if !strings.Contains(body, "<b>Ponuda_2025.xlsx</b>") || !strings.Contains(body, "Searched 3 backups") || !strings.Contains(body, "2 versions") {
		t.Fatalf("search page lacks the file or its versions:\n%s", body[strings.Index(body, "<h1>"):])
	}
	if !strings.Contains(body, "unchanged in 2 backups") || !strings.Contains(body, "/files?path=") {
		t.Error("versions do not group unchanged backups or lack the link")
	}
	if _, _, body := admin.do("GET", "/search?q=*.xlsx", nil); !strings.Contains(body, "Ponuda_2025.xlsx") || strings.Contains(body, "<b>notes.txt</b>") {
		t.Error("pattern search")
	}
	if _, _, body := admin.do("GET", "/search?q=nothing-like-this", nil); !strings.Contains(body, "Nothing found") {
		t.Error("empty result")
	}
	if _, _, body := admin.do("GET", "/search?q=a", nil); !strings.Contains(body, "at least two characters") {
		t.Error("too short query accepted")
	}
	tok := apiToken(t, admin, "restore")
	if code, body := apiCall(t, e, tok, "GET", "/api/v1/search?q=ponuda"); code != 200 || !strings.Contains(body, `"name":"Ponuda_2025.xlsx"`) || !strings.Contains(body, `"backups_searched":3`) {
		t.Errorf("API search: %d %s", code, body)
	}
}
