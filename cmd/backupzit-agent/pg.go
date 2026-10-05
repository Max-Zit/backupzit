package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/pgsql"
	"github.com/max-zit/backupzit/internal/repo"
)

// cmdPG handles: pg list|backup|restore (PostgreSQL on this machine, as the
// postgres account).
func cmdPG(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: backupzit-agent pg list|backup|restore [options]")
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("pg list", flag.ExitOnError)
		port := fs.Int("port", 5432, "PostgreSQL port")
		fs.Parse(args[1:])
		s, err := pgsql.Open(ctx, *port)
		if err != nil {
			return err
		}
		dbs, err := s.Databases(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("PostgreSQL %s, data directory %s\n", s.Version, s.DataDir)
		for _, d := range dbs {
			fmt.Printf("  %-30s %10s\n", d.Name, humanBytes(d.Size))
		}
		if ok, problem, err := s.ArchiveState(ctx); err == nil && !ok {
			fmt.Println("WAL archiving for point-in-time recovery: off (" + problem + ")")
		} else if ok {
			fmt.Println("WAL archiving for point-in-time recovery: on")
		}
		return nil
	case "backup":
		fs := flag.NewFlagSet("pg backup", flag.ExitOnError)
		rf := addRepoFlags(fs)
		port := fs.Int("port", 5432, "PostgreSQL port")
		dbs := fs.String("db", "", "comma-separated databases to dump (default: all)")
		wal := fs.Bool("wal", false, "store the archived WAL instead of a full backup")
		pitr := fs.Bool("pitr", false, "full backup: set up WAL archiving for point-in-time recovery")
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
		if *wal {
			kind = "log"
		}
		var list []string
		if *dbs != "" {
			list = strings.Split(*dbs, ",")
		}
		sn, problems, err := pgsql.BackupToRepo(ctx, r, pgsql.BackupOptions{Port: *port, Databases: list, Kind: kind, PITR: *pitr || *wal,
			Tags: []string{"cli"}, Version: version, Log: func(msg string, kv ...any) { fmt.Println(append([]any{" ", msg}, kv...)...) }})
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
		fs := flag.NewFlagSet("pg restore", flag.ExitOnError)
		rf := addRepoFlags(fs)
		port := fs.Int("port", 5432, "PostgreSQL port to restore a database into")
		db := fs.String("db", "", "database in the backup, or "+pgsql.Cluster+" for the whole cluster (as a separate instance)")
		target := fs.String("as", "", "name of the restored database (default: the original name)")
		replace := fs.Bool("replace", false, "replace an existing database with that name")
		at := fs.String("at", "", `point in time, "2006-01-02 15:04:05" (local time; needs archived WAL)`)
		latest := fs.Bool("latest", false, "replay all archived WAL after the full backup")
		fs.Parse(args[1:])
		if *db == "" || fs.NArg() != 1 {
			return errors.New(`usage: backupzit-agent pg restore --repo R --db NAME [--as NEW] [--replace] [--at "YYYY-MM-DD HH:MM:SS" | --latest] FULL-SNAPSHOT|latest`)
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
				if sn.SQL != nil && sn.SQL.Engine == "postgres" && sn.SQL.Kind == "full" && (stop.IsZero() || sn.Time.Before(stop)) {
					full = sn
				}
			}
			if full == nil {
				return errors.New("no full PostgreSQL backup found")
			}
		} else if full, err = r.LoadSnapshot(ctx, fs.Arg(0)); err != nil {
			return err
		}
		var wals []*repo.Snapshot
		for _, sn := range sns {
			if sn.SQL != nil && sn.SQL.Engine == "postgres" && sn.SQL.Kind == "log" && sn.Time.After(full.Time) {
				wals = append(wals, sn)
			}
		}
		res, err := pgsql.RestoreFromRepo(ctx, r, full, wals, pgsql.RestoreOptions{Port: *port, Database: *db, Target: *target, Replace: *replace,
			PointInTime: *latest || !stop.IsZero(), StopAt: stop, Log: func(msg string, kv ...any) { fmt.Println(append([]any{" ", msg}, kv...)...) }})
		if err != nil {
			return err
		}
		if *db == pgsql.Cluster {
			fmt.Printf("cluster running on port %d, data directory %s\n", res.Port, res.DataDir)
			return nil
		}
		fmt.Printf("database restored (%d WAL backups replayed)\n", res.WALFiles)
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}
