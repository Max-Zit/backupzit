package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/mysqldb"
	"github.com/max-zit/backupzit/internal/repo"
)

// cmdMySQL handles: mysql list|backup|restore (MySQL or MariaDB on this
// machine, as root over the local socket).
func cmdMySQL(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: backupzit-agent mysql list|backup|restore [options]")
	}
	switch args[0] {
	case "list":
		s, err := mysqldb.Open(ctx)
		if err != nil {
			return err
		}
		dbs, err := s.Databases(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("%s, binary log %s\n", s.Version, map[bool]string{true: "on (point-in-time recovery possible)", false: "off"}[s.LogBin])
		for _, d := range dbs {
			fmt.Println(" ", d)
		}
		return nil
	case "backup":
		fs := flag.NewFlagSet("mysql backup", flag.ExitOnError)
		rf := addRepoFlags(fs)
		dbs := fs.String("db", "", "comma-separated databases (default: all user databases)")
		binlog := fs.Bool("binlog", false, "store the binary logs closed since the last run instead of dumps")
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
		o := mysqldb.BackupOptions{Kind: "full", PITR: true, Tags: []string{"cli"}, Version: version,
			Log: func(msg string, kv ...any) { fmt.Println(append([]any{" ", msg}, kv...)...) }}
		if *dbs != "" {
			o.Databases = strings.Split(*dbs, ",")
		}
		if *binlog {
			o.Kind = "log"
			sns, err := r.ListSnapshots(ctx)
			if err != nil {
				return err
			}
			o.Previous, o.From = mysqldb.StoredState(sns)
		}
		sn, problems, err := mysqldb.BackupToRepo(ctx, r, o)
		for _, p := range problems {
			fmt.Println("  note:", p)
		}
		if err != nil {
			return err
		}
		if sn == nil {
			fmt.Println("nothing to store")
			return nil
		}
		for _, d := range sn.SQL.Databases {
			fmt.Printf("  %-20s %10s  %s %s\n", d.Name, humanBytes(d.Size), d.FirstLSN, d.LastLSN)
		}
		fmt.Printf("snapshot %s\n", sn.ID.Short())
		return nil
	case "restore":
		fs := flag.NewFlagSet("mysql restore", flag.ExitOnError)
		rf := addRepoFlags(fs)
		db := fs.String("db", "", "database in the backup")
		target := fs.String("as", "", "name of the restored database (default: the original name)")
		replace := fs.Bool("replace", false, "replace an existing database with that name")
		at := fs.String("at", "", `point in time, "2006-01-02 15:04:05" (local time; needs the binary logs)`)
		latest := fs.Bool("latest", false, "replay all stored binary logs after the dump")
		fs.Parse(args[1:])
		if *db == "" || fs.NArg() != 1 {
			return errors.New(`usage: backupzit-agent mysql restore --repo R --db NAME [--as NEW] [--replace] [--at "YYYY-MM-DD HH:MM:SS" | --latest] FULL-SNAPSHOT|latest`)
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
				if sn.SQL != nil && sn.SQL.Engine == "mysql" && sn.SQL.Kind == "full" && (stop.IsZero() || sn.Time.Before(stop)) {
					full = sn
				}
			}
			if full == nil {
				return errors.New("no MySQL/MariaDB backup found")
			}
		} else if full, err = r.LoadSnapshot(ctx, fs.Arg(0)); err != nil {
			return err
		}
		var logs []*repo.Snapshot
		for _, sn := range sns {
			if sn.SQL != nil && sn.SQL.Engine == "mysql" && sn.SQL.Kind == "log" && sn.Time.After(full.Time) {
				logs = append(logs, sn)
			}
		}
		n, err := mysqldb.RestoreFromRepo(ctx, r, full, logs, mysqldb.RestoreOptions{Database: *db, Target: *target, Replace: *replace,
			PointInTime: *latest || !stop.IsZero(), StopAt: stop, Log: func(msg string, kv ...any) { fmt.Println(append([]any{" ", msg}, kv...)...) }})
		if err != nil {
			return err
		}
		fmt.Printf("database restored (%d binary logs replayed)\n", n)
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}
