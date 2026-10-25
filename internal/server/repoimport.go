package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/runinfo"
)

// scanTimeout bounds a search for backups on a storage target (tests
// shorten it).
var scanTimeout = 10 * time.Minute

// openTargetBackend connects to a location on a storage target with the
// target's credentials.
func openTargetBackend(ctx context.Context, t Target, url string) (backend.Backend, func(), error) {
	opts := backend.Options{
		SFTPPassword: t.SFTPPassword, SFTPHostKey: t.SFTPHostKey,
		S3AccessKey: t.S3AccessKey, S3SecretKey: t.S3SecretKey, S3Region: t.S3Region, S3LockDays: t.S3LockDays,
		SMBPassword: t.SMBPassword, SMBDomain: t.SMBDomain,
		HardenedKey: t.HardenedKey, HardenedFingerprint: t.HardenedFingerprint,
		AzureKey: t.AzureKey, AzureSAS: t.AzureSAS,
	}
	cleanup := func() {}
	if t.SFTPKey != "" {
		f, err := os.CreateTemp("", "bz-key-*")
		if err != nil {
			return nil, nil, err
		}
		f.WriteString(t.SFTPKey)
		f.Close()
		opts.SFTPKeyFile = f.Name()
		cleanup = func() { os.Remove(f.Name()) }
	}
	be, err := backend.Open(ctx, url, opts)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return be, func() { be.Close(); cleanup() }, nil
}

// foundRepo is a repository on a storage target that the console does not
// know (no agent, VMware connection or run uses it).
type foundRepo struct {
	Dir     string
	Error   string // it could not be read
	Hosts   []string
	Jobs    []string
	Backups int
	Oldest  time.Time
	Newest  time.Time
}

// scanResult is what a search found on a storage target.
type scanResult struct {
	Unknown []foundRepo
	Known   int // repositories the console already has
}

// errScanLocal: local disks are only reachable by their agent.
var errScanLocal = errors.New("a local or USB disk is only reachable by its agent; connect the disk to a network storage or restore with the command line agent")

// scanTarget searches a storage target for repositories the console does
// not know and reads their backups.
func (s *Server) scanTarget(ctx context.Context, t Target) (scanResult, error) {
	var res scanResult
	if t.Kind == "local" || t.Kind == "usb" {
		return res, errScanLocal
	}
	be, done, err := openTargetBackend(ctx, t, t.URL)
	if err != nil {
		return res, fmt.Errorf("connect: %w", err)
	}
	dirs, err := backend.RepositoryDirs(ctx, be)
	done()
	if err != nil {
		return res, fmt.Errorf("list: %w", err)
	}
	known, err := s.store.knownRepoDirs(ctx, t)
	if err != nil {
		return res, err
	}
	for _, d := range dirs {
		if d == consoleRepoDir {
			continue
		}
		if known(d) {
			res.Known++
			continue
		}
		f := foundRepo{Dir: d}
		sns, err := readRepoSnapshots(ctx, t, d)
		if err != nil {
			f.Error = err.Error()
		} else {
			f.summarize(sns)
		}
		res.Unknown = append(res.Unknown, f)
	}
	return res, nil
}

// consoleRepoDir holds the console backups (restored with --restore-console).
const consoleRepoDir = "backupzit-console"

func (f *foundRepo) summarize(sns []*repo.Snapshot) {
	hosts, jobs := map[string]bool{}, map[string]bool{}
	for _, g := range groupSnapshots(sns) {
		jobs[g.name] = true
	}
	for _, sn := range sns {
		if sn.Hostname != "" {
			hosts[sn.Hostname] = true
		}
		if f.Oldest.IsZero() || sn.Time.Before(f.Oldest) {
			f.Oldest = sn.Time
		}
		if sn.Time.After(f.Newest) {
			f.Newest = sn.Time
		}
	}
	f.Backups = len(sns)
	f.Hosts, f.Jobs = sortedKeys(hosts), sortedKeys(jobs)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// readRepoSnapshots lists the backups of a repository on a target.
func readRepoSnapshots(ctx context.Context, t Target, dir string) ([]*repo.Snapshot, error) {
	r, closeRepo, err := openRepository(ctx, targetRepository(t, repoURL(t, Agent{RepoDir: dir})), false)
	if errors.Is(err, repo.ErrWrongPassword) {
		return nil, errors.New("encrypted with another recovery key")
	}
	if err != nil {
		return nil, err
	}
	defer closeRepo()
	return r.ListSnapshots(ctx)
}

// knownRepoDirs reports whether a repository folder on t belongs to a
// current agent or a VMware connection or is used by a run on t (a removed
// agent's folder on another storage can still be added).
func (s *Store) knownRepoDirs(ctx context.Context, t Target) (func(string) bool, error) {
	dirs, urls := map[string]bool{}, map[string]bool{}
	rows, err := s.db.Query(ctx, `SELECT repo_dir FROM agents WHERE removed_at IS NULL UNION SELECT repo_dir FROM vmware_hosts
		UNION SELECT DISTINCT repo_url FROM runs WHERE target_id=$1 AND repo_url<>''`, t.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		dirs[v], urls[v] = true, true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return func(d string) bool { return dirs[d] || urls[repoURL(t, Agent{RepoDir: d})] }, nil
}

// snapshotGroup are the backups of one job in a repository.
type snapshotGroup struct {
	name string
	sns  []*repo.Snapshot
}

// groupSnapshots groups backups by their job (tag job:N) and names each
// group after what it backs up.
func groupSnapshots(sns []*repo.Snapshot) []snapshotGroup {
	byTag := map[string]*snapshotGroup{}
	var order []string
	for _, sn := range sns {
		tag := ""
		for _, t := range sn.Tags {
			if strings.HasPrefix(t, "job:") {
				tag = t
			}
		}
		g := byTag[tag]
		if g == nil {
			g = &snapshotGroup{}
			byTag[tag] = g
			order = append(order, tag)
		}
		g.sns = append(g.sns, sn)
	}
	names := map[string]int{}
	for _, tag := range order {
		g := byTag[tag]
		g.name = describeSnapshot(g.sns[len(g.sns)-1])
		names[g.name]++
	}
	out := make([]snapshotGroup, 0, len(order))
	for _, tag := range order {
		g := byTag[tag]
		if names[g.name] > 1 && tag != "" {
			g.name += " (" + strings.TrimPrefix(tag, "job:") + ")"
		}
		out = append(out, *g)
	}
	return out
}

// describeSnapshot names a backup after its content, as the job name of
// backups found on storage.
func describeSnapshot(sn *repo.Snapshot) string {
	list := func(items []string) string {
		if len(items) > 3 {
			return strings.Join(items[:3], ", ") + fmt.Sprintf(" +%d", len(items)-3)
		}
		return strings.Join(items, ", ")
	}
	switch {
	case sn.SQL != nil:
		engine := map[string]string{"postgres": "PostgreSQL", "mysql": "MySQL"}[sn.SQL.Engine]
		if engine == "" {
			engine = "SQL Server"
		}
		var dbs []string
		for _, d := range sn.SQL.Databases {
			dbs = append(dbs, d.Name)
		}
		return strings.TrimSpace(engine + " " + list(dbs))
	case sn.System != nil:
		return "Linux system " + sn.System.OS
	case len(sn.Guests) > 0:
		var names []string
		for _, g := range sn.Guests {
			n := g.Name
			if n == "" {
				n = fmt.Sprint(g.VMID)
			}
			names = append(names, n)
		}
		return "VMs " + list(names)
	case len(sn.Images) > 0:
		return "Disk image"
	case strings.HasPrefix(sn.Hostname, "m365-"):
		return "Microsoft 365 " + strings.TrimPrefix(sn.Hostname, "m365-")
	}
	return "Files " + list(sn.Paths)
}

// importedRun is the run recorded for a backup found on storage.
func importedRun(sn *repo.Snapshot) (kind string, details json.RawMessage, message string) {
	switch {
	case sn.SQL != nil:
		kind = api.KindSQLBackup
		if sn.SQL.Kind == "log" {
			kind = api.KindSQLLog
		}
		details = runinfo.SQL(sn)
	case sn.System != nil:
		kind, details = api.KindSystemBackup, runinfo.System(sn)
		message = fmt.Sprintf("%s: %d file systems, %d files", sn.System.OS, len(sn.System.FileSystems), sn.Stats.Files)
	case len(sn.Guests) > 0:
		kind, details = api.KindVMBackup, runinfo.Guests(sn)
	case len(sn.Images) > 0:
		kind, details = api.KindImageBackup, runinfo.Image(sn)
	default:
		kind = api.KindBackup
	}
	return kind, details, message
}

// importRepo records the backups of a repository found on t as the history
// of a removed agent named after the backed up computer: they appear in
// Activity and the restore wizard, and can be connected to the agent of
// that computer. Backups already recorded are skipped.
func (s *Store) importRepo(ctx context.Context, t Target, dir string, sns []*repo.Snapshot) (agentID int64, added int, err error) {
	hosts := map[string]int{}
	host := ""
	for _, sn := range sns {
		hosts[sn.Hostname]++
		if sn.Hostname != "" && hosts[sn.Hostname] > hosts[host] {
			host = sn.Hostname
		}
	}
	if host == "" {
		host = dir
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)
	var removed bool
	err = tx.QueryRow(ctx, `SELECT id, removed_at IS NOT NULL FROM agents WHERE repo_dir=$1`, dir).Scan(&agentID, &removed)
	switch {
	case err == nil && !removed:
		return 0, 0, fmt.Errorf("%s belongs to the agent %d", dir, agentID)
	case errors.Is(err, pgx.ErrNoRows):
		err = tx.QueryRow(ctx, `INSERT INTO agents(uuid, secret_hash, hostname, repo_dir, removed_at) VALUES($1,$2,$3,$4,now()) RETURNING id`,
			newUUID(), hashToken(randomToken(32)), clip(host, 255), dir).Scan(&agentID)
	}
	if err != nil {
		return 0, 0, err
	}
	url := repoURL(t, Agent{RepoDir: dir})
	have := map[string]bool{}
	rows, err := tx.Query(ctx, `SELECT snapshot_id FROM runs WHERE repo_url=$1 AND snapshot_id<>''`, url)
	if err != nil {
		return 0, 0, err
	}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		have[id] = true
	}
	rows.Close()
	for _, g := range groupSnapshots(sns) {
		for _, sn := range g.sns {
			if have[sn.ID.String()] {
				continue
			}
			kind, details, msg := importedRun(sn)
			stats, _ := json.Marshal(sn.Stats)
			status := api.StatusSuccess
			if len(sn.Stats.Errors) > 0 {
				status = api.StatusWarning
			}
			var det any
			if len(details) > 0 {
				det = details
			}
			errs := sn.Stats.Errors
			if errs == nil {
				errs = []string{}
			}
			paths := sn.Paths
			if paths == nil {
				paths = []string{}
			}
			if _, err := tx.Exec(ctx, `INSERT INTO runs(agent_id, job_name, kind, status, trigger, repo_url, target_id, paths, snapshot_id,
					queued_at, started_at, finished_at, stats, errors, message, details)
				VALUES($1,$2,$3,$4,'imported',$5,$6,$7,$8,$9,$9,$10,$11,$12,$13,$14)`,
				agentID, g.name, kind, status, url, t.ID, paths, sn.ID.String(), sn.Time, sn.Time.Add(sn.Stats.Duration), stats, errs, msg, det); err != nil {
				return 0, 0, err
			}
			added++
		}
	}
	return agentID, added, tx.Commit(ctx)
}

func (s *Server) handleTargetScan(w http.ResponseWriter, r *http.Request, user string) {
	id, _ := pathID(r)
	t, err := s.store.GetTarget(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), scanTimeout)
	defer cancel()
	res, err := s.scanTarget(ctx, t)
	data := map[string]any{"Target": t, "Result": res, "Error": ""}
	if err != nil {
		data["Error"] = requestLanguage(r).T(err.Error())
	}
	s.render(w, r, "target_scan", pageData{Title: "Find backups", Nav: "targets", User: user, Data: data})
}

func (s *Server) handleTargetImport(w http.ResponseWriter, r *http.Request, user string) {
	id, _ := pathID(r)
	t, err := s.store.GetTarget(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	r.ParseForm()
	l := requestLanguage(r)
	back := fmt.Sprintf("/targets/%d/scan", t.ID)
	dirs := r.Form["dir"]
	if len(dirs) == 0 {
		redirectErr(w, r, back, errors.New(l.T("Choose the backups to add.")))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), scanTimeout)
	defer cancel()
	known, err := s.store.knownRepoDirs(ctx, t)
	if err != nil {
		s.serverError(w, err)
		return
	}
	computers, backups := 0, 0
	for _, d := range dirs {
		if d == "" || d == consoleRepoDir || strings.ContainsAny(d, `/\`) || d == "." || d == ".." || known(d) {
			continue
		}
		sns, err := readRepoSnapshots(ctx, t, d)
		if err == nil && len(sns) == 0 {
			continue
		}
		var n int
		if err == nil {
			_, n, err = s.store.importRepo(ctx, t, d, sns)
		}
		if err != nil {
			redirectErr(w, r, back, fmt.Errorf("%s: %w", d, err))
			return
		}
		computers++
		backups += n
		s.audit(r, "storage.import", "added %d backups found in %s on storage %s", n, d, t.Name)
	}
	if backups == 0 {
		redirectErr(w, r, back, errors.New(l.T("No backups were added: the console already has them.")))
		return
	}
	redirectMsg(w, r, "/agents", l.T("Backups added: %d, of computers: %d. They are listed under removed agents and in Activity, and can be restored or connected to an agent.", backups, computers))
}
