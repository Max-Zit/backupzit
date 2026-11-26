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
	// movedAt is when the reported numbers last changed; samples are the
	// bytes done over the last minutes, for the speed (#75).
	movedAt time.Time
	samples []progressSample
	// stallNoted: the run was handed to noteCopyStalls once.
	stallNoted bool
}

type progressSample struct {
	at   time.Time
	done uint64
}

// moved reports whether a run got further between two reports.
func moved(a, b api.RunProgress) bool {
	return a.Done != b.Done || a.Files != b.Files || a.Scanned != b.Scanned || a.Stage != b.Stage ||
		a.StageDone != b.StageDone || a.VM != b.VM || a.VMsDone != b.VMsDone
}

// speedWindow is how far back the speed looks.
const speedWindow = 5 * time.Minute

// set records the progress an agent reported; entries of runs that stopped
// reporting are dropped after a while.
func (p *liveProgress) set(agentID int64, list []api.RunProgress, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[int64]progressEntry{}
	}
	for _, x := range list {
		n := progressEntry{RunProgress: x, agentID: agentID, at: now, movedAt: now}
		if e, ok := p.m[x.RunID]; ok && e.agentID == agentID {
			if !moved(e.RunProgress, x) {
				n.movedAt = e.movedAt
			}
			n.stallNoted = e.stallNoted
			if x.Done >= e.Done && x.Total == e.Total {
				n.samples = e.samples
			}
		}
		n.samples = append(n.samples, progressSample{now, x.Done})
		cut := 0
		for cut < len(n.samples)-2 && now.Sub(n.samples[cut].at) > speedWindow {
			cut++
		}
		n.samples = append([]progressSample(nil), n.samples[cut:]...)
		p.m[x.RunID] = n
	}
	for id, e := range p.m {
		if now.Sub(e.at) > 10*time.Minute {
			delete(p.m, id)
		}
	}
}

// get is the progress of the run; only its own agent can report it.
func (p *liveProgress) get(agentID, runID int64) (progressEntry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.m[runID]
	return e, ok && e.agentID == agentID
}

// rate is the bytes per second over the last minutes, 0 when not known
// yet (less than 20 seconds of samples).
func (e progressEntry) rate() float64 {
	if len(e.samples) < 2 {
		return 0
	}
	first, last := e.samples[0], e.samples[len(e.samples)-1]
	secs := last.at.Sub(first.at).Seconds()
	if secs < 20 || last.done < first.done {
		return 0
	}
	return float64(last.done-first.done) / secs
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
	// Packs marks a data check or repair: the agent counts the data packs it
	// has read, not bytes, so the page shows PacksDone of PacksTotal (#64).
	Packs                 bool
	PacksDone, PacksTotal uint64
	// Stage is a step after the data ("locks": extending object locks)
	// with its own count.
	Stage                 string
	StageDone, StageTotal uint64
	// VM is the VM a VM backup works on, number VMsDone+1 of VMsTotal;
	// VMsFinished are the VMs done so far (0.57).
	VM                 string
	VMNumber, VMsTotal int
	VMsFinished        []api.VMFinished
	// Speed is the bytes per second over the last minutes and LeftH and
	// LeftM the time left at that speed (runs that know their size, #75).
	Speed        string
	Left         bool
	LeftH, LeftM int
	// Backup is the time of the backup a copy run works on, First its
	// first copy to that storage (everything is transferred), StalledMin
	// the minutes without data moving while the agent answers (#75).
	Backup     string
	First      bool
	StalledMin int
}

// copyStallAfter is how long a copy may move no data before the run page
// and a notice point it out; it is not failed for that (slow storage may
// pause and recover).
const copyStallAfter = 30 * time.Minute

// withProgress adds the reported progress to the running runs.
func (s *Server) withProgress(ctx context.Context, runs []Run) []Run {
	for i := range runs {
		if runs[i].Status != "running" {
			continue
		}
		x, ok := s.live.get(runs[i].AgentID, runs[i].ID)
		if !ok || (x.Done == 0 && x.Total == 0 && x.Files == 0 && x.Stage == "" && x.VMsTotal == 0) {
			continue
		}
		p := &runProgress{Percent: -1, Done: humanBytes(x.Done), Files: int(x.Files), // #nosec G115 -- sizes and counts for display and statistics, not negative
			Stage: x.Stage, StageDone: x.StageDone, StageTotal: x.StageTotal,
			VM: x.VM, VMsTotal: x.VMsTotal, VMsFinished: x.VMsFinished}
		if x.VM != "" {
			p.VMNumber = min(x.VMsDone+1, x.VMsTotal)
		}
		if x.Backup != nil {
			p.Backup = x.Backup.Local().Format("2006-01-02 15:04")
		}
		p.First = x.First
		if runs[i].Kind == api.KindCopy && x.Stage == "copy" {
			if d := s.clock().Sub(x.movedAt); d >= copyStallAfter {
				p.StalledMin = int(d.Minutes())
			}
		}
		if countsPacks(runs[i].Kind) {
			p.Packs, p.PacksDone, p.PacksTotal = true, x.Done, x.Total
		}
		switch {
		case x.Total > 0:
			p.Total = humanBytes(x.Total)
			p.Percent = int(min(x.Done*100/x.Total, 100))
			if r := x.rate(); r > 0 && !p.Packs {
				p.Speed = humanBytes(uint64(r)) + "/s"
				if x.Done < x.Total {
					left := time.Duration(float64(x.Total-x.Done) / r * float64(time.Second))
					p.Left, p.LeftH, p.LeftM = true, int(left.Hours()), int(left.Minutes())%60
				}
			}
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

// countsPacks reports whether the agent counts the progress of a run of this
// kind in data packs rather than bytes: the data check and the repair read
// the repository pack by pack (every agent version since the check exists).
func countsPacks(kind string) bool {
	return kind == api.KindRepoCheck || kind == api.KindRepoRepair
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
	// Of the same part of a split VMware backup (the parts hold other VMs).
	if s.db.QueryRow(ctx, `SELECT stats FROM runs WHERE job_id=$1 AND id<>$2 AND status IN ('success','warning') AND stats IS NOT NULL
		AND part=(SELECT part FROM runs WHERE id=$2) ORDER BY finished_at DESC LIMIT 1`, jobID, runID).Scan(&stats) != nil {
		return 0, 0
	}
	var st struct {
		Files uint64 `json:"files"`
		Bytes uint64 `json:"bytes"`
	}
	_ = json.Unmarshal(stats, &st)
	return st.Files, st.Bytes
}
