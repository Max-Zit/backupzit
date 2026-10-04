package mssql

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/archiver"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/restorer"
)

// BackupOptions select what BackupToRepo backs up.
type BackupOptions struct {
	Instance  string
	Databases []string // empty: all user databases
	System    bool     // with no Databases: also master, model and msdb
	Kind      string   // "full" or "log"
	// CopyOnly takes full backups that do not reset other tools'
	// differential base; off when BackupZit also backs up the logs.
	CopyOnly bool
	Tags     []string
	Version  string
	Log      func(msg string, args ...any)
	Progress func(path string, s *repo.SnapshotStats)
}

// BackupToRepo backs up databases with native BACKUP statements into a
// folder SQL Server can write, stores the files in a snapshot of r and
// deletes them. It returns the snapshot (nil when there was nothing to
// back up) and per-database problems.
func BackupToRepo(ctx context.Context, r *repo.Repository, o BackupOptions) (*repo.Snapshot, []string, error) {
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	srv, err := Open(ctx, o.Instance)
	if err != nil {
		return nil, nil, err
	}
	defer srv.Close()
	all, err := srv.Databases(ctx)
	if err != nil {
		return nil, nil, err
	}
	var problems []string
	var dbs []Database
	if len(o.Databases) > 0 {
		for _, want := range o.Databases {
			found := false
			for _, d := range all {
				if strings.EqualFold(d.Name, want) {
					dbs, found = append(dbs, d), true
				}
			}
			if !found {
				problems = append(problems, fmt.Sprintf("database %s does not exist", want))
			}
		}
	} else {
		for _, d := range all {
			if !d.System || o.System {
				dbs = append(dbs, d)
			}
		}
	}
	dir, err := srv.BackupDir(ctx)
	if err != nil {
		return nil, problems, err
	}
	stage := filepath.Join(dir, fmt.Sprintf("backupzit-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return nil, problems, err
	}
	defer os.RemoveAll(stage)
	meta := &repo.SQLBackup{Instance: srv.Instance, Kind: o.Kind}
	ext := ".bak"
	if o.Kind == "log" {
		ext = ".trn"
	}
	for _, d := range dbs {
		if d.State != "ONLINE" {
			problems = append(problems, fmt.Sprintf("database %s is %s", d.Name, strings.ToLower(d.State)))
			continue
		}
		if o.Kind == "log" && (d.Recovery == "SIMPLE" || strings.EqualFold(d.Name, "master")) {
			continue // no log backups in the simple recovery model
		}
		file := filepath.Join(stage, safeFile(d.Name)+ext)
		o.Log("backing up", "database", d.Name, "kind", o.Kind)
		info, err := srv.Backup(ctx, d.Name, o.Kind, file, o.CopyOnly && o.Kind == "full")
		if err != nil {
			if o.Kind == "log" && NoFullBackup(err) {
				problems = append(problems, fmt.Sprintf("database %s: no full backup yet that starts the log chain; run the job's full backup first", d.Name))
				continue
			}
			problems = append(problems, fmt.Sprintf("database %s: %v", d.Name, err))
			continue
		}
		meta.Databases = append(meta.Databases, repo.SQLDatabase{Name: d.Name, File: file, Recovery: d.Recovery, Size: info.Size,
			FirstLSN: info.FirstLSN, LastLSN: info.LastLSN, CheckpointLSN: info.CheckpointLSN, DatabaseLSN: info.DatabaseLSN,
			Start: info.Start, Finish: info.Finish})
	}
	if len(meta.Databases) == 0 {
		if len(problems) > 0 && o.Kind == "full" {
			return nil, problems, errors.New("no database was backed up")
		}
		return nil, problems, nil
	}
	sn, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{stage}, NoParent: true, Version: o.Version, Tags: o.Tags, SQL: meta, Progress: o.Progress})
	if err != nil {
		return nil, problems, err
	}
	return sn, problems, nil
}

// RestoreOptions select what RestoreFromRepo restores.
type RepoRestoreOptions struct {
	Instance string
	Database string // in the backup
	Target   string // name of the restored database ("" = Database)
	Replace  bool
	StopAt   time.Time // zero: the end of the last log backup given
	Log      func(msg string, args ...any)
}

// RestoreFromRepo restores a database from a full backup snapshot and the
// log backup snapshots after it (in order). Only the logs needed up to
// StopAt are applied. It returns how many log backups were applied.
func RestoreFromRepo(ctx context.Context, r *repo.Repository, full *repo.Snapshot, logs []*repo.Snapshot, o RepoRestoreOptions) (int, error) {
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	if o.Target == "" {
		o.Target = o.Database
	}
	fdb := findDB(full, o.Database)
	if fdb == nil || full.SQL.Kind != "full" {
		return 0, fmt.Errorf("the backup has no full backup of database %s", o.Database)
	}
	if !o.StopAt.IsZero() && o.StopAt.Before(fdb.Finish) {
		return 0, fmt.Errorf("the full backup ended at %s; choose a later point in time or an earlier backup", fdb.Finish.Format("2006-01-02 15:04:05"))
	}
	type piece struct {
		sn *repo.Snapshot
		db *repo.SQLDatabase
	}
	var chain []piece
	last := fdb.LastLSN
	for _, sn := range logs {
		ldb := findDB(sn, o.Database)
		if ldb == nil || sn.SQL.Kind != "log" || !LSNLess(last, ldb.LastLSN) {
			continue // not this database, or already covered by the full backup
		}
		if LSNLess(last, ldb.FirstLSN) {
			return 0, fmt.Errorf("the log chain of %s is broken before the log backup of %s (LSN %s missing); restore to an earlier point", o.Database, ldb.Finish.Format("2006-01-02 15:04"), last)
		}
		chain = append(chain, piece{sn, ldb})
		last = ldb.LastLSN
		if !o.StopAt.IsZero() && !ldb.Finish.Before(o.StopAt) {
			break // this log covers the point in time
		}
	}
	if !o.StopAt.IsZero() && (len(chain) == 0 || chain[len(chain)-1].db.Finish.Before(o.StopAt)) {
		end := fdb.Finish
		if len(chain) > 0 {
			end = chain[len(chain)-1].db.Finish
		}
		if o.StopAt.After(end) {
			return 0, fmt.Errorf("the backups of %s reach until %s; choose an earlier point in time", o.Database, end.Format("2006-01-02 15:04:05"))
		}
	}
	srv, err := Open(ctx, o.Instance)
	if err != nil {
		return 0, err
	}
	defer srv.Close()
	dir, err := srv.BackupDir(ctx)
	if err != nil {
		return 0, err
	}
	stage := filepath.Join(dir, fmt.Sprintf("backupzit-restore-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return 0, err
	}
	defer os.RemoveAll(stage)
	fetch := func(sn *repo.Snapshot, db *repo.SQLDatabase, n int) (string, error) {
		sub := filepath.Join(stage, fmt.Sprintf("%03d", n))
		if _, err := restorer.Run(ctx, r, sn, restorer.Options{Target: sub, Include: []string{db.File}}); err != nil {
			return "", err
		}
		return findFile(sub, filepath.Base(db.File))
	}
	o.Log("reading the full backup", "database", o.Database)
	fullFile, err := fetch(full, fdb, 0)
	if err != nil {
		return 0, err
	}
	var logFiles []string
	for i, p := range chain {
		f, err := fetch(p.sn, p.db, i+1)
		if err != nil {
			return 0, err
		}
		logFiles = append(logFiles, f)
	}
	err = srv.Restore(ctx, RestoreOptions{Target: o.Target, Full: fullFile, Logs: logFiles, StopAt: o.StopAt, Replace: o.Replace, Log: o.Log})
	return len(logFiles), err
}

func findDB(sn *repo.Snapshot, name string) *repo.SQLDatabase {
	if sn == nil || sn.SQL == nil {
		return nil
	}
	for i := range sn.SQL.Databases {
		if strings.EqualFold(sn.SQL.Databases[i].Name, name) {
			return &sn.SQL.Databases[i]
		}
	}
	return nil
}

// findFile finds a restored file below dir (the restorer recreates the
// original path).
func findFile(dir, base string) (string, error) {
	var found string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.EqualFold(d.Name(), base) {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	if found == "" {
		return "", fmt.Errorf("%s was not restored", base)
	}
	return found, nil
}
