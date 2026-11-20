// Package pgsql backs up and restores PostgreSQL with its own tools:
// pg_basebackup for the whole cluster, pg_dump for single databases and
// WAL archiving for point-in-time recovery. The agent runs as root and uses
// the postgres operating system account over the local socket.
package pgsql

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// WALSpool is where PostgreSQL's archive_command copies finished WAL
// segments until the agent stores them in the repository.
const WALSpool = BaseDir + "/wal"

// BaseDir holds the WAL spool and temporary files; the postgres account can
// enter it but not list it.
const BaseDir = "/var/lib/backupzit-pg"

// workDir creates a new directory below BaseDir owned by root (0711, so
// postgres can reach a folder of its own inside, see pgDir). It stays
// root's: what root writes there cannot be swapped for a link (H-N4).
func workDir(name string) (string, error) {
	if err := os.MkdirAll(BaseDir, 0o711); err != nil { // #nosec G301 -- postgres must traverse it
		return "", err
	}
	_ = os.Chmod(BaseDir, 0o711) // #nosec G302 -- a directory needs the x bit
	d := filepath.Join(BaseDir, name)
	if err := os.Mkdir(d, 0o711); err != nil { // #nosec G301 -- postgres must traverse it; a new name only
		return "", err
	}
	_ = os.Chmod(d, 0o711) // #nosec G302 -- a directory needs the x bit (umask)
	return d, nil
}

// spoolDir creates the WAL spool (WALSpool), which archive_command writes
// as postgres. It sits in the root-owned BaseDir, so postgres cannot put a
// link in its place.
func spoolDir() error {
	if err := os.MkdirAll(BaseDir, 0o711); err != nil { // #nosec G301 -- postgres must traverse it
		return err
	}
	_ = os.Chmod(BaseDir, 0o711) // #nosec G302 -- a directory needs the x bit
	fi, err := os.Lstat(WALSpool)
	switch {
	case os.IsNotExist(err):
		if err := os.Mkdir(WALSpool, 0o700); err != nil {
			return err
		}
	case err != nil:
		return err
	case !fi.IsDir():
		return fmt.Errorf("%s is not a folder", WALSpool)
	}
	return givePostgres(WALSpool)
}

// pgDir creates the folder name inside the root-owned dir for postgres to
// write in (pg_basebackup, pg_dump output).
func pgDir(dir, name string) (string, error) {
	p := filepath.Join(dir, name)
	if err := os.Mkdir(p, 0o700); err != nil {
		return "", err
	}
	if err := givePostgres(p); err != nil {
		return "", fmt.Errorf("give %s to postgres: %w", p, err)
	}
	return p, nil
}

// ArchiveCommand is the archive_command BackupZit sets (run by /bin/sh). It
// copies to a temporary name, syncs the copy to disk and only then renames
// it (and syncs the folder), so a segment in the spool is always complete:
// the agent may read the spool while PostgreSQL is still copying, and a
// power loss cannot leave a short file under the final name. A segment that
// is already there is accepted when it is identical: PostgreSQL archives a
// segment again after a crash, and refusing it would stop archiving.
const ArchiveCommand = "if test -f " + WALSpool + "/%f; then cmp -s %p " + WALSpool + "/%f; " +
	"else cp %p " + WALSpool + "/%f.part && sync " + WALSpool + "/%f.part && mv " + WALSpool + "/%f.part " + WALSpool + "/%f && sync " + WALSpool + "; fi"

// olderArchiveCommands are the archive_commands of earlier agents, which
// UpgradeArchiveCommand replaces: before 0.55 the copy went in place (a
// failed copy stayed as a short segment), the first 0.55 builds renamed it
// without syncing.
var olderArchiveCommands = []string{
	"test ! -f " + WALSpool + "/%f && cp %p " + WALSpool + "/%f",
	"test ! -f " + WALSpool + "/%f && cp %p " + WALSpool + "/%f.part && mv " + WALSpool + "/%f.part " + WALSpool + "/%f",
}

// ownArchiveCommand reports whether command was set by BackupZit.
func ownArchiveCommand(command string) bool {
	return command == ArchiveCommand || slices.Contains(olderArchiveCommands, command)
}

// UpgradeArchiveCommand replaces BackupZit's old archive_command with
// ArchiveCommand (a reload, no restart).
func (s *Server) UpgradeArchiveCommand(ctx context.Context) error {
	cur, err := s.Query(ctx, "", "SELECT current_setting('archive_command')")
	if err != nil || !slices.Contains(olderArchiveCommands, cur) {
		return err
	}
	if _, err := s.Query(ctx, "", "ALTER SYSTEM SET archive_command = '"+strings.ReplaceAll(ArchiveCommand, "'", "''")+"'"); err != nil {
		return err
	}
	_, err = s.Query(ctx, "", "SELECT pg_reload_conf()")
	return err
}

// Server is the local PostgreSQL cluster on Port.
type Server struct {
	Port    int
	DataDir string
	BinDir  string
	Version string
}

// Database is a database of the cluster.
type Database struct {
	Name string
	Size uint64
}

// asPostgres runs a command as the postgres account (in /, which it can
// read) and returns its standard output; tests replace it.
var asPostgres = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "runuser", append([]string{"-u", "postgres", "--", name}, args...)...) // #nosec G204 -- fixed program; arguments are separate values, no shell
	cmd.Dir = "/"
	cmd.Env = append(os.Environ(), "LC_ALL=C", "PGCONNECT_TIMEOUT=20")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.Bytes(), fmt.Errorf("%s: %s", filepath.Base(name), lastLines(msg, 5))
	}
	return out.Bytes(), nil
}

func lastLines(s string, n int) string {
	l := strings.Split(s, "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, " ")
}

// Open finds the cluster listening on port (0 = 5432).
func Open(ctx context.Context, port int) (*Server, error) {
	if port == 0 {
		port = 5432
	}
	s := &Server{Port: port}
	out, err := asPostgres(ctx, "psql", "-XAtq", "-p", strconv.Itoa(port), "-c", "SHOW data_directory", "-c", "SHOW server_version")
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || strings.Contains(err.Error(), "executable file not found") {
			return nil, errors.New("PostgreSQL client tools (psql) and the postgres account are needed on this machine")
		}
		return nil, fmt.Errorf("cannot connect to PostgreSQL on port %d: %w", port, err)
	}
	f := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(f) < 2 {
		return nil, fmt.Errorf("unexpected answer from PostgreSQL: %q", out)
	}
	s.DataDir, s.Version = f[0], f[1]
	// The server's own binaries: those of the running postmaster.
	if b, err := os.ReadFile(filepath.Join(s.DataDir, "postmaster.pid")); err == nil {
		pid := strings.SplitN(string(b), "\n", 2)[0]
		if exe, err := os.Readlink("/proc/" + strings.TrimSpace(pid) + "/exe"); err == nil {
			s.BinDir = filepath.Dir(exe)
		}
	}
	if s.BinDir == "" {
		if p, err := exec.LookPath("pg_basebackup"); err == nil {
			s.BinDir = filepath.Dir(p)
		}
	}
	return s, nil
}

func (s *Server) bin(name string) string {
	if s.BinDir != "" {
		if _, err := os.Stat(filepath.Join(s.BinDir, name)); err == nil {
			return filepath.Join(s.BinDir, name)
		}
	}
	return name
}

// Query runs SQL in database db (default postgres) and returns the rows
// as lines with |-separated columns.
func (s *Server) Query(ctx context.Context, db, sql string) (string, error) {
	if db == "" {
		db = "postgres"
	}
	out, err := asPostgres(ctx, s.bin("psql"), "-XAtq", "-p", strconv.Itoa(s.Port), dbnameArg(db), "-c", sql)
	return strings.TrimSpace(string(out)), err
}

// dbnameArg is the argument that names database db for psql, pg_dump and
// pg_restore. They read a database name containing = (or starting with
// postgres://) as a connection string, so a role that may create databases
// could name one "host=elsewhere ..." and send the nightly dump there.
// Wrapped as dbname='...' the name is only ever a name; host and port stay
// those of the command line.
func dbnameArg(db string) string { return "--dbname=dbname=" + conninfoQuote(db) }

// conninfoQuote quotes a value for a libpq connection string: in single
// quotes, with backslash and single quote escaped by a backslash.
func conninfoQuote(v string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
}

// Databases lists the databases except templates.
func (s *Server) Databases(ctx context.Context) ([]Database, error) {
	out, err := s.Query(ctx, "", "SELECT datname, pg_database_size(datname) FROM pg_database WHERE NOT datistemplate AND datallowconn ORDER BY datname")
	if err != nil {
		return nil, err
	}
	var dbs []Database
	for _, l := range strings.Split(out, "\n") {
		name, size, ok := strings.Cut(l, "|")
		if !ok {
			continue
		}
		n, _ := strconv.ParseUint(size, 10, 64)
		dbs = append(dbs, Database{Name: name, Size: n})
	}
	return dbs, nil
}

// ArchiveState reports whether WAL archiving into WALSpool is active and,
// if not, what is missing.
func (s *Server) ArchiveState(ctx context.Context) (ok bool, problem string, err error) {
	out, err := s.Query(ctx, "", "SELECT current_setting('archive_mode') || '|' || current_setting('archive_command') || '|' || current_setting('wal_level')")
	if err != nil {
		return false, "", err
	}
	f := strings.SplitN(out, "|", 3)
	if len(f) < 3 {
		return false, "", fmt.Errorf("unexpected settings %q", out)
	}
	mode, command, level := f[0], f[1], f[2]
	switch {
	case level == "minimal":
		return false, "wal_level is minimal; set it to replica and restart PostgreSQL", nil
	case !ownArchiveCommand(command) && command != "" && command != "(disabled)":
		return false, "archive_command is already set by another tool (" + command + "); BackupZit does not change it", nil
	case !ownArchiveCommand(command):
		return false, "archive_command is not set", nil
	case mode == "off":
		return false, "archive_mode is off", nil
	}
	return true, "", nil
}

// EnableArchiving sets archive_mode and archive_command (ALTER SYSTEM). It
// returns true when PostgreSQL must be restarted for archive_mode.
func (s *Server) EnableArchiving(ctx context.Context) (restart bool, err error) {
	if err := spoolDir(); err != nil {
		return false, err
	}
	mode, _ := s.Query(ctx, "", "SHOW archive_mode")
	if _, err := s.Query(ctx, "", "ALTER SYSTEM SET archive_command = '"+strings.ReplaceAll(ArchiveCommand, "'", "''")+"'"); err != nil {
		return false, err
	}
	if mode == "off" {
		if _, err := s.Query(ctx, "", "ALTER SYSTEM SET archive_mode = 'on'"); err != nil {
			return false, err
		}
	}
	if _, err := s.Query(ctx, "", "SELECT pg_reload_conf()"); err != nil {
		return false, err
	}
	return mode == "off", nil
}

// WALFileName is the WAL segment holding lsn (e.g. "0/2000028").
func (s *Server) WALFileName(ctx context.Context, lsn string) (string, error) {
	return s.Query(ctx, "", "SELECT pg_walfile_name('"+strings.ReplaceAll(lsn, "'", "")+"')")
}

var segmentRe = regexp.MustCompile(`^[0-9A-F]{24}$`)

// IsSegment reports whether name is a WAL segment file name.
func IsSegment(name string) bool { return segmentRe.MatchString(name) }

// NextSegment returns the segment after seg (for 16 MiB segments).
func NextSegment(seg string) string {
	if !IsSegment(seg) {
		return ""
	}
	tli, log, nr := seg[:8], seg[8:16], seg[16:24]
	l, _ := strconv.ParseUint(log, 16, 32)
	n, _ := strconv.ParseUint(nr, 16, 32)
	n++
	if n > 0xFF {
		n, l = 0, l+1
	}
	return fmt.Sprintf("%s%08X%08X", tli, l, n)
}
