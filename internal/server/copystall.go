package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/api"
)

// Stalled copies (#75). A copy run that moves no data for copyStallAfter
// while its agent still polls gets a warning on the run page and one
// notice (email and chat, with the failure notifications). The run is not
// failed: slow storage may pause and recover, and a run whose agent stops
// answering is failed by FailStaleRuns anyway.

const settingCopyStalls = "copy_stalls"

// copyStall is a stalled copy run waiting for its notice.
type copyStall struct {
	Since    time.Time `json:"since"`
	Notified bool      `json:"notified,omitempty"`
}

// stalledCopies returns the copy runs (in the transfer step) that have
// moved no data for copyStallAfter and were not returned before.
func (p *liveProgress) stalledCopies(now time.Time) map[int64]time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out map[int64]time.Time
	for id, e := range p.m {
		if e.Stage != "copy" || e.stallNoted || now.Sub(e.movedAt) < copyStallAfter {
			continue
		}
		e.stallNoted = true
		p.m[id] = e
		if out == nil {
			out = map[int64]time.Time{}
		}
		out[id] = e.movedAt
	}
	return out
}

// noteCopyStalls records the stalled copy runs for their notice.
func (s *Server) noteCopyStalls(ctx context.Context) {
	stalled := s.live.stalledCopies(s.clock())
	if len(stalled) == 0 {
		return
	}
	add := map[string]copyStall{}
	for id, since := range stalled {
		run, err := s.store.GetRun(ctx, id)
		if err != nil || run.Kind != api.KindCopy || run.Status != "running" {
			continue
		}
		s.log.Warn("copy run moves no data, the agent still answers", "run", id, "since", since)
		add[strconv.FormatInt(id, 10)] = copyStall{Since: since}
	}
	if len(add) == 0 {
		return
	}
	// Added to what is stored, in one transaction (polls of several agents
	// and the notifier write it at the same time).
	if err := updateSettingMap(ctx, s.store, settingCopyStalls, func(m map[string]copyStall) bool {
		for k, x := range add {
			m[k] = x
		}
		return true
	}); err != nil {
		s.log.Error("save stalled copies", "err", err)
	}
}

// copyStallAlerts sends the notices of stalled copy runs and forgets the
// runs that ended.
func (n *Notifier) copyStallAlerts(ctx context.Context, l *language, e EmailSettings, chat ChatSettings) {
	m := map[string]copyStall{}
	if err := n.store.GetSetting(ctx, settingCopyStalls, &m); err != nil || len(m) == 0 {
		return
	}
	var drop, notified []string
	for key, x := range m {
		id, err := strconv.ParseInt(key, 10, 64)
		if err != nil {
			drop = append(drop, key)
			continue
		}
		run, err := n.store.GetRun(ctx, id)
		if err != nil && !errors.Is(err, ErrNotFound) {
			continue // the database did not answer: again on the next pass
		}
		if err != nil || run.Status != "running" {
			drop = append(drop, key) // ended (or gone): nothing to report any more
			continue
		}
		if x.Notified {
			continue
		}
		name := ""
		if run.JobName != nil {
			name = *run.JobName
		}
		since := x.Since.Local().Format("2006-01-02 15:04")
		subject := "[BackupZit] " + l.T("Copy moves no data: %s", name)
		text := l.T("The copy run %d of %s has moved no data since %s, although its agent still answers. It keeps running; check the source and destination storage and the agent log.", run.ID, name, since)
		var b strings.Builder
		b.WriteString(text + "\n")
		if u := n.publicURL(); u != "" {
			fmt.Fprintf(&b, "\n%s %s/runs/%d\n", l.T("Details:"), u, run.ID)
		}
		if e.Enabled && e.OnFailure && e.Host != "" && len(e.To) > 0 {
			if err := n.send(ctx, e, subject, b.String()); err != nil {
				n.log.Warn("send stalled copy notification", "run", run.ID, "err", err)
				continue // again on the next pass
			}
		}
		for _, c := range chat.Channels {
			if !c.OnFailure {
				continue
			}
			if err := n.sendChatText(ctx, l, c, strings.TrimPrefix(subject, "[BackupZit] "), text, fmt.Sprintf("/runs/%d", run.ID), map[string]any{
				"event": "copy.stalled", "run_id": run.ID, "job": name, "since": x.Since}); err != nil {
				n.log.Warn("send stalled copy chat notification", "run", run.ID, "channel", c.Name, "err", err)
			}
		}
		notified = append(notified, key)
	}
	// Only what this pass did is written: stalls noted meanwhile stay.
	if err := settleNotices(ctx, n.store, settingCopyStalls, m, drop, notified,
		func(x copyStall) time.Time { return x.Since }, func(x copyStall) copyStall { x.Notified = true; return x }); err != nil {
		n.log.Error("save stalled copies", "err", err)
	}
}
