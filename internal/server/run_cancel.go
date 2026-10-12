package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Cancelling runs: a queued run fails at once; a running run is marked and
// its agent stops it on its next poll (the result then carries the reason).
// A run whose agent does not confirm within cancelGrace is failed by the
// console.

const cancelGrace = 10 * time.Minute

// timeLimitKinds are the runs of a job that its time limit applies to.
var timeLimitKinds = []string{"backup", "image-backup", "vm-backup", "system-backup", "sql-backup", "sql-log", "copy", "verify"}

var errNotCancellable = errors.New("the run has already finished")

// CancelRun cancels a queued or running run. by is the user ("" for the
// job's time limit), reason becomes the run's message. It reports whether
// the run was still waiting (and so is finished now).
func (s *Store) CancelRun(ctx context.Context, id int64, by, reason string) (queued bool, err error) {
	// A cancel by a user sends no failure alert; the time limit does.
	ct, err := s.db.Exec(ctx, `UPDATE runs SET status=CASE WHEN $3<>'' THEN 'cancelled' ELSE 'failed' END, finished_at=now(), message=$2, cancel_reason=$2, cancel_by=$3, cancel_at=now(), notified=($3<>'')
		WHERE id=$1 AND status='queued'`, id, reason, by)
	if err != nil {
		return false, err
	}
	if ct.RowsAffected() == 1 {
		return true, nil
	}
	ct, err = s.db.Exec(ctx, `UPDATE runs SET cancel_reason=$2, cancel_by=$3, cancel_at=now()
		WHERE id=$1 AND status='running' AND kind<>'agent-update' AND cancel_reason IS NULL`, id, reason, by)
	if err != nil {
		return false, err
	}
	if ct.RowsAffected() == 0 {
		var status, kind string
		if err := s.db.QueryRow(ctx, `SELECT status, kind FROM runs WHERE id=$1`, id).Scan(&status, &kind); err != nil {
			return false, notFound(err)
		}
		switch {
		case status == "running" && kind == "agent-update":
			return false, errors.New("an agent update cannot be cancelled")
		case status == "running":
			return false, nil // already being cancelled
		}
		return false, errNotCancellable
	}
	return false, nil
}

// cancelRequests lists the running runs of an agent that are to stop.
func (s *Store) cancelRequests(ctx context.Context, agentID int64) []int64 {
	rows, err := s.db.Query(ctx, `SELECT id FROM runs WHERE agent_id=$1 AND status='running' AND cancel_reason IS NOT NULL`, agentID)
	if err != nil {
		return nil
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	return ids
}

// enforceTimeLimits asks agents to stop runs that exceeded their job's time
// limit (JobOptions.MaxHours), and fails cancelled runs whose agent did not
// confirm in time. It returns the runs stopped for the time limit.
func (s *Store) enforceTimeLimits(ctx context.Context, now time.Time) ([]Run, error) {
	rows, err := s.db.Query(ctx, `SELECT r.id, COALESCE(j.options->>'max_hours','0')::int FROM runs r JOIN jobs j ON j.id=r.job_id
		WHERE r.status='running' AND r.cancel_reason IS NULL AND r.kind = ANY($1)
		AND COALESCE(j.options->>'max_hours','0')::int > 0
		AND r.started_at < $2::timestamptz - make_interval(hours => COALESCE(j.options->>'max_hours','0')::int)`, timeLimitKinds, now)
	if err != nil {
		return nil, err
	}
	type over struct {
		id    int64
		hours int
	}
	var list []over
	for rows.Next() {
		var o over
		if err := rows.Scan(&o.id, &o.hours); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, o)
	}
	rows.Close()
	var stopped []Run
	for _, o := range list {
		reason := fmt.Sprintf("Stopped: the run took longer than the job's time limit of %d h", o.hours)
		if _, err := s.CancelRun(ctx, o.id, "", reason); err != nil {
			return stopped, err
		}
		if r, err := s.GetRun(ctx, o.id); err == nil {
			stopped = append(stopped, r)
		}
	}
	// The agent did not stop the run (it may hang in an operation that
	// cannot be interrupted): fail it so the job can run again.
	_, err = s.db.Exec(ctx, `UPDATE runs SET status=CASE WHEN cancel_by<>'' THEN 'cancelled' ELSE 'failed' END, finished_at=now(), notified=(cancel_by<>''),
		message=cancel_reason || ' (the agent did not confirm the stop)'
		WHERE status='running' AND cancel_reason IS NOT NULL AND cancel_at < now() - make_interval(secs => $1)`, cancelGrace.Seconds())
	return stopped, err
}

// cancelRun cancels a run for the signed-in user (console or API).
func (s *Server) cancelRun(r *http.Request, id int64) (queued bool, err error) {
	by := "api"
	if u := currentUser(r); u != nil {
		by = u.Username
	}
	queued, err = s.store.CancelRun(r.Context(), id, by, "Cancelled by "+by)
	if err == nil {
		s.audit(r, "run.cancel", "run %d", id)
	}
	return queued, err
}

// handleRunCancel serves POST /runs/{id}/cancel.
func (s *Server) handleRunCancel(w http.ResponseWriter, r *http.Request, _ string) {
	id, _ := pathID(r)
	back := fmt.Sprintf("/runs/%d", id)
	queued, err := s.cancelRun(r, id)
	if err != nil {
		redirectErr(w, r, back, err)
		return
	}
	if queued {
		redirectMsg(w, r, back, "The run was cancelled.")
		return
	}
	redirectMsg(w, r, back, "The agent stops the run within a few seconds.")
}

// activeRuns are the queued and running runs of a list that can be stopped.
func activeRuns(runs []Run) []Run {
	var out []Run
	for _, r := range runs {
		if (r.Status == "queued" || r.Status == "running") && r.Kind != "agent-update" {
			out = append(out, r)
		}
	}
	return out
}
