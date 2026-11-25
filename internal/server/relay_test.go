package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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

// A backup on storage reachable by agents only is browsed and downloaded in
// the console through the agent that made it (0.61, #63): the agent gets the
// reads with the relay API, does them and answers; the console never connects
// to the storage itself.
func TestBrowseAgentsOnlyThroughAgent(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	s3, err := testutil.StartS3Server("backups")
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	targetID, err := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "s3", URL: "s3://" + s3.Host + "/backups/files?tls=false", S3AccessKey: "AK", S3SecretKey: "SK"})
	if err != nil {
		t.Fatal(err)
	}
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "0.61.0")
	if err != nil {
		t.Fatal(err)
	}
	// The agent's requests end with the test (no long poll left waiting).
	actx, stop := context.WithCancel(ctx)
	defer stop()
	call := func(p string, body []byte, hdr map[string]string, out any) int {
		req, _ := http.NewRequestWithContext(actx, "POST", e.ts.URL+p, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+cfg.AgentUUID+":"+cfg.Secret)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			if actx.Err() == nil {
				t.Error(err)
			}
			return 0
		}
		defer resp.Body.Close()
		if out != nil {
			json.NewDecoder(resp.Body).Decode(out)
		}
		return resp.StatusCode
	}
	poll := func() api.PollResponse {
		b, _ := json.Marshal(api.PollRequest{Hostname: "site-pc", OS: "Linux", Arch: "amd64", Version: "0.61.0", Relay: true})
		var resp api.PollResponse
		call(api.PathPoll, b, nil, &resp)
		return resp
	}
	poll()
	agents, _ := e.store.ListAgents(ctx)
	a := agents[0]

	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "docs"), 0o755)
	os.WriteFile(filepath.Join(src, "docs", "report.txt"), []byte("quarterly report"), 0o644)
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
	if err := e.store.SetTargetAgentsOnly(ctx, targetID, true); err != nil {
		t.Fatal(err)
	}

	// The agent: reads what the console asks for, with the credentials of the
	// request, and answers.
	served := make(chan string, 1000)
	go func() {
		for actx.Err() == nil {
			var next api.RelayNextResponse
			if call(api.PathRelayNext, []byte("{}"), nil, &next) != 200 {
				return
			}
			for _, rq := range next.Requests {
				served <- rq.Op + " " + rq.Name
				if rq.Repository.Password != "" {
					t.Errorf("the repository password went to the agent")
				}
				rb, err := backend.Open(actx, rq.Repository.URL, backend.Options{S3AccessKey: rq.Repository.S3AccessKey, S3SecretKey: rq.Repository.S3SecretKey})
				var data []byte
				if err == nil {
					switch rq.Op {
					case api.RelayLoad:
						data, err = rb.Load(actx, rq.Name)
					case api.RelayLoadRange:
						data, err = rb.LoadRange(actx, rq.Name, rq.Offset, rq.Length)
					case api.RelaySize:
						var n int64
						n, err = rb.Size(actx, rq.Name)
						data = []byte(strconv.FormatInt(n, 10))
					case api.RelayList:
						var names []string
						names, err = rb.List(actx, rq.Name)
						data, _ = json.Marshal(names)
					}
					rb.Close()
				}
				hdr := map[string]string{}
				switch {
				case errors.Is(err, backend.ErrNotFound):
					hdr[api.RelayNotFoundHeader], data = "1", nil
				case err != nil:
					hdr[api.RelayErrorHeader], data = err.Error(), nil
				}
				call(api.PathRelayReplyPrefix+rq.ID, data, hdr, nil)
			}
		}
	}()

	comps, _ := fsutil.ParseSnapshotPath(src)
	docs := strings.Join(append(append([]string{}, comps...), "docs"), "/")
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	_, _, page := admin.do("GET", fmt.Sprintf("/runs/%d/files?path=%s", runID, url.QueryEscape(docs)), nil)
	if !strings.Contains(page, "report.txt") {
		t.Fatalf("listing through the agent lacks the file: %s", page)
	}
	v := url.Values{"path": {docs}, "sel": {docs + "/report.txt"}}
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/runs/%d/files-download", e.ts.URL, runID), strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.ts.URL)
	resp, err := admin.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "quarterly report" {
		t.Fatalf("download through the agent: %q", body)
	}
	if len(served) == 0 {
		t.Fatal("nothing was read through the agent")
	}
}
