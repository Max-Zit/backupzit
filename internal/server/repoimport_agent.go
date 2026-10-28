package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/repo"
)

// Local and USB disks are only reachable by the agent of the machine they
// are connected to: Find backups asks that agent to search the disk
// (a repo-scan run) and shows what it found.

// minRepoScanVersion is the first agent version that searches its disks.
const minRepoScanVersion = "0.44.0"

func localTarget(t Target) bool { return t.Kind == "local" || t.Kind == "usb" }

// QueueRepoScan asks an agent to search target t.
func (s *Store) QueueRepoScan(ctx context.Context, t Target, agentID int64) (int64, error) {
	a, err := s.GetAgent(ctx, agentID)
	if err != nil || a.RemovedAt != nil || a.Recovery {
		return 0, errors.New("choose an agent")
	}
	if agentOlder(a, minRepoScanVersion) {
		return 0, fmt.Errorf("the agent on %s (%s) cannot search its disks yet; update it to %s or newer", a.Hostname, a.Version, minRepoScanVersion)
	}
	var id int64
	err = s.db.QueryRow(ctx, `INSERT INTO runs(agent_id, kind, trigger, repo_url, target_id, notified) VALUES($1,'repo-scan','manual','',$2,true) RETURNING id`,
		agentID, t.ID).Scan(&id)
	return id, err
}

// scanAgents are the agents that can search a local disk, the one that
// used the target last first.
func (s *Store) scanAgents(ctx context.Context, t Target) ([]Agent, error) {
	agents, err := s.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	var last int64
	s.db.QueryRow(ctx, `SELECT agent_id FROM runs WHERE target_id=$1 AND kind<>'repo-scan' ORDER BY queued_at DESC LIMIT 1`, t.ID).Scan(&last)
	var out []Agent
	for _, a := range agents {
		if a.Recovery {
			continue
		}
		if a.ID == last {
			out = append([]Agent{a}, out...)
		} else {
			out = append(out, a)
		}
	}
	return out, nil
}

// repoScanRun is the search run of target t, if id names one.
func (s *Store) repoScanRun(ctx context.Context, t Target, id string) (Run, bool) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return Run{}, false
	}
	run, err := s.GetRun(ctx, n)
	if err != nil || run.Kind != api.KindRepoScan || run.TargetID == nil || *run.TargetID != t.ID {
		return Run{}, false
	}
	return run, true
}

// foundSnapshots are the backups of each repository a search found.
func foundSnapshots(run Run) (api.RepoScan, map[string][]*repo.Snapshot, error) {
	var scan api.RepoScan
	if err := json.Unmarshal(run.Details, &scan); err != nil {
		return scan, nil, errors.New("the agent sent no result")
	}
	out := map[string][]*repo.Snapshot{}
	for _, f := range scan.Repos {
		var sns []*repo.Snapshot
		for _, x := range f.Snapshots {
			id, err := repo.ParseID(x.ID)
			if err != nil {
				continue
			}
			sn := &repo.Snapshot{}
			if json.Unmarshal(x.Snapshot, sn) != nil {
				continue
			}
			sn.ID = id
			sns = append(sns, sn)
		}
		out[f.Dir] = sns
	}
	return scan, out, nil
}

// scanFromRun is what the search run found, as for network storage.
func (s *Server) scanFromRun(ctx context.Context, t Target, run Run) (scanResult, error) {
	var res scanResult
	scan, sns, err := foundSnapshots(run)
	if err != nil {
		return res, err
	}
	known, err := s.store.knownRepoDirs(ctx, t, scan.Base)
	if err != nil {
		return res, err
	}
	for _, d := range scan.Repos {
		if d.Dir == consoleRepoDir {
			continue
		}
		if known(d.Dir) {
			res.Known++
			continue
		}
		f := foundRepo{Dir: d.Dir, Error: d.Error}
		if f.Error == "" {
			f.summarize(sns[d.Dir])
		}
		res.Unknown = append(res.Unknown, f)
	}
	return res, nil
}

// handleTargetScanStart serves POST /targets/{id}/scan (local and USB disks).
func (s *Server) handleTargetScanStart(w http.ResponseWriter, r *http.Request, _ string) {
	id, _ := pathID(r)
	t, err := s.store.GetTarget(r.Context(), id)
	if err != nil || !localTarget(t) {
		http.NotFound(w, r)
		return
	}
	back := fmt.Sprintf("/targets/%d/scan", t.ID)
	agentID, _ := strconv.ParseInt(r.FormValue("agent_id"), 10, 64)
	rid, err := s.store.QueueRepoScan(r.Context(), t, agentID)
	if err != nil {
		redirectErr(w, r, back, err)
		return
	}
	s.audit(r, "storage.scan", "search of storage %s by agent %d, run %d", t.Name, agentID, rid)
	http.Redirect(w, r, fmt.Sprintf("%s?run=%d", back, rid), http.StatusSeeOther)
}
