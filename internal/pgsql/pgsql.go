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
	"strconv"
	"strings"
)

// WALSpool is where PostgreSQL's archive_command copies finished WAL
// segments until the agent stores them in the repository.
const WALSpool = BaseDir + "/wal"

// BaseDir holds the WAL spool and temporary files; the postgres account can
// enter it but not list it.
const BaseDir = "/var/lib/backupzit-pg"

// workDir creates a directory below BaseDir owned by postgres.
func workDir(ctx context.Context, name string) (string, error) {
	if err := os.MkdirAll(BaseDir, 0o711); err != nil {
		return "", err
	}
	os.Chmod(BaseDir, 0o711)
	d := filepath.Join(BaseDir, name)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	if out, err := exec.CommandContext(ctx, "chown", "postgres:", d).CombinedOutput(); err != nil {
		return "", fmt.Errorf("chown %s: %v: %s", d, err, out)
	}
	return d, nil
}

// ArchiveCommand is the archive_command BackupZit sets.
const ArchiveCommand = "test ! -f " + WALSpool + "/%f && cp %p " + WALSpool + "/%f"

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
// read) and returns its standard output.
func asPostgres(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "runuser", append([]string{"-u", "postgres", "--", name}, args...)...)
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
	out, err := asPostgres(ctx, s.bin("psql"), "-XAtq", "-p", strconv.Itoa(s.Port), "-d", db, "-c", sql)
	return strings.TrimSpace(string(out)), err
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
	case command != ArchiveCommand && command != "" && command != "(disabled)":
		return false, "archive_command is already set by another tool (" + command + "); BackupZit does not change it", nil
	case command != ArchiveCommand:
		return false, "archive_command is not set", nil
	case mode == "off":
		return false, "archive_mode is off", nil
	}
	return true, "", nil
}

// EnableArchiving sets archive_mode and archive_command (ALTER SYSTEM). It
// returns true when PostgreSQL must be restarted for archive_mode.
func (s *Server) EnableArchiving(ctx context.Context) (restart bool, err error) {
	if _, err := workDir(ctx, "wal"); err != nil {
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
