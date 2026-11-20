package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Recovery point objective: a job may set how old its newest good backup
// may get (JobOptions.RPOHours). The dashboard counts the jobs within it,
// lists the others, and a notification goes out once when a job misses it.

// RPOState is a job's standing against its RPO.
type RPOState struct {
	Hours    int
	LastGood *time.Time
	// Missed is true when the newest good backup (or, without one, the job
	// itself) is older than the RPO.
	Missed bool
	// Age is how old the newest good backup is (0 without one).
	Age time.Duration
}

// rpoState computes a job's RPO standing (Hours 0: no RPO set).
func rpoState(j Job, c *JobCoverage, now time.Time) RPOState {
	st := RPOState{Hours: j.Options.RPOHours}
	if c != nil {
		st.LastGood = c.LastOK
	}
	if st.LastGood != nil {
		st.Age = now.Sub(*st.LastGood)
	}
	if st.Hours <= 0 || !j.Enabled {
		return st
	}
	limit := time.Duration(st.Hours) * time.Hour
	if st.LastGood == nil {
		st.Missed = now.Sub(j.CreatedAt) > limit
	} else {
		st.Missed = st.Age > limit
	}
	return st
}

// RPOSummary counts the enabled jobs with an RPO.
type RPOSummary struct {
	Jobs, Met int
}

func (s *Server) rpoSummary(ctx context.Context, now time.Time) *RPOSummary {
	jobs, err := s.store.ListJobs(ctx)
	if err != nil {
		return nil
	}
	cov, _ := s.store.jobCoverage(ctx)
	var x RPOSummary
	for _, j := range jobs {
		if !j.Enabled || j.Options.RPOHours <= 0 {
			continue
		}
		x.Jobs++
		if !rpoState(j, cov[j.ID], now).Missed {
			x.Met++
		}
	}
	if x.Jobs == 0 {
		return nil
	}
	return &x
}

// rpoText describes a missed RPO, e.g. "no good backup for 30 hours (RPO
// 24 h)".
func rpoText(l *language, st RPOState) string {
	if st.LastGood == nil {
		return l.T("no good backup yet (RPO %d h)", st.Hours)
	}
	return l.T("newest good backup is %s old (RPO %d h)", humanAge(l, st.Age), st.Hours)
}

const settingRPOAlerts = "rpo_alerts"

// settingLeftoverAlerts holds the leftovers already notified (0.56).
const settingLeftoverAlerts = "leftover_alerts"

// settingRPOChatAlerts: the jobs whose RPO chat alert went out (0.55).
const settingRPOChatAlerts = "rpo_chat_alerts"

// rpoAlerts notifies once when a job misses its RPO (again after it met it
// in between).
func (n *Notifier) rpoAlerts(ctx context.Context, l *language, e EmailSettings, chat ChatSettings, now time.Time) {
	jobs, err := n.store.ListJobs(ctx)
	if err != nil {
		return
	}
	cov, err := n.store.jobCoverage(ctx)
	if err != nil {
		return
	}
	// Email and chat are tracked apart: a failing mail server retries the
	// email on every pass, the chat alert goes once all the same.
	sent := map[string]bool{}
	_ = n.store.GetSetting(ctx, settingRPOAlerts, &sent)
	var chatSent map[string]bool // stays nil while the setting does not exist
	if n.store.GetSetting(ctx, settingRPOChatAlerts, &chatSent) != nil || chatSent == nil {
		chatSent = map[string]bool{}
		for k := range sent { // before 0.55 both went together
			chatSent[k] = true
		}
	}
	next, nextChat := map[string]bool{}, map[string]bool{}
	for _, j := range jobs {
		st := rpoState(j, cov[j.ID], now)
		key := strconv.FormatInt(j.ID, 10)
		if !st.Missed {
			continue
		}
		next[key], nextChat[key] = true, true
		if sent[key] && chatSent[key] {
			continue
		}
		subject := "[BackupZit] " + l.T("RPO missed: %s", j.Name)
		var b strings.Builder
		b.WriteString(l.T("The job %s on %s missed its recovery point objective: %s.", j.Name, j.Hostname, rpoText(l, st)) + "\n\n")
		b.WriteString(l.T("Check the job's last runs and the agent; the dashboard lists every job outside its RPO.") + "\n")
		if u := n.publicURL(); u != "" {
			fmt.Fprintf(&b, "\n%s %s/jobs/%d\n", l.T("Details:"), u, j.ID)
		}
		if !sent[key] && e.Enabled && e.OnFailure && e.Host != "" && len(e.To) > 0 {
			if err := n.send(ctx, e, subject, b.String()); err != nil {
				n.log.Warn("send RPO notification", "job", j.ID, "err", err)
				delete(next, key) // try again on the next pass
			}
		}
		if chatSent[key] {
			continue
		}
		for _, c := range chat.Channels {
			if !c.OnFailure {
				continue
			}
			if err := n.sendChatText(ctx, l, c, strings.TrimPrefix(subject, "[BackupZit] "), rpoText(l, st), fmt.Sprintf("/jobs/%d", j.ID), map[string]any{
				"event": "rpo.missed", "job_id": j.ID, "job": j.Name, "agent": j.Hostname, "rpo_hours": st.Hours, "last_good_backup": st.LastGood}); err != nil {
				n.log.Warn("send RPO chat notification", "job", j.ID, "channel", c.Name, "err", err)
			}
		}
		n.log.Warn("job missed its RPO", "job", j.Name, "rpo_hours", st.Hours)
	}
	if b1, _ := json.Marshal(sent); string(b1) != mustJSON(next) {
		_ = n.store.SetSetting(ctx, settingRPOAlerts, next)
	}
	if b2, _ := json.Marshal(chatSent); string(b2) != mustJSON(nextChat) {
		_ = n.store.SetSetting(ctx, settingRPOChatAlerts, nextChat)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
