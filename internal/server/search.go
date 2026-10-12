package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/max-zit/backupzit/internal/repo"
)

// Search across backups: finds files, folders and Microsoft 365 mails (whose
// file names hold the subject) by name in every file backup the console can
// read, across all dates, without opening restore points one by one.
// Unchanged folders have the same tree in every backup, so each tree is read
// once per search (and kept a while for the next one).

// searchMaxHits limits the distinct paths a search returns.
const searchMaxHits = 500

// searchTreeHits limits the matches remembered below one folder.
const searchTreeHits = 5000

// SearchVersion is one state of a found file: the backups that hold it
// unchanged.
type SearchVersion struct {
	Size    uint64
	ModTime time.Time
	// Run is the newest backup with this state, At its time; Count is how
	// many backups hold it and Oldest the time of the oldest.
	Run    int64
	At     time.Time
	Count  int
	Oldest time.Time
}

// SearchHit is a found file or folder of a job.
type SearchHit struct {
	JobID    int64
	Job      string
	Agent    string
	Name     string
	Path     string // components joined with "/"
	Dir      string // its folder, for the file browser
	Display  string // the full path in the agent's syntax
	IsDir    bool
	Versions []SearchVersion // newest first
}

// SearchResult is the outcome of a search.
type SearchResult struct {
	Hits     []SearchHit
	Backups  int // backups searched
	Skipped  int // backups the console cannot read (agent disks, SSH keys)
	Partial  bool
	TooMany  bool
	Duration time.Duration
}

type searchMatch struct {
	comps   []string
	isDir   bool
	size    uint64
	modTime time.Time
}

// liteNode is what a search keeps of a tree entry.
type liteNode struct {
	name    string
	isDir   bool
	size    uint64
	modTime time.Time
	subtree *repo.ID
}

// treeCache keeps the names of recently read trees for later searches.
type treeCache struct {
	mu    sync.Mutex
	trees map[string][]liteNode
	nodes int
}

const treeCacheNodes = 500000

func (c *treeCache) get(key string) ([]liteNode, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.trees[key]
	return t, ok
}

func (c *treeCache) put(key string, t []liteNode) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.trees == nil || c.nodes+len(t) > treeCacheNodes {
		c.trees, c.nodes = map[string][]liteNode{}, 0 // start over when full
	}
	c.trees[key] = t
	c.nodes += len(t)
}

// searchMatcher matches names: with * or ? as a pattern, otherwise as a
// part of the name; both without case.
func searchMatcher(q string) func(string) bool {
	q = strings.ToLower(strings.TrimSpace(q))
	if strings.ContainsAny(q, "*?") {
		return func(name string) bool {
			ok, _ := path.Match(q, strings.ToLower(name))
			return ok
		}
	}
	return func(name string) bool { return strings.Contains(strings.ToLower(name), q) }
}

// searchBackups searches the file backups of all jobs (or of one).
func (s *Server) searchBackups(ctx context.Context, q string, jobID int64) (*SearchResult, error) {
	q = strings.TrimSpace(q)
	if len([]rune(strings.Trim(q, "*?"))) < 2 {
		return nil, errors.New("enter at least two characters of the name")
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	match := searchMatcher(q)
	rows, err := s.store.db.Query(ctx, `SELECT `+runCols+runFrom+` WHERE r.kind IN ('backup','system-backup')
		AND r.status IN ('success','warning') AND r.snapshot_id<>'' AND NOT r.expired AND r.target_id IS NOT NULL
		AND r.job_id IS NOT NULL AND ($1=0 OR r.job_id=$1) ORDER BY r.finished_at DESC LIMIT 5000`, jobID)
	if err != nil {
		return nil, err
	}
	var runs []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		runs = append(runs, r)
	}
	rows.Close()

	res := &SearchResult{}
	hits := map[string]*SearchHit{}
	var order []string
	memo := map[string][]searchMatch{}
	windows := map[int64]bool{}
	for _, run := range runs {
		if ctx.Err() != nil {
			res.Partial = true
			break
		}
		r, sn, err := s.fileSnapshot(ctx, run)
		if err != nil {
			res.Skipped++
			continue
		}
		res.Backups++
		repoKey := fmt.Sprintf("%d|%s|", *run.TargetID, run.RepoURL)
		ms, err := s.searchTree(ctx, r, repoKey, sn.Tree, match, memo)
		if err != nil {
			if ctx.Err() != nil {
				res.Partial = true
				break
			}
			res.Skipped++
			continue
		}
		if _, ok := windows[run.AgentID]; !ok {
			windows[run.AgentID] = s.runIsWindows(ctx, run)
		}
		for _, m := range ms {
			p := strings.Join(m.comps, "/")
			key := fmt.Sprintf("%d|%s", *run.JobID, p)
			h := hits[key]
			if h == nil {
				if len(hits) >= searchMaxHits {
					res.TooMany = true
					continue
				}
				h = &SearchHit{JobID: *run.JobID, Job: deref(run.JobName), Agent: run.Hostname, Name: m.comps[len(m.comps)-1], Path: p,
					Dir: strings.Join(m.comps[:len(m.comps)-1], "/"), Display: agentSyntaxPath(m.comps, windows[run.AgentID]), IsDir: m.isDir}
				hits[key] = h
				order = append(order, key)
			}
			at := run.QueuedAt
			if run.FinishedAt != nil {
				at = *run.FinishedAt
			}
			// Runs come newest first: extend the last version while the
			// file is unchanged.
			if n := len(h.Versions); n > 0 && h.Versions[n-1].Size == m.size && h.Versions[n-1].ModTime.Equal(m.modTime) {
				h.Versions[n-1].Count++
				h.Versions[n-1].Oldest = at
				continue
			}
			h.Versions = append(h.Versions, SearchVersion{Size: m.size, ModTime: m.modTime, Run: run.ID, At: at, Count: 1, Oldest: at})
		}
	}
	for _, k := range order {
		res.Hits = append(res.Hits, *hits[k])
	}
	sort.SliceStable(res.Hits, func(i, j int) bool { return res.Hits[i].Versions[0].At.After(res.Hits[j].Versions[0].At) })
	res.Duration = time.Since(start)
	return res, nil
}

// searchTree returns the matches below a tree, reading each tree once.
func (s *Server) searchTree(ctx context.Context, r *repo.Repository, repoKey string, id repo.ID, match func(string) bool, memo map[string][]searchMatch) ([]searchMatch, error) {
	key := repoKey + id.String()
	if m, ok := memo[key]; ok {
		return m, nil
	}
	nodes, ok := s.trees.get(key)
	if !ok {
		t, err := r.LoadTree(ctx, id)
		if err != nil {
			return nil, err
		}
		nodes = make([]liteNode, 0, len(t.Nodes))
		for _, n := range t.Nodes {
			nodes = append(nodes, liteNode{name: n.Name, isDir: n.Type == repo.NodeDir, size: n.Size, modTime: n.ModTime, subtree: n.Subtree})
		}
		s.trees.put(key, nodes)
	}
	var out []searchMatch
	for _, n := range nodes {
		if len(out) >= searchTreeHits {
			break
		}
		if match(n.name) {
			out = append(out, searchMatch{comps: []string{n.name}, isDir: n.isDir, size: n.size, modTime: n.modTime})
		}
		if n.isDir && n.subtree != nil {
			sub, err := s.searchTree(ctx, r, repoKey, *n.subtree, match, memo)
			if err != nil {
				return nil, err
			}
			for _, m := range sub {
				if len(out) >= searchTreeHits {
					break
				}
				out = append(out, searchMatch{comps: append([]string{n.name}, m.comps...), isDir: m.isDir, size: m.size, modTime: m.modTime})
			}
		}
	}
	memo[key] = out
	return out, nil
}

// handleSearch serves GET /search.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request, user string) {
	q := r.URL.Query().Get("q")
	jobID, _ := strconv.ParseInt(r.URL.Query().Get("job"), 10, 64)
	jobs, err := s.store.ListJobs(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	data := map[string]any{"Q": q, "JobID": jobID, "Jobs": jobs}
	pd := pageData{Title: "Search backups", Nav: "search", User: user, Data: data}
	if strings.TrimSpace(q) != "" {
		res, err := s.searchBackups(r.Context(), q, jobID)
		if err != nil {
			pd.Error = err.Error()
		} else {
			data["Result"] = res
		}
	}
	s.render(w, r, "search", pd)
}
