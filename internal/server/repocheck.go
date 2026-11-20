package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/max-zit/backupzit/internal/api"
)

// Full checks of the stored data: an agent reads every data pack of a
// repository and verifies it, which finds damage (bit rot, a failing NAS
// disk) that restore tests of a few files would miss. Checks run per
// repository — jobs of one agent on one target share it — on the interval of
// Settings → Restore tests.

// minRepoCheckVersion is the first agent version that checks repositories.
const minRepoCheckVersion = "0.39.0"

// repoCheckInterval is the interval of automatic checks (0: off).
func (x RestoreTestSettings) repoCheckInterval() time.Duration {
	switch x.CheckEvery {
	case "monthly":
		return 30 * 24 * time.Hour
	case "quarterly":
		return 91 * 24 * time.Hour
	}
	return 0
}

// QueueRepoCheck checks the repository of a job's newest backup.
func (s *Store) QueueRepoCheck(ctx context.Context, jobID int64, trigger string) (int64, error) {
	var agentID, targetID int64
	var repoURL string
	// On the job's agent: the newest run may be a part of a further VMware
	// proxy that is offline or removed now (M-N4); the repository is one.
	err := s.db.QueryRow(ctx, `SELECT j.agent_id, r.target_id, r.repo_url FROM runs r JOIN jobs j ON j.id=r.job_id
		WHERE r.job_id=$1 AND r.kind IN ('backup','image-backup','vm-backup','system-backup','copy','sql-backup','sql-log') AND r.status IN ('success','warning')
		AND r.target_id IS NOT NULL AND r.repo_url<>'' ORDER BY r.finished_at DESC LIMIT 1`, jobID).Scan(&agentID, &targetID, &repoURL)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errors.New("the job has no backup to check yet")
	}
	if err != nil {
		return 0, err
	}
	return s.queueRepoCheckOf(ctx, jobID, agentID, targetID, repoURL, trigger)
}

// queueRepoCheckOf queues a check of one repository (target and URL).
func (s *Store) queueRepoCheckOf(ctx context.Context, jobID, agentID, targetID int64, repoURL, trigger string) (int64, error) {
	if a, err := s.GetAgent(ctx, agentID); err == nil && agentOlder(a, minRepoCheckVersion) {
		return 0, fmt.Errorf("the agent on %s (%s) cannot check the stored data yet; update it to %s or newer", a.Hostname, a.Version, minRepoCheckVersion)
	}
	var id int64
	err := s.db.QueryRow(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, target_id, notified)
		SELECT $1,$2,'repo-check',$3,$4,$5,false
		WHERE NOT EXISTS (SELECT 1 FROM runs WHERE kind='repo-check' AND target_id=$5 AND repo_url=$4 AND status IN ('queued','running'))
		RETURNING id`, agentID, jobID, trigger, repoURL, targetID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errors.New("a check of this storage is already queued")
	}
	return id, err
}

// queueDueRepoChecks queues a check of every repository not checked within
// the interval (cloud storage only when allowed: reading all data costs).
// A repository is first checked once it is older than the interval.
func (s *Store) queueDueRepoChecks(ctx context.Context, now time.Time) ([]int64, error) {
	x := s.restoreTestSettings(ctx)
	every := x.repoCheckInterval()
	if every == 0 {
		return nil, nil
	}
	// The due pair itself is checked: a job with runs under two repository
	// URLs (rotating USB disks, a moved folder) would otherwise get its newest
	// repository checked again and again while the old pair stayed due.
	// The job's agent checks it: the run's may be a further VMware proxy that
	// was removed since (a check queued there would never start, C10).
	rows, err := s.db.Query(ctx, `SELECT DISTINCT ON (r.target_id, r.repo_url) r.job_id, j.agent_id, r.target_id, r.repo_url FROM runs r
		JOIN jobs j ON j.id=r.job_id JOIN storage_targets t ON t.id=r.target_id
		WHERE j.enabled AND r.kind IN ('backup','image-backup','vm-backup','system-backup','copy','sql-backup','sql-log')
		AND r.status IN ('success','warning') AND r.repo_url<>''
		AND ($1 OR t.kind NOT IN ('s3','azure'))
		AND NOT EXISTS (SELECT 1 FROM runs c WHERE c.kind='repo-check' AND c.target_id=r.target_id AND c.repo_url=r.repo_url AND c.queued_at > $2)
		AND EXISTS (SELECT 1 FROM runs o WHERE o.target_id=r.target_id AND o.repo_url=r.repo_url AND o.status IN ('success','warning') AND o.finished_at < $2)
		ORDER BY r.target_id, r.repo_url, r.finished_at DESC`, x.CheckCloud, now.Add(-every))
	if err != nil {
		return nil, err
	}
	type due struct {
		Job, Agent, Target int64
		URL                string
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (due, error) {
		var d due
		return d, r.Scan(&d.Job, &d.Agent, &d.Target, &d.URL)
	})
	if err != nil {
		return nil, err
	}
	var queued []int64
	for _, d := range list {
		if id, err := s.queueRepoCheckOf(ctx, d.Job, d.Agent, d.Target, d.URL, "schedule"); err == nil {
			queued = append(queued, id)
		}
	}
	return queued, nil
}

// handleJobCheck serves POST /jobs/{id}/check.
func (s *Server) handleJobCheck(w http.ResponseWriter, r *http.Request, _ string) {
	id, _ := pathID(r)
	back := fmt.Sprintf("/jobs/%d", id)
	rid, err := s.store.QueueRepoCheck(r.Context(), id, "manual")
	if err != nil {
		redirectErr(w, r, back, err)
		return
	}
	s.audit(r, "job.check", "job %d, run %d", id, rid)
	redirectMsg(w, r, fmt.Sprintf("/runs/%d", rid), "Check of the stored data queued.")
}

// minRepoRepairVersion is the first agent that repairs only proven damage
// (0.55, R-1): 0.54.2 also took out packs that only arrived short.
const minRepoRepairVersion = "0.55.0"

// damagedPacks is the number of damaged packs a failed data check found
// (0 for other runs).
func damagedPacks(run Run) int {
	if run.Kind != api.KindRepoCheck || run.Status != api.StatusFailed || len(run.Stats) == 0 {
		return 0
	}
	var st struct{ Damaged []json.RawMessage }
	if json.Unmarshal(run.Stats, &st) != nil {
		return 0
	}
	return len(st.Damaged)
}

// repairForgetFlag in the paths of a repo-repair run: also remove the
// backups that need the lost data (the runs table has no column for it).
const repairForgetFlag = "forget-damaged-backups"

// handleRepoRepair serves POST /runs/{id}/repair: after a data check that
// found damaged packs, the agent checks again under the exclusive lock and
// takes them out of the index (KindRepoRepair). Removing the backups that
// need the lost data too (forget=on) deletes backups: it is refused while
// retention of a job on that storage is paused after a suspicious backup,
// needs the password or two-factor code again, and waits for a second user
// with four-eyes approval on.
func (s *Server) handleRepoRepair(w http.ResponseWriter, r *http.Request, _ string) {
	id, _ := pathID(r)
	back := fmt.Sprintf("/runs/%d", id)
	run, err := s.store.GetRun(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.checkRepoRepair(r.Context(), run); err != nil {
		redirectErr(w, r, back, err)
		return
	}
	forget := r.FormValue("forget") == "on"
	if forget {
		if s.store.repoRetentionHold(r.Context(), *run.TargetID, run.RepoURL) {
			redirectErr(w, r, back, errRepairHold)
			return
		}
		if err := s.confirmIdentity(r); err != nil {
			redirectErr(w, r, back, err)
			return
		}
		name := fmt.Sprintf("data check %d", id)
		if run.JobName != nil {
			name = fmt.Sprintf("%s (data check %d)", *run.JobName, id)
		}
		if s.needsApproval(w, r, back, "repair.forget", id, name, nil) {
			return
		}
	}
	rid, err := s.store.queueRepoRepair(r.Context(), run, forget)
	if errors.Is(err, pgx.ErrNoRows) {
		redirectErr(w, r, back, errors.New("a check or repair of this storage is already queued"))
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	msg := "Repair queued: the storage is checked again, then the damaged data packs leave the index."
	if forget {
		msg = "Repair queued: the storage is checked again, then the damaged data packs leave the index and the backups that need their data are removed."
	}
	s.audit(r, "repo.repair", "run %d after data check %d (%d damaged packs, remove affected backups: %v)", rid, id, damagedPacks(run), forget)
	redirectMsg(w, r, fmt.Sprintf("/runs/%d", rid), msg)
}

// errRepairHold refuses removing backups while retention is paused.
var errRepairHold = errors.New("Retention of a job on this storage is paused after a suspicious backup: resume it before a repair removes backups, or repair without removing them.")

// checkRepoRepair tells whether run is a data check that can be repaired.
func (s *Server) checkRepoRepair(ctx context.Context, run Run) error {
	if damagedPacks(run) == 0 || run.JobID == nil || run.TargetID == nil {
		return errors.New("only a data check that found damaged packs can be repaired")
	}
	if a, err := s.store.GetAgent(ctx, run.AgentID); err == nil && agentOlder(a, minRepoRepairVersion) {
		return fmt.Errorf("the agent on %s (%s) cannot repair a repository yet; update it to %s or newer", a.Hostname, a.Version, minRepoRepairVersion)
	}
	return nil
}

// executeRepoRepairForget queues the approved repair that also removes the
// affected backups (four-eyes approval "repair.forget").
func (s *Server) executeRepoRepairForget(ctx context.Context, checkID int64) error {
	run, err := s.store.GetRun(ctx, checkID)
	if err != nil {
		return err
	}
	if err := s.checkRepoRepair(ctx, run); err != nil {
		return err
	}
	if s.store.repoRetentionHold(ctx, *run.TargetID, run.RepoURL) {
		return errRepairHold
	}
	if _, err := s.store.queueRepoRepair(ctx, run, true); errors.Is(err, pgx.ErrNoRows) {
		return errors.New("a check or repair of this storage is already queued")
	} else if err != nil {
		return err
	}
	return nil
}

// queueRepoRepair queues a repair of the repository of a data check, unless
// a check or repair of it is queued or running (pgx.ErrNoRows).
func (s *Store) queueRepoRepair(ctx context.Context, check Run, forget bool) (int64, error) {
	paths := []string{}
	if forget {
		paths = []string{repairForgetFlag}
	}
	var rid int64
	err := s.db.QueryRow(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, target_id, paths, notified)
		SELECT $1,$2,'repo-repair','manual',$3,$4,$5,false
		WHERE NOT EXISTS (SELECT 1 FROM runs WHERE kind IN ('repo-check','repo-repair') AND target_id=$4 AND repo_url=$3 AND status IN ('queued','running'))
		RETURNING id`, check.AgentID, *check.JobID, check.RepoURL, *check.TargetID, paths).Scan(&rid)
	return rid, err
}

// repoRetentionHold tells whether retention of a job whose backups are in the
// repository is paused after a suspicious backup (true also when unknown).
func (s *Store) repoRetentionHold(ctx context.Context, targetID int64, repoURL string) bool {
	// By repository, not by agent: the check of a split VMware job may run on
	// a further proxy that has no job of its own (M-N2).
	var hold bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM jobs j WHERE j.retention_hold AND j.target_id=$1
		AND EXISTS (SELECT 1 FROM runs r WHERE r.job_id=j.id AND r.target_id=$1 AND r.repo_url=$2))`, targetID, repoURL).Scan(&hold); err != nil {
		return true
	}
	return hold
}
