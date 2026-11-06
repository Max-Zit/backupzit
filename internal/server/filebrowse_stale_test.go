package server_test

import (
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

// A backup made after the console opened (and cached) the repository is
// still browsable and downloadable; a file that cannot be read gives a
// message, never an empty archive (found by the 0.48.0 regression).
func TestFileBrowseNewerBackup(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	s3, err := testutil.StartS3Server("backups")
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	targetID, _ := e.store.CreateTarget(ctx, server.Target{Name: "s3", Kind: "s3", URL: "s3://" + s3.Host + "/backups/files?tls=false", S3AccessKey: "AK", S3SecretKey: "SK"})
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	if _, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test"); err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	a := agents[0]
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "first.txt"), []byte("first"), 0o644)
	jobID, _ := e.store.CreateJob(ctx, server.Job{AgentID: a.ID, TargetID: targetID, Name: "Docs", Paths: []string{src}, Enabled: true})

	backup := func(init bool) int64 {
		runID, _ := e.store.QueueBackup(ctx, jobID, "manual")
		run, err := e.store.ClaimRun(ctx, a.ID)
		if err != nil || run == nil {
			t.Fatalf("claim: %v", err)
		}
		be, err := backend.Open(ctx, run.RepoURL, backend.Options{S3AccessKey: "AK", S3SecretKey: "SK"})
		if err != nil {
			t.Fatal(err)
		}
		var r *repo.Repository
		if init {
			r, err = repo.Init(ctx, be, repo.Password(""))
		} else {
			r, err = repo.Open(ctx, be, repo.Password(""))
		}
		if err != nil {
			t.Fatal(err)
		}
		sn, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}})
		if err != nil {
			t.Fatal(err)
		}
		r.Close()
		e.store.FinishRun(ctx, a.ID, runID, api.RunResult{Status: api.StatusSuccess, SnapshotID: sn.ID.String()})
		return runID
	}
	comps, _ := fsutil.ParseSnapshotPath(src)
	dir := strings.Join(comps, "/")
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	download := func(runID int64, sel string) (string, string) {
		v := url.Values{"path": {dir}, "sel": {sel}}
		req, _ := http.NewRequest("POST", fmt.Sprintf("%s/runs/%d/files-download", e.ts.URL, runID), strings.NewReader(v.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", e.ts.URL)
		resp, err := admin.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b), resp.Header.Get("Location")
	}

	first := backup(true)
	if body, _ := download(first, dir+"/first.txt"); body != "first" { // the console now caches the repository
		t.Fatalf("first backup: %q", body)
	}
	os.WriteFile(filepath.Join(src, "second.txt"), []byte("second"), 0o644)
	second := backup(false)
	if body, loc := download(second, dir+"/second.txt"); body != "second" {
		t.Errorf("file of the newer backup: %q %s", body, loc)
	}
	if body, loc := download(second, dir+"/missing.txt"); !strings.Contains(loc, "err=") || strings.HasPrefix(body, "PK") {
		t.Errorf("missing file: %q %s", body, loc)
	}
}
