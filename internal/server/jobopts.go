package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/backupzit/backupzit/internal/api"
	"github.com/backupzit/backupzit/internal/repo"
)

// JobOptions are optional settings of a job.
type JobOptions struct {
	// LimitMBps limits the upload speed to the storage (MB/s; 0 = no
	// limit), between LimitFrom and LimitTo o'clock on the agent (equal:
	// all day).
	LimitMBps int `json:"limit_mbps,omitempty"`
	LimitFrom int `json:"limit_from,omitempty"`
	LimitTo   int `json:"limit_to,omitempty"`
	// PreCommand and PostCommand run on the agent before and after the
	// backup (sh -c on Linux, cmd /c on Windows), as the agent's account.
	PreCommand  string `json:"pre_command,omitempty"`
	PostCommand string `json:"post_command,omitempty"`
	// CommandMinutes is the time limit of each command (default 30).
	CommandMinutes int `json:"command_minutes,omitempty"`
}

func (o JobOptions) Validate() error {
	if o.LimitMBps < 0 || o.LimitMBps > 100000 {
		return errors.New("the speed limit is between 1 and 100000 MB/s (0 for none)")
	}
	if o.LimitFrom < 0 || o.LimitFrom > 23 || o.LimitTo < 0 || o.LimitTo > 24 {
		return errors.New("the speed limit hours are between 0 and 24")
	}
	for _, c := range []string{o.PreCommand, o.PostCommand} {
		if len(c) > 4000 || strings.ContainsRune(c, 0) {
			return errors.New("commands have at most 4000 characters")
		}
	}
	if o.CommandMinutes < 0 || o.CommandMinutes > 1440 {
		return errors.New("the command time limit is between 1 and 1440 minutes")
	}
	return nil
}

// HasCommands reports whether the job runs commands on the agent.
func (o JobOptions) HasCommands() bool { return o.PreCommand != "" || o.PostCommand != "" }

// LimitText describes the speed limit for the job page.
func (o JobOptions) LimitText() string {
	if o.LimitMBps == 0 {
		return "no limit"
	}
	s := fmt.Sprintf("%d MB/s", o.LimitMBps)
	if o.LimitFrom != o.LimitTo {
		s += fmt.Sprintf(" from %02d:00 to %02d:00, unlimited otherwise", o.LimitFrom, o.LimitTo)
	}
	return s
}

// apiOptions are the job options an agent needs for a run.
func (o JobOptions) apply(ar *api.Run, kind string) {
	if o.LimitMBps > 0 {
		ar.Limit = &api.SpeedLimit{BytesPerSec: int64(o.LimitMBps) << 20, FromHour: o.LimitFrom, ToHour: o.LimitTo}
	}
	if kind != api.KindCopy && o.HasCommands() {
		min := o.CommandMinutes
		if min == 0 {
			min = 30
		}
		ar.PreCommand, ar.PostCommand, ar.CommandSeconds = o.PreCommand, o.PostCommand, min*60
	}
}

// UpdateJob changes the editable settings of a job: name, what it backs
// up (not the disk of image jobs), schedule, retention and options. Agent,
// storage and kind stay.
func (s *Store) UpdateJob(ctx context.Context, j Job) error {
	old, err := s.GetJob(ctx, j.ID)
	if err != nil {
		return err
	}
	j.Kind, j.AgentID, j.TargetID, j.SourceJobID, j.VMwareHostID = old.Kind, old.AgentID, old.TargetID, old.SourceJobID, old.VMwareHostID
	j.ImageDisk, j.ImagePartitions = old.ImageDisk, old.ImagePartitions
	switch j.Kind {
	case JobImage, JobCopy:
		j.Paths, j.Excludes = old.Paths, old.Excludes
	}
	if j.Kind == JobCopy {
		j.Options.PreCommand, j.Options.PostCommand = "", ""
	}
	if err := s.checkJob(ctx, &j); err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `UPDATE jobs SET name=$2, paths=$3, excludes=$4, schedule=$5, retention=$6, options=$7,
		last_scheduled_at=CASE WHEN schedule IS DISTINCT FROM $5 THEN now() ELSE last_scheduled_at END WHERE id=$1`,
		j.ID, j.Name, j.Paths, j.Excludes, j.Schedule, j.Retention, j.Options)
	return err
}

// jobForm fills the retention, schedule and options fields of the job forms
// (templates/_jobform.html).
type jobForm struct {
	J           Job
	S           Schedule
	Edit        bool
	CanCommands bool
	Times       [3]string
	Days        map[int]bool
	Window      bool
	From, To    int
	MonthTime   string
	LimitFrom   int
	LimitTo     int
}

// newJobForm prepares the form for job j (a new job: defaults).
func newJobForm(j Job, edit, canCommands bool) jobForm {
	f := jobForm{J: j, Edit: edit, CanCommands: canCommands, Days: map[int]bool{}, From: 8, To: 18, MonthTime: "22:00", LimitFrom: 8, LimitTo: 18}
	if !edit {
		f.J.Retention = repo.RetentionPolicy{KeepLast: 3, KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6}
		f.S = Schedule{Kind: SchedDaily, Times: []string{"22:00"}, EveryMinutes: 240, DayOfMonth: 1}
	} else if sc, err := ParseSchedule(j.Schedule); err == nil {
		f.S = sc
	} else {
		f.S = Schedule{Kind: SchedManual}
	}
	if f.S.Kind == "cron" {
		f.S = Schedule{Kind: SchedDaily, Times: []string{"22:00"}} // shown as a new choice
	}
	if f.S.EveryMinutes == 0 {
		f.S.EveryMinutes = 240
	}
	if f.S.DayOfMonth == 0 {
		f.S.DayOfMonth = 1
	}
	if f.S.Kind == SchedMonthly && len(f.S.Times) > 0 {
		f.MonthTime = f.S.Times[0]
	} else {
		copy(f.Times[:], f.S.Times)
	}
	if f.Times[0] == "" {
		f.Times[0] = "22:00"
	}
	for _, d := range f.S.Days {
		f.Days[d] = true
	}
	if len(f.S.Days) == 0 {
		for d := 0; d < 7; d++ {
			f.Days[d] = true
		}
	}
	if f.S.FromHour != f.S.ToHour {
		f.Window, f.From, f.To = true, f.S.FromHour, f.S.ToHour
	}
	if o := j.Options; o.LimitFrom != o.LimitTo {
		f.LimitFrom, f.LimitTo = o.LimitFrom, o.LimitTo
		if f.LimitTo == 0 {
			f.LimitTo = 24
		}
	}
	return f
}

// optionsFromForm reads the speed limit and commands of a job form. Users
// who may not change commands keep the job's current ones.
func optionsFromForm(r *http.Request, old JobOptions, canCommands bool) JobOptions {
	o := JobOptions{LimitMBps: atoiDefault(r.FormValue("limit_mbps"))}
	if o.LimitMBps > 0 && r.FormValue("limit_window") == "on" {
		o.LimitFrom, o.LimitTo = atoiDefault(r.FormValue("limit_from")), atoiDefault(r.FormValue("limit_to"))
		if o.LimitTo == 24 {
			o.LimitTo = 0
		}
	}
	if canCommands {
		o.PreCommand = strings.TrimSpace(strings.ReplaceAll(r.FormValue("pre_command"), "\r\n", "\n"))
		o.PostCommand = strings.TrimSpace(strings.ReplaceAll(r.FormValue("post_command"), "\r\n", "\n"))
		if o.HasCommands() {
			o.CommandMinutes = atoiDefault(r.FormValue("command_minutes"))
			if o.CommandMinutes == 30 {
				o.CommandMinutes = 0
			}
		}
	} else {
		o.PreCommand, o.PostCommand, o.CommandMinutes = old.PreCommand, old.PostCommand, old.CommandMinutes
	}
	return o
}

func retentionFromForm(r *http.Request) repo.RetentionPolicy {
	return repo.RetentionPolicy{
		KeepLast:    atoiDefault(r.FormValue("keep_last")),
		KeepDaily:   atoiDefault(r.FormValue("keep_daily")),
		KeepWeekly:  atoiDefault(r.FormValue("keep_weekly")),
		KeepMonthly: atoiDefault(r.FormValue("keep_monthly")),
	}
}

func canCommands(r *http.Request) bool { return currentUser(r).Can(string(PermSettings)) }

// handleJobEdit serves POST /jobs/{id}/edit.
func (s *Server) handleJobEdit(w http.ResponseWriter, r *http.Request, _ string) {
	r.ParseForm()
	id, _ := pathID(r)
	back := fmt.Sprintf("/jobs/%d", id)
	old, err := s.store.GetJob(r.Context(), id)
	if err != nil {
		redirectErr(w, r, back, err)
		return
	}
	sched, err := scheduleFromForm(r)
	if err != nil {
		redirectErr(w, r, back+"#edit", err)
		return
	}
	j := old
	j.Name, j.Schedule, j.Retention = r.FormValue("name"), sched, retentionFromForm(r)
	j.Options = optionsFromForm(r, old.Options, canCommands(r))
	switch old.Kind {
	case JobFiles:
		j.Paths, j.Excludes = lines(r.FormValue("paths")), lines(r.FormValue("excludes"))
	case JobSystem:
		j.Excludes = lines(r.FormValue("excludes"))
	case JobVM:
		if r.FormValue("vm_mode") == "all" {
			j.Paths = []string{"*"}
		} else {
			j.Paths = r.Form["vms"]
		}
	}
	if err := s.store.UpdateJob(r.Context(), j); err != nil {
		redirectErr(w, r, back+"#edit", err)
		return
	}
	s.audit(r, "job.update", "#%d %s%s", id, j.Name, commandsAudit(old.Options, j.Options))
	redirectMsg(w, r, back, "Job saved.")
}

// commandsAudit notes changed commands in the audit log.
func commandsAudit(old, cur JobOptions) string {
	if old.PreCommand == cur.PreCommand && old.PostCommand == cur.PostCommand {
		return ""
	}
	return fmt.Sprintf("; commands changed: before=%q after=%q", cur.PreCommand, cur.PostCommand)
}

func everyChoices() []int { return []int{15, 30, 60, 120, 180, 240, 360, 480, 720} }

func minutesText(m int) string {
	if m < 60 {
		return fmt.Sprintf("%d minutes", m)
	}
	if m == 60 {
		return "1 hour"
	}
	return fmt.Sprintf("%d hours", m/60)
}
