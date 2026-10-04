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

func TestSQLServerJobs(t *testing.T) {
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
		call("POST", api.PathPoll, api.PollRequest{Hostname: "SQL01", OS: "Windows Server 2022", Arch: "amd64", Version: "test"}, &pr)
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

	form := url.Values{"name": {"sql"}, "kind": {"sql"}, "agent_id": {fmt.Sprint(a.ID)}, "target_id": {fmt.Sprint(target)},
		"sql_instance": {"SQL EXPRESS"}, "sql_databases": {"shop\r\nhr"}, "sql_log_minutes": {"15"}, "sched_kind": {"manual"}, "keep_last": {"3"}}
	if _, loc, _ := admin.do("POST", "/jobs", form); !strings.Contains(loc, "err=") {
		t.Error("instance name with a space accepted")
	}
	form.Set("sql_instance", "SQLEXPRESS")
	if _, loc, _ := admin.do("POST", "/jobs", form); !strings.Contains(loc, "msg=") {
		t.Fatalf("create job: %s", loc)
	}
	jobs, _ := e.store.ListJobs(ctx)
	j := jobs[0]
	if j.Kind != server.JobSQL || strings.Join(j.Paths, ",") != "shop,hr" || j.Options.SQLInstance != "SQLEXPRESS" || j.Options.SQLLogMinutes != 15 {
		t.Fatalf("job: %+v", j)
	}
	if _, _, body := admin.do("GET", fmt.Sprintf("/jobs/%d", j.ID), nil); !strings.Contains(body, "instance SQLEXPRESS: shop, hr") || !strings.Contains(body, "every 15 minutes") {
		t.Error("job page lacks the SQL settings")
	}

	// No log backups before the first full backup.
	if ids, _ := e.store.QueueDueSQLLogs(ctx, time.Now().Add(time.Hour)); len(ids) != 0 {
		t.Errorf("log backup queued without a full backup: %v", ids)
	}
	full, _ := e.store.QueueBackup(ctx, j.ID, "manual")
	r := poll()
	if r == nil || r.ID != full || r.Kind != api.KindSQLBackup || r.SQL == nil || r.SQL.Instance != "SQLEXPRESS" || !r.SQL.Logs || len(r.SQL.Databases) != 2 || r.Retention == nil {
		t.Fatalf("full backup run: %+v", r)
	}
	t0 := time.Date(2026, 10, 4, 10, 0, 0, 0, time.Local)
	db := func(name, kind string, first, last string, fin time.Time) string {
		return fmt.Sprintf(`{"name":%q,"file":"C:\\b\\%s.%s","recovery":"FULL","size":1000,"first_lsn":%q,"last_lsn":%q,"start":%q,"finish":%q}`,
			name, name, kind, first, last, fin.Add(-time.Second).Format(time.RFC3339), fin.Format(time.RFC3339))
	}
	finish(full, api.RunResult{Status: api.StatusSuccess, SnapshotID: strings.Repeat("aa", 32),
		Details: json.RawMessage(`{"sql":{"instance":"SQLEXPRESS","kind":"full","databases":[` + db("shop", "bak", "100", "200", t0) + `,` + db("hr", "bak", "100", "150", t0) + `]}}`)})

	logRun := func(details string) int64 {
		ids, err := e.store.QueueDueSQLLogs(ctx, time.Now().Add(20*time.Minute))
		if err != nil || len(ids) != 1 {
			t.Fatalf("queue log backup: %v %v", ids, err)
		}
		r := poll()
		if r == nil || r.ID != ids[0] || r.Kind != api.KindSQLLog || r.SQL == nil {
			t.Fatalf("log run: %+v", r)
		}
		finish(r.ID, api.RunResult{Status: api.StatusSuccess, SnapshotID: fmt.Sprintf("%064d", r.ID), Details: json.RawMessage(details)})
		return r.ID
	}
	logRun(`{"sql":{"kind":"log","databases":[` + db("shop", "trn", "200", "300", t0.Add(15*time.Minute)) + `]}}`)
	if ids, _ := e.store.QueueDueSQLLogs(ctx, time.Now().Add(time.Minute)); len(ids) != 0 {
		t.Error("log backup queued before the interval passed")
	}
	logRun(`{"sql":{"kind":"log","databases":[` + db("shop", "trn", "300", "400", t0.Add(30*time.Minute)) + `]}}`)

	_, _, body := admin.do("GET", fmt.Sprintf("/runs/%d", full), nil)
	if !strings.Contains(body, "Restore a database") || !strings.Contains(body, "no log backups") || !strings.Contains(body, "2026-10-04 10:30") {
		t.Error("run page lacks the restore form or the log range")
	}
	// Point in time beyond the log backups is refused, within is queued.
	bad := url.Values{"kind": {"sql"}, "database": {"shop"}, "agent_id": {fmt.Sprint(a.ID)}, "instance": {"SQLEXPRESS"}, "target_mode": {"new"},
		"target": {"shop_pit"}, "point": {"time"}, "stop_at": {"2026-10-04T11:00:00"}}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/runs/%d/restore", full), bad); !strings.Contains(loc, "err=") {
		t.Error("point in time after the last log backup accepted")
	}
	good := bad
	good.Set("stop_at", "2026-10-04T10:20:00")
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/runs/%d/restore", full), good); !strings.Contains(loc, "/runs/") || strings.Contains(loc, "err=") {
		t.Fatalf("restore: %s", loc)
	}
	r = poll()
	if r == nil || r.Kind != api.KindSQLRestore || r.SnapshotID != strings.Repeat("aa", 32) || r.SQL == nil || r.SQL.Restore == nil ||
		r.SQL.Restore.Target != "shop_pit" || len(r.SQL.Restore.LogSnapshots) != 2 || r.SQL.Restore.StopAt.Format("15:04") != "10:20" {
		t.Fatalf("restore run: %+v %+v", r, r.SQL)
	}
	finish(r.ID, api.RunResult{Status: api.StatusSuccess})
	if _, _, body := admin.do("GET", "/restore?type=sql", nil); !strings.Contains(body, "shop") || !strings.Contains(body, "SQL Server SQLEXPRESS on SQL01") {
		t.Error("restore wizard lacks the databases")
	}
	src := url.QueryEscape("sql:SQL01:SQLEXPRESS:hr")
	if _, _, body := admin.do("GET", fmt.Sprintf("/restore?type=sql&src=%s&run=%d", src, full), nil); !strings.Contains(body, `<option value="hr" selected>`) {
		t.Error("restore wizard does not preselect the database")
	}
}
