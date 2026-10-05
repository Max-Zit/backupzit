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

func TestPostgresAndMySQLJobs(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	target, _ := e.store.CreateTarget(ctx, server.Target{Name: "local", Kind: "local", URL: t.TempDir()})
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, p string, body any, out any) int {
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
	poll := func() *api.Run {
		var pr api.PollResponse
		call("POST", api.PathPoll, api.PollRequest{Hostname: "db1", OS: "Debian GNU/Linux 12", Arch: "amd64", Version: "test"}, &pr)
		return pr.Run
	}
	finish := func(id int64, res api.RunResult) {
		if c := call("POST", fmt.Sprintf("%s%d/finish", api.PathRunsPrefix, id), res, nil); c != 200 {
			t.Fatalf("finish run %d: %d", id, c)
		}
	}
	poll()
	agents, _ := e.store.ListAgents(ctx)
	a := agents[0]
	t0 := time.Date(2026, 10, 5, 10, 0, 0, 0, time.Local)
	entry := func(name, first, last string, fin time.Time) string {
		return fmt.Sprintf(`{"name":%q,"file":"/x/%s","size":1000,"first_lsn":%q,"last_lsn":%q,"start":%q,"finish":%q}`,
			name, name, first, last, fin.Add(-time.Second).Format(time.RFC3339), fin.Format(time.RFC3339))
	}

	// SQL Server needs Windows; PostgreSQL and MySQL need Linux.
	form := url.Values{"name": {"sql"}, "kind": {"sql"}, "agent_id": {fmt.Sprint(a.ID)}, "target_id": {fmt.Sprint(target)}, "sched_kind": {"manual"}, "keep_last": {"3"}}
	if _, loc, _ := admin.do("POST", "/jobs", form); !strings.Contains(loc, "err=") || !strings.Contains(loc, "Windows") {
		t.Errorf("SQL Server job on Linux accepted: %s", loc)
	}

	// PostgreSQL.
	form.Set("name", "pg")
	form.Set("sql_engine", "postgres")
	form.Set("sql_instance", "abc")
	if _, loc, _ := admin.do("POST", "/jobs", form); !strings.Contains(loc, "err=") {
		t.Error("PostgreSQL port abc accepted")
	}
	form.Set("sql_instance", "5433")
	form.Set("sql_log_minutes", "15")
	if _, loc, _ := admin.do("POST", "/jobs", form); !strings.Contains(loc, "msg=") {
		t.Fatalf("create pg job: %s", loc)
	}
	jobs, _ := e.store.ListJobs(ctx)
	pg := jobs[0]
	full, _ := e.store.QueueBackup(ctx, pg.ID, "manual")
	r := poll()
	if r == nil || r.ID != full || r.SQL == nil || r.SQL.Engine != "postgres" || r.SQL.Instance != "5433" || !r.SQL.Logs {
		t.Fatalf("pg backup run: %+v", r)
	}
	finish(full, api.RunResult{Status: api.StatusSuccess, SnapshotID: strings.Repeat("a1", 32), Details: json.RawMessage(`{"sql":{"engine":"postgres","kind":"full","databases":[` +
		entry("(cluster)", "000000010000000000000004", "000000010000000000000004", t0) + `,` + entry("(globals)", "", "", t0) + `,` +
		entry("shop", "000000010000000000000004", "000000010000000000000004", t0) + `]}}`)})
	// logRun queues the due log backups and finishes the one of job with
	// details (others: nothing to store).
	logRun := func(job int64, details string) {
		ids, err := e.store.QueueDueSQLLogs(ctx, time.Now().Add(time.Hour))
		if err != nil || len(ids) == 0 {
			t.Fatalf("queue log run: %v %v", ids, err)
		}
		for range ids {
			r := poll()
			if r.JobID == job {
				finish(r.ID, api.RunResult{Status: api.StatusSuccess, SnapshotID: fmt.Sprintf("%064d", r.ID), Details: json.RawMessage(details)})
			} else {
				finish(r.ID, api.RunResult{Status: api.StatusSuccess})
			}
		}
	}
	logRun(pg.ID, `{"sql":{"engine":"postgres","kind":"log","databases":[` + entry("(wal)", "000000010000000000000005", "000000010000000000000006", t0.Add(15*time.Minute)) + `]}}`)
	// A gap (segment 7 missing): later WAL cannot be used.
	logRun(pg.ID, `{"sql":{"engine":"postgres","kind":"log","databases":[` + entry("(wal)", "000000010000000000000008", "000000010000000000000008", t0.Add(30*time.Minute)) + `]}}`)
	_, _, body := admin.do("GET", fmt.Sprintf("/runs/%d", full), nil)
	if !strings.Contains(body, "Whole PostgreSQL cluster") || !strings.Contains(body, "2026-10-05 10:15") || strings.Contains(body, "10:30:00") || !strings.Contains(body, "Port") {
		t.Error("pg restore form: cluster option, WAL range or port missing")
	}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/runs/%d/restore", full), url.Values{"kind": {"sql"}, "database": {"(cluster)"}, "agent_id": {fmt.Sprint(a.ID)},
		"target_mode": {"new"}, "point": {"time"}, "stop_at": {"2026-10-05T10:10:00"}}); !strings.Contains(loc, "/runs/") || strings.Contains(loc, "err=") {
		t.Fatalf("cluster restore: %s", loc)
	}
	r = poll()
	if r == nil || r.Kind != api.KindSQLRestore || r.SQL.Engine != "postgres" || r.SQL.Restore.Database != "(cluster)" || r.SQL.Restore.Target != "" || len(r.SQL.Restore.LogSnapshots) != 2 {
		t.Fatalf("cluster restore run: %+v %+v", r, r.SQL)
	}
	finish(r.ID, api.RunResult{Status: api.StatusSuccess})
	if _, _, list := admin.do("GET", "/jobs", nil); !strings.Contains(list, "PostgreSQL") || !strings.Contains(list, "1 database: shop") {
		t.Error("job list: PostgreSQL job without its database")
	}

	// MySQL / MariaDB.
	form.Set("name", "my")
	form.Set("sql_engine", "mysql")
	form.Set("sql_instance", "ignored")
	if _, loc, _ := admin.do("POST", "/jobs", form); !strings.Contains(loc, "msg=") {
		t.Fatalf("create mysql job: %s", loc)
	}
	jobs, _ = e.store.ListJobs(ctx)
	var my server.Job
	for _, j := range jobs {
		if j.Name == "my" {
			my = j
		}
	}
	if my.Options.SQLInstance != "" {
		t.Errorf("mysql instance kept: %q", my.Options.SQLInstance)
	}
	mfull, _ := e.store.QueueBackup(ctx, my.ID, "manual")
	r = poll()
	if r == nil || r.ID != mfull || r.SQL.Engine != "mysql" {
		t.Fatalf("mysql backup run: %+v", r)
	}
	finish(mfull, api.RunResult{Status: api.StatusSuccess, SnapshotID: strings.Repeat("b2", 32), Details: json.RawMessage(`{"sql":{"engine":"mysql","kind":"full","databases":[` +
		entry("shop", "mysql-bin.000003:157", "mysql-bin.000003:157", t0) + `,` + entry("hr", "", "", t0) + `]}}`)})
	logRun(my.ID, `{"sql":{"engine":"mysql","kind":"log","databases":[` + entry("(binlog)", "mysql-bin.000003", "mysql-bin.000004", t0.Add(20*time.Minute)) + `]}}`)
	_, _, body = admin.do("GET", fmt.Sprintf("/runs/%d", mfull), nil)
	if !strings.Contains(body, "2026-10-05 10:20") || strings.Contains(body, "Port") || strings.Contains(body, "Whole PostgreSQL cluster") {
		t.Error("mysql restore form wrong")
	}
	// hr has no binary log position: no point in time.
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/runs/%d/restore", mfull), url.Values{"kind": {"sql"}, "database": {"hr"}, "agent_id": {fmt.Sprint(a.ID)},
		"target_mode": {"new"}, "target": {"hr2"}, "point": {"time"}, "stop_at": {"2026-10-05T10:10:00"}}); !strings.Contains(loc, "err=") {
		t.Error("point in time without binary log position accepted")
	}
}
