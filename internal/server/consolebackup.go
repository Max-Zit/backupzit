package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/backupzit/backupzit/internal/api"
	"github.com/backupzit/backupzit/internal/archiver"
	"github.com/backupzit/backupzit/internal/backend"
	"github.com/backupzit/backupzit/internal/repo"
	"github.com/backupzit/backupzit/internal/restorer"
)

// Backup of the console itself: every table of the database (exported in
// Go with COPY, so no pg_dump of a matching version is needed), the
// secrets key that decrypts the stored credentials, and the console's
// certificate, which agents pin. It is stored as a snapshot in a
// repository of a storage target, encrypted with the target's recovery
// key, and restored with `backupzit-server --restore-console`.

const consoleTag = "console-backup"

// consoleFiles are the files of the data directory in a console backup.
var consoleFiles = []string{SecretKeyFile, "cert.pem", "key.pem", "web/cert.pem", "web/key.pem", "web/acme-account.key"}

// consoleMeta describes a console backup.
type consoleMeta struct {
	Version    string    `json:"version"`
	Created    time.Time `json:"created"`
	Migrations []string  `json:"migrations"`
	Tables     []string  `json:"tables"` // in load order
}

// tablesInLoadOrder lists the console's tables so that every table comes
// after the tables it references.
func tablesInLoadOrder(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT table_name FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_type = 'BASE TABLE' AND table_name <> 'schema_migrations' ORDER BY table_name`)
	if err != nil {
		return nil, err
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, `SELECT DISTINCT cl.relname, ref.relname FROM pg_constraint c
		JOIN pg_class cl ON cl.oid = c.conrelid JOIN pg_class ref ON ref.oid = c.confrelid
		JOIN pg_namespace n ON n.oid = cl.relnamespace
		WHERE c.contype = 'f' AND n.nspname = current_schema() AND cl.relname <> ref.relname`)
	if err != nil {
		return nil, err
	}
	deps := map[string][]string{}
	for rows.Next() {
		var t, ref string
		if err := rows.Scan(&t, &ref); err != nil {
			rows.Close()
			return nil, err
		}
		deps[t] = append(deps[t], ref)
	}
	rows.Close()
	var out []string
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(t string) error
	visit = func(t string) error {
		switch state[t] {
		case 1:
			return fmt.Errorf("tables reference each other in a cycle (%s)", t)
		case 2:
			return nil
		}
		state[t] = 1
		for _, d := range deps[t] {
			if err := visit(d); err != nil {
				return err
			}
		}
		state[t] = 2
		out = append(out, t)
		return nil
	}
	for _, t := range tables {
		if err := visit(t); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func quoteIdent(s string) string { return pgx.Identifier{s}.Sanitize() }

// exportConsole writes the database and the data directory's key files into dir.
func exportConsole(ctx context.Context, pool *pgxpool.Pool, dataDir, version, dir string) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	// One consistent view of all tables.
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	meta := consoleMeta{Version: version, Created: time.Now().UTC()}
	rows, err := tx.Query(ctx, `SELECT name FROM schema_migrations ORDER BY name`)
	if err != nil {
		return err
	}
	if meta.Migrations, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return err
	}
	if meta.Tables, err = tablesInLoadOrder(ctx, tx); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "db"), 0o700); err != nil {
		return err
	}
	for _, t := range meta.Tables {
		f, err := os.OpenFile(filepath.Join(dir, "db", t+".csv"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		order := ""
		var hasID bool
		tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=$1 AND column_name='id')`, t).Scan(&hasID)
		if hasID {
			order = " ORDER BY id"
		}
		_, err = tx.Conn().PgConn().CopyTo(ctx, f, "COPY (SELECT * FROM "+quoteIdent(t)+order+") TO STDOUT WITH (FORMAT csv, HEADER true)")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("export %s: %w", t, err)
		}
	}
	for _, name := range consoleFiles {
		b, err := os.ReadFile(filepath.Join(dataDir, filepath.FromSlash(name)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		p := filepath.Join(dir, "files", filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			return err
		}
	}
	mb, _ := json.MarshalIndent(meta, "", "  ")
	return os.WriteFile(filepath.Join(dir, "meta.json"), mb, 0o600)
}

// ---- backup to a storage target

const settingConsoleBackup = "console_backup"

// ConsoleBackupSettings are set under Settings → Console backup.
type ConsoleBackupSettings struct {
	Enabled  bool  `json:"enabled"`
	TargetID int64 `json:"target_id"`
	Hour     int   `json:"hour"` // daily at this hour (local time)
	Keep     int   `json:"keep"` // number of backups kept
	// Last result.
	LastAt       time.Time `json:"last_at,omitempty"`
	LastSnapshot string    `json:"last_snapshot,omitempty"`
	LastBytes    uint64    `json:"last_bytes,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
	LastRepo     string    `json:"last_repo,omitempty"`
}

func (s *Server) consoleBackupSettings(ctx context.Context) ConsoleBackupSettings {
	c := ConsoleBackupSettings{Hour: 2, Keep: 14}
	s.store.GetSetting(ctx, settingConsoleBackup, &c)
	if c.Keep < 1 {
		c.Keep = 14
	}
	return c
}

// consoleRepoURL is where a target keeps console backups.
func consoleRepoURL(t Target) string { return repoURL(t, Agent{RepoDir: "backupzit-console"}) }

// BackupConsole stores a console backup on the configured target.
func (s *Server) BackupConsole(ctx context.Context) error {
	cfg := s.consoleBackupSettings(ctx)
	err := s.backupConsole(ctx, &cfg)
	cfg.LastAt = time.Now().UTC()
	cfg.LastError = ""
	if err != nil {
		cfg.LastError = err.Error()
	}
	if serr := s.store.SetSetting(ctx, settingConsoleBackup, cfg); serr != nil && err == nil {
		err = serr
	}
	detail := "backup " + cfg.LastSnapshot
	if err != nil {
		detail = "failed: " + err.Error()
		s.mailAdmins(ctx, "[BackupZit] Backup of the console failed",
			"The daily backup of the BackupZit console failed:\n\n"+err.Error()+"\n\nCheck Settings → Console backup.\n")
	}
	s.store.db.Exec(ctx, `INSERT INTO audit_log(username, action, detail, remote) VALUES('', 'console.backup', $1, '')`, clip(detail, 2000))
	return err
}

func (s *Server) backupConsole(ctx context.Context, cfg *ConsoleBackupSettings) error {
	if s.dataDir == "" {
		return errors.New("the console's data directory is unknown")
	}
	t, err := s.store.GetTarget(ctx, cfg.TargetID)
	if err != nil {
		return errors.New("choose the storage target for the console backup")
	}
	if t.Kind == "local" || t.Kind == "usb" {
		return errors.New("console backups need network storage (SFTP, S3, SMB, Azure or a hardened repository), not a local path of an agent")
	}
	ar := targetRepository(t, consoleRepoURL(t))
	r, closeRepo, err := openRepository(ctx, ar, true)
	if err != nil {
		return fmt.Errorf("open repository: %w", err)
	}
	defer closeRepo()
	stage := filepath.Join(s.dataDir, "console-backup")
	os.RemoveAll(stage)
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := exportConsole(ctx, s.store.db, s.dataDir, s.Version, stage); err != nil {
		return fmt.Errorf("export: %w", err)
	}
	lock, err := r.Lock(ctx, false, 10*time.Minute)
	if err != nil {
		return err
	}
	sn, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{stage}, Hostname: "backupzit-console", Version: s.Version, Tags: []string{consoleTag}})
	lock.Unlock()
	if err != nil {
		return err
	}
	if _, err := r.KeepImmutable(ctx, sn); err != nil {
		return fmt.Errorf("extend immutability: %w", err)
	}
	cfg.LastSnapshot, cfg.LastBytes, cfg.LastRepo = sn.ID.String(), sn.Stats.Bytes, ar.URL
	// Keep the newest backups.
	sns, err := r.ListSnapshots(ctx)
	if err != nil {
		return err
	}
	var mine []*repo.Snapshot
	for _, x := range sns {
		for _, tag := range x.Tags {
			if tag == consoleTag {
				mine = append(mine, x)
			}
		}
	}
	_, remove := repo.ApplyPolicy(mine, repo.RetentionPolicy{KeepLast: cfg.Keep}, time.Local)
	if len(remove) > 0 {
		lock, err := r.Lock(ctx, true, 10*time.Minute)
		if err != nil {
			return err
		}
		defer lock.Unlock()
		for _, x := range remove {
			if err := r.RemoveSnapshot(ctx, x.ID); err != nil {
				return err
			}
		}
		if _, err := r.Prune(ctx, repo.PruneOptions{}); err != nil {
			return fmt.Errorf("free space of old console backups: %w", err)
		}
	}
	return nil
}

// consoleBackupLoop runs the daily console backup.
func (s *Server) consoleBackupLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Minute):
		}
		cfg := s.consoleBackupSettings(ctx)
		now := time.Now()
		due := time.Date(now.Year(), now.Month(), now.Day(), cfg.Hour, 0, 0, 0, now.Location())
		if !cfg.Enabled || now.Before(due) || cfg.LastAt.After(due) {
			continue
		}
		if err := s.BackupConsole(ctx); err != nil {
			s.log.Error("console backup", "err", err)
		}
	}
}

// ---- restore (backupzit-server --restore-console)

// RestoreConsole restores the latest (or the given) console backup of r
// into the database and the data directory. The database is emptied first.
func RestoreConsole(ctx context.Context, dbURL, dataDir string, r *repo.Repository, snapshot string, logf func(string, ...any)) (*repo.Snapshot, error) {
	sns, err := r.ListSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	var sn *repo.Snapshot
	for _, x := range sns {
		isConsole := false
		for _, t := range x.Tags {
			isConsole = isConsole || t == consoleTag
		}
		if isConsole && (snapshot == "" || snapshot == "latest" || strings.HasPrefix(x.ID.String(), snapshot)) && (sn == nil || x.Time.After(sn.Time)) {
			sn = x
		}
	}
	if sn == nil {
		return nil, errors.New("no console backup found in this repository")
	}
	logf("restoring console backup %s from %s", sn.ID.Short(), sn.Time.Local().Format("2006-01-02 15:04"))
	tmp, err := os.MkdirTemp("", "bz-console-restore-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if _, err := restorer.Run(ctx, r, sn, restorer.Options{Target: tmp, Verify: true}); err != nil {
		return nil, fmt.Errorf("read the backup: %w", err)
	}
	// The files lie below the original staging path; find meta.json.
	var root string
	filepath.WalkDir(tmp, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "meta.json" && root == "" {
			root = filepath.Dir(p)
		}
		return nil
	})
	if root == "" {
		return nil, errors.New("the backup holds no console data")
	}
	var meta consoleMeta
	b, err := os.ReadFile(filepath.Join(root, "meta.json"))
	if err != nil || json.Unmarshal(b, &meta) != nil {
		return nil, errors.New("the console backup is damaged (meta.json)")
	}
	if err := importConsoleDB(ctx, dbURL, root, meta, logf); err != nil {
		return nil, err
	}
	for _, name := range consoleFiles {
		data, err := os.ReadFile(filepath.Join(root, "files", filepath.FromSlash(name)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		dst := filepath.Join(dataDir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(dst), 0o700)
		mode := os.FileMode(0o600)
		if strings.HasSuffix(name, "cert.pem") {
			mode = 0o644
		}
		if err := os.WriteFile(dst, data, mode); err != nil {
			return nil, err
		}
		matchOwner(dst, dataDir)
		logf("restored %s", name)
	}
	return sn, nil
}

// importConsoleDB replaces the database's contents with the backup.
func importConsoleDB(ctx context.Context, dbURL, root string, meta consoleMeta, logf func(string, ...any)) error {
	known := map[string]bool{}
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return err
	}
	for _, e := range entries {
		known[e.Name()] = true
	}
	for _, m := range meta.Migrations {
		if !known[m] {
			return fmt.Errorf("the backup was made by a newer BackupZit (%s, database step %s); install that version or newer first", meta.Version, m)
		}
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	// Empty the database: drop the console's tables.
	rows, err := pool.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'`)
	if err != nil {
		return err
	}
	existing, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, t := range existing {
		if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS "+quoteIdent(t)+" CASCADE"); err != nil {
			return fmt.Errorf("empty the database: %w", err)
		}
	}
	// Recreate the schema as it was at the backup, load the data, then
	// bring the schema up to this version.
	if err := migrateNames(ctx, pool, meta.Migrations); err != nil {
		return err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	for _, t := range meta.Tables {
		f, err := os.Open(filepath.Join(root, "db", t+".csv"))
		if err != nil {
			return fmt.Errorf("backup of table %s: %w", t, err)
		}
		header, _ := readLine(f)
		f.Seek(0, io.SeekStart)
		cols := strings.Split(strings.TrimSpace(header), ",")
		for i, c := range cols {
			cols[i] = quoteIdent(strings.Trim(c, `"`))
		}
		_, err = conn.Conn().PgConn().CopyFrom(ctx, f, "COPY "+quoteIdent(t)+" ("+strings.Join(cols, ",")+") FROM STDIN WITH (FORMAT csv, HEADER true)")
		f.Close()
		if err != nil {
			return fmt.Errorf("load table %s: %w", t, err)
		}
	}
	// Sequences continue after the restored IDs.
	srows, err := pool.Query(ctx, `SELECT table_name, column_name, pg_get_serial_sequence(quote_ident(table_name), column_name)
		FROM information_schema.columns WHERE table_schema = current_schema() AND column_default LIKE 'nextval(%'`)
	if err != nil {
		return err
	}
	type seq struct{ table, col, seq string }
	var seqs []seq
	for srows.Next() {
		var x seq
		var s *string
		if err := srows.Scan(&x.table, &x.col, &s); err != nil {
			srows.Close()
			return err
		}
		if s != nil {
			x.seq = *s
			seqs = append(seqs, x)
		}
	}
	srows.Close()
	for _, x := range seqs {
		q := fmt.Sprintf("SELECT setval(%s, COALESCE((SELECT max(%s) FROM %s), 0) + 1, false)",
			quoteLiteral(x.seq), quoteIdent(x.col), quoteIdent(x.table))
		if _, err := pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("sequence of %s: %w", x.table, err)
		}
	}
	logf("database restored (%d tables); applying the database steps of this version", len(meta.Tables))
	return migrate(ctx, pool)
}

func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func readLine(r io.Reader) (string, error) {
	var b bytes.Buffer
	one := make([]byte, 1)
	for {
		n, err := r.Read(one)
		if n == 1 {
			if one[0] == '\n' {
				return b.String(), nil
			}
			b.WriteByte(one[0])
		}
		if err != nil {
			return b.String(), err
		}
	}
}

// openRepository opens (or with create, initializes) a target's repository.
func openRepository(ctx context.Context, rs api.Repository, create bool) (*repo.Repository, func(), error) {
	opts := backend.Options{SFTPPassword: rs.SFTPPassword, SFTPHostKey: rs.SFTPHostKey,
		S3AccessKey: rs.S3AccessKey, S3SecretKey: rs.S3SecretKey, S3Region: rs.S3Region, S3LockDays: rs.S3LockDays,
		SMBPassword: rs.SMBPassword, SMBDomain: rs.SMBDomain,
		HardenedKey: rs.HardenedKey, HardenedFingerprint: rs.HardenedFingerprint,
		AzureKey: rs.AzureKey, AzureSAS: rs.AzureSAS}
	cleanup := func() {}
	if rs.SFTPKey != "" {
		f, err := os.CreateTemp("", "bz-key-*")
		if err != nil {
			return nil, nil, err
		}
		f.Write([]byte(rs.SFTPKey))
		f.Close()
		opts.SFTPKeyFile = f.Name()
		cleanup = func() { os.Remove(f.Name()) }
	}
	be, err := backend.Open(ctx, rs.URL, opts)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	r, err := repo.Open(ctx, be, repo.Password(rs.Password))
	if errors.Is(err, repo.ErrNotInitialized) && create {
		r, err = repo.Init(ctx, be, repo.Password(rs.Password))
	}
	if err != nil {
		be.Close()
		cleanup()
		return nil, nil, err
	}
	return r, func() { r.Close(); cleanup() }, nil
}

// StartConsoleBackup runs the daily backup of the console.
func (s *Server) StartConsoleBackup(ctx context.Context, dataDir string) {
	s.dataDir = dataDir
	go s.consoleBackupLoop(ctx)
}

// ---- Settings → Console backup

type consoleBackupPage struct {
	ConsoleBackupSettings
	Targets []Target // network targets
	RepoURL string
}

func (s *Server) consoleBackupPage(ctx context.Context) consoleBackupPage {
	p := consoleBackupPage{ConsoleBackupSettings: s.consoleBackupSettings(ctx)}
	targets, _ := s.store.ListTargets(ctx)
	for _, t := range targets {
		if t.Kind != "local" && t.Kind != "usb" {
			p.Targets = append(p.Targets, t)
			if t.ID == p.TargetID {
				p.RepoURL = consoleRepoURL(t)
			}
		}
	}
	return p
}

func (s *Server) handleConsoleBackupSettings(w http.ResponseWriter, r *http.Request, _ string) {
	const back = "/settings/console"
	cfg := s.consoleBackupSettings(r.Context())
	cfg.Enabled = r.FormValue("enabled") == "on"
	cfg.TargetID = formID(r, "target_id")
	hour, err1 := strconv.Atoi(r.FormValue("hour"))
	keep, err2 := strconv.Atoi(r.FormValue("keep"))
	if err1 != nil || err2 != nil || hour < 0 || hour > 23 || keep < 1 || keep > 365 {
		redirectErr(w, r, back, errors.New("choose an hour between 0 and 23 and keep between 1 and 365 backups"))
		return
	}
	cfg.Hour, cfg.Keep = hour, keep
	if cfg.Enabled {
		t, err := s.store.GetTarget(r.Context(), cfg.TargetID)
		if err != nil || t.Kind == "local" || t.Kind == "usb" {
			redirectErr(w, r, back, errors.New("choose a network storage target for the console backup"))
			return
		}
	}
	if err := s.store.SetSetting(r.Context(), settingConsoleBackup, cfg); err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, "settings.console_backup", "enabled %v, target %d, %02d:00, keep %d", cfg.Enabled, cfg.TargetID, cfg.Hour, cfg.Keep)
	redirectMsg(w, r, back, "Console backup settings saved.")
}

func (s *Server) handleConsoleBackupRun(w http.ResponseWriter, r *http.Request, _ string) {
	const back = "/settings/console"
	s.audit(r, "console.backup_now", "")
	if err := s.BackupConsole(r.Context()); err != nil {
		redirectErr(w, r, back, err)
		return
	}
	redirectMsg(w, r, back, "Console backed up.")
}
