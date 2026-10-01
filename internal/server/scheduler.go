package server

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"
)

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// NextRun returns the next scheduled time after t for a stored schedule,
// or zero for manual jobs.
func NextRun(stored string, t time.Time) time.Time {
	s, err := ParseSchedule(stored)
	if err != nil {
		return time.Time{}
	}
	return s.Next(t)
}

// DescribeSchedule renders a stored schedule for humans.
func DescribeSchedule(stored string) string {
	s, err := ParseSchedule(stored)
	if err != nil {
		return "invalid schedule"
	}
	return s.Describe()
}

// Scheduler queues scheduled backups and fails runs of vanished agents.
type Scheduler struct {
	store    *Store
	log      *slog.Logger
	interval time.Duration
	// StaleAfter is how long an agent may be silent while a run is in progress.
	StaleAfter time.Duration
}

func NewScheduler(store *Store, log *slog.Logger) *Scheduler {
	return &Scheduler{store: store, log: log, interval: 20 * time.Second, StaleAfter: 10 * time.Minute}
}

// Run loops until ctx is done.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		s.Tick(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick performs one scheduling pass.
func (s *Scheduler) Tick(ctx context.Context, now time.Time) {
	if n, err := s.store.FailStaleRuns(ctx, s.StaleAfter); err != nil {
		s.log.Error("fail stale runs", "err", err)
	} else if n > 0 {
		s.log.Warn("marked runs of unresponsive agents as failed", "count", n)
	}

	jobs, err := s.store.ListJobs(ctx)
	if err != nil {
		s.log.Error("list jobs", "err", err)
		return
	}
	for _, j := range jobs {
		if !j.Enabled || j.Schedule == "" {
			continue
		}
		from := j.CreatedAt
		if j.LastSched != nil {
			from = *j.LastSched
		}
		next := NextRun(j.Schedule, from)
		if next.IsZero() || next.After(now) {
			continue
		}
		// Advance the marker first so a missed window is not queued repeatedly.
		if _, err := s.store.db.Exec(ctx, `UPDATE jobs SET last_scheduled_at=$2 WHERE id=$1`, j.ID, now); err != nil {
			s.log.Error("update job schedule", "job", j.ID, "err", err)
			continue
		}
		id, err := s.store.QueueBackup(ctx, j.ID, "schedule")
		switch {
		case errors.Is(err, ErrRunActive):
			s.log.Warn("skipping scheduled backup, previous run still active", "job", j.Name)
		case err != nil:
			s.log.Error("queue scheduled backup", "job", j.ID, "err", err)
		default:
			s.log.Info("queued scheduled backup", "job", j.Name, "run", id)
		}
	}
}
