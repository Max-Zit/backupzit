package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/mssql"
	"github.com/max-zit/backupzit/internal/mysqldb"
	"github.com/max-zit/backupzit/internal/pgsql"
	"github.com/max-zit/backupzit/internal/repo"
)

// sqlBackup backs up SQL Server databases (full, or transaction logs).
func (a *Agent) sqlBackup(ctx context.Context, run api.Run) api.RunResult {
	o := run.SQL
	if o == nil {
		o = &api.SQLRun{}
	}
	r, closeRepo, err := a.openRepo(ctx, run.Repository, true)
	if err != nil {
		return failed(fmt.Errorf("open repository: %w", err))
	}
	defer closeRepo()
	lock, err := r.Lock(ctx, false, lockWait)
	if err != nil {
		return failed(err)
	}
	defer lock.Unlock()
	kind := "full"
	if run.Kind == api.KindSQLLog {
		kind = "log"
	}
	logf := func(msg string, kv ...any) { a.log.Info(msg, append([]any{"run", run.ID}, kv...)...) }
	prog := func(_ string, s *repo.SnapshotStats) { a.progress(s.BytesRead, 0, s.Files) }
	tags := []string{fmt.Sprintf("run:%d", run.ID), jobTag(run.JobID)}
	var sn *repo.Snapshot
	var problems []string
	switch o.Engine {
	case "postgres":
		port, _ := strconv.Atoi(o.Instance)
		sn, problems, err = pgsql.BackupToRepo(ctx, r, pgsql.BackupOptions{Port: port, Databases: o.Databases, Kind: kind, PITR: o.Logs,
			Version: a.version, Tags: tags, Log: logf, Progress: prog})
	case "mysql":
		mo := mysqldb.BackupOptions{Databases: o.Databases, Kind: kind, PITR: o.Logs, Version: a.version, Tags: tags, Log: logf, Progress: prog}
		if kind == "log" {
			// Where the stored binary logs of this job end.
			all, lerr := r.ListSnapshots(ctx)
			if lerr != nil {
				return failed(lerr)
			}
			var mine []*repo.Snapshot
			for _, s := range all {
				if hasTag(s, jobTag(run.JobID)) {
					mine = append(mine, s)
				}
			}
			mo.Previous, mo.From = mysqldb.StoredState(mine)
		}
		sn, problems, err = mysqldb.BackupToRepo(ctx, r, mo)
	default:
		sn, problems, err = mssql.BackupToRepo(ctx, r, mssql.BackupOptions{Instance: o.Instance, Databases: o.Databases, System: o.System,
			Kind: kind, CopyOnly: !o.Logs, Version: a.version, Tags: tags, Log: logf, Progress: prog})
	}
	if err != nil {
		res := failed(err)
		res.Errors = problems
		return res
	}
	if sn == nil {
		res := api.RunResult{Status: api.StatusSuccess, Message: "No database needed a log backup", Errors: problems}
		if len(problems) > 0 {
			res.Status, res.Message = api.StatusWarning, strings.Join(problems, "; ")
		}
		return res
	}
	stats, _ := json.Marshal(sn.Stats)
	details, _ := json.Marshal(map[string]any{"sql": sn.SQL})
	res := api.RunResult{Status: api.StatusSuccess, SnapshotID: sn.ID.String(), Stats: stats, Details: details, Errors: problems}
	var size uint64
	dbs := 0
	for _, d := range sn.SQL.Databases {
		size += d.Size
		if !strings.HasPrefix(d.Name, "(") {
			dbs++
		}
	}
	switch {
	case kind == "log" && o.Engine == "mysql":
		res.Message = fmt.Sprintf("Stored binary logs %s to %s (%s)", sn.SQL.Databases[0].FirstLSN, sn.SQL.Databases[0].LastLSN, humanSize(size))
	case kind == "log" && o.Engine == "postgres":
		res.Message = fmt.Sprintf("Archived WAL %s to %s (%s)", sn.SQL.Databases[0].FirstLSN, sn.SQL.Databases[0].LastLSN, humanSize(size))
	case kind == "log":
		res.Message = fmt.Sprintf("Backed up the transaction logs of %d databases (%s)", len(sn.SQL.Databases), humanSize(size))
	case o.Engine == "postgres":
		res.Message = fmt.Sprintf("Backed up the PostgreSQL cluster and %d databases (%s)", dbs, humanSize(size))
	default:
		res.Message = fmt.Sprintf("Backed up %d databases (%s)", len(sn.SQL.Databases), humanSize(size))
	}
	if len(problems) > 0 {
		res.Status = api.StatusWarning
		res.Message += ". Problems: " + strings.Join(problems, "; ")
	}
	if kind == "log" {
		return res
	}
	lock.Unlock() // retention needs an exclusive lock
	return a.withRetention(ctx, r, run, res)
}

// sqlRestore restores one database from a full backup and log backups.
func (a *Agent) sqlRestore(ctx context.Context, run api.Run) api.RunResult {
	if run.SQL == nil || run.SQL.Restore == nil {
		return failed(fmt.Errorf("options missing"))
	}
	o := run.SQL.Restore
	r, closeRepo, err := a.openRepo(ctx, run.Repository, false)
	if err != nil {
		return failed(fmt.Errorf("open repository: %w", err))
	}
	defer closeRepo()
	full, err := r.LoadSnapshot(ctx, run.SnapshotID)
	if err != nil {
		return failed(err)
	}
	var logs []*repo.Snapshot
	for _, id := range o.LogSnapshots {
		sn, err := r.LoadSnapshot(ctx, id)
		if err != nil {
			return failed(fmt.Errorf("log backup %s: %w", id, err))
		}
		logs = append(logs, sn)
	}
	logf := func(msg string, kv ...any) { a.log.Info(msg, append([]any{"run", run.ID}, kv...)...) }
	var n int
	if run.SQL.Engine == "postgres" {
		port, _ := strconv.Atoi(run.SQL.Instance)
		pr, err := pgsql.RestoreFromRepo(ctx, r, full, logs, pgsql.RestoreOptions{Port: port, Database: o.Database, Target: o.Target, Replace: o.Replace,
			PointInTime: o.LogSnapshots != nil || !o.StopAt.IsZero(), StopAt: o.StopAt, Log: logf})
		if err != nil {
			return failed(err)
		}
		if o.Database == pgsql.Cluster {
			msg := fmt.Sprintf("The cluster was restored as a separate PostgreSQL instance on port %d, data directory %s", pr.Port, pr.DataDir)
			if !o.StopAt.IsZero() {
				msg += ", as of " + o.StopAt.Local().Format("2006-01-02 15:04:05 MST")
			}
			msg += fmt.Sprintf(". Connect with: sudo -u postgres psql -h %s -p %d. Stop it with: sudo -u postgres pg_ctl -D %s stop", filepath.Dir(pr.DataDir), pr.Port, pr.DataDir)
			return api.RunResult{Status: api.StatusSuccess, Message: msg}
		}
		n = pr.WALFiles
	} else if run.SQL.Engine == "mysql" {
		n, err = mysqldb.RestoreFromRepo(ctx, r, full, logs, mysqldb.RestoreOptions{Database: o.Database, Target: o.Target, Replace: o.Replace,
			PointInTime: o.LogSnapshots != nil || !o.StopAt.IsZero(), StopAt: o.StopAt, Log: logf})
		if err != nil {
			return failed(err)
		}
	} else {
		n, err = mssql.RestoreFromRepo(ctx, r, full, logs, mssql.RepoRestoreOptions{Instance: run.SQL.Instance, Database: o.Database, Target: o.Target,
			Replace: o.Replace, StopAt: o.StopAt, Log: logf})
		if err != nil {
			return failed(err)
		}
	}
	target := o.Target
	if target == "" {
		target = o.Database
	}
	msg := fmt.Sprintf("Database %s restored from the full backup", target)
	if n > 0 {
		msg += fmt.Sprintf(" and %d log backups", n)
	}
	if !o.StopAt.IsZero() {
		msg += ", to " + o.StopAt.Local().Format("2006-01-02 15:04:05 MST")
	}
	return api.RunResult{Status: api.StatusSuccess, Message: msg}
}
