package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/mysqldb"
	"github.com/max-zit/backupzit/internal/pgsql"
	"github.com/max-zit/backupzit/internal/repo"
)

// SQL Server jobs back up databases with SQL Server's own BACKUP statement
// (sql-backup runs, full backups on the job's schedule) and, if set, the
// transaction logs every few minutes (sql-log runs queued by the
// scheduler), so databases can be restored to a point in time.

// sqlDetails are the details of a sql-backup or sql-log run.
func sqlDetails(run Run) *repo.SQLBackup {
	if (run.Kind != api.KindSQLBackup && run.Kind != api.KindSQLLog) || len(run.Details) == 0 {
		return nil
	}
	var d struct {
		SQL *repo.SQLBackup `json:"sql"`
	}
	if json.Unmarshal(run.Details, &d) != nil || d.SQL == nil || len(d.SQL.Databases) == 0 {
		return nil
	}
	return d.SQL
}

// addSQL fills the SQL options of a backup run from its job.
func (s *Server) addSQL(ctx context.Context, run *Run, ar *api.Run) error {
	if run.JobID == nil {
		return errors.New("SQL Server backup without job")
	}
	j, err := s.store.GetJob(ctx, *run.JobID)
	if err != nil {
		return err
	}
	ar.SQL = &api.SQLRun{Engine: j.Options.SQLEngine, Instance: j.Options.SQLInstance, Databases: j.Paths, System: j.Options.SQLSystem, Logs: j.Options.SQLLogMinutes > 0}
	return nil
}

// queueDueSQLLogs queues transaction log backups of SQL Server jobs whose
// interval has passed since their last SQL backup, once a full backup
// exists.
func (s *Store) queueDueSQLLogs(ctx context.Context, now time.Time) ([]int64, error) {
	rows, err := s.db.Query(ctx, `SELECT j.id, j.agent_id, j.target_id, (j.options->>'sql_log_minutes')::int FROM jobs j
		WHERE j.enabled AND j.kind='sql' AND coalesce((j.options->>'sql_log_minutes')::int, 0) > 0
		AND EXISTS (SELECT 1 FROM runs b WHERE b.job_id=j.id AND b.kind='sql-backup' AND b.status IN ('success','warning'))
		AND NOT EXISTS (SELECT 1 FROM runs q WHERE q.job_id=j.id AND q.kind IN ('sql-backup','sql-log') AND q.status IN ('queued','running'))`)
	if err != nil {
		return nil, err
	}
	type due struct {
		job, agent, target int64
		minutes            int
	}
	var list []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.job, &d.agent, &d.target, &d.minutes); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, d)
	}
	rows.Close()
	var queued []int64
	for _, d := range list {
		var last time.Time
		s.db.QueryRow(ctx, `SELECT coalesce(max(coalesce(started_at, queued_at)), 'epoch') FROM runs WHERE job_id=$1 AND kind IN ('sql-backup','sql-log')`, d.job).Scan(&last)
		if now.Sub(last) < time.Duration(d.minutes)*time.Minute {
			continue
		}
		a, err := s.GetAgent(ctx, d.agent)
		if err != nil {
			continue
		}
		t, err := s.GetTarget(ctx, d.target)
		if err != nil {
			continue
		}
		var id int64
		err = s.db.QueryRow(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, target_id) VALUES($1,$2,'sql-log','schedule',$3,$4) RETURNING id`,
			d.agent, d.job, repoURL(t, a), t.ID).Scan(&id)
		if err != nil {
			return queued, err
		}
		queued = append(queued, id)
	}
	return queued, nil
}

// sqlLogRuns are the successful log backups of a job after a full backup
// run, oldest first.
func (s *Store) sqlLogRuns(ctx context.Context, full Run) ([]Run, error) {
	if full.JobID == nil {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, `SELECT `+runCols+runFrom+` WHERE r.job_id=$1 AND r.kind='sql-log' AND r.status IN ('success','warning')
		AND coalesce(r.snapshot_id,'') <> '' AND NOT r.expired AND r.id > $2 ORDER BY r.id`, *full.JobID, full.ID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Run, error) { return scanRun(r) })
}

// SQLRestorePoint is what the restore form offers for one database.
type SQLRestorePoint struct {
	Name     string
	Full     time.Time // end of the full backup
	LogUntil time.Time // end of the newest usable log backup (zero: none)
}

// sqlRestorePoints lists the databases of a full backup run and how far
// their log backups reach.
func (s *Store) sqlRestorePoints(ctx context.Context, full Run) []SQLRestorePoint {
	d := sqlDetails(full)
	if d == nil || d.Kind != "full" {
		return nil
	}
	logs, _ := s.sqlLogRuns(ctx, full)
	switch d.Engine {
	case "postgres":
		return pgRestorePoints(d, logs)
	case "mysql":
		return mysqlRestorePoints(d, logs)
	}
	var out []SQLRestorePoint
	for _, db := range d.Databases {
		p := SQLRestorePoint{Name: db.Name, Full: db.Finish}
		last := db.LastLSN
		broken := false
		for _, lr := range logs {
			ld := sqlDetails(lr)
			if ld == nil || broken {
				continue
			}
			for _, x := range ld.Databases {
				if !strings.EqualFold(x.Name, db.Name) || !lsnLess(last, x.LastLSN) {
					continue
				}
				if lsnLess(last, x.FirstLSN) {
					broken = true // gap in the chain: later logs cannot be used
					break
				}
				last, p.LogUntil = x.LastLSN, x.Finish
			}
		}
		out = append(out, p)
	}
	return out
}

func lsnLess(a, b string) bool {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

// QueueSQLRestore restores a database of a full SQL Server backup run.
// o.LogSnapshots non-nil asks for the log backups to be applied (they are
// filled in here); nil restores the full backup only.
func (s *Store) QueueSQLRestore(ctx context.Context, fullRunID, agentID int64, instance string, o api.SQLRestore) (int64, error) {
	b, err := s.GetRun(ctx, fullRunID)
	if err != nil {
		return 0, err
	}
	d := sqlDetails(b)
	if d == nil || d.Kind != "full" || b.SnapshotID == "" {
		return 0, errors.New("run has no full SQL Server backup")
	}
	if b.Expired {
		return 0, errors.New("this backup was removed by the retention policy")
	}
	var point *SQLRestorePoint
	for _, p := range s.sqlRestorePoints(ctx, b) {
		if strings.EqualFold(p.Name, o.Database) {
			point = &p
		}
	}
	if point == nil {
		return 0, fmt.Errorf("database %s is not in this backup", o.Database)
	}
	o.Target = strings.TrimSpace(o.Target)
	if o.Database == pgsql.Cluster {
		o.Target, o.Replace = "", false
	}
	if len(o.Target) > 128 || strings.ContainsRune(o.Target, 0) {
		return 0, errors.New("invalid database name")
	}
	if strings.EqualFold(o.Target, o.Database) {
		o.Target = ""
	}
	if !o.StopAt.IsZero() {
		if o.StopAt.Before(point.Full) || point.LogUntil.IsZero() || o.StopAt.After(point.LogUntil) {
			return 0, fmt.Errorf("choose a point in time between %s and %s", point.Full.Format("2006-01-02 15:04:05"), point.LogUntil.Format("2006-01-02 15:04:05"))
		}
	}
	a, err := s.GetAgent(ctx, agentID)
	if err != nil {
		return 0, errors.New("unknown agent")
	}
	if err := sqlAgentOK(a, d.Engine); err != nil {
		return 0, err
	}
	if err := (JobOptions{SQLEngine: d.Engine, SQLInstance: instance}).Validate(); err != nil {
		return 0, err
	}
	if d.Engine == "" && (len(instance) > 100 || strings.ContainsAny(instance, " \\/:;\"'")) {
		return 0, errors.New("enter the instance name only, e.g. SQLEXPRESS")
	}
	if o.LogSnapshots != nil {
		o.LogSnapshots = []string{}
		logs, err := s.sqlLogRuns(ctx, b)
		if err != nil {
			return 0, err
		}
		for _, lr := range logs {
			o.LogSnapshots = append(o.LogSnapshots, lr.SnapshotID)
		}
	}
	opts, _ := json.Marshal(api.SQLRun{Engine: d.Engine, Instance: instance, Restore: &o})
	var id int64
	err = s.db.QueryRow(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, target_id, snapshot_id, vm_restore)
		VALUES($1,$2,'sql-restore','manual',$3,$4,$5,$6) RETURNING id`, agentID, b.JobID, b.RepoURL, b.TargetID, b.SnapshotID, opts).Scan(&id)
	return id, err
}

func sqlRestoreOptions(run Run) *api.SQLRun {
	if run.Kind != api.KindSQLRestore || len(run.VMRestore) == 0 {
		return nil
	}
	var o api.SQLRun
	if json.Unmarshal(run.VMRestore, &o) != nil {
		return nil
	}
	return &o
}

// jobSQLInstance is the instance of the SQL Server job of a run.
func (s *Store) jobSQLInstance(ctx context.Context, run Run) string {
	if run.JobID == nil {
		return ""
	}
	j, err := s.GetJob(ctx, *run.JobID)
	if err != nil {
		return ""
	}
	return j.Options.SQLInstance
}

// sqlAgentOK checks that an agent can run jobs and restores of a database
// engine: SQL Server on Windows, PostgreSQL and MySQL on Linux.
func sqlAgentOK(a Agent, engine string) error {
	windows := strings.Contains(strings.ToLower(a.OS), "windows")
	switch {
	case a.Recovery:
		return fmt.Errorf("%s is a recovery environment", a.Hostname)
	case engine == "" && !windows:
		return fmt.Errorf("%s is not a Windows machine; SQL Server jobs run on the Windows machine with SQL Server", a.Hostname)
	case engine != "" && windows:
		return fmt.Errorf("%s is a Windows machine; %s jobs run on the Linux database server", a.Hostname, engineTitle(engine))
	}
	return nil
}

func engineTitle(e string) string {
	return map[string]string{"": "SQL Server", "postgres": "PostgreSQL", "mysql": "MySQL/MariaDB"}[e]
}

// sqlRestoreAgents are the agents a database of this backup can be
// restored on.
func sqlRestoreAgents(agents []Agent, d *repo.SQLBackup) []Agent {
	if d == nil {
		return nil
	}
	var out []Agent
	for _, a := range agents {
		if sqlAgentOK(a, d.Engine) == nil {
			out = append(out, a)
		}
	}
	return out
}

// pgRestorePoints: every database (from its pg_dump) and the whole
// cluster; WAL covers all of them alike, as far as it is complete.
func pgRestorePoints(d *repo.SQLBackup, logs []Run) []SQLRestorePoint {
	var cl *repo.SQLDatabase
	for i := range d.Databases {
		if d.Databases[i].Name == pgsql.Cluster {
			cl = &d.Databases[i]
		}
	}
	if cl == nil {
		return nil
	}
	var until time.Time
	next := pgsql.NextSegment(cl.LastLSN)
	for _, lr := range logs {
		ld := sqlDetails(lr)
		if ld == nil || ld.Engine != "postgres" {
			continue
		}
		for _, w := range ld.Databases {
			if w.Name != pgsql.WAL || w.LastLSN < cl.FirstLSN {
				continue
			}
			if next != "" && w.FirstLSN > next {
				return pgPoints(d, cl, until)
			}
			until, next = w.Finish, pgsql.NextSegment(w.LastLSN)
		}
	}
	return pgPoints(d, cl, until)
}

func pgPoints(d *repo.SQLBackup, cl *repo.SQLDatabase, until time.Time) []SQLRestorePoint {
	var out []SQLRestorePoint
	for _, db := range d.Databases {
		if strings.HasPrefix(db.Name, "(") {
			continue
		}
		out = append(out, SQLRestorePoint{Name: db.Name, Full: cl.Finish, LogUntil: until})
	}
	return append(out, SQLRestorePoint{Name: pgsql.Cluster, Full: cl.Finish, LogUntil: until})
}

// mysqlRestorePoints: each database from its dump, then the binary logs
// from the dump's position on, as far as they are complete.
func mysqlRestorePoints(d *repo.SQLBackup, logs []Run) []SQLRestorePoint {
	var out []SQLRestorePoint
	for _, db := range d.Databases {
		if strings.HasPrefix(db.Name, "(") {
			continue
		}
		p := SQLRestorePoint{Name: db.Name, Full: db.Finish}
		if f, _, ok := strings.Cut(db.FirstLSN, ":"); ok {
			want := f
		chain:
			for _, lr := range logs {
				ld := sqlDetails(lr)
				if ld == nil || ld.Engine != "mysql" {
					continue
				}
				for _, b := range ld.Databases {
					if b.Name != mysqldb.Binlog || b.LastLSN < want {
						continue
					}
					if b.FirstLSN > want {
						break chain
					}
					p.LogUntil, want = b.Finish, mysqldb.NextBinlog(b.LastLSN)
				}
			}
		}
		out = append(out, p)
	}
	return out
}

// handleSQLRestore handles the SQL Server restore form.
func (s *Server) handleSQLRestore(w http.ResponseWriter, r *http.Request, runID int64, back string) {
	o := api.SQLRestore{Database: r.FormValue("database"), Target: r.FormValue("target")}
	o.Replace = r.FormValue("replace") == "on"
	if r.FormValue("target_mode") == "original" || o.Database == pgsql.Cluster {
		o.Target = ""
	} else if strings.TrimSpace(o.Target) == "" {
		redirectErr(w, r, back, errors.New("enter the name of the new database"))
		return
	}
	switch r.FormValue("point") {
	case "latest":
		o.LogSnapshots = []string{}
	case "time":
		v := r.FormValue("stop_at")
		t, err := time.ParseInLocation("2006-01-02T15:04:05", v, time.Local)
		if err != nil {
			if t, err = time.ParseInLocation("2006-01-02T15:04", v, time.Local); err != nil {
				redirectErr(w, r, back, errors.New("enter the point in time"))
				return
			}
		}
		o.StopAt, o.LogSnapshots = t, []string{}
	}
	rid, err := s.store.QueueSQLRestore(r.Context(), runID, formID(r, "agent_id"), strings.TrimSpace(r.FormValue("instance")), o)
	if err != nil {
		redirectErr(w, r, back, err)
		return
	}
	s.audit(r, "restore.sql", "run #%d: database %s from backup #%d as %q (replace %v, point %s)", rid, o.Database, runID, o.Target, o.Replace, r.FormValue("point"))
	redirectMsg(w, r, fmt.Sprintf("/runs/%d", rid), "Database restore queued.")
}
