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
	// Estimated marks a percentage estimated from the files and the size of
	// the job's previous backup (file backups do not know their size in
	// advance); none once both are exceeded.
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
		case x.Files > 0 && runs[i].JobID != nil && (runs[i].Kind == api.KindBackup || runs[i].Kind == api.KindSystemBackup):
			files, bytes := s.store.lastBackupSize(ctx, *runs[i].JobID, runs[i].ID)
			p.FilesAll = files
			if pc, ok := estimatePercent(x.Files, files, x.Scanned, bytes); ok {
				p.Percent, p.Estimated = pc, true
			}
		}
		runs[i].Progress = p
	}
	return runs
}

// estimatePercent estimates a file backup from the previous one: the share
// of its files and of its size processed so far, averaged (many small
// files and a few big ones both count). A measure already past the
// previous backup counts as done; when both are past it, there is no
// estimate (the data grew a lot).
func estimatePercent(files, prevFiles, scanned, prevBytes uint64) (int, bool) {
	var parts []float64
	past := 0
	add := func(done, prev uint64) {
		if prev == 0 || done == 0 {
			return
		}
		f := float64(done) / float64(prev)
		if f >= 1 {
			f, past = 1, past+1
		}
		parts = append(parts, f)
	}
	add(files, prevFiles)
	add(scanned, prevBytes)
	if len(parts) == 0 || past == len(parts) {
		return 0, false
	}
	sum := 0.0
	for _, f := range parts {
		sum += f
	}
	return min(int(sum*100/float64(len(parts))), 99), true
}

// lastBackupSize is the number of files and their size of the job's newest
// successful backup before run (0 if none).
func (s *Store) lastBackupSize(ctx context.Context, jobID, runID int64) (files, bytes uint64) {
	var stats json.RawMessage
	if s.db.QueryRow(ctx, `SELECT stats FROM runs WHERE job_id=$1 AND id<>$2 AND status IN ('success','warning') AND stats IS NOT NULL
		ORDER BY finished_at DESC LIMIT 1`, jobID, runID).Scan(&stats) != nil {
		return 0, 0
	}
	var st struct {
		Files uint64 `json:"files"`
		Bytes uint64 `json:"bytes"`
	}
	json.Unmarshal(stats, &st)
	return st.Files, st.Bytes
}
