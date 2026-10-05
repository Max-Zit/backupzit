// Package mssql backs up and restores Microsoft SQL Server databases with
// the server's native BACKUP and RESTORE statements. It talks to a local
// instance with the Windows account of the agent.
package mssql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/microsoft/go-mssqldb" // driver "sqlserver"
)

// Server is a connection to a local SQL Server instance.
type Server struct {
	db       *sql.DB
	Instance string // "" = default instance
}

// System databases; tempdb is never backed up.
var systemDBs = map[string]bool{"master": true, "model": true, "msdb": true}

// IsSystem reports whether name is a system database.
func IsSystem(name string) bool { return systemDBs[strings.ToLower(name)] }

// Open connects to instance on this machine ("" or "MSSQLSERVER" for the
// default instance), trying shared memory, named pipes and TCP.
func Open(ctx context.Context, instance string) (*Server, error) {
	if strings.EqualFold(instance, "MSSQLSERVER") {
		instance = ""
	}
	var errs []string
	for _, host := range hosts(instance) {
		dsn := "server=" + host + ";database=master;encrypt=disable;app name=BackupZit;dial timeout=10;connection timeout=20"
		db, err := sql.Open("sqlserver", dsn)
		if err != nil {
			return nil, err
		}
		pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = db.PingContext(pctx)
		cancel()
		if err == nil {
			db.SetMaxOpenConns(1) // one session: SET options and USE stick
			return &Server{db: db, Instance: instance}, nil
		}
		db.Close()
		errs = append(errs, host+": "+err.Error())
	}
	name := instance
	if name == "" {
		name = "the default instance"
	}
	return nil, fmt.Errorf("cannot connect to SQL Server %s: %s", name, strings.Join(errs, "; "))
}

// hosts are the server names to try, best first.
func hosts(instance string) []string {
	suffix := ""
	if instance != "" {
		suffix = `\` + instance
	}
	return []string{"lpc:." + suffix, "np:." + suffix, "localhost" + suffix}
}

func (s *Server) Close() error { return s.db.Close() }

// quote makes a name safe as a [bracketed] identifier.
func quote(name string) string { return "[" + strings.ReplaceAll(name, "]", "]]") + "]" }

// Database is a database of the instance.
type Database struct {
	Name     string
	Recovery string // FULL, BULK_LOGGED, SIMPLE
	State    string // ONLINE, ...
	Size     uint64 // data and log files, bytes
	System   bool
}

// Databases lists the databases except tempdb and database snapshots.
func (s *Server) Databases(ctx context.Context) ([]Database, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.name, d.recovery_model_desc, d.state_desc,
		(SELECT CAST(SUM(CAST(f.size AS bigint)) * 8192 AS bigint) FROM sys.master_files f WHERE f.database_id = d.database_id)
		FROM sys.databases d WHERE d.name <> 'tempdb' AND d.source_database_id IS NULL ORDER BY d.name`)
	if err != nil {
		return nil, permissionHint(err)
	}
	defer rows.Close()
	var out []Database
	for rows.Next() {
		var d Database
		var size sql.NullInt64
		if err := rows.Scan(&d.Name, &d.Recovery, &d.State, &size); err != nil {
			return nil, err
		}
		d.Size, d.System = uint64(size.Int64), IsSystem(d.Name)
		out = append(out, d)
	}
	return out, rows.Err()
}

// BackupDir is the instance's default backup folder: SQL Server's service
// account can write there, and the agent can read it.
func (s *Server) BackupDir(ctx context.Context) (string, error) {
	var dir sql.NullString
	s.db.QueryRowContext(ctx, `SELECT CAST(SERVERPROPERTY('InstanceDefaultBackupPath') AS nvarchar(4000))`).Scan(&dir)
	if !dir.Valid || dir.String == "" {
		// Before SQL Server 2019: from the instance's registry settings.
		var name string
		err := s.db.QueryRowContext(ctx, `DECLARE @d nvarchar(4000);
			EXEC master.dbo.xp_instance_regread N'HKEY_LOCAL_MACHINE', N'Software\Microsoft\MSSQLServer\MSSQLServer', N'BackupDirectory', @d OUTPUT;
			SELECT N'BackupDirectory', @d`).Scan(&name, &dir)
		if err != nil || !dir.Valid || dir.String == "" {
			return "", errors.New("the instance has no default backup folder")
		}
	}
	return dir.String, nil
}

// SetInfo describes a backup set written by Backup.
type SetInfo struct {
	FirstLSN, LastLSN, CheckpointLSN, DatabaseLSN string
	Start, Finish                                 time.Time
	Size                                          uint64
}

// Backup writes a full ("full") or transaction log ("log") backup of
// database name to file. copyOnly leaves the backup chain of other tools
// untouched (full backups only).
func (s *Server) Backup(ctx context.Context, name, kind, file string, copyOnly bool) (*SetInfo, error) {
	stmt := "BACKUP DATABASE " + quote(name) + " TO DISK = @p1 WITH INIT, FORMAT, CHECKSUM"
	if kind == "log" {
		stmt = "BACKUP LOG " + quote(name) + " TO DISK = @p1 WITH INIT, FORMAT, CHECKSUM"
	} else if copyOnly {
		stmt += ", COPY_ONLY"
	}
	if _, err := s.db.ExecContext(ctx, stmt, file); err != nil {
		return nil, permissionHint(err)
	}
	var info SetInfo
	var start, finish time.Time
	var size sql.NullInt64
	var ckp, dbl sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT TOP 1 CAST(b.first_lsn AS varchar(30)), CAST(b.last_lsn AS varchar(30)),
		CAST(b.checkpoint_lsn AS varchar(30)), CAST(b.database_backup_lsn AS varchar(30)), b.backup_start_date, b.backup_finish_date, CAST(b.backup_size AS bigint)
		FROM msdb.dbo.backupset b JOIN msdb.dbo.backupmediafamily m ON m.media_set_id = b.media_set_id
		WHERE m.physical_device_name = @p1 AND b.database_name = @p2 ORDER BY b.backup_set_id DESC`, file, name).
		Scan(&info.FirstLSN, &info.LastLSN, &ckp, &dbl, &start, &finish, &size)
	if err != nil {
		return nil, fmt.Errorf("read the backup history: %w", err)
	}
	info.CheckpointLSN, info.DatabaseLSN, info.Size = ckp.String, dbl.String, uint64(size.Int64)
	// msdb keeps the server's local time without a zone.
	info.Start, info.Finish = localTime(start), localTime(finish)
	return &info, nil
}

func localTime(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.Local)
}

// NoFullBackup reports the error of a log backup without a full backup
// that starts the log chain (or after the recovery model changed).
func NoFullBackup(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "4214") || strings.Contains(err.Error(), "no current database backup"))
}

// RestoreOptions restore one database from a full backup and optionally
// transaction log backups.
type RestoreOptions struct {
	Target  string   // name of the restored database
	Full    string   // full backup file
	Logs    []string // log backup files, in order
	StopAt  time.Time
	Replace bool // overwrite an existing database named Target
	Log     func(msg string, args ...any)
}

// Restore restores a database. A database with another name than the one
// in the backup gets its own files in the instance's default folders.
func (s *Server) Restore(ctx context.Context, o RestoreOptions) error {
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	if o.Target == "" || strings.ContainsAny(o.Target, "\x00") {
		return errors.New("invalid database name")
	}
	var exists int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sys.databases WHERE name = @p1`, o.Target).Scan(&exists)
	if exists > 0 && !o.Replace {
		return fmt.Errorf("database %s exists; restore under another name or choose to replace it", o.Target)
	}
	files, origName, err := s.fileList(ctx, o.Full)
	if err != nil {
		return err
	}
	args := []any{o.Full}
	with := []string{}
	if !strings.EqualFold(origName, o.Target) {
		dataDir, logDir, err := s.defaultDirs(ctx)
		if err != nil {
			return err
		}
		for i, f := range files {
			dir, ext := dataDir, filepath.Ext(f.Physical)
			if f.Type == "L" {
				dir = logDir
			}
			if ext == "" {
				ext = map[bool]string{true: ".ldf", false: ".mdf"}[f.Type == "L"]
			}
			suffix := ""
			if i > 0 {
				suffix = fmt.Sprintf("_%d", i)
			}
			args = append(args, f.Logical, filepath.Join(dir, safeFile(o.Target)+suffix+ext))
			with = append(with, fmt.Sprintf("MOVE @p%d TO @p%d", len(args)-1, len(args)))
		}
	}
	if o.Replace {
		with = append(with, "REPLACE")
		if exists > 0 {
			o.Log("disconnecting users", "database", o.Target)
			s.db.ExecContext(ctx, "ALTER DATABASE "+quote(o.Target)+" SET SINGLE_USER WITH ROLLBACK IMMEDIATE")
		}
	}
	with = append(with, "NORECOVERY", "CHECKSUM")
	o.Log("restoring full backup", "database", o.Target)
	if _, err := s.db.ExecContext(ctx, "RESTORE DATABASE "+quote(o.Target)+" FROM DISK = @p1 WITH "+strings.Join(with, ", "), args...); err != nil {
		return permissionHint(err)
	}
	for _, lf := range o.Logs {
		o.Log("restoring log backup", "database", o.Target, "file", filepath.Base(lf))
		stmt := "RESTORE LOG " + quote(o.Target) + " FROM DISK = @p1 WITH NORECOVERY"
		largs := []any{lf}
		if !o.StopAt.IsZero() {
			stmt += ", STOPAT = @p2"
			largs = append(largs, o.StopAt.Local().Format("2006-01-02T15:04:05"))
		}
		if _, err := s.db.ExecContext(ctx, stmt, largs...); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, "RESTORE DATABASE "+quote(o.Target)+" WITH RECOVERY"); err != nil {
		return err
	}
	s.db.ExecContext(ctx, "ALTER DATABASE "+quote(o.Target)+" SET MULTI_USER")
	return nil
}

type backupFile struct{ Logical, Physical, Type string }

// fileList reads the files of the database in a backup and its name.
func (s *Server) fileList(ctx context.Context, file string) ([]backupFile, string, error) {
	rows, err := s.db.QueryContext(ctx, "RESTORE FILELISTONLY FROM DISK = @p1", file)
	if err != nil {
		return nil, "", permissionHint(err)
	}
	cols, _ := rows.Columns()
	var out []backupFile
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			rows.Close()
			return nil, "", err
		}
		var f backupFile
		for i, c := range cols {
			v := fmt.Sprint(vals[i])
			switch c {
			case "LogicalName":
				f.Logical = v
			case "PhysicalName":
				f.Physical = v
			case "Type":
				f.Type = v
			}
		}
		out = append(out, f)
	}
	rows.Close()
	name, err := s.headerName(ctx, file)
	if err != nil {
		return nil, "", err
	}
	return out, name, nil
}

func (s *Server) headerName(ctx context.Context, file string) (string, error) {
	rows, err := s.db.QueryContext(ctx, "RESTORE HEADERONLY FROM DISK = @p1", file)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	if !rows.Next() {
		return "", errors.New("the backup file holds no backup set")
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return "", err
	}
	for i, c := range cols {
		if c == "DatabaseName" {
			return fmt.Sprint(vals[i]), nil
		}
	}
	return "", errors.New("database name missing in the backup header")
}

func (s *Server) defaultDirs(ctx context.Context) (data, log string, err error) {
	var d, l sql.NullString
	s.db.QueryRowContext(ctx, `SELECT CAST(SERVERPROPERTY('InstanceDefaultDataPath') AS nvarchar(4000)), CAST(SERVERPROPERTY('InstanceDefaultLogPath') AS nvarchar(4000))`).Scan(&d, &l)
	if !d.Valid || d.String == "" {
		// Older versions: next to master's files.
		if err := s.db.QueryRowContext(ctx, `SELECT physical_name FROM sys.master_files WHERE database_id = 1 AND type = 0`).Scan(&d); err != nil {
			return "", "", err
		}
		d.String = filepath.Dir(d.String)
	}
	if !l.Valid || l.String == "" {
		l = d
	}
	return d.String, l.String, nil
}

// safeFile turns a database name into a file name.
func safeFile(name string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(`<>:"/\|?*`, r) || r < 32 {
			return '_'
		}
		return r
	}, name)
}

// SafeFile is safeFile for callers naming backup files.
func SafeFile(name string) string { return safeFile(name) }

// permissionHint explains missing rights of the agent's account.
func permissionHint(err error) error {
	if err == nil {
		return nil
	}
	m := err.Error()
	if strings.Contains(m, "permission") || strings.Contains(m, "3201") && strings.Contains(m, "Access is denied") || strings.Contains(m, "262") || strings.Contains(m, "Login failed") {
		return fmt.Errorf("%w — the agent runs as NT AUTHORITY\\SYSTEM; give it the sysadmin role in this instance: ALTER SERVER ROLE sysadmin ADD MEMBER [NT AUTHORITY\\SYSTEM]", err)
	}
	return err
}

// LSNLess compares two LSNs (decimal numbers as text).
func LSNLess(a, b string) bool {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}
