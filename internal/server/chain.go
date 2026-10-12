package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/api"
)

// Job chains: a job with a SchedChain schedule starts when another job's
// backup has finished (by default only when it succeeded), e.g. first the
// database, then the file server.

// chainKinds are the runs that start the jobs chained after their job (SQL
// log backups every few minutes do not).
var chainKinds = map[string]bool{api.KindBackup: true, api.KindImageBackup: true, api.KindVMBackup: true,
	api.KindSystemBackup: true, api.KindSQLBackup: true, api.KindCopy: true}

// checkChain refuses a chain to a missing job, to the job itself or one that
// would loop back to it.
func (s *Store) checkChain(ctx context.Context, jobID, after int64) error {
	if after == jobID && jobID != 0 {
		return errors.New("a job cannot run after itself")
	}
	cur := after
	for i := 0; i < 50; i++ {
		j, err := s.GetJob(ctx, cur)
		if err != nil {
			if i == 0 {
				return errors.New("the job to run after does not exist")
			}
			return nil
		}
		sc, err := ParseSchedule(j.Schedule)
		if err != nil || sc.Kind != SchedChain {
			return nil
		}
		if sc.AfterJob == jobID && jobID != 0 {
			return errors.New("the chain of jobs would run in a circle")
		}
		cur = sc.AfterJob
	}
	return errors.New("the chain of jobs is too long")
}

// QueueChainAfter queues the jobs chained after a job whose run finished
// (ok: it succeeded or finished with warnings).
func (s *Store) QueueChainAfter(ctx context.Context, jobID int64, ok bool) ([]int64, error) {
	rows, err := s.db.Query(ctx, `SELECT id, schedule FROM jobs WHERE enabled AND schedule LIKE '%"kind":"chain"%'`)
	if err != nil {
		return nil, err
	}
	type cand struct {
		id    int64
		sched string
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.sched); err != nil {
			rows.Close()
			return nil, err
		}
		cands = append(cands, c)
	}
	rows.Close()
	var queued []int64
	for _, c := range cands {
		sc, err := ParseSchedule(c.sched)
		if err != nil || sc.Kind != SchedChain || sc.AfterJob != jobID || !(ok || sc.AfterAny) {
			continue
		}
		rid, err := s.QueueBackup(ctx, c.id, "after-job")
		if errors.Is(err, ErrRunActive) {
			continue
		}
		if err != nil {
			return queued, err
		}
		queued = append(queued, rid)
	}
	return queued, nil
}

// chainAfterRun starts the jobs chained after a finished run's job.
func (s *Server) chainAfterRun(ctx context.Context, runID int64) {
	run, err := s.store.GetRun(ctx, runID)
	if err != nil || run.JobID == nil || !chainKinds[run.Kind] || run.Status == api.StatusCancelled {
		return
	}
	ok := run.Status == api.StatusSuccess || run.Status == api.StatusWarning
	ids, err := s.store.QueueChainAfter(ctx, *run.JobID, ok)
	if err != nil {
		s.log.Error("queue chained jobs", "job", *run.JobID, "err", err)
	} else if len(ids) > 0 {
		s.log.Info("chained jobs queued", "after_job", *run.JobID, "runs", ids)
	}
}

// chainTargets are the jobs a job can run after (not itself).
func chainTargets(jobs []Job, self int64) []Job {
	var out []Job
	for _, j := range jobs {
		if j.ID != self && !strings.EqualFold(j.Kind, "") {
			out = append(out, j)
		}
	}
	return out
}

// windowClosedTmpl is an SQL condition on runs r joined with jobs j: the
// run waits for its job's backup window (scheduled and chained runs only;
// $H stands for the parameter with the console's current hour).
const windowClosedTmpl = `(r.trigger IN ('schedule','after-job','after-backup') AND COALESCE((j.options->>'window')::boolean, false)
	AND NOT (CASE WHEN COALESCE((j.options->>'window_from')::int,0) < COALESCE((j.options->>'window_to')::int,0)
		THEN $H >= COALESCE((j.options->>'window_from')::int,0) AND $H < COALESCE((j.options->>'window_to')::int,0)
		ELSE $H >= COALESCE((j.options->>'window_from')::int,0) OR $H < COALESCE((j.options->>'window_to')::int,0) END))`

// expireWindowRuns cancels scheduled and chained runs that could not start
// within their job's backup window (queued over a minute ago, window closed
// now), so they do not start during working hours.
func (s *Store) expireWindowRuns(ctx context.Context, now time.Time) (int64, error) {
	ct, err := s.db.Exec(ctx, `UPDATE runs r SET status='cancelled', finished_at=now(), notified=true,
		message=format('Not started: the backup window %s:00–%s:00 closed', lpad(COALESCE(j.options->>'window_from','0'),2,'0'), lpad(COALESCE(j.options->>'window_to','0'),2,'0'))
		FROM jobs j WHERE j.id=r.job_id AND r.status='queued' AND r.queued_at < $2 AND `+windowClosed(1),
		now.Hour(), now.Add(-time.Minute))
	return ct.RowsAffected(), err
}

// windowClosed is windowClosedTmpl with the hour as parameter $n.
func windowClosed(n int) string {
	return strings.ReplaceAll(windowClosedTmpl, "$H", fmt.Sprintf("$%d", n))
}

// afterJob is the job a chained job runs after (nil if none).
func (s *Server) afterJob(ctx context.Context, j Job) *Job {
	sc, err := ParseSchedule(j.Schedule)
	if err != nil || sc.Kind != SchedChain {
		return nil
	}
	a, err := s.store.GetJob(ctx, sc.AfterJob)
	if err != nil {
		return nil
	}
	return &a
}
