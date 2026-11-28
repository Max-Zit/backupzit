package pgsql

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
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
	stage, err := workDir(fmt.Sprintf("stage-%d", time.Now().UnixNano()))
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
		if err := s.UpgradeArchiveCommand(ctx); err != nil {
			problems = append(problems, "could not update archive_command: "+err.Error())
		}
		if ok, problem, err := s.ArchiveState(ctx); err != nil {
			return nil, nil, err
		} else if !ok {
			return nil, []string{"WAL archiving is not active: " + problem}, nil
		}
		// A segment is taken only with its full size: one PostgreSQL is still
		// copying (archive_command of agents before 0.55) waits for the next
		// log backup and stays in the spool.
		var segBytes int64
		if out, err := s.Query(ctx, "", "SELECT pg_size_bytes(current_setting('wal_segment_size'))"); err == nil {
			segBytes, _ = strconv.ParseInt(strings.TrimSpace(out), 10, 64)
		}
		// Close the current segment so recent changes are archived.
		before := time.Now()
		_, _ = s.Query(ctx, "", "SELECT pg_switch_wal()")
		caughtUp := waitArchived(ctx, s, before)
		// A short segment that stays in the spool can stop archiving for
		// good (PostgreSQL retries it and the command refuses an existing
		// file); what PostgreSQL reports decides what is done with it.
		ast, astErr := s.archiverState(ctx)
		reported := false
		dir := filepath.Join(stage, "wal")
		if err := os.Mkdir(dir, 0o700); err != nil {
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
			if IsSegment(n) && segBytes > 0 {
				fi, err := e.Info()
				if err != nil {
					continue
				}
				if fi.Size() != segBytes {
					if astErr == nil {
						if msg := handlePartial(filepath.Join(WALSpool, n), fi, segBytes, ast, time.Now()); msg != "" {
							o.Log(msg)
							problems = append(problems, msg)
							reported = reported || n == ast.lastFailed
						}
					}
					continue
				}
			}
			if err := copyFile(filepath.Join(WALSpool, n), filepath.Join(dir, n)); err != nil {
				return nil, problems, err
			}
			if fi, err := e.Info(); err == nil {
				size += uint64(fi.Size()) // #nosec G115 -- file sizes and counts are not negative
			}
			stored = append(stored, filepath.Join(WALSpool, n))
			if IsSegment(n) {
				segs = append(segs, n)
			}
		}
		// Archiving that fails for another reason (spool disk full, folder
		// gone) must not go unnoticed either: pg_wal grows meanwhile.
		if astErr == nil && ast.stuck && !caughtUp && !reported {
			problems = append(problems, fmt.Sprintf("PostgreSQL cannot archive WAL segment %s (archive_command failed, last at %s); point-in-time recovery stops there and pg_wal grows until it works again — see the PostgreSQL log",
				ast.lastFailed, ast.failedAt.Format("2006-01-02 15:04:05")))
		}
		if len(segs) == 0 {
			return nil, problems, nil
		}
		sort.Strings(segs)
		now := time.Now()
		meta.Databases = []repo.SQLDatabase{{Name: WAL, File: dir, FirstLSN: segs[0], LastLSN: segs[len(segs)-1], Size: size, Start: before, Finish: now}}
	default:
		meta.Kind = "full"
		if o.PITR {
			if err := s.UpgradeArchiveCommand(ctx); err != nil {
				problems = append(problems, "could not update archive_command: "+err.Error())
			}
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
		// pg_basebackup writes as postgres into an empty folder of its own.
		cdir, err := pgDir(stage, "cluster")
		if err != nil {
			return nil, problems, err
		}
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
		_ = filepath.WalkDir(cdir, func(_ string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if fi, err := d.Info(); err == nil {
					csize += uint64(fi.Size()) // #nosec G115 -- file sizes and counts are not negative
				}
			}
			return nil
		})
		meta.Databases = append(meta.Databases, repo.SQLDatabase{Name: Cluster, File: cdir, Recovery: s.Version, Size: csize,
			FirstLSN: first, LastLSN: last, Start: start, Finish: time.Now()})
		gfile := filepath.Join(stage, "globals.sql")
		if out, err := asPostgres(ctx, s.bin("pg_dumpall"), "-p", strconv.Itoa(s.Port), "--globals-only"); err == nil {
			if err := writeNew(gfile, out, 0o600); err != nil {
				return nil, problems, err
			}
			meta.Databases = append(meta.Databases, repo.SQLDatabase{Name: Globals, File: gfile, Size: uint64(len(out)), Start: time.Now(), Finish: time.Now()})
		} else {
			problems = append(problems, "roles were not saved: "+err.Error())
		}
		dbs, err := s.Databases(ctx)
		if err != nil {
			return nil, problems, err
		}
		ddir, err := pgDir(stage, "dumps")
		if err != nil {
			return nil, problems, err
		}
		for _, d := range dbs {
			if len(o.Databases) > 0 && !containsFold(o.Databases, d.Name) {
				continue
			}
			f := filepath.Join(ddir, safeName(d.Name)+".dump")
			t0 := time.Now()
			o.Log("pg_dump", "database", d.Name)
			if _, err := asPostgres(ctx, s.bin("pg_dump"), "-p", strconv.Itoa(s.Port), "-Fc", "-Z", "0", "-f", f, dbnameArg(d.Name)); err != nil {
				problems = append(problems, fmt.Sprintf("database %s: %v", d.Name, err))
				continue
			}
			fi, _ := os.Stat(f)
			var size uint64
			if fi != nil {
				size = uint64(fi.Size()) // #nosec G115 -- file sizes and counts are not negative
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
	// Spooled WAL goes only when everything was read: otherwise the next log
	// backup takes it again (PostgreSQL counts it as archived already).
	if len(sn.Stats.Errors) == 0 {
		for _, f := range stored {
			_ = os.Remove(f)
		}
	}
	return sn, problems, nil
}

// basebackupLSNs reads the WAL range of a tar-format base backup from its
// backup_manifest.
func basebackupLSNs(dir string) (start, end string) {
	b, err := os.ReadFile(filepath.Join(dir, "backup_manifest")) // #nosec G304 -- PostgreSQL files and dumps in the agent's working folder
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
// switched at t; false: it did not within the wait.
func waitArchived(ctx context.Context, s *Server, t time.Time) bool {
	for i := 0; i < 30; i++ {
		out, err := s.Query(ctx, "", "SELECT coalesce(extract(epoch from last_archived_time)::bigint, 0) FROM pg_stat_archiver")
		if err == nil {
			if n, _ := strconv.ParseInt(out, 10, 64); n >= t.Unix() {
				return true
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
	return false
}

// archiverState is PostgreSQL's view of WAL archiving (pg_stat_archiver).
type archiverState struct {
	lastArchived string    // last WAL file archived
	lastFailed   string    // last WAL file the archive_command failed on
	failedAt     time.Time // when that was
	stuck        bool      // the latest attempt failed (no success since)
}

func (s *Server) archiverState(ctx context.Context) (archiverState, error) {
	out, err := s.Query(ctx, "", "SELECT coalesce(last_archived_wal, ''), coalesce(last_failed_wal, ''), coalesce(extract(epoch from last_failed_time)::bigint, 0), "+
		"(last_failed_time IS NOT NULL AND (last_archived_time IS NULL OR last_failed_time > last_archived_time))::int FROM pg_stat_archiver")
	if err != nil {
		return archiverState{}, err
	}
	f := strings.Split(out, "|")
	if len(f) != 4 {
		return archiverState{}, fmt.Errorf("unexpected pg_stat_archiver row %q", out)
	}
	at, _ := strconv.ParseInt(f[2], 10, 64)
	return archiverState{lastArchived: f[0], lastFailed: f[1], failedAt: time.Unix(at, 0), stuck: f[3] == "1"}, nil
}

// partialAge is how long a short segment in the spool must be untouched
// before it counts as stale: a copy in progress changes it all the time.
const partialAge = 5 * time.Minute

// partialVerdict is what a log backup does with a segment in the spool that
// is shorter than a WAL segment.
type partialVerdict int

const (
	partialWait    partialVerdict = iota // probably still being copied: looked at again next time
	partialBlocked                       // archiving fails on it, but it may still be written: reported
	partialRetry                         // stale copy that stops archiving: removed, PostgreSQL copies it again
	partialLost                          // stale copy PostgreSQL never sends again: removed, the gap reported
)

// judgePartial decides about the short spool segment name, last written at
// mtime. Only a copy nothing can still be writing is removed: PostgreSQL
// fails on it since a time after it was written, or has archived later
// segments of the same timeline.
func judgePartial(name string, mtime, now time.Time, st archiverState) partialVerdict {
	old := now.Sub(mtime) > partialAge
	switch {
	case st.stuck && st.lastFailed == name:
		// PostgreSQL keeps a segment in pg_wal until it is archived, so the
		// spool copy is not the only one.
		if old && mtime.Before(st.failedAt) {
			return partialRetry
		}
		return partialBlocked
	case old && segmentAfter(st.lastArchived, name):
		return partialLost
	}
	return partialWait
}

// segmentAfter reports whether the WAL file a (a segment, or a .partial or
// .backup file named after one) is a later segment than seg on the same
// timeline.
func segmentAfter(a, seg string) bool {
	if len(a) < 24 || !IsSegment(a[:24]) || !IsSegment(seg) {
		return false
	}
	return a[:8] == seg[:8] && a[:24] > seg
}

// partialCopy reports whether the spool file p of size bytes is provably an
// incomplete copy: shorter than a segment, and not a complete segment of
// another size either (the long header of a segment's first page records
// its size, at offset 32, in the server's byte order).
func partialCopy(p string, size, segBytes int64) bool {
	if size >= segBytes {
		return false
	}
	f, err := os.OpenFile(p, os.O_RDONLY|oNoFollow, 0) // #nosec G304 -- a file in the agent's WAL spool (not through a link)
	if err != nil {
		return false
	}
	defer f.Close()
	h := make([]byte, 40)
	if n, _ := io.ReadFull(f, h); n < len(h) {
		return true
	}
	le, be := binary.LittleEndian.Uint32(h[32:]), binary.BigEndian.Uint32(h[32:])
	return int64(le) != size && int64(be) != size
}

// handlePartial acts on the short spool segment p and returns what the run
// reports about it ("" when it is most likely still being copied).
func handlePartial(p string, fi fs.FileInfo, segBytes int64, st archiverState, now time.Time) string {
	name := filepath.Base(p)
	if !partialCopy(p, fi.Size(), segBytes) {
		return ""
	}
	switch judgePartial(name, fi.ModTime(), now, st) {
	case partialBlocked:
		return fmt.Sprintf("WAL archiving is stuck at segment %s: an incomplete copy (%d of %d bytes) is in %s and PostgreSQL's archive_command fails on it; a later log backup removes it once nothing writes to it",
			name, fi.Size(), segBytes, WALSpool)
	case partialRetry:
		if err := os.Remove(p); err != nil {
			return fmt.Sprintf("WAL archiving is stuck at segment %s: an incomplete copy (%d of %d bytes) in %s could not be removed: %v", name, fi.Size(), segBytes, WALSpool, err)
		}
		return fmt.Sprintf("WAL segment %s in %s was an incomplete copy (%d of %d bytes) that stopped WAL archiving; it was removed and PostgreSQL archives the segment again",
			name, WALSpool, fi.Size(), segBytes)
	case partialLost:
		if err := os.Remove(p); err != nil {
			return fmt.Sprintf("WAL segment %s in %s is incomplete (%d of %d bytes) and could not be removed: %v", name, WALSpool, fi.Size(), segBytes, err)
		}
		return fmt.Sprintf("WAL segment %s in %s was incomplete (%d of %d bytes, probably left by a crash) and PostgreSQL does not archive it again; it was removed. Point-in-time recovery from earlier full backups cannot go past it: run a full backup",
			name, WALSpool, fi.Size(), segBytes)
	}
	return ""
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
	Socket   string // whole cluster: the folder of its Unix socket (psql -h)
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
	// work stays root's: postgres gets only what it must use, once root has
	// stopped writing there (startRecovered; the dump before pg_restore).
	work, err := workDir("restore-" + time.Now().Format("20060102-150405"))
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(work)
		}
	}()
	res := &RestoreResult{Database: o.Database}
	fetch := func(sn *repo.Snapshot, path, sub string) (string, error) {
		target := filepath.Join(work, sub)
		// Private: root's own files, not their recorded owner. The cluster
		// backup was postgres's folder; restored as postgres's, postgres
		// could swap base.tar while root fetches the WAL, and root would
		// unpack and write through what postgres chose (NEW-1).
		st, err := restorer.Run(ctx, r, sn, restorer.Options{Target: target, Include: []string{path}, Private: true})
		if err != nil {
			return "", err
		}
		// One unreadable WAL segment would make the recovery stop early
		// or replay across a gap while reporting success.
		if len(st.Errors) > 0 {
			return "", fmt.Errorf("could not read the backup completely: %s", strings.Join(st.Errors, "; "))
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
		got, err := fetch(full, d.File, "dump")
		if err != nil {
			return nil, err
		}
		if err := regularFile(got); err != nil {
			return nil, err
		}
		// pg_restore reads it as postgres: the file alone goes to postgres,
		// moved into work (root's, 0711) so the folders the restore created
		// on the way do not have to be opened up (LOW 8).
		dump = filepath.Join(work, "restore.dump")
		if err := os.Rename(got, dump); err != nil {
			return nil, err
		}
		if err := givePostgres(dump); err != nil {
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
		if err := os.Mkdir(walDir, 0o700); err != nil {
			return nil, err
		}
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
				_ = os.Rename(filepath.Join(got, e.Name()), filepath.Join(walDir, e.Name()))
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
			res.DataDir, res.Port, res.Socket = inst.DataDir, inst.Port, inst.Socket
			return res, nil
		}
		defer inst.stop(context.Background())
		// A new file in root's work folder, for pg_dump (as postgres).
		dump = filepath.Join(work, "pitr.dump")
		if err := writeNew(dump, nil, 0o600); err != nil {
			return nil, err
		}
		if err := givePostgres(dump); err != nil {
			return nil, err
		}
		o.Log("pg_dump from the recovered instance", "database", o.Database)
		if _, err := asPostgres(ctx, inst.bin("pg_dump"), "-h", inst.Socket, "-p", strconv.Itoa(inst.Port), "-Fc", "-Z", "0", "-f", dump, dbnameArg(o.Database)); err != nil {
			return nil, err
		}
	}
	// The dump is postgres's already: no chown of all of work, where
	// postgres could have planted a hard link to a file of root (LOW 8).
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
		_, _ = s.Query(ctx, "", "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = "+lit+" AND pid <> pg_backend_pid()")
		if _, err := s.Query(ctx, "", "DROP DATABASE "+q); err != nil {
			return err
		}
	}
	if _, err := s.Query(ctx, "", "CREATE DATABASE "+q+" TEMPLATE template0"); err != nil {
		return err
	}
	o.Log("pg_restore", "database", target)
	if _, err := asPostgres(ctx, s.bin("pg_restore"), "-p", strconv.Itoa(s.Port), dbnameArg(target), "--no-password", dump); err != nil {
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
	_, _ = asPostgres(ctx, i.bin("pg_ctl"), "-D", i.DataDir, "-m", "fast", "-w", "stop")
}

// stopTimeout bounds the stop after a failed start (pg_ctl itself waits up
// to 60 seconds).
const stopTimeout = 90 * time.Second

// start runs pg_ctl start and waits for the server to accept connections.
func (i *instance) start(ctx context.Context, logFile string) error {
	_, err := asPostgres(ctx, i.bin("pg_ctl"), "-D", i.DataDir, "-l", logFile, "-w", "-t", "3600", "start")
	if err != nil {
		// pg_ctl may have started the server before it failed or was
		// cancelled (its wait cut short): stop it with a context of its
		// own, or the caller's clean-up deletes the data folder under a
		// running server (L-N16). Without a server this fails at once.
		sctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
		defer cancel()
		i.stop(sctx)
	}
	return err
}

// startRecovered unpacks a tar base backup into work/data and starts it on
// a free port, replaying WAL from walDir up to stopAt (zero: all).
func startRecovered(ctx context.Context, work, base, walDir string, stopAt time.Time, bindir, version string, log func(string, ...any)) (*instance, error) {
	data := filepath.Join(work, "data")
	tablespaces := filepath.Join(work, "tablespaces")
	// The socket and the server log go into a folder of postgres's own:
	// work stays root's (LOW 8).
	run := filepath.Join(work, "run")
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	if err := prepareData(ctx, data, base, tablespaces, run, walDir, port, stopAt); err != nil {
		return nil, err
	}
	if _, err := pgDir(work, "run"); err != nil {
		return nil, err
	}
	// Only now that root has stopped writing there, the cluster's folders
	// go to postgres (-h: a link changes itself, not where it points).
	own := []string{data, walDir}
	if _, err := os.Lstat(tablespaces); err == nil {
		own = append(own, tablespaces)
	}
	if out, err := exec.CommandContext(ctx, "chown", append([]string{"-R", "-h", "postgres:"}, own...)...).CombinedOutput(); err != nil { // #nosec G204 -- fixed program; arguments are separate values, no shell
		return nil, fmt.Errorf("chown: %v: %s", err, out)
	}
	if bindir == "" {
		bindir = findBinDir(version)
	}
	inst := &instance{DataDir: data, Socket: run, BinDir: bindir, Port: port}
	log("starting the recovered cluster", "port", port)
	if err := inst.start(ctx, filepath.Join(run, "postgres.log")); err != nil {
		b, _ := readNoFollow(filepath.Join(run, "postgres.log"), 1<<20)
		return nil, fmt.Errorf("the recovered cluster did not start: %s", lastLines(strings.TrimSpace(string(b)), 4))
	}
	// Wait for the end of recovery (promotion).
	for i := 0; i < 3600; i++ {
		out, err := asPostgres(ctx, inst.bin("psql"), "-XAtq", "-h", run, "-p", strconv.Itoa(port), "-d", "postgres", "-c", "SELECT pg_is_in_recovery()")
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

// prepareData unpacks the tar base backup in base into the new folder data
// (its tablespaces below tablespaces) and writes the configuration of the
// recovered cluster: port, socket folder, WAL from walDir up to stopAt
// (zero: all). Root does it while data is root's and private.
//
// pg_basebackup wrote the base backup as postgres, so its content is
// postgres's choice (NEW-1): the archives are unpacked without their owners
// and permissions, and root never writes or reads through a link they may
// hold (a pg_ident.conf -> /etc/passwd would have made root empty that
// file). pg_basebackup itself puts no links into a tar backup.
func prepareData(ctx context.Context, data, base, tablespaces, socket, walDir string, port int, stopAt time.Time) error {
	if err := os.Mkdir(data, 0o700); err != nil {
		return err
	}
	if err := untar(ctx, filepath.Join(base, "base.tar"), data); err != nil {
		return fmt.Errorf("unpack base.tar: %w", err)
	}
	// A "." entry of the archive sets the folder's own mode.
	if err := os.Chmod(data, 0o700); err != nil { // #nosec G302 -- a directory needs the x bit
		return err
	}
	if _, err := os.Lstat(filepath.Join(base, "pg_wal.tar")); err == nil {
		// tar follows links on its way into the folder: base.tar must have
		// made pg_wal a real folder, with no link in it.
		if err := noLinks(filepath.Join(data, "pg_wal")); err != nil {
			return err
		}
		if err := untar(ctx, filepath.Join(base, "pg_wal.tar"), filepath.Join(data, "pg_wal")); err != nil {
			return fmt.Errorf("unpack pg_wal.tar: %w", err)
		}
	}
	if err := mapTablespaces(ctx, data, base, tablespaces); err != nil {
		return err
	}
	// A separate configuration: socket only in the restore's own folder, no
	// archiving into the production spool, small memory.
	conf := fmt.Sprintf("port = %d\nlisten_addresses = ''\nunix_socket_directories = '%s'\narchive_mode = off\nshared_buffers = '128MB'\n"+
		"restore_command = 'cp %s/%%f \"%%p\"'\nrecovery_target_action = 'promote'\n", port, socket, walDir)
	if !stopAt.IsZero() {
		conf += "recovery_target_time = '" + stopAt.Format("2006-01-02 15:04:05-07:00") + "'\n"
	}
	files := [][2]string{ // name, content
		{"postgresql.auto.conf", conf},
		{"pg_hba.conf", "local all all peer\n"},
		{"pg_ident.conf", ""},
		{"recovery.signal", ""},
	}
	// The backup's postgresql.conf stays (RHEL keeps it in the data folder).
	if fi, err := os.Lstat(filepath.Join(data, "postgresql.conf")); err != nil || !fi.Mode().IsRegular() {
		files = append(files, [2]string{"postgresql.conf", "# written by BackupZit for a restored cluster\n"})
	}
	for _, f := range files {
		if err := putFile(filepath.Join(data, f[0]), []byte(f[1])); err != nil {
			return err
		}
	}
	_ = os.Remove(filepath.Join(data, "postmaster.pid"))
	return os.Chmod(walDir, 0o700) // #nosec G302 -- a directory needs the x bit
}

// untar unpacks the tar archive f, which must be a regular file, into dir.
// Root takes the archive's content as it is but not its owners and
// permissions: what it creates is root's, with the umask applied and no
// setuid bits, until it is handed to postgres.
func untar(ctx context.Context, f, dir string) error {
	if err := regularFile(f); err != nil {
		return err
	}
	if out, err := exec.CommandContext(ctx, "tar", "--no-same-owner", "--no-same-permissions", "-xf", f, "-C", dir).CombinedOutput(); err != nil { // #nosec G204 -- fixed program; arguments are separate values, no shell
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}

// noLinks fails unless dir is a folder with no link in it (looked at
// without following any).
func noLinks(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 || p == dir && !d.IsDir() {
			return fmt.Errorf("%s in the cluster backup is a link or not a folder (pg_basebackup writes no links); the backup is not restored", p)
		}
		return nil
	})
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
			_ = l.Close()
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

// copyFile copies a segment of the WAL spool (written by postgres) into the
// stage: not through a link in either place (H-N4).
func copyFile(src, dst string) error {
	b, err := readNoFollow(src, 0)
	if err != nil {
		return err
	}
	return writeNew(dst, b, 0o600)
}

// mapTablespaces unpacks the user tablespaces of a tar base backup
// (<oid>.tar next to base.tar) into dir and points tablespace_map there.
// Otherwise PostgreSQL links pg_tblspc to the original absolute paths when
// the recovered cluster starts: on the same server those belong to the live
// cluster, and the recovery would write into production data.
//
// The map comes from the base backup, which postgres wrote (NEW-1): it is
// read and replaced without following a link, and each tablespace is
// unpacked into a new, empty folder of its own (a second line of the same
// tablespace would unpack through the links the first one left).
func mapTablespaces(ctx context.Context, data, base, dir string) error {
	p := filepath.Join(data, "tablespace_map")
	b, err := readNoFollow(p, 1<<20)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var out strings.Builder
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		oid, _, _ := strings.Cut(line, " ")
		if oid == "" || strings.Trim(oid, "0123456789") != "" {
			return fmt.Errorf("unexpected line in tablespace_map: %q", line)
		}
		tarFile := filepath.Join(base, oid+".tar")
		if _, err := os.Lstat(tarFile); err != nil { // #nosec G703 -- oid is digits only (checked above), base is the agent's working folder
			return fmt.Errorf("tablespace %s is not in the backup (%s.tar is missing)", oid, oid)
		}
		target := filepath.Join(dir, oid)
		if err := os.Mkdir(target, 0o700); err != nil { // #nosec G703 -- oid is digits only, dir is the agent's working folder
			if errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("tablespace %s is listed twice in tablespace_map", oid)
			}
			return err
		}
		if err := untar(ctx, tarFile, target); err != nil {
			return fmt.Errorf("unpack tablespace %s: %w", oid, err)
		}
		fmt.Fprintf(&out, "%s %s\n", oid, target)
	}
	return putFile(p, []byte(out.String()))
}
