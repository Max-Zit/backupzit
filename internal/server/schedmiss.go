package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Missed schedules (B2, 0.62). A scheduled backup that is skipped because
// the job's previous run is still active, or that is cancelled because it
// did not start within the job's backup window, was only in the console
// log: a run that hangs could keep a job from backing up for days. Each
// such job now gets one notice (email and chat, with the failure
// notifications); the next notice only after the job backed up
// successfully in between and misses again.

const settingScheduleMisses = "schedule_misses"

const (
	missSkipped = "skipped" // previous run still active
	missWindow  = "window"  // not started within the backup window
)

// scheduleMiss is the state of one job's missed schedules.
type scheduleMiss struct {
	Kind     string    `json:"kind"`
	Since    time.Time `json:"since"`
	Count    int       `json:"count"`
	Notified bool      `json:"notified,omitempty"`
}

// noteScheduleMiss records a missed schedule of a job, in one transaction
// with the other writers (the notifier, LOW: before, a stale copy written
// back lost entries, and a failed read wrote over all of them).
func (s *Store) noteScheduleMiss(ctx context.Context, jobID int64, kind string, now time.Time) error {
	key := strconv.FormatInt(jobID, 10)
	return updateSettingMap(ctx, s, settingScheduleMisses, func(m map[string]scheduleMiss) bool {
		x, ok := m[key]
		if !ok {
			x = scheduleMiss{Kind: kind, Since: now}
		}
		x.Count++
		if x.Kind != kind && !x.Notified {
			x.Kind = kind
		}
		m[key] = x
		return true
	})
}

// lastSuccessAt is when the newest successful (or warning) backup of the
// job finished, zero if none.
func (s *Store) lastSuccessAt(ctx context.Context, jobID int64) time.Time {
	var t *time.Time
	_ = s.db.QueryRow(ctx, `SELECT max(finished_at) FROM runs WHERE job_id=$1 AND status IN ('success','warning')`, jobID).Scan(&t)
	if t == nil {
		return time.Time{}
	}
	return *t
}

// scheduleMissAlerts sends the notices of missed schedules and forgets the
// jobs that backed up successfully since.
func (n *Notifier) scheduleMissAlerts(ctx context.Context, l *language, e EmailSettings, chat ChatSettings) {
	m := map[string]scheduleMiss{}
	if err := n.store.GetSetting(ctx, settingScheduleMisses, &m); err != nil || len(m) == 0 {
		return
	}
	var drop, notified []string
	for key, x := range m {
		id, err := strconv.ParseInt(key, 10, 64)
		if err != nil {
			drop = append(drop, key)
			continue
		}
		j, err := n.store.GetJob(ctx, id)
		if err != nil && !errors.Is(err, ErrNotFound) {
			continue // the database did not answer: again on the next pass
		}
		if err != nil || n.store.lastSuccessAt(ctx, id).After(x.Since) {
			drop = append(drop, key) // deleted, or it backed up since
			continue
		}
		if x.Notified {
			continue
		}
		var subject, text, event string
		since := x.Since.Local().Format("2006-01-02 15:04")
		if x.Kind == missWindow {
			subject = "[BackupZit] " + l.T("Backup not started in its window: %s", j.Name)
			text = l.T("The backup of %s on %s did not start within its backup window (first missed at %s).", j.Name, j.Hostname, since)
			event = "schedule.window_missed"
		} else {
			subject = "[BackupZit] " + l.T("Scheduled backup skipped: %s", j.Name)
			text = l.T("The scheduled backup of %s on %s was skipped because its previous run is still active (first skipped at %s).", j.Name, j.Hostname, since)
			event = "schedule.skipped"
		}
		var b strings.Builder
		b.WriteString(text + "\n\n")
		b.WriteString(l.T("Check the job's running backup and the agent; until a backup succeeds, no further notice is sent for this job.") + "\n")
		if u := n.publicURL(); u != "" {
			fmt.Fprintf(&b, "\n%s %s/jobs/%d\n", l.T("Details:"), u, j.ID)
		}
		if e.Enabled && e.OnFailure && e.Host != "" && len(e.To) > 0 {
			if err := n.send(ctx, e, subject, b.String()); err != nil {
				n.log.Warn("send missed schedule notification", "job", j.ID, "err", err)
				continue // again on the next pass
			}
		}
		for _, c := range chat.Channels {
			if !c.OnFailure {
				continue
			}
			if err := n.sendChatText(ctx, l, c, strings.TrimPrefix(subject, "[BackupZit] "), text, fmt.Sprintf("/jobs/%d", j.ID), map[string]any{
				"event": event, "job_id": j.ID, "job": j.Name, "agent": j.Hostname, "count": x.Count, "since": x.Since}); err != nil {
				n.log.Warn("send missed schedule chat notification", "job", j.ID, "channel", c.Name, "err", err)
			}
		}
		notified = append(notified, key)
	}
	// Only what this pass did is written: misses noted meanwhile stay.
	if err := settleNotices(ctx, n.store, settingScheduleMisses, m, drop, notified,
		func(x scheduleMiss) time.Time { return x.Since }, func(x scheduleMiss) scheduleMiss { x.Notified = true; return x }); err != nil {
		n.log.Error("save missed schedules", "err", err)
	}
}
