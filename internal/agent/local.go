package agent

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/localipc"
)

// trayActive is how long after the last local status request the agent
// keeps asking the console for the job summary.
const trayActive = 2 * time.Minute

// localState is what the tray app sees.
type localState struct {
	mu      sync.Mutex
	st      localipc.Status
	askedAt time.Time // last status request from the tray
	pollNow chan struct{}
}

func (a *Agent) local() *localState {
	a.localOnce.Do(func() {
		h, _ := os.Hostname()
		a.ls = &localState{pollNow: make(chan struct{}, 1),
			st: localipc.Status{Version: a.version, Hostname: h, ServerURL: a.client.cfg.ServerURL, Enrolled: true, Jobs: []localipc.Job{}}}
	})
	return a.ls
}

func (a *Agent) wantStatus() bool {
	ls := a.local()
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return time.Since(ls.askedAt) < trayActive || ls.st.JobsAt == nil
}

// recordPoll stores the outcome of a poll.
func (a *Agent) recordPoll(err error, st *api.AgentStatus) {
	ls := a.local()
	ls.mu.Lock()
	defer ls.mu.Unlock()
	now := time.Now()
	if err != nil {
		ls.st.Connected, ls.st.LastError = false, err.Error()
		return
	}
	ls.st.Connected, ls.st.LastError, ls.st.LastContact = true, "", &now
	if st != nil {
		jobs := make([]localipc.Job, 0, len(st.Jobs))
		for _, j := range st.Jobs {
			jobs = append(jobs, localipc.Job{ID: j.ID, Name: j.Name, Kind: j.Kind, Enabled: j.Enabled, Schedule: j.Schedule,
				NextRun: j.NextRun, LastStatus: j.LastStatus, LastFinished: j.LastFinished, LastMessage: j.LastMessage, Running: j.Running})
		}
		ls.st.Jobs, ls.st.JobsAt = jobs, &now
	}
}

func (a *Agent) runStarted(run api.Run) {
	ls := a.local()
	ls.mu.Lock()
	ls.st.Current = &localipc.Run{ID: run.ID, Kind: run.Kind, Job: run.JobName, Started: time.Now()}
	ls.mu.Unlock()
}

func (a *Agent) runFinished(res api.RunResult) {
	ls := a.local()
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.st.Current == nil {
		return
	}
	r := *ls.st.Current
	now := time.Now()
	r.Finished, r.Status, r.Message = &now, res.Status, res.Message
	if r.Message == "" && len(res.Errors) > 0 {
		r.Message = res.Errors[0]
	}
	ls.st.Current, ls.st.Last = nil, &r
	ls.st.JobsAt = nil // refresh the job list at the next poll
}

// progress records how far the current run is.
func (a *Agent) progress(done, total, files uint64) {
	ls := a.local()
	ls.mu.Lock()
	if c := ls.st.Current; c != nil {
		c.Done, c.Total, c.Files = done, total, files
	}
	ls.mu.Unlock()
}

// ServeLocal answers the tray app until ctx ends.
func (a *Agent) ServeLocal(ctx context.Context) {
	l, err := localipc.Listen()
	if err != nil {
		a.log.Warn("local status channel unavailable", "err", err)
		return
	}
	localipc.Serve(ctx, l, a.handleLocal)
}

func (a *Agent) handleLocal(ctx context.Context, req localipc.Request) localipc.Response {
	ls := a.local()
	switch req.Cmd {
	case localipc.CmdStatus:
		ls.mu.Lock()
		first := time.Since(ls.askedAt) > trayActive
		ls.askedAt = time.Now()
		st := ls.st
		if st.Current != nil {
			c := *st.Current
			st.Current = &c
		}
		ls.mu.Unlock()
		if first {
			a.pollSoon() // fetch the job list now
		}
		return localipc.Response{OK: true, Status: &st}
	case localipc.CmdRun:
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		err := a.client.post(cctx, api.PathJobRunPrefix+strconv.FormatInt(req.JobID, 10)+"/run", struct{}{}, nil, true)
		if err != nil {
			return localipc.Response{Error: fmt.Sprintf("could not start the backup: %v", err)}
		}
		ls.mu.Lock()
		ls.st.JobsAt = nil
		ls.mu.Unlock()
		a.pollSoon()
		return localipc.Response{OK: true}
	}
	return localipc.Response{Error: "unknown command " + req.Cmd}
}

func (a *Agent) pollSoon() {
	select {
	case a.local().pollNow <- struct{}{}:
	default:
	}
}
