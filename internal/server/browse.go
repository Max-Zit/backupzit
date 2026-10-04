package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/imaging"
	"github.com/max-zit/backupzit/internal/repo"
)

// repoCache keeps repositories and image volumes open for a while so that
// browsing an image does not reload the index on every click.
type repoCache struct {
	mu    sync.Mutex
	repos map[string]*cachedRepo
	vols  map[string]*cachedVolume
	vm    map[string]*cachedVMDisk
}

type cachedRepo struct {
	r    *repo.Repository
	used time.Time
}

type cachedVolume struct {
	v    *imaging.Volume
	p    *repo.PartitionImage
	used time.Time
}

const cacheTTL = 10 * time.Minute

func newRepoCache() *repoCache {
	return &repoCache{repos: map[string]*cachedRepo{}, vols: map[string]*cachedVolume{}}
}

func (c *repoCache) expire() {
	for k, v := range c.vols {
		if time.Since(v.used) > cacheTTL {
			delete(c.vols, k)
		}
	}
	for k, r := range c.repos {
		if time.Since(r.used) > cacheTTL {
			r.r.Close()
			delete(c.repos, k)
		}
	}
}

// ErrBrowseLocal means the repository lives on the agent's own disk.
var ErrBrowseLocal = errors.New("this backup is stored on a local path of the agent; the console can only browse backups on network storage (SFTP, S3 or SMB)")

func (c *repoCache) repo(ctx context.Context, t Target, url string) (*repo.Repository, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expire()
	if cr, ok := c.repos[url]; ok {
		cr.used = time.Now()
		return cr.r, nil
	}
	if t.Kind == "local" || t.Kind == "usb" {
		return nil, ErrBrowseLocal
	}
	// The repository is used for many requests; do not tie it to one.
	be, err := backend.Open(context.WithoutCancel(ctx), url, backend.Options{
		SFTPPassword: t.SFTPPassword, SFTPKeyFile: "", SFTPHostKey: t.SFTPHostKey,
		S3AccessKey: t.S3AccessKey, S3SecretKey: t.S3SecretKey, S3Region: t.S3Region, S3LockDays: t.S3LockDays,
		SMBPassword: t.SMBPassword, SMBDomain: t.SMBDomain,
		HardenedKey: t.HardenedKey, HardenedFingerprint: t.HardenedFingerprint,
		AzureKey: t.AzureKey, AzureSAS: t.AzureSAS,
	})
	if err != nil {
		return nil, fmt.Errorf("connect to storage: %w", err)
	}
	r, err := repo.Open(context.WithoutCancel(ctx), be, repo.Password(t.RecoveryKey))
	if err != nil {
		be.Close()
		return nil, fmt.Errorf("open repository: %w", err)
	}
	c.repos[url] = &cachedRepo{r: r, used: time.Now()}
	return r, nil
}

// volume opens partition part of the image produced by backup run run.
func (s *Server) imageVolume(ctx context.Context, run Run, part int) (*imaging.Volume, *repo.PartitionImage, error) {
	if run.Kind != api.KindImageBackup || run.SnapshotID == "" {
		return nil, nil, errors.New("run has no disk image")
	}
	if run.TargetID == nil {
		return nil, nil, errors.New("storage target was deleted")
	}
	key := fmt.Sprintf("%s/%d", run.SnapshotID, part)
	s.cache.mu.Lock()
	if cv, ok := s.cache.vols[key]; ok {
		cv.used = time.Now()
		s.cache.mu.Unlock()
		return cv.v, cv.p, nil
	}
	s.cache.mu.Unlock()

	t, err := s.store.GetTarget(ctx, *run.TargetID)
	if err != nil {
		return nil, nil, errors.New("storage target was deleted")
	}
	if t.SFTPKey != "" {
		return nil, nil, errors.New("browsing storage that uses SSH key authentication is not supported yet")
	}
	r, err := s.cache.repo(ctx, t, run.RepoURL)
	if err != nil {
		return nil, nil, err
	}
	sn, err := r.LoadSnapshot(ctx, run.SnapshotID)
	if err != nil {
		return nil, nil, err
	}
	p, v, err := imaging.OpenSnapshotVolume(context.WithoutCancel(ctx), r, sn, part)
	if err != nil {
		return nil, nil, err
	}
	s.cache.mu.Lock()
	s.cache.vols[key] = &cachedVolume{v: v, p: p, used: time.Now()}
	s.cache.mu.Unlock()
	return v, p, nil
}

type crumb struct{ Name, Path string }

func crumbs(p string) []crumb {
	out := []crumb{{Name: "root", Path: `\`}}
	cur := ""
	for _, s := range strings.Split(strings.Trim(p, `\`), `\`) {
		if s == "" {
			continue
		}
		cur += `\` + s
		out = append(out, crumb{Name: s, Path: cur})
	}
	return out
}

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request, user string) {
	id, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	run, err := s.store.GetRun(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.serverError(w, err)
		return
	}
	part, _ := strconv.Atoi(r.URL.Query().Get("part"))
	dir := imaging.CleanPath(r.URL.Query().Get("path"))
	data := map[string]any{"Run": run, "Part": part, "Path": dir, "Crumbs": crumbs(dir)}
	v, p, err := s.imageVolume(r.Context(), run, part)
	if err == nil {
		data["Partition"] = p
		var entries []imaging.Entry
		entries, err = v.List(dir)
		data["Entries"] = entries
	}
	agents, aerr := s.store.ListAgents(r.Context())
	if aerr != nil {
		s.serverError(w, aerr)
		return
	}
	data["Agents"] = agents
	pd := pageData{Title: fmt.Sprintf("Browse image #%d", run.ID), Nav: "runs", User: user, Data: data}
	if err != nil {
		pd.Error = err.Error()
	}
	s.render(w, r, "browse", pd)
}

func (s *Server) handleFilesRestore(w http.ResponseWriter, r *http.Request, _ string) {
	id, _ := pathID(r)
	r.ParseForm()
	part, _ := strconv.Atoi(r.FormValue("part"))
	back := fmt.Sprintf("/runs/%d/browse?", id) + url.Values{"part": {strconv.Itoa(part)}, "path": {r.FormValue("path")}}.Encode()
	paths := r.Form["sel"]
	if len(paths) == 0 {
		redirectErr(w, r, back, errors.New("tick the files and folders to restore"))
		return
	}
	target := strings.TrimSpace(r.FormValue("target"))
	if r.FormValue("mode") == "original" {
		target = ""
	} else if target == "" {
		redirectErr(w, r, back, errors.New("enter a folder to restore into, or choose original location"))
		return
	}
	rid, err := s.store.QueueImageFileRestore(r.Context(), id, formID(r, "agent_id"), part, paths, target)
	if err != nil {
		redirectErr(w, r, back, err)
		return
	}
	s.audit(r, "restore.files", "run #%d from backup #%d, %d paths to %q", rid, id, len(paths), target)
	redirectMsg(w, r, fmt.Sprintf("/runs/%d", rid), "File restore queued.")
}

// QueueImageFileRestore creates a run that extracts paths from partition
// part of an image backup on the given agent.
func (s *Store) QueueImageFileRestore(ctx context.Context, backupRunID, agentID int64, part int, paths []string, target string) (int64, error) {
	b, err := s.GetRun(ctx, backupRunID)
	if err != nil {
		return 0, err
	}
	if b.Kind != api.KindImageBackup || b.SnapshotID == "" {
		return 0, errors.New("run has no disk image")
	}
	if _, err := s.GetAgent(ctx, agentID); err != nil {
		return 0, errors.New("unknown agent")
	}
	clean := make([]string, len(paths))
	for i, p := range paths {
		clean[i] = imaging.CleanPath(p)
	}
	var id int64
	err = s.db.QueryRow(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, target_id, snapshot_id, paths, restore_target, image_partitions)
		VALUES($1,$2,'image-file-restore','manual',$3,$4,$5,$6,$7,$8) RETURNING id`,
		agentID, b.JobID, b.RepoURL, b.TargetID, b.SnapshotID, clean, target, []int{part}).Scan(&id)
	return id, err
}
