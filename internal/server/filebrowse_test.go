package server_test

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/archiver"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/fsutil"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/server"
	"github.com/max-zit/backupzit/internal/testutil"
)

// TestFileBrowse: a file backup on network storage is browsed in the
// console; one file downloads as is, a folder as a ZIP, and ticked entries
// are restored on an agent.
func TestFileBrowse(t *testing.T) {
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
	if _, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test"); err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	a := agents[0]

	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "docs", "deep"), 0o755)
	os.WriteFile(filepath.Join(src, "docs", "report.txt"), []byte("quarterly report"), 0o644)
	os.WriteFile(filepath.Join(src, "docs", "deep", "note.md"), []byte("a note"), 0o644)
	os.WriteFile(filepath.Join(src, "top.bin"), bytes.Repeat([]byte{7}, 3000), 0o644)

	jobID, err := e.store.CreateJob(ctx, server.Job{AgentID: a.ID, TargetID: targetID, Name: "Docs", Paths: []string{src}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	runID, _ := e.store.QueueBackup(ctx, jobID, "manual")
	run, err := e.store.ClaimRun(ctx, a.ID)
	if err != nil || run == nil || run.ID != runID {
		t.Fatalf("claim: %+v %v", run, err)
	}
	be, err := backend.Open(ctx, run.RepoURL, backend.Options{S3AccessKey: "AK", S3SecretKey: "SK"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be, repo.Password(""))
	if err != nil {
		t.Fatal(err)
	}
	sn, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}})
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	if err := e.store.FinishRun(ctx, a.ID, runID, api.RunResult{Status: api.StatusSuccess, SnapshotID: sn.ID.String()}); err != nil {
		t.Fatal(err)
	}

	comps, _ := fsutil.ParseSnapshotPath(src)
	docs := strings.Join(append(append([]string{}, comps...), "docs"), "/")
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	_, _, page := admin.do("GET", fmt.Sprintf("/runs/%d", runID), nil)
	if !strings.Contains(page, fmt.Sprintf("/runs/%d/files", runID)) {
		t.Error("run page lacks the file browser link")
	}
	_, _, page = admin.do("GET", fmt.Sprintf("/runs/%d/files?path=%s", runID, url.QueryEscape(docs)), nil)
	for _, want := range []string{"report.txt", "deep", `value="` + docs + `/report.txt"`} {
		if !strings.Contains(page, want) {
			t.Errorf("listing lacks %q", want)
		}
	}

	download := func(sel ...string) (*http.Response, []byte) {
		v := url.Values{"path": {docs}, "sel": sel}
		req, _ := http.NewRequest("POST", fmt.Sprintf("%s/runs/%d/files-download", e.ts.URL, runID), strings.NewReader(v.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", e.ts.URL)
		resp, err := admin.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}
	resp, body := download(docs + "/report.txt")
	if string(body) != "quarterly report" || !strings.Contains(resp.Header.Get("Content-Disposition"), "report.txt") {
		t.Fatalf("single file: %q %s", body, resp.Header.Get("Content-Disposition"))
	}
	resp, body = download(docs)
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil || resp.Header.Get("Content-Type") != "application/zip" {
		t.Fatalf("zip: %v %s", err, resp.Header.Get("Content-Type"))
	}
	names := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		names[f.Name] = string(b)
	}
	if names["docs/report.txt"] != "quarterly report" || names["docs/deep/note.md"] != "a note" {
		t.Fatalf("zip content: %v", names)
	}

	// Restore the ticked file into a folder on the agent.
	_, loc, _ := admin.do("POST", fmt.Sprintf("/runs/%d/files-pick-restore", runID), url.Values{"path": {docs},
		"sel": {docs + "/report.txt"}, "agent_id": {fmt.Sprint(a.ID)}, "mode": {"folder"}, "target": {`D:\Restore`}})
	if !strings.Contains(loc, "msg=") {
		t.Fatalf("pick restore: %s", loc)
	}
	runs, _ := e.store.ListRuns(ctx, server.RunFilter{Limit: 1})
	if want := fsutil.OriginalPath(append(append([]string{}, comps...), "docs", "report.txt")); runs[0].Kind != api.KindRestore || len(runs[0].Paths) != 1 || runs[0].Paths[0] != want {
		t.Fatalf("restore run: %s %v (want %s)", runs[0].Kind, runs[0].Paths, want)
	}
}
