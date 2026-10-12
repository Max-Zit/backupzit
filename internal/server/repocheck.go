package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
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
	err := s.db.QueryRow(ctx, `SELECT agent_id, target_id, repo_url FROM runs
		WHERE job_id=$1 AND kind IN ('backup','image-backup','vm-backup','system-backup','copy','sql-backup','sql-log') AND status IN ('success','warning')
		AND target_id IS NOT NULL AND repo_url<>'' ORDER BY finished_at DESC LIMIT 1`, jobID).Scan(&agentID, &targetID, &repoURL)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errors.New("the job has no backup to check yet")
	}
	if err != nil {
		return 0, err
	}
	if a, err := s.GetAgent(ctx, agentID); err == nil && agentOlder(a, minRepoCheckVersion) {
		return 0, fmt.Errorf("the agent on %s (%s) cannot check the stored data yet; update it to %s or newer", a.Hostname, a.Version, minRepoCheckVersion)
	}
	var id int64
	err = s.db.QueryRow(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, target_id, notified)
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
	rows, err := s.db.Query(ctx, `SELECT DISTINCT ON (r.target_id, r.repo_url) r.job_id FROM runs r
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
	jobs, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	var queued []int64
	for _, j := range jobs {
		if id, err := s.QueueRepoCheck(ctx, j, "schedule"); err == nil {
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
