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
	mu sync.Mutex
	st localipc.Status
	// running are the runs in progress; st.Current is one of them (the
	// tray shows one).
	running map[int64]*localipc.Run
	// stages are steps after the data of running runs (for the console).
	stages map[int64]api.RunProgress
	// vms: the VM a VM backup works on and the VMs it finished (0.57).
	vms     map[int64]*api.RunProgress
	askedAt time.Time // last status request from the tray
	pollNow chan struct{}
}

func (a *Agent) local() *localState {
	a.localOnce.Do(func() {
		h, _ := os.Hostname()
		server := ""
		if a.client != nil && a.client.cfg != nil {
			server = a.client.cfg.ServerURL
		}
		a.ls = &localState{pollNow: make(chan struct{}, 1),
			st: localipc.Status{Version: a.version, Hostname: h, ServerURL: server, Enrolled: true, Jobs: []localipc.Job{}}}
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
		if st.ConsoleURL != "" {
			ls.st.ConsoleURL = st.ConsoleURL
		}
	}
}

func (a *Agent) runStarted(run api.Run) {
	ls := a.local()
	ls.mu.Lock()
	r := &localipc.Run{ID: run.ID, Kind: run.Kind, Job: run.JobName, Started: time.Now()}
	if ls.running == nil {
		ls.running = map[int64]*localipc.Run{}
	}
	ls.running[run.ID] = r
	if ls.st.Current == nil {
		ls.st.Current = r
	}
	ls.mu.Unlock()
}

func (a *Agent) runFinished(id int64, res api.RunResult) {
	ls := a.local()
	ls.mu.Lock()
	defer ls.mu.Unlock()
	cur := ls.running[id]
	if cur == nil {
		return
	}
	delete(ls.running, id)
	delete(ls.stages, id)
	delete(ls.vms, id)
	r := *cur
	now := time.Now()
	r.Finished, r.Status, r.Message = &now, res.Status, res.Message
	if r.Message == "" && len(res.Errors) > 0 {
		r.Message = res.Errors[0]
	}
	ls.st.Last = &r
	if ls.st.Current == cur {
		ls.st.Current = nil
		for _, o := range ls.running { // another run still in progress
			if ls.st.Current == nil || o.Started.Before(ls.st.Current.Started) {
				ls.st.Current = o
			}
		}
	}
	ls.st.JobsAt = nil // refresh the job list at the next poll
}

// progress records how far the run of ctx is.
func (a *Agent) progress(ctx context.Context, done, total, files uint64) {
	a.progressScanned(ctx, done, total, files, 0)
}

// progressScanned also records the size of the files processed so far
// (file backups).
func (a *Agent) progressScanned(ctx context.Context, done, total, files, scanned uint64) {
	ls := a.local()
	ls.mu.Lock()
	if c := ls.running[runIDOf(ctx)]; c != nil {
		c.Done, c.Total, c.Files, c.Scanned = done, total, files, scanned
	}
	ls.mu.Unlock()
}

// runProgress is the progress of the runs in progress, for the console.
func (a *Agent) runProgress() []api.RunProgress {
	ls := a.local()
	ls.mu.Lock()
	defer ls.mu.Unlock()
	var out []api.RunProgress
	for id, r := range ls.running {
		st := ls.stages[id]
		vm := ls.vms[id]
		if r.Done > 0 || r.Total > 0 || r.Files > 0 || st.Stage != "" || vm != nil {
			p := api.RunProgress{RunID: id, Done: r.Done, Total: r.Total, Files: r.Files, Scanned: r.Scanned,
				Stage: st.Stage, StageDone: st.StageDone, StageTotal: st.StageTotal}
			if vm != nil {
				p.VM, p.VMsDone, p.VMsTotal = vm.VM, vm.VMsDone, vm.VMsTotal
				p.VMsFinished = append([]api.VMFinished(nil), vm.VMsFinished...)
			}
			out = append(out, p)
		}
	}
	return out
}

// progressStage reports a step after the data of the run in ctx, e.g.
// "locks" (extending object locks) with done of total; "" ends it.
func (a *Agent) progressStage(ctx context.Context, stage string, done, total uint64) {
	ls := a.local()
	ls.mu.Lock()
	defer ls.mu.Unlock()
	id := runIDOf(ctx)
	if stage == "" {
		delete(ls.stages, id)
		return
	}
	if ls.stages == nil {
		ls.stages = map[int64]api.RunProgress{}
	}
	ls.stages[id] = api.RunProgress{Stage: stage, StageDone: done, StageTotal: total}
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
		st.MacPermissions = MacPermissions()
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

type runIDKey struct{}

// withRunID marks ctx as belonging to a run, for progress reports.
func withRunID(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, runIDKey{}, id)
}

func runIDOf(ctx context.Context) int64 {
	id, _ := ctx.Value(runIDKey{}).(int64)
	return id
}

// maxVMsListed bounds the finished VMs a run reports with its progress.
const maxVMsListed = 500

// vmStarted records that the VM backup of ctx works on the VM name, one of
// total (#44: the run showed only a percentage).
func (a *Agent) vmStarted(ctx context.Context, name string, total int) {
	ls := a.local()
	ls.mu.Lock()
	defer ls.mu.Unlock()
	id := runIDOf(ctx)
	if ls.vms == nil {
		ls.vms = map[int64]*api.RunProgress{}
	}
	v := ls.vms[id]
	if v == nil {
		v = &api.RunProgress{}
		ls.vms[id] = v
	}
	v.VM, v.VMsTotal = name, total
}

// vmFinished records that the VM backup of ctx finished the VM name.
func (a *Agent) vmFinished(ctx context.Context, name string, failed bool) {
	ls := a.local()
	ls.mu.Lock()
	defer ls.mu.Unlock()
	v := ls.vms[runIDOf(ctx)]
	if v == nil {
		return
	}
	v.VMsDone++
	if v.VM == name {
		v.VM = ""
	}
	if len(v.VMsFinished) < maxVMsListed {
		v.VMsFinished = append(v.VMsFinished, api.VMFinished{Name: name, Failed: failed})
	}
}
