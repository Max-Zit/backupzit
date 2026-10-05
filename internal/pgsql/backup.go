package pgsql

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/archiver"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/restorer"
)

// Entries of a PostgreSQL backup besides the single databases.
const (
	Cluster = "(cluster)" // pg_basebackup of the whole cluster
	Globals = "(globals)" // roles and tablespaces (pg_dumpall --globals-only)
	WAL     = "(wal)"     // archived WAL segments
)

// BackupOptions select what BackupToRepo does.
type BackupOptions struct {
	Port      int
	Databases []string // empty: all databases
	Kind      string   // "full" or "log" (archived WAL)
	// PITR: archive WAL for point-in-time recovery (set up on full backups).
	PITR     bool
	Tags     []string
	Version  string
	Log      func(msg string, args ...any)
	Progress func(path string, s *repo.SnapshotStats)
}

var lsnRe = regexp.MustCompile(`write-ahead log (start|end) point: ([0-9A-F]+/[0-9A-F]+)`)

// BackupToRepo backs up the cluster (full: pg_basebackup plus a pg_dump of
// each database; log: the WAL segments archived since the last run) into a
// snapshot of r. It returns the snapshot (nil if there was nothing to store)
// and problems that did not stop the backup.
func BackupToRepo(ctx context.Context, r *repo.Repository, o BackupOptions) (*repo.Snapshot, []string, error) {
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	s, err := Open(ctx, o.Port)
	if err != nil {
		return nil, nil, err
	}
	stage, err := workDir(ctx, fmt.Sprintf("stage-%d", time.Now().UnixNano()))
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(stage)
	meta := &repo.SQLBackup{Engine: "postgres", Instance: strconv.Itoa(s.Port), Kind: o.Kind}
	var problems []string
	var stored []string // spool files to delete once stored
	switch o.Kind {
	case "log":
		if !o.PITR {
			return nil, nil, nil
		}
		if ok, problem, err := s.ArchiveState(ctx); err != nil {
			return nil, nil, err
		} else if !ok {
			return nil, []string{"WAL archiving is not active: " + problem}, nil
		}
		// Close the current segment so recent changes are archived.
		before := time.Now()
		s.Query(ctx, "", "SELECT pg_switch_wal()")
		waitArchived(ctx, s, before)
		dir := filepath.Join(stage, "wal")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, nil, err
		}
		entries, _ := os.ReadDir(WALSpool)
		var segs []string
		var size uint64
		for _, e := range entries {
			n := e.Name()
			if !IsSegment(n) && !strings.HasSuffix(n, ".history") && !strings.HasSuffix(n, ".backup") {
				continue
			}
			if err := copyFile(filepath.Join(WALSpool, n), filepath.Join(dir, n)); err != nil {
				return nil, nil, err
			}
			if fi, err := e.Info(); err == nil {
				size += uint64(fi.Size())
			}
			stored = append(stored, filepath.Join(WALSpool, n))
			if IsSegment(n) {
				segs = append(segs, n)
			}
		}
		if len(segs) == 0 {
			return nil, nil, nil
		}
		sort.Strings(segs)
		now := time.Now()
		meta.Databases = []repo.SQLDatabase{{Name: WAL, File: dir, FirstLSN: segs[0], LastLSN: segs[len(segs)-1], Size: size, Start: before, Finish: now}}
	default:
		meta.Kind = "full"
		if o.PITR {
			if ok, problem, err := s.ArchiveState(ctx); err != nil {
				return nil, nil, err
			} else if !ok {
				if strings.Contains(problem, "another tool") || strings.Contains(problem, "wal_level") {
					problems = append(problems, "point-in-time recovery is not possible: "+problem)
				} else if restart, err := s.EnableArchiving(ctx); err != nil {
					problems = append(problems, "could not set up WAL archiving: "+err.Error())
				} else if restart {
					problems = append(problems, "WAL archiving was set up (archive_mode = on); restart PostgreSQL once (systemctl restart postgresql) so that it starts — until then only full backups can be restored")
				}
			}
		}
		start := time.Now()
		o.Log("pg_basebackup", "port", s.Port)
		cdir := filepath.Join(stage, "cluster")
		out, err := asPostgres(ctx, s.bin("pg_basebackup"), "-p", strconv.Itoa(s.Port), "-D", cdir, "-Ft", "-X", "fetch", "-c", "fast", "-v", "--no-password")
		if err != nil {
			return nil, problems, err
		}
		_ = out
		// pg_basebackup reports the WAL range on stderr; read it from the
		// backup_manifest-independent log by asking the server instead.
		startLSN, endLSN := basebackupLSNs(cdir)
		first, _ := s.WALFileName(ctx, startLSN)
		last, _ := s.WALFileName(ctx, endLSN)
		var csize uint64
		filepath.WalkDir(cdir, func(_ string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if fi, err := d.Info(); err == nil {
					csize += uint64(fi.Size())
				}
			}
			return nil
		})
		meta.Databases = append(meta.Databases, repo.SQLDatabase{Name: Cluster, File: cdir, Recovery: s.Version, Size: csize,
			FirstLSN: first, LastLSN: last, Start: start, Finish: time.Now()})
		gfile := filepath.Join(stage, "globals.sql")
		if out, err := asPostgres(ctx, s.bin("pg_dumpall"), "-p", strconv.Itoa(s.Port), "--globals-only"); err == nil {
			os.WriteFile(gfile, out, 0o600)
			meta.Databases = append(meta.Databases, repo.SQLDatabase{Name: Globals, File: gfile, Size: uint64(len(out)), Start: time.Now(), Finish: time.Now()})
		} else {
			problems = append(problems, "roles were not saved: "+err.Error())
		}
		dbs, err := s.Databases(ctx)
		if err != nil {
			return nil, problems, err
		}
		ddir := filepath.Join(stage, "dumps")
		os.MkdirAll(ddir, 0o700)
		exec.CommandContext(ctx, "chown", "postgres:", ddir).Run()
		for _, d := range dbs {
			if len(o.Databases) > 0 && !containsFold(o.Databases, d.Name) {
				continue
			}
			f := filepath.Join(ddir, safeName(d.Name)+".dump")
			t0 := time.Now()
			o.Log("pg_dump", "database", d.Name)
			if _, err := asPostgres(ctx, s.bin("pg_dump"), "-p", strconv.Itoa(s.Port), "-Fc", "-Z", "0", "-f", f, d.Name); err != nil {
				problems = append(problems, fmt.Sprintf("database %s: %v", d.Name, err))
				continue
			}
			fi, _ := os.Stat(f)
			var size uint64
			if fi != nil {
				size = uint64(fi.Size())
			}
			meta.Databases = append(meta.Databases, repo.SQLDatabase{Name: d.Name, File: f, Size: size, FirstLSN: first, LastLSN: last, Start: t0, Finish: time.Now()})
		}
		for _, want := range o.Databases {
			found := false
			for _, d := range dbs {
				found = found || strings.EqualFold(d.Name, want)
			}
			if !found {
				problems = append(problems, fmt.Sprintf("database %s does not exist", want))
			}
		}
	}
	sn, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{stage}, NoParent: true, Version: o.Version, Tags: o.Tags, SQL: meta, Progress: o.Progress})
	if err != nil {
		return nil, problems, err
	}
	for _, f := range stored {
		os.Remove(f)
	}
	return sn, problems, nil
}

// basebackupLSNs reads the WAL range of a tar-format base backup from its
// backup_manifest.
func basebackupLSNs(dir string) (start, end string) {
	b, err := os.ReadFile(filepath.Join(dir, "backup_manifest"))
	if err != nil {
		return "", ""
	}
	m := regexp.MustCompile(`"Start-LSN": "([0-9A-F/]+)", "End-LSN": "([0-9A-F/]+)"`).FindSubmatch(b)
	if m == nil {
		return "", ""
	}
	return string(m[1]), string(m[2])
}

// waitArchived waits briefly until the archiver has handled the segment
// switched at t.
func waitArchived(ctx context.Context, s *Server, t time.Time) {
	for i := 0; i < 30; i++ {
		out, err := s.Query(ctx, "", "SELECT coalesce(extract(epoch from last_archived_time)::bigint, 0) FROM pg_stat_archiver")
		if err == nil {
			if n, _ := strconv.ParseInt(out, 10, 64); n >= t.Unix() {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// RestoreOptions select what RestoreFromRepo restores.
type RestoreOptions struct {
	Port     int    // the running cluster to restore a database into
	Database string // a database, or Cluster for the whole cluster
	Target   string // name of the restored database ("" = original)
	Replace  bool
	// PointInTime replays WAL up to StopAt (zero: as far as the archived
	// WAL reaches); without it a database comes from its pg_dump.
	PointInTime bool
	StopAt      time.Time
	Log         func(msg string, args ...any)
}

// RestoreResult reports where a restored cluster runs.
type RestoreResult struct {
	Database string
	DataDir  string // whole cluster: its data directory
	Port     int    // whole cluster: its port
	WALFiles int
}

// RestoreFromRepo restores a database (into the running cluster) or the
// whole cluster (as a separate instance on a free port) from a full backup
// snapshot and the WAL snapshots after it.
func RestoreFromRepo(ctx context.Context, r *repo.Repository, full *repo.Snapshot, wals []*repo.Snapshot, o RestoreOptions) (*RestoreResult, error) {
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	if full.SQL == nil || full.SQL.Engine != "postgres" || full.SQL.Kind != "full" {
		return nil, errors.New("not a full PostgreSQL backup")
	}
	cl := find(full, Cluster)
	if cl == nil {
		return nil, errors.New("the backup has no copy of the cluster")
	}
	s, err := Open(ctx, o.Port)
	if err != nil && o.Database != Cluster {
		return nil, err
	}
	work, err := workDir(ctx, "restore-"+time.Now().Format("20060102-150405"))
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(work)
		}
	}()
	res := &RestoreResult{Database: o.Database}
	fetch := func(sn *repo.Snapshot, path, sub string) (string, error) {
		target := filepath.Join(work, sub)
		if _, err := restorer.Run(ctx, r, sn, restorer.Options{Target: target, Include: []string{path}}); err != nil {
			return "", err
		}
		return filepath.Join(target, strings.TrimPrefix(path, "/")), nil
	}
	var dump string
	if o.Database != Cluster && !o.PointInTime {
		d := find(full, o.Database)
		if d == nil {
			return nil, fmt.Errorf("database %s is not in this backup", o.Database)
		}
		o.Log("reading the dump", "database", o.Database)
		if dump, err = fetch(full, d.File, "dump"); err != nil {
			return nil, err
		}
	} else {
		if o.Database != Cluster && find(full, o.Database) == nil {
			return nil, fmt.Errorf("database %s is not in this backup", o.Database)
		}
		o.Log("reading the cluster backup")
		base, err := fetch(full, cl.File, "base")
		if err != nil {
			return nil, err
		}
		walDir := filepath.Join(work, "wal")
		os.MkdirAll(walDir, 0o700)
		next := NextSegment(cl.LastLSN)
		for _, sn := range wals {
			w := find(sn, WAL)
			if w == nil || sn.SQL.Engine != "postgres" || w.LastLSN < cl.FirstLSN {
				continue
			}
			if next != "" && w.FirstLSN > next {
				// A segment is missing: recovery cannot go further.
				if !o.StopAt.IsZero() && w.Start.Before(o.StopAt) {
					return nil, fmt.Errorf("WAL segment %s is missing (archiving was interrupted); restore to a point before %s", next, w.Start.Format("2006-01-02 15:04"))
				}
				break
			}
			got, err := fetch(sn, w.File, fmt.Sprintf("w%d", res.WALFiles))
			if err != nil {
				return nil, err
			}
			entries, _ := os.ReadDir(got)
			for _, e := range entries {
				os.Rename(filepath.Join(got, e.Name()), filepath.Join(walDir, e.Name()))
			}
			res.WALFiles++
			next = NextSegment(w.LastLSN)
			if !o.StopAt.IsZero() && !w.Finish.Before(o.StopAt) {
				break
			}
		}
		bindir := ""
		if s != nil {
			bindir = s.BinDir
		}
		inst, err := startRecovered(ctx, work, base, walDir, o.StopAt, bindir, cl.Recovery, o.Log)
		if err != nil {
			return nil, err
		}
		if o.Database == Cluster {
			keep = true
			res.DataDir, res.Port = inst.DataDir, inst.Port
			return res, nil
		}
		defer inst.stop(context.Background())
		dump = filepath.Join(work, "pitr.dump")
		os.WriteFile(dump, nil, 0o600)
		exec.CommandContext(ctx, "chown", "postgres:", dump).Run()
		o.Log("pg_dump from the recovered instance", "database", o.Database)
		if _, err := asPostgres(ctx, inst.bin("pg_dump"), "-h", inst.Socket, "-p", strconv.Itoa(inst.Port), "-Fc", "-Z", "0", "-f", dump, o.Database); err != nil {
			return nil, err
		}
	}
	// pg_restore runs as postgres: give it the restored files.
	exec.CommandContext(ctx, "chown", "-R", "postgres:", work).Run()
	if err := s.restoreDump(ctx, dump, o); err != nil {
		return nil, err
	}
	return res, nil
}

// restoreDump creates the target database and restores a pg_dump into it.
func (s *Server) restoreDump(ctx context.Context, dump string, o RestoreOptions) error {
	target := o.Target
	if target == "" {
		target = o.Database
	}
	q := quoteIdent(target)
	lit := "'" + strings.ReplaceAll(target, "'", "''") + "'"
	exists, err := s.Query(ctx, "", "SELECT count(*) FROM pg_database WHERE datname = "+lit)
	if err != nil {
		return err
	}
	if exists != "0" {
		if !o.Replace {
			return fmt.Errorf("database %s exists; restore under another name or choose to replace it", target)
		}
		o.Log("dropping the existing database", "database", target)
		s.Query(ctx, "", "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = "+lit+" AND pid <> pg_backend_pid()")
		if _, err := s.Query(ctx, "", "DROP DATABASE "+q); err != nil {
			return err
		}
	}
	if _, err := s.Query(ctx, "", "CREATE DATABASE "+q+" TEMPLATE template0"); err != nil {
		return err
	}
	o.Log("pg_restore", "database", target)
	if _, err := asPostgres(ctx, s.bin("pg_restore"), "-p", strconv.Itoa(s.Port), "-d", target, "--no-password", dump); err != nil {
		return fmt.Errorf("pg_restore into %s: %w", target, err)
	}
	return nil
}

// instance is a recovered cluster started by startRecovered.
type instance struct {
	DataDir, Socket, BinDir string
	Port                    int
}

func (i *instance) bin(name string) string {
	if i.BinDir != "" {
		return filepath.Join(i.BinDir, name)
	}
	return name
}

func (i *instance) stop(ctx context.Context) {
	asPostgres(ctx, i.bin("pg_ctl"), "-D", i.DataDir, "-m", "fast", "-w", "stop")
}

// startRecovered unpacks a tar base backup into work/data and starts it on
// a free port, replaying WAL from walDir up to stopAt (zero: all).
func startRecovered(ctx context.Context, work, base, walDir string, stopAt time.Time, bindir, version string, log func(string, ...any)) (*instance, error) {
	data := filepath.Join(work, "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		return nil, err
	}
	if out, err := exec.CommandContext(ctx, "tar", "-xf", filepath.Join(base, "base.tar"), "-C", data).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("unpack base.tar: %v: %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(base, "pg_wal.tar")); err == nil {
		exec.CommandContext(ctx, "tar", "-xf", filepath.Join(base, "pg_wal.tar"), "-C", filepath.Join(data, "pg_wal")).Run()
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	// A separate configuration: socket only in work, no archiving into the
	// production spool, small memory.
	conf := fmt.Sprintf("port = %d\nlisten_addresses = ''\nunix_socket_directories = '%s'\narchive_mode = off\nshared_buffers = '128MB'\n"+
		"restore_command = 'cp %s/%%f \"%%p\"'\nrecovery_target_action = 'promote'\n", port, work, walDir)
	if !stopAt.IsZero() {
		conf += "recovery_target_time = '" + stopAt.Format("2006-01-02 15:04:05-07:00") + "'\n"
	}
	if _, err := os.Stat(filepath.Join(data, "postgresql.conf")); err != nil {
		os.WriteFile(filepath.Join(data, "postgresql.conf"), []byte("# written by BackupZit for a restored cluster\n"), 0o600)
	}
	os.WriteFile(filepath.Join(data, "postgresql.auto.conf"), []byte(conf), 0o600)
	os.WriteFile(filepath.Join(data, "pg_hba.conf"), []byte("local all all peer\n"), 0o600)
	os.WriteFile(filepath.Join(data, "pg_ident.conf"), nil, 0o600)
	os.WriteFile(filepath.Join(data, "recovery.signal"), nil, 0o600)
	os.Remove(filepath.Join(data, "postmaster.pid"))
	for _, d := range []string{work, walDir} {
		os.Chmod(d, 0o700)
	}
	if out, err := exec.CommandContext(ctx, "chown", "-R", "postgres:", work).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("chown: %v: %s", err, out)
	}
	if bindir == "" {
		bindir = findBinDir(version)
	}
	inst := &instance{DataDir: data, Socket: work, BinDir: bindir, Port: port}
	log("starting the recovered cluster", "port", port)
	if _, err := asPostgres(ctx, inst.bin("pg_ctl"), "-D", data, "-l", filepath.Join(work, "postgres.log"), "-w", "-t", "3600", "start"); err != nil {
		b, _ := os.ReadFile(filepath.Join(work, "postgres.log"))
		return nil, fmt.Errorf("the recovered cluster did not start: %s", lastLines(strings.TrimSpace(string(b)), 4))
	}
	// Wait for the end of recovery (promotion).
	for i := 0; i < 3600; i++ {
		out, err := asPostgres(ctx, inst.bin("psql"), "-XAtq", "-h", work, "-p", strconv.Itoa(port), "-d", "postgres", "-c", "SELECT pg_is_in_recovery()")
		if err == nil && strings.TrimSpace(string(out)) == "f" {
			return inst, nil
		}
		select {
		case <-ctx.Done():
			inst.stop(context.Background())
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	inst.stop(context.Background())
	return nil, errors.New("the recovered cluster did not finish recovery")
}

func findBinDir(version string) string {
	major := strings.SplitN(version, ".", 2)[0]
	for _, d := range []string{"/usr/lib/postgresql/" + major + "/bin", "/usr/pgsql-" + major + "/bin"} {
		if _, err := os.Stat(filepath.Join(d, "pg_ctl")); err == nil {
			return d
		}
	}
	return ""
}

func freePort() (int, error) {
	for p := 54330; p < 54400; p++ {
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err == nil {
			l.Close()
			if _, err := os.Stat(fmt.Sprintf("/tmp/.s.PGSQL.%d", p)); err != nil {
				return p, nil
			}
		}
	}
	return 0, errors.New("no free port for the restored cluster")
}

func find(sn *repo.Snapshot, name string) *repo.SQLDatabase {
	if sn == nil || sn.SQL == nil {
		return nil
	}
	for i := range sn.SQL.Databases {
		if sn.SQL.Databases[i].Name == name {
			return &sn.SQL.Databases[i]
		}
	}
	return nil
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r < 32 {
			return '_'
		}
		return r
	}, s)
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}
