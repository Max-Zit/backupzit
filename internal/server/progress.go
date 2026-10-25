package server

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/max-zit/backupzit/internal/api"
)

// liveProgress holds how far running runs are, as the agents report it
// with their polls (kept in memory only).
type liveProgress struct {
	mu sync.Mutex
	m  map[int64]progressEntry
}

type progressEntry struct {
	api.RunProgress
	agentID int64 // the agent that reported it
	at      time.Time
}

// set records the progress an agent reported; entries of runs that stopped
// reporting are dropped after a while.
func (p *liveProgress) set(agentID int64, list []api.RunProgress, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[int64]progressEntry{}
	}
	for _, x := range list {
		p.m[x.RunID] = progressEntry{RunProgress: x, agentID: agentID, at: now}
	}
	for id, e := range p.m {
		if now.Sub(e.at) > 10*time.Minute {
			delete(p.m, id)
		}
	}
}

// get is the progress of the run; only its own agent can report it.
func (p *liveProgress) get(agentID, runID int64) (api.RunProgress, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.m[runID]
	return e.RunProgress, ok && e.agentID == agentID
}

// runProgress is the progress of a running run for the pages.
type runProgress struct {
	// Percent is 0–100, or -1 when the size is not known yet.
	Percent int
	// Estimated marks a percentage estimated from the files of the job's
	// previous backup (file backups do not know their size in advance); none once the files exceed that count.
	Estimated bool
	Done      string // bytes processed
	Total     string // "" if unknown
	Files     int
	FilesAll  uint64 // files of the previous backup, 0 if unknown
}

// withProgress adds the reported progress to the running runs.
func (s *Server) withProgress(ctx context.Context, runs []Run) []Run {
	for i := range runs {
		if runs[i].Status != "running" {
			continue
		}
		x, ok := s.live.get(runs[i].AgentID, runs[i].ID)
		if !ok || (x.Done == 0 && x.Total == 0 && x.Files == 0) {
			continue
		}
		p := &runProgress{Percent: -1, Done: humanBytes(x.Done), Files: int(x.Files)}
		switch {
		case x.Total > 0:
			p.Total = humanBytes(x.Total)
			p.Percent = int(min(x.Done*100/x.Total, 100))
		case x.Files > 0 && runs[i].JobID != nil:
			// Past the previous count the estimate means nothing: no percentage.
			if all := s.store.lastBackupFiles(ctx, *runs[i].JobID, runs[i].ID); all > 0 && x.Files < all {
				p.FilesAll = all
				p.Percent = int(min(x.Files*100/all, 99))
				p.Estimated = true
			}
		}
		runs[i].Progress = p
	}
	return runs
}

// lastBackupFiles is the number of files of the job's newest successful
// backup before run (0 if none).
func (s *Store) lastBackupFiles(ctx context.Context, jobID, runID int64) uint64 {
	var stats json.RawMessage
	if s.db.QueryRow(ctx, `SELECT stats FROM runs WHERE job_id=$1 AND id<>$2 AND status IN ('success','warning') AND stats IS NOT NULL
		ORDER BY finished_at DESC LIMIT 1`, jobID, runID).Scan(&stats) != nil {
		return 0
	}
	var st struct {
		Files uint64 `json:"files"`
	}
	json.Unmarshal(stats, &st)
	return st.Files
}
