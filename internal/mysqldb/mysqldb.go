// Package mysqldb backs up and restores MySQL and MariaDB databases with
// their own tools: a mysqldump of each database that records the binary log
// position, and the binary logs for point-in-time recovery. The agent runs
// as root and signs in over the local socket (unix_socket/auth_socket for
// root, or /root/.my.cnf).
package mysqldb

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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

// Binlog is the entry name of stored binary logs in a backup.
const Binlog = "(binlog)"

// System databases, never dumped.
var systemDBs = map[string]bool{"information_schema": true, "performance_schema": true, "sys": true, "mysql": true}

// Server is the local MySQL or MariaDB server.
type Server struct {
	Version string
	MariaDB bool
	LogBin  bool
	BinBase string // log_bin_basename
	tool    map[string]string
}

func pick(names ...string) string {
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return names[len(names)-1]
}

func run(ctx context.Context, stdin io.Reader, stdout io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = "/"
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var errb bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		if l := strings.Split(msg, "\n"); len(l) > 4 {
			msg = strings.Join(l[len(l)-4:], " ")
		}
		return fmt.Errorf("%s: %s", filepath.Base(name), msg)
	}
	return nil
}

// Open connects to the local server.
func Open(ctx context.Context) (*Server, error) {
	s := &Server{tool: map[string]string{
		"client": pick("mariadb", "mysql"),
		"dump":   pick("mariadb-dump", "mysqldump"),
		"binlog": pick("mariadb-binlog", "mysqlbinlog"),
	}}
	out, err := s.Query(ctx, "SELECT @@version, @@log_bin, coalesce(@@log_bin_basename, '')")
	if err != nil {
		if strings.Contains(err.Error(), "executable file not found") {
			return nil, errors.New("the MySQL/MariaDB client tools are needed on this machine")
		}
		return nil, fmt.Errorf("cannot connect to MySQL/MariaDB as root over the local socket: %w", err)
	}
	f := strings.Split(out, "\t")
	if len(f) < 3 {
		return nil, fmt.Errorf("unexpected answer %q", out)
	}
	s.Version, s.LogBin, s.BinBase = f[0], f[1] == "1", f[2]
	s.MariaDB = strings.Contains(strings.ToLower(s.Version), "mariadb")
	return s, nil
}

// Query runs SQL and returns the rows (tab-separated columns).
func (s *Server) Query(ctx context.Context, sql string) (string, error) {
	var out bytes.Buffer
	err := run(ctx, nil, &out, s.tool["client"], "-N", "-B", "-e", sql)
	return strings.TrimSpace(out.String()), err
}

// Databases lists the user databases.
func (s *Server) Databases(ctx context.Context) ([]string, error) {
	out, err := s.Query(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, err
	}
	var dbs []string
	for _, d := range strings.Split(out, "\n") {
		if d != "" && !systemDBs[strings.ToLower(d)] {
			dbs = append(dbs, d)
		}
	}
	return dbs, nil
}

var posRe = regexp.MustCompile(`(?:MASTER_LOG_FILE|SOURCE_LOG_FILE)='([^']+)',\s*(?:MASTER_LOG_POS|SOURCE_LOG_POS)=(\d+)`)

// dump writes a mysqldump of db (without CREATE DATABASE, so it can be
// loaded under any name) and returns the binary log position it matches.
func (s *Server) dump(ctx context.Context, db, file string) (string, error) {
	f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	args := []string{"--single-transaction", "--routines", "--events", "--triggers", "--hex-blob", "--default-character-set=utf8mb4"}
	if s.LogBin {
		if s.MariaDB {
			args = append(args, "--master-data=2")
		} else {
			args = append(args, "--source-data=2")
		}
	}
	err = run(ctx, nil, f, s.tool["dump"], append(args, "--", db)...)
	f.Close()
	if err != nil && s.LogBin && !s.MariaDB && strings.Contains(err.Error(), "source-data") {
		// MySQL before 8.0.26.
		f, _ = os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		args[len(args)-1] = "--master-data=2"
		err = run(ctx, nil, f, s.tool["dump"], append(args, "--", db)...)
		f.Close()
	}
	if err != nil || !s.LogBin {
		return "", err
	}
	h, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer h.Close()
	sc := bufio.NewScanner(h)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for i := 0; i < 200 && sc.Scan(); i++ {
		if m := posRe.FindStringSubmatch(sc.Text()); m != nil {
			return m[1] + ":" + m[2], nil
		}
	}
	return "", nil
}

// BackupOptions select what BackupToRepo does.
type BackupOptions struct {
	Databases []string // empty: all user databases
	Kind      string   // "full" or "log" (binary logs)
	PITR      bool
	Previous  string // newest binary log already stored ("" = none)
	From      string // with no Previous: the oldest binary log needed
	Tags      []string
	Version   string
	Log       func(msg string, args ...any)
	Progress  func(path string, s *repo.SnapshotStats)
}

// BackupToRepo stores dumps of the databases (full) or the binary logs
// closed since Previous (log) in a snapshot of r.
func BackupToRepo(ctx context.Context, r *repo.Repository, o BackupOptions) (*repo.Snapshot, []string, error) {
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	s, err := Open(ctx)
	if err != nil {
		return nil, nil, err
	}
	stage := filepath.Join("/var/lib/backupzit", fmt.Sprintf("mysqlstage-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(stage)
	meta := &repo.SQLBackup{Engine: "mysql", Instance: s.Version, Kind: o.Kind}
	var problems []string
	if o.Kind == "log" {
		if !o.PITR {
			return nil, nil, nil
		}
		if !s.LogBin {
			return nil, []string{"the binary log is off: add log_bin and server_id to the server configuration and restart it (see BackupZit documentation)"}, nil
		}
		start := time.Now()
		if _, err := s.Query(ctx, "FLUSH BINARY LOGS"); err != nil {
			return nil, nil, err
		}
		out, err := s.Query(ctx, "SHOW BINARY LOGS")
		if err != nil {
			return nil, nil, err
		}
		var files []string
		for _, l := range strings.Split(out, "\n") {
			if n := strings.SplitN(l, "\t", 2)[0]; n != "" {
				files = append(files, n)
			}
		}
		sort.Strings(files)
		if len(files) > 0 {
			files = files[:len(files)-1] // the newest one is still being written
		}
		dir := filepath.Join(stage, "binlog")
		os.MkdirAll(dir, 0o700)
		var stored []string
		var size uint64
		for _, f := range files {
			if (o.Previous != "" && f <= o.Previous) || (o.Previous == "" && o.From != "" && f < o.From) {
				continue
			}
			src := filepath.Join(filepath.Dir(s.BinBase), f)
			if err := copyFile(src, filepath.Join(dir, f)); err != nil {
				return nil, nil, err
			}
			fi, _ := os.Stat(src)
			if fi != nil {
				size += uint64(fi.Size())
			}
			stored = append(stored, f)
		}
		if len(stored) == 0 {
			return nil, nil, nil
		}
		if o.Previous != "" && nextBinlog(o.Previous) != stored[0] {
			problems = append(problems, fmt.Sprintf("binary logs between %s and %s are missing (removed by the server before they were stored); point-in-time restores cannot cross this gap", o.Previous, stored[0]))
		}
		meta.Databases = []repo.SQLDatabase{{Name: Binlog, File: dir, FirstLSN: stored[0], LastLSN: stored[len(stored)-1], Size: size, Start: start, Finish: time.Now()}}
	} else {
		meta.Kind = "full"
		if o.PITR && !s.LogBin {
			problems = append(problems, "point-in-time recovery is not possible: the binary log is off (add log_bin and server_id to the server configuration and restart it)")
		}
		dbs, err := s.Databases(ctx)
		if err != nil {
			return nil, nil, err
		}
		for _, want := range o.Databases {
			if !containsFold(dbs, want) {
				problems = append(problems, fmt.Sprintf("database %s does not exist", want))
			}
		}
		for _, db := range dbs {
			if len(o.Databases) > 0 && !containsFold(o.Databases, db) {
				continue
			}
			f := filepath.Join(stage, safeName(db)+".sql")
			t0 := time.Now()
			o.Log("mysqldump", "database", db)
			pos, err := s.dump(ctx, db, f)
			if err != nil {
				problems = append(problems, fmt.Sprintf("database %s: %v", db, err))
				continue
			}
			fi, _ := os.Stat(f)
			var size uint64
			if fi != nil {
				size = uint64(fi.Size())
			}
			meta.Databases = append(meta.Databases, repo.SQLDatabase{Name: db, File: f, Size: size, FirstLSN: pos, LastLSN: pos, Start: t0, Finish: time.Now()})
		}
		if len(meta.Databases) == 0 {
			return nil, problems, errors.New("no database was backed up")
		}
	}
	sn, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{stage}, NoParent: true, Version: o.Version, Tags: o.Tags, SQL: meta, Progress: o.Progress})
	return sn, problems, err
}

// nextBinlog returns the binary log after name ("mysql-bin.000007" ->
// "mysql-bin.000008").
func nextBinlog(name string) string {
	i := strings.LastIndex(name, ".")
	if i < 0 {
		return ""
	}
	n, err := strconv.Atoi(name[i+1:])
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%s.%0*d", name[:i], len(name)-i-1, n+1)
}

// NextBinlog is nextBinlog for the console.
func NextBinlog(name string) string { return nextBinlog(name) }

// RestoreOptions select what RestoreFromRepo restores.
type RestoreOptions struct {
	Database string
	Target   string // "" = original name
	Replace  bool
	// PointInTime replays the binary logs after the dump up to StopAt
	// (zero: as far as they reach).
	PointInTime bool
	StopAt      time.Time
	Log         func(msg string, args ...any)
}

// RestoreFromRepo loads a database dump into the server (under Target) and
// replays the stored binary logs for it.
func RestoreFromRepo(ctx context.Context, r *repo.Repository, full *repo.Snapshot, logs []*repo.Snapshot, o RestoreOptions) (int, error) {
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	d := find(full, o.Database)
	if full.SQL == nil || full.SQL.Engine != "mysql" || full.SQL.Kind != "full" || d == nil {
		return 0, fmt.Errorf("database %s is not in this backup", o.Database)
	}
	target := o.Target
	if target == "" {
		target = o.Database
	}
	s, err := Open(ctx)
	if err != nil {
		return 0, err
	}
	work := filepath.Join("/var/lib/backupzit", fmt.Sprintf("mysql-restore-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(work, 0o700); err != nil {
		return 0, err
	}
	defer os.RemoveAll(work)
	fetch := func(sn *repo.Snapshot, path, sub string) (string, error) {
		t := filepath.Join(work, sub)
		if _, err := restorer.Run(ctx, r, sn, restorer.Options{Target: t, Include: []string{path}}); err != nil {
			return "", err
		}
		return filepath.Join(t, strings.TrimPrefix(path, "/")), nil
	}
	// Binary logs to replay, checked before anything is changed.
	var binlogs []string
	startFile, startPos := "", ""
	if o.PointInTime {
		if d.FirstLSN == "" {
			return 0, errors.New("this dump has no binary log position (the binary log was off); only the dump can be restored")
		}
		startFile, startPos, _ = strings.Cut(d.FirstLSN, ":")
		want := startFile
		for i, sn := range logs {
			b := find(sn, Binlog)
			if b == nil || sn.SQL.Engine != "mysql" || b.LastLSN < want {
				continue
			}
			if b.FirstLSN > want {
				return 0, fmt.Errorf("binary log %s is missing; restore to a point before %s", want, b.Start.Format("2006-01-02 15:04"))
			}
			dir, err := fetch(sn, b.File, fmt.Sprintf("b%d", i))
			if err != nil {
				return 0, err
			}
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				if e.Name() >= want {
					binlogs = append(binlogs, filepath.Join(dir, e.Name()))
				}
			}
			want = nextBinlog(b.LastLSN)
			if !o.StopAt.IsZero() && !b.Finish.Before(o.StopAt) {
				break
			}
		}
		sort.Slice(binlogs, func(i, j int) bool { return filepath.Base(binlogs[i]) < filepath.Base(binlogs[j]) })
		if len(binlogs) == 0 || filepath.Base(binlogs[0]) != startFile {
			return 0, fmt.Errorf("binary log %s (the position of the dump) is not stored yet; wait for the next log backup or restore the dump only", startFile)
		}
	}
	o.Log("reading the dump", "database", o.Database)
	dump, err := fetch(full, d.File, "dump")
	if err != nil {
		return 0, err
	}
	q := "`" + strings.ReplaceAll(target, "`", "``") + "`"
	exists, err := s.Query(ctx, "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = '"+strings.ReplaceAll(target, "'", "''")+"'")
	if err != nil {
		return 0, err
	}
	if exists != "0" {
		if !o.Replace {
			return 0, fmt.Errorf("database %s exists; restore under another name or choose to replace it", target)
		}
		o.Log("dropping the existing database", "database", target)
		if _, err := s.Query(ctx, "DROP DATABASE "+q); err != nil {
			return 0, err
		}
	}
	if _, err := s.Query(ctx, "CREATE DATABASE "+q+" CHARACTER SET utf8mb4"); err != nil {
		return 0, err
	}
	o.Log("loading the dump", "database", target)
	f, err := os.Open(dump)
	if err != nil {
		return 0, err
	}
	err = run(ctx, f, io.Discard, s.tool["client"], "--default-character-set=utf8mb4", target)
	f.Close()
	if err != nil {
		return 0, fmt.Errorf("load the dump into %s: %w", target, err)
	}
	if len(binlogs) == 0 {
		return 0, nil
	}
	// Replay the changes of this database after the dump, renamed to the
	// target. The replay itself is not written to the binary log.
	// --database filters by the original name in MariaDB, by the rewritten
	// one in MySQL.
	filter := target
	if s.MariaDB {
		filter = o.Database
	}
	args := []string{"--start-position=" + startPos, "--database=" + filter}
	if target != o.Database {
		args = append(args, "--rewrite-db="+o.Database+"->"+target)
	}
	if !o.StopAt.IsZero() {
		args = append(args, "--stop-datetime="+o.StopAt.Local().Format("2006-01-02 15:04:05"))
	}
	args = append(args, binlogs...)
	o.Log("replaying binary logs", "files", len(binlogs))
	pr, pw := io.Pipe()
	errc := make(chan error, 1)
	go func() {
		err := run(ctx, nil, pw, s.tool["binlog"], args...)
		pw.CloseWithError(err)
		errc <- err
	}()
	err = run(ctx, io.MultiReader(strings.NewReader("SET SESSION sql_log_bin = 0;\n"), pr), io.Discard, s.tool["client"], "--default-character-set=utf8mb4")
	pr.Close()
	if berr := <-errc; berr != nil && err == nil {
		err = berr
	}
	if err != nil {
		return 0, fmt.Errorf("replay the binary logs: %w", err)
	}
	return len(binlogs), nil
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

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r < 32 {
			return '_'
		}
		return r
	}, s)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// StoredState returns, from the snapshots of a job, the newest binary log
// already stored and the oldest one the full backups need.
func StoredState(sns []*repo.Snapshot) (previous, from string) {
	for _, sn := range sns {
		if sn.SQL == nil || sn.SQL.Engine != "mysql" {
			continue
		}
		for _, d := range sn.SQL.Databases {
			if d.Name == Binlog && d.LastLSN > previous {
				previous = d.LastLSN
			}
			if d.Name != Binlog && d.FirstLSN != "" {
				f, _, _ := strings.Cut(d.FirstLSN, ":")
				if from == "" || f < from {
					from = f
				}
			}
		}
	}
	return previous, from
}
