package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/mssql"
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
	sn, problems, err := mssql.BackupToRepo(ctx, r, mssql.BackupOptions{Instance: o.Instance, Databases: o.Databases, System: o.System,
		Kind: kind, CopyOnly: !o.Logs, Version: a.version, Tags: []string{fmt.Sprintf("run:%d", run.ID), jobTag(run.JobID)},
		Log:      func(msg string, kv ...any) { a.log.Info(msg, append([]any{"run", run.ID}, kv...)...) },
		Progress: func(_ string, s *repo.SnapshotStats) { a.progress(s.BytesRead, 0, s.Files) }})
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
	for _, d := range sn.SQL.Databases {
		size += d.Size
	}
	if kind == "log" {
		res.Message = fmt.Sprintf("Backed up the transaction logs of %d databases (%s)", len(sn.SQL.Databases), humanSize(size))
	} else {
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
	n, err := mssql.RestoreFromRepo(ctx, r, full, logs, mssql.RepoRestoreOptions{Instance: run.SQL.Instance, Database: o.Database, Target: o.Target,
		Replace: o.Replace, StopAt: o.StopAt, Log: func(msg string, kv ...any) { a.log.Info(msg, append([]any{"run", run.ID}, kv...)...) }})
	if err != nil {
		return failed(err)
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
		msg += ", to " + o.StopAt.Local().Format("2006-01-02 15:04:05")
	}
	return api.RunResult{Status: api.StatusSuccess, Message: msg}
}
