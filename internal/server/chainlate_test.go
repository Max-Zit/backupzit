package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/server"
)

// MED 10: a backup the console failed as stale (its agent did not poll for
// minutes) that the agent then reports as successful still starts the jobs
// chained to run only after a success; the ones that also follow a failure
// started at the stale fail and are not queued twice.
func TestChainAfterLateSuccess(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "0.62.0")
	if err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	aid := agents[0].ID
	target, _ := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "local", URL: t.TempDir()})
	job := func(name string, sc server.Schedule) int64 {
		id, err := e.store.CreateJob(ctx, server.Job{AgentID: aid, TargetID: target, Name: name, Paths: []string{"/srv"}, Enabled: true, Schedule: sc.Encode()})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	db := job("database", server.Schedule{Kind: server.SchedManual})
	onSuccess := job("files", server.Schedule{Kind: server.SchedChain, AfterJob: db})
	always := job("report", server.Schedule{Kind: server.SchedChain, AfterJob: db, AfterAny: true})
	count := func(j int64) int {
		var n int
		e.pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE job_id=$1 AND trigger='after-job'`, j).Scan(&n)
		return n
	}
	rid, err := e.store.QueueBackup(ctx, db, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if r, err := e.store.ClaimRun(ctx, aid); err != nil || r == nil || r.ID != rid {
		t.Fatalf("claim: %v %v", r, err)
	}
	// The agent falls silent; the console fails the run and chains as
	// after a failure.
	e.pool.Exec(ctx, `UPDATE agents SET last_seen_at=now()-interval '20 minutes' WHERE id=$1`, aid)
	ids, err := e.store.FailStaleRuns(ctx, 10*time.Minute, time.Now().Add(-time.Hour))
	if err != nil || len(ids) != 1 {
		t.Fatalf("stale: %v %v", ids, err)
	}
	e.store.ChainAfterRun(ctx, rid)
	if count(always) != 1 || count(onSuccess) != 0 {
		t.Fatalf("at the stale fail: %d 'also after failure', %d 'after success'", count(always), count(onSuccess))
	}
	// The agent comes back and reports success.
	b, _ := json.Marshal(api.RunResult{Status: api.StatusSuccess, SnapshotID: "abc"})
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s%s%d/finish", e.ts.URL, api.PathRunsPrefix, rid), bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+cfg.AgentUUID+":"+cfg.Secret)
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("late finish: %d", resp.StatusCode)
	}
	if r, _ := e.store.GetRun(ctx, rid); r.Status != "success" {
		t.Fatalf("late result not taken: %s", r.Status)
	}
	if count(onSuccess) != 1 || count(always) != 1 {
		t.Fatalf("after the late success: %d 'after success' (want 1), %d 'also after failure' (want 1)", count(onSuccess), count(always))
	}
}

// LOW 2: a job chained after a job that is deleted never starts again; the
// delete message, Needs attention, the job page and the edit form say so.
func TestChainAfterDeletedJob(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	if _, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "0.62.0"); err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	aid := agents[0].ID
	target, _ := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "local", URL: t.TempDir()})
	first, _ := e.store.CreateJob(ctx, server.Job{AgentID: aid, TargetID: target, Name: "database", Paths: []string{"/db"}, Enabled: true})
	other, _ := e.store.CreateJob(ctx, server.Job{AgentID: aid, TargetID: target, Name: "mail", Paths: []string{"/mail"}, Enabled: true})
	next, _ := e.store.CreateJob(ctx, server.Job{AgentID: aid, TargetID: target, Name: "files", Paths: []string{"/srv"}, Enabled: true,
		Schedule: server.Schedule{Kind: server.SchedChain, AfterJob: first}.Encode()})
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	if _, _, body := admin.do("GET", "/", nil); strings.Contains(body, "runs after a job that was deleted") {
		t.Fatal("an intact chain is reported")
	}
	_, loc, _ := admin.do("POST", fmt.Sprintf("/jobs/%d/delete", first), url.Values{})
	if msg, _ := url.QueryUnescape(loc); !strings.Contains(msg, "Jobs that ran after it no longer start") {
		t.Errorf("delete message: %s", msg)
	}
	if _, _, body := admin.do("GET", "/", nil); !strings.Contains(body, "runs after a job that was deleted") {
		t.Error("Needs attention does not list the broken chain")
	}
	if _, _, body := admin.do("GET", fmt.Sprintf("/jobs/%d", next), nil); !strings.Contains(body, "deleted job") {
		t.Error("the job page does not show the broken chain")
	}
	// The form does not quietly pick another job to run after.
	_, _, body := admin.do("GET", fmt.Sprintf("/jobs/%d/edit", next), nil)
	if !strings.Contains(body, `<option value="" selected>the job it ran after was deleted: choose one</option>`) ||
		strings.Contains(body, fmt.Sprintf(`<option value="%d" selected>`, other)) {
		t.Error("the edit form preselects another job")
	}
}
