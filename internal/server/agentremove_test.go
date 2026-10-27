package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/server"
	"github.com/max-zit/backupzit/internal/testutil"
)

// removeEnv is an agent with three backups of one job on S3 storage.
type removeEnv struct {
	e       *env
	admin   *client
	agentID int64
	host    string
	repoURL string
	call    func(method, p string, body, out any) int
}

func newRemoveEnv(t *testing.T, version string) *removeEnv {
	e := setup(t)
	ctx := e.ctx
	s3, err := testutil.StartS3Server("backups")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s3.Close)
	targetID, err := e.store.CreateTarget(ctx, server.Target{Name: "s3", Kind: "s3", URL: "s3://" + s3.Host + "/backups/files?tls=false", S3AccessKey: "AK", S3SecretKey: "SK"})
	if err != nil {
		t.Fatal(err)
	}
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, version)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, p string, body, out any) int {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, e.ts.URL+p, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+cfg.AgentUUID+":"+cfg.Secret)
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if out != nil {
			json.NewDecoder(resp.Body).Decode(out)
		}
		return resp.StatusCode
	}
	call("POST", api.PathPoll, api.PollRequest{Hostname: "PC-17", OS: "Windows 11", Arch: "amd64", Version: version}, nil)
	agents, _ := e.store.ListAgents(ctx)
	a := agents[0]
	src := t.TempDir()
	jobID, err := e.store.CreateJob(ctx, server.Job{AgentID: a.ID, TargetID: targetID, Name: "Docs", Paths: []string{src}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	var repoURL string
	for i := 0; i < 3; i++ {
		os.WriteFile(filepath.Join(src, "f.txt"), []byte(fmt.Sprint("version ", i)), 0o644)
		runID, _ := e.store.QueueBackup(ctx, jobID, "manual")
		run, err := e.store.ClaimRun(ctx, a.ID)
		if err != nil || run == nil || run.ID != runID {
			t.Fatalf("claim: %+v %v", run, err)
		}
		repoURL = run.RepoURL
		be, err := backend.Open(ctx, run.RepoURL, backend.Options{S3AccessKey: "AK", S3SecretKey: "SK"})
		if err != nil {
			t.Fatal(err)
		}
		r, err := repo.Open(ctx, be, repo.Password(""))
		if err != nil {
			r, err = repo.Init(ctx, be, repo.Password(""))
		}
		if err != nil {
			t.Fatal(err)
		}
		sn, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}, Tags: []string{fmt.Sprintf("job:%d", jobID)}})
		if err != nil {
			t.Fatal(err)
		}
		r.Close()
		if err := e.store.FinishRun(ctx, a.ID, runID, api.RunResult{Status: api.StatusSuccess, SnapshotID: sn.ID.String(), Stats: json.RawMessage(`{"files":10}`)}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	return &removeEnv{e: e, admin: admin, agentID: a.ID, host: a.Hostname, repoURL: repoURL, call: call}
}

// snapshots counts the backups in the agent's repository (-1: no repository).
func (x *removeEnv) snapshots(t *testing.T) int {
	ctx := context.Background()
	be, err := backend.Open(ctx, x.repoURL, backend.Options{S3AccessKey: "AK", S3SecretKey: "SK"})
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	r, err := repo.Open(ctx, be, repo.Password(""))
	if err != nil {
		return -1
	}
	defer r.Close()
	sns, err := r.ListSnapshots(ctx)
	if errors.Is(err, backend.ErrNotFound) {
		return -2 // a snapshot was deleted while listing: the clean-up is running
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(sns)
}

// waitSnapshots waits for the background clean-up.
func (x *removeEnv) waitSnapshots(t *testing.T, want int) {
	for i := 0; i < 100; i++ {
		if x.snapshots(t) == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("backups: %d, want %d", x.snapshots(t), want)
}

func (x *removeEnv) agentExists() bool {
	_, err := x.e.store.GetAgent(x.e.ctx, x.agentID)
	return err == nil
}

func TestAgentRemoveKeepNewest(t *testing.T) {
	x := newRemoveEnv(t, "test")
	_, _, page := x.admin.do("GET", fmt.Sprintf("/agents/remove/%d", x.agentID), nil)
	for _, want := range []string{"Keep only the newest backup of each job", "Docs", ">3<", "cannot uninstall itself"} {
		if !strings.Contains(page, want) {
			t.Errorf("remove page lacks %q", want)
		}
	}
	if _, loc, _ := x.admin.do("POST", fmt.Sprintf("/agents/%d/delete", x.agentID), url.Values{"backups": {"last"}}); !strings.Contains(loc, "err=") || !x.agentExists() {
		t.Fatalf("deleted backups without the name typed: %s", loc)
	}
	if _, loc, _ := x.admin.do("POST", fmt.Sprintf("/agents/%d/delete", x.agentID), url.Values{"backups": {"last"}, "confirm": {x.host}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("remove: %s", loc)
	}
	if x.agentExists() {
		t.Fatal("agent not removed")
	}
	x.waitSnapshots(t, 1)
	// The result is logged when the whole clean-up has finished.
	var log []server.AuditEntry
	found := false
	for i := 0; i < 100 && !found; i++ {
		log, _ = x.e.store.ListAudit(x.e.ctx, 20)
		for _, a := range log {
			found = found || (a.Action == "agent.backups" && strings.Contains(a.Detail, "deleted 2 older backups"))
		}
		if !found {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !found {
		t.Errorf("audit log has no clean-up result: %+v", log)
	}
}

func TestAgentRemoveAllBackups(t *testing.T) {
	x := newRemoveEnv(t, "test")
	if _, loc, _ := x.admin.do("POST", fmt.Sprintf("/agents/%d/delete", x.agentID), url.Values{"backups": {"all"}, "confirm": {strings.ToLower(x.host)}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("remove: %s", loc)
	}
	x.waitSnapshots(t, -1)
}

func TestAgentRemoveKeepsBackupsByDefault(t *testing.T) {
	x := newRemoveEnv(t, "test")
	if _, loc, _ := x.admin.do("POST", fmt.Sprintf("/agents/%d/delete", x.agentID), url.Values{}); !strings.Contains(loc, "msg=") || x.agentExists() {
		t.Fatalf("remove: %s", loc)
	}
	time.Sleep(300 * time.Millisecond)
	if n := x.snapshots(t); n != 3 {
		t.Errorf("backups: %d, want 3", n)
	}
}

// An online agent uninstalls itself first and is removed when it reports.
func TestAgentRemoveUninstall(t *testing.T) {
	x := newRemoveEnv(t, "0.42.7")
	_, _, page := x.admin.do("GET", fmt.Sprintf("/agents/remove/%d", x.agentID), nil)
	if !strings.Contains(page, `name="uninstall" checked`) {
		t.Fatal("no uninstall option for an online agent")
	}
	if _, loc, _ := x.admin.do("POST", fmt.Sprintf("/agents/%d/delete", x.agentID), url.Values{"uninstall": {"on"}, "backups": {"last"}, "confirm": {x.host}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("remove: %s", loc)
	}
	if !x.agentExists() {
		t.Fatal("removed before it uninstalled")
	}
	var pr api.PollResponse
	x.call("POST", api.PathPoll, api.PollRequest{Hostname: "PC-17", OS: "Windows 11", Arch: "amd64", Version: "0.42.7"}, &pr)
	if pr.Run == nil || pr.Run.Kind != api.KindAgentUninstall {
		t.Fatalf("poll: %+v", pr.Run)
	}
	if c := x.call("POST", fmt.Sprintf("%s%d/finish", api.PathRunsPrefix, pr.Run.ID), api.RunResult{Status: api.StatusSuccess, Message: "uninstalling"}, nil); c != 200 {
		t.Fatalf("finish: %d", c)
	}
	if x.agentExists() {
		t.Fatal("agent not removed after uninstalling")
	}
	x.waitSnapshots(t, 1)
}

// A failed uninstall keeps the agent and its backups.
func TestAgentRemoveUninstallFails(t *testing.T) {
	x := newRemoveEnv(t, "0.42.7")
	x.admin.do("POST", fmt.Sprintf("/agents/%d/delete", x.agentID), url.Values{"uninstall": {"on"}, "backups": {"all"}, "confirm": {x.host}})
	var pr api.PollResponse
	x.call("POST", api.PathPoll, api.PollRequest{Hostname: "PC-17", OS: "Windows 11", Arch: "amd64", Version: "0.42.7"}, &pr)
	if pr.Run == nil || pr.Run.Kind != api.KindAgentUninstall {
		t.Fatalf("poll: %+v", pr.Run)
	}
	x.call("POST", fmt.Sprintf("%s%d/finish", api.PathRunsPrefix, pr.Run.ID), api.RunResult{Status: api.StatusFailed, Message: "not installed from MSI"}, nil)
	if !x.agentExists() {
		t.Fatal("agent removed although the uninstall failed")
	}
	time.Sleep(300 * time.Millisecond)
	if n := x.snapshots(t); n != 3 {
		t.Errorf("backups: %d, want 3", n)
	}
	_, _, page := x.admin.do("GET", fmt.Sprintf("/agents/remove/%d", x.agentID), nil)
	if strings.Contains(page, "is uninstalling itself") {
		t.Error("still shown as uninstalling")
	}
}

// With four-eyes approval the choices wait with the request and are
// carried out when a second administrator approves.
func TestAgentRemoveFourEyes(t *testing.T) {
	x := newRemoveEnv(t, "test")
	const pw = "correct-horse-9"
	x.admin.do("POST", "/users", url.Values{"username": {"admin2"}, "role": {"admin"}, "password": {pw}, "password2": {pw}})
	if _, loc, _ := x.admin.do("POST", "/settings/four-eyes", url.Values{"four_eyes": {"on"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("four-eyes: %s", loc)
	}
	if _, loc, _ := x.admin.do("POST", fmt.Sprintf("/agents/%d/delete", x.agentID), url.Values{"backups": {"last"}, "confirm": {x.host}}); !strings.Contains(loc, "msg=Waiting") {
		t.Fatalf("not held: %s", loc)
	}
	if !x.agentExists() || x.snapshots(t) != 3 {
		t.Fatal("removed before approval")
	}
	var id int64
	x.e.pool.QueryRow(x.e.ctx, `SELECT id FROM approvals WHERE status='pending' ORDER BY id DESC LIMIT 1`).Scan(&id)
	admin2 := newClient(t, x.e)
	admin2.login("admin2", pw)
	if _, loc, _ := admin2.do("POST", fmt.Sprintf("/approvals/%d/approve", id), url.Values{}); !strings.Contains(loc, "msg=") {
		t.Fatalf("approve: %s", loc)
	}
	if x.agentExists() {
		t.Fatal("agent not removed after approval")
	}
	x.waitSnapshots(t, 1)
}

// Kept backups of a removed agent stay in Activity, and the agent of the
// reinstalled computer (same name) takes them over and continues in the
// same repository.
func TestAgentRemoveKeepsHistoryAndAdopt(t *testing.T) {
	x := newRemoveEnv(t, "test")
	ctx := x.e.ctx
	old, _ := x.e.store.ListAgents(ctx)
	oldRepoDir := old[0].RepoDir
	if _, loc, _ := x.admin.do("POST", fmt.Sprintf("/agents/%d/delete", x.agentID), url.Values{"backups": {"keep"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("remove: %s", loc)
	}
	if x.agentExists() {
		t.Fatal("removed agent still listed")
	}
	if code := x.call("POST", api.PathPoll, api.PollRequest{Hostname: "PC-17", Version: "test"}, nil); code != http.StatusUnauthorized {
		t.Errorf("removed agent may still sign in: %d", code)
	}
	if jobs, _ := x.e.store.ListJobs(ctx); len(jobs) != 0 {
		t.Errorf("jobs of the removed agent remain: %d", len(jobs))
	}
	_, _, page := x.admin.do("GET", fmt.Sprintf("/runs?agent=%d", x.agentID), nil)
	if strings.Count(page, ">removed<") < 3 || !strings.Contains(page, "Docs") {
		t.Errorf("Activity does not show the kept backups with their job")
	}
	_, _, page = x.admin.do("GET", "/agents", nil)
	if !strings.Contains(page, "Removed agents with kept backups") || strings.Contains(page, "Connect to the current agent") {
		t.Errorf("agents page: removed section or an early connect button")
	}
	// The computer gets the agent again.
	token, _, _ := x.e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, x.e.ts.URL, token, x.e.fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", x.e.ts.URL+api.PathPoll, strings.NewReader(`{"hostname":"PC-17","version":"test"}`))
	req.Header.Set("Authorization", "Bearer "+cfg.AgentUUID+":"+cfg.Secret)
	if resp, err := x.e.ts.Client().Do(req); err == nil {
		resp.Body.Close()
	}
	agents, _ := x.e.store.ListAgents(ctx)
	if len(agents) != 1 || agents[0].Hostname != x.host {
		t.Fatalf("new agent: %+v", agents)
	}
	newID := agents[0].ID
	_, _, page = x.admin.do("GET", "/agents", nil)
	if !strings.Contains(page, fmt.Sprintf(`action="/agents/%d/adopt"`, newID)) {
		t.Fatal("no connect button for the reinstalled computer")
	}
	if _, loc, _ := x.admin.do("POST", fmt.Sprintf("/agents/%d/adopt", newID), url.Values{"removed": {fmt.Sprint(x.agentID)}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("adopt: %s", loc)
	}
	a, _ := x.e.store.GetAgent(ctx, newID)
	if a.RepoDir != oldRepoDir {
		t.Errorf("repo dir %q, want the old %q", a.RepoDir, oldRepoDir)
	}
	runs, _ := x.e.store.ListRuns(ctx, server.RunFilter{AgentID: newID})
	if len(runs) < 3 || runs[0].AgentRemoved {
		t.Errorf("history not moved: %d runs", len(runs))
	}
	if _, _, page := x.admin.do("GET", "/agents", nil); strings.Contains(page, "Removed agents with kept backups") {
		t.Error("removed agent still listed after connecting")
	}
}
