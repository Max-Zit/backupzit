package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/mssql"
	"github.com/max-zit/backupzit/internal/repo"
)

// cmdSQL handles: sql list|backup|show|restore (Microsoft SQL Server on this
// machine, as the agent's account).
func cmdSQL(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: backupzit-agent sql list|backup|show|restore [options]")
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("sql list", flag.ExitOnError)
		inst := fs.String("instance", "", "instance name (default instance if empty), e.g. SQLEXPRESS")
		fs.Parse(args[1:])
		srv, err := mssql.Open(ctx, *inst)
		if err != nil {
			return err
		}
		defer srv.Close()
		dbs, err := srv.Databases(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("%-30s %-12s %-10s %10s\n", "DATABASE", "RECOVERY", "STATE", "SIZE")
		for _, d := range dbs {
			name := d.Name
			if d.System {
				name += " (system)"
			}
			fmt.Printf("%-30s %-12s %-10s %10s\n", name, d.Recovery, d.State, humanBytes(d.Size))
		}
		return nil
	case "backup":
		fs := flag.NewFlagSet("sql backup", flag.ExitOnError)
		rf := addRepoFlags(fs)
		inst := fs.String("instance", "", "instance name (default instance if empty)")
		dbs := fs.String("db", "", "comma-separated databases (default: all user databases)")
		system := fs.Bool("system", false, "also back up master, model and msdb")
		logs := fs.Bool("log", false, "back up the transaction logs instead of full backups")
		copyOnly := fs.Bool("copy-only", false, "full backups that do not affect other backup tools (no log backups possible after them)")
		fs.Parse(args[1:])
		r, err := rf.open(ctx)
		if err != nil {
			return err
		}
		defer r.Close()
		lock, err := r.Lock(ctx, false, time.Minute)
		if err != nil {
			return err
		}
		defer lock.Unlock()
		kind := "full"
		if *logs {
			kind = "log"
		}
		var list []string
		if *dbs != "" {
			list = strings.Split(*dbs, ",")
		}
		sn, problems, err := mssql.BackupToRepo(ctx, r, mssql.BackupOptions{Instance: *inst, Databases: list, System: *system, Kind: kind, CopyOnly: *copyOnly,
			Tags: []string{"cli"}, Version: version, Log: func(msg string, kv ...any) { fmt.Println(append([]any{" ", msg}, kv...)...) }})
		for _, p := range problems {
			fmt.Println("  problem:", p)
		}
		if err != nil {
			return err
		}
		if sn == nil {
			fmt.Println("nothing to back up")
			return nil
		}
		fmt.Printf("snapshot %s: %d databases (%s backup)\n", sn.ID.Short(), len(sn.SQL.Databases), kind)
		return nil
	case "show":
		fs := flag.NewFlagSet("sql show", flag.ExitOnError)
		rf := addRepoFlags(fs)
		fs.Parse(args[1:])
		r, err := rf.open(ctx)
		if err != nil {
			return err
		}
		defer r.Close()
		sns, err := r.ListSnapshots(ctx)
		if err != nil {
			return err
		}
		for _, sn := range sns {
			if sn.SQL == nil {
				continue
			}
			for _, d := range sn.SQL.Databases {
				fmt.Printf("%s  %-4s  %-25s  %s - %s  LSN %s-%s\n", sn.ID.Short(), sn.SQL.Kind, d.Name,
					d.Start.Format("2006-01-02 15:04:05"), d.Finish.Format("15:04:05"), d.FirstLSN, d.LastLSN)
			}
		}
		return nil
	case "restore":
		fs := flag.NewFlagSet("sql restore", flag.ExitOnError)
		rf := addRepoFlags(fs)
		inst := fs.String("instance", "", "instance to restore into")
		db := fs.String("db", "", "database in the backup")
		target := fs.String("as", "", "name of the restored database (default: the original name)")
		replace := fs.Bool("replace", false, "overwrite an existing database with that name")
		at := fs.String("at", "", `point in time, "2006-01-02 15:04:05" (local time; needs log backups)`)
		fs.Parse(args[1:])
		if *db == "" || fs.NArg() != 1 {
			return errors.New(`usage: backupzit-agent sql restore --repo R --db NAME [--as NEW] [--replace] [--at "YYYY-MM-DD HH:MM:SS"] FULL-SNAPSHOT|latest`)
		}
		var stop time.Time
		if *at != "" {
			t, err := time.ParseInLocation("2006-01-02 15:04:05", *at, time.Local)
			if err != nil {
				return fmt.Errorf("--at: %w", err)
			}
			stop = t
		}
		r, err := rf.open(ctx)
		if err != nil {
			return err
		}
		defer r.Close()
		sns, err := r.ListSnapshots(ctx)
		if err != nil {
			return err
		}
		var full *repo.Snapshot
		if fs.Arg(0) == "latest" {
			for _, sn := range sns {
				if sn.SQL != nil && sn.SQL.Kind == "full" && hasDB(sn, *db) && (stop.IsZero() || sn.Time.Before(stop)) {
					full = sn
				}
			}
			if full == nil {
				return fmt.Errorf("no full backup of %s found", *db)
			}
		} else if full, err = r.LoadSnapshot(ctx, fs.Arg(0)); err != nil {
			return err
		}
		var logs []*repo.Snapshot
		for _, sn := range sns {
			if sn.SQL != nil && sn.SQL.Kind == "log" && sn.Time.After(full.Time) {
				logs = append(logs, sn)
			}
		}
		n, err := mssql.RestoreFromRepo(ctx, r, full, logs, mssql.RepoRestoreOptions{Instance: *inst, Database: *db, Target: *target, Replace: *replace, StopAt: stop,
			Log: func(msg string, kv ...any) { fmt.Println(append([]any{" ", msg}, kv...)...) }})
		if err != nil {
			return err
		}
		name := *target
		if name == "" {
			name = *db
		}
		fmt.Printf("database %s restored from %s and %d log backups\n", name, full.ID.Short(), n)
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func hasDB(sn *repo.Snapshot, name string) bool {
	for _, d := range sn.SQL.Databases {
		if strings.EqualFold(d.Name, name) {
			return true
		}
	}
	return false
}
